package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktr "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// inboundToOpenAI translates a request payload from the client's protocol into
// OpenAI chat-completions (the format this executor consumes), using the SDK's
// built-in translators. A payload already in OpenAI form is returned unchanged.
func inboundToOpenAI(opts clipexec.Options, model string, payload []byte) []byte {
	src := opts.SourceFormat
	if src == "" || src == sdktr.FormatOpenAI {
		return payload
	}
	return sdktr.TranslateRequest(src, sdktr.FormatOpenAI, model, payload, opts.Stream)
}

// responseFormat returns the protocol the client expects back.
func responseFormat(opts clipexec.Options) sdktr.Format {
	if opts.ResponseFormat != "" {
		return opts.ResponseFormat
	}
	if opts.SourceFormat != "" {
		return opts.SourceFormat
	}
	return sdktr.FormatOpenAI
}

// codeiumExecutor talks to the Codeium/Devin GetChatMessage backend and speaks
// OpenAI chat-completions in and out (the registered translator is identity).
type codeiumExecutor struct{}

func (codeiumExecutor) Identifier() string { return providerKey }

func (codeiumExecutor) PrepareRequest(*http.Request, *coreauth.Auth) error { return nil }

func (codeiumExecutor) CountTokens(context.Context, *coreauth.Auth, clipexec.Request, clipexec.Options) (clipexec.Response, error) {
	return clipexec.Response{}, errors.New("codeium: count tokens not supported")
}

// Refresh proactively mints a fresh JWT so scheduling sees a healthy auth.
func (codeiumExecutor) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	cfg := configFromAuth(a)
	if _, err := getValidJWT(ctx, buildHTTPClient(a), cfg); err != nil {
		return a, err
	}
	return a, nil
}

func (e codeiumExecutor) HttpRequest(ctx context.Context, a *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codeium: nil request")
	}
	return buildHTTPClient(a).Do(req.WithContext(ctx))
}

// Execute performs a non-streaming completion by draining the upstream stream.
func (e codeiumExecutor) Execute(ctx context.Context, a *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (clipexec.Response, error) {
	cfg := configFromAuth(a)
	preparedRequest, client, errPrepare := prepareExecutorOpenAIRequest(
		ctx,
		a,
		cfg,
		opts,
		req.Model,
		req.Payload,
	)
	if errPrepare != nil {
		return clipexec.Response{}, errPrepare
	}
	compatibilityModes := []toolCompatibilityMode{
		toolCompatibilityNormal,
		toolCompatibilityFallback,
	}
	var body []byte
	model := req.Model
	for attemptIndex, compatibilityMode := range compatibilityModes {
		response, compatibility, errOpen := e.upstream(
			ctx,
			cfg,
			client,
			preparedRequest,
			compatibilityMode,
		)
		if errOpen != nil {
			return clipexec.Response{}, errOpen
		}

		var errDrain error
		body, model, errDrain = drainNonStreamResponse(response.Body, req.Model, compatibility)
		_ = response.Body.Close()
		shouldRetry := attemptIndex == 0 &&
			len(compatibility.originalNameByCompatibleName) > 0 &&
			isMCPConfigurationError(errDrain)
		if shouldRetry {
			continue
		}
		if errDrain != nil {
			return clipexec.Response{}, errDrain
		}
		break
	}

	// Translate the OpenAI response into the client's protocol (identity for
	// /v1/chat/completions).
	if rf := responseFormat(opts); rf != sdktr.FormatOpenAI {
		var param any
		body = sdktr.TranslateNonStream(ctx, sdktr.FormatOpenAI, rf, model, req.Payload, req.Payload, body, &param)
	}
	return clipexec.Response{Payload: body}, nil
}

func drainNonStreamResponse(
	responseBody io.Reader,
	requestedModel string,
	compatibility toolCompatibility,
) ([]byte, string, error) {
	var content, reasoning strings.Builder
	var tools []*toolAcc
	toolIndexes := map[string]int{}
	activeToolIndex := -1
	model := requestedModel
	envelopeReader := newEnvelopeReader(responseBody)
	for {
		frame, errRead := envelopeReader.read()
		if errRead == io.EOF {
			break
		}
		if errRead != nil {
			return nil, model, errRead
		}
		if frame.end {
			if errTrailer := trailerError(frame.body); errTrailer != nil {
				return nil, model, errTrailer
			}
			continue
		}
		delta := parseResponseFrame(frame.body)
		for toolIndex := range delta.tools {
			delta.tools[toolIndex].name = compatibility.restoreName(delta.tools[toolIndex].name)
		}
		if delta.model != "" {
			model = delta.model
		}
		content.WriteString(delta.content)
		reasoning.WriteString(delta.reasoning)
		tools, activeToolIndex = accumulateToolDeltas(tools, toolIndexes, activeToolIndex, delta.tools)
	}

	completion := openAICompletion(model, content.String(), reasoning.String(), tools)
	completionJSON, errMarshal := json.Marshal(completion)
	return completionJSON, model, errMarshal
}

// ExecuteStream performs a streaming completion, emitting OpenAI SSE chunks.
func (e codeiumExecutor) ExecuteStream(ctx context.Context, a *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (*clipexec.StreamResult, error) {
	cfg := configFromAuth(a)
	preparedRequest, client, errPrepare := prepareExecutorOpenAIRequest(
		ctx,
		a,
		cfg,
		opts,
		req.Model,
		req.Payload,
	)
	if errPrepare != nil {
		return nil, errPrepare
	}
	response, compatibility, errOpen := e.upstream(
		ctx,
		cfg,
		client,
		preparedRequest,
		toolCompatibilityNormal,
	)
	if errOpen != nil {
		return nil, errOpen
	}
	responseHeaders := response.Header.Clone()

	requestedResponseFormat := responseFormat(opts)
	translateResponse := requestedResponseFormat != sdktr.FormatOpenAI

	streamChunks := make(chan clipexec.StreamChunk)
	go func() {
		defer close(streamChunks)

		// send delivers a chunk, but aborts if the caller cancelled (client
		// disconnect) so the goroutine never leaks blocking on the channel.
		send := func(chunk clipexec.StreamChunk) bool {
			select {
			case streamChunks <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		// emit forwards one OpenAI SSE chunk, translating it into the client's
		// protocol when needed (stateful across the stream via param).
		var translationState any
		emit := func(openaiChunk []byte) bool {
			if !translateResponse {
				return send(clipexec.StreamChunk{Payload: openaiChunk})
			}
			translatedChunks := sdktr.TranslateStream(
				ctx,
				sdktr.FormatOpenAI,
				requestedResponseFormat,
				req.Model,
				req.Payload,
				req.Payload,
				openaiChunk,
				&translationState,
			)
			for _, translatedChunk := range translatedChunks {
				if !send(clipexec.StreamChunk{Payload: translatedChunk}) {
					return false
				}
			}
			return true
		}

		currentResponse := response
		currentCompatibility := compatibility
		for attemptIndex := 0; attemptIndex < 2; attemptIndex++ {
			emittedOutput, errForward := forwardSDKStreamResponse(
				currentResponse.Body,
				req.Model,
				currentCompatibility,
				emit,
			)
			_ = currentResponse.Body.Close()

			shouldRetry := attemptIndex == 0 &&
				!emittedOutput &&
				len(currentCompatibility.originalNameByCompatibleName) > 0 &&
				isMCPConfigurationError(errForward)
			if shouldRetry {
				translationState = nil
				var errRetry error
				currentResponse, currentCompatibility, errRetry = e.upstream(
					ctx,
					cfg,
					client,
					preparedRequest,
					toolCompatibilityFallback,
				)
				if errRetry != nil {
					send(clipexec.StreamChunk{Err: errRetry})
					return
				}
				continue
			}
			if errForward != nil {
				send(clipexec.StreamChunk{Err: errForward})
			}
			return
		}
	}()

	return &clipexec.StreamResult{Chunks: streamChunks, Headers: responseHeaders}, nil
}

func forwardSDKStreamResponse(
	responseBody io.Reader,
	requestedModel string,
	compatibility toolCompatibility,
	emit func([]byte) bool,
) (bool, error) {
	completionID := "chatcmpl-" + nowID()
	model := requestedModel
	firstChunk := true
	roleForNextChunk := func() string {
		if firstChunk {
			firstChunk = false
			return "assistant"
		}
		return ""
	}
	toolIndexes := map[string]int{}
	activeToolIndex := -1
	sawTool := false
	emittedOutput := false
	envelopeReader := newEnvelopeReader(responseBody)
	for {
		frame, errRead := envelopeReader.read()
		if errRead == io.EOF {
			break
		}
		if errRead != nil {
			return emittedOutput, errRead
		}
		if frame.end {
			if errTrailer := trailerError(frame.body); errTrailer != nil {
				return emittedOutput, errTrailer
			}
			continue
		}

		delta := parseResponseFrame(frame.body)
		for toolIndex := range delta.tools {
			delta.tools[toolIndex].name = compatibility.restoreName(delta.tools[toolIndex].name)
		}
		if delta.model != "" {
			model = delta.model
		}
		if delta.content != "" || delta.reasoning != "" {
			if !emit(sseChunk(completionID, model, roleForNextChunk(), delta.content, delta.reasoning, "")) {
				return emittedOutput, context.Canceled
			}
			emittedOutput = true
		}
		for _, tool := range delta.tools {
			if tool.id != "" {
				toolIndex, alreadyStarted := toolIndexes[tool.id]
				if !alreadyStarted {
					toolIndex = len(toolIndexes)
					toolIndexes[tool.id] = toolIndex
					sawTool = true
					if !emit(sseToolChunk(completionID, model, roleForNextChunk(), toolIndex, tool.id, tool.name, "")) {
						return emittedOutput, context.Canceled
					}
					emittedOutput = true
				}
				activeToolIndex = toolIndex
			}
			if tool.args != "" && activeToolIndex >= 0 {
				if !emit(sseToolChunk(completionID, model, "", activeToolIndex, "", "", tool.args)) {
					return emittedOutput, context.Canceled
				}
				emittedOutput = true
			}
		}
	}

	finishReason := "stop"
	if sawTool {
		finishReason = "tool_calls"
	}
	if !emit(sseChunk(completionID, model, "", "", "", finishReason)) {
		return emittedOutput, context.Canceled
	}
	emittedOutput = true
	if !emit([]byte("data: [DONE]\n\n")) {
		return emittedOutput, context.Canceled
	}
	return emittedOutput, nil
}

func prepareExecutorOpenAIRequest(
	ctx context.Context,
	a *coreauth.Auth,
	cfg providerConfig,
	opts clipexec.Options,
	model string,
	rawPayload []byte,
) (oaiRequest, *http.Client, error) {
	payload := inboundToOpenAI(opts, model, rawPayload)
	var oai oaiRequest
	if err := json.Unmarshal(payload, &oai); err != nil {
		return oaiRequest{}, nil, fmt.Errorf("codeium: invalid request payload: %w", err)
	}
	// The client's reasoning effort (used to compose thinking variants) may arrive
	// in the payload or in the host's execution metadata.
	if oai.ReasoningEffort == "" {
		if v, ok := opts.Metadata[clipexec.ReasoningEffortMetadataKey].(string); ok {
			oai.ReasoningEffort = v
		}
	}
	client := buildHTTPClient(a)
	if errSupport := validateRequestImageModelSupport(cfg.sessionToken, oai); errSupport != nil {
		return oaiRequest{}, nil, fmt.Errorf("codeium: invalid image input: %w", errSupport)
	}
	imageCompatibleRequest, errImages := prepareImageCompatibleRequest(ctx, client, oai)
	if errImages != nil {
		return oaiRequest{}, nil, fmt.Errorf("codeium: invalid image input: %w", errImages)
	}
	return imageCompatibleRequest, client, nil
}

// upstream builds and sends one GetChatMessage Connect request. Multimodal
// content is prepared before the retry loop so a remote image is downloaded at
// most once for one downstream request.
func (e codeiumExecutor) upstream(
	ctx context.Context,
	cfg providerConfig,
	client *http.Client,
	oai oaiRequest,
	compatibilityMode toolCompatibilityMode,
) (*http.Response, toolCompatibility, error) {
	entry, err := getValidJWT(ctx, client, cfg)
	if err != nil {
		return nil, toolCompatibility{}, err
	}
	// Fill account identifiers from the JWT when not provided in config.
	if cfg.teamID == "" {
		cfg.teamID = entry.teamID
	}
	if cfg.userID == "" {
		cfg.userID = entry.userID
	}
	compatibleRequest, compatibility := prepareToolCompatibleRequest(oai, compatibilityMode)
	if compatibleRequest.ToolCompatibilityError != "" {
		return nil, compatibility, fmt.Errorf(
			"codeium: invalid tool configuration: %s",
			compatibleRequest.ToolCompatibilityError,
		)
	}
	msg, _ := buildChatRequest(cfg, entry.token, compatibleRequest)
	env, err := encodeEnvelope(msg, true)
	if err != nil {
		return nil, compatibility, err
	}

	url := strings.TrimRight(cfg.endpoint, "/") + "/exa.api_server_pb.ApiServerService/GetChatMessage"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(env))
	if err != nil {
		return nil, compatibility, err
	}
	httpReq.Header.Set("Content-Type", "application/connect+proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("Connect-Content-Encoding", "gzip")
	httpReq.Header.Set("Connect-Accept-Encoding", "gzip")
	httpReq.Header.Set("User-Agent", "connect-go/1.18.1 (go1.26.3)")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, compatibility, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, compatibility, &statusErr{code: resp.StatusCode, msg: fmt.Sprintf("GetChatMessage HTTP %d: %s", resp.StatusCode, truncate(raw, 300))}
	}
	return resp, compatibility, nil
}

// trailerError inspects an end-of-stream trailer frame for a Connect error.
func trailerError(body []byte) error {
	if len(body) == 0 {
		return nil
	}
	var t struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &t) == nil && t.Error != nil && t.Error.Code != "" {
		return fmt.Errorf("codeium upstream error [%s]: %s", t.Error.Code, t.Error.Message)
	}
	return nil
}

// ---- OpenAI output helpers ----

// toolAcc accumulates a streamed tool call. The id is Codeium's
// "functions.<name>:<idx>", reused verbatim as the OpenAI tool_call id so tool
// results round-trip back to f7 without a mapping table.
type toolAcc struct {
	id, name string
	args     strings.Builder
}

// accumulateToolDeltas preserves every tool call in a frame and routes argument
// fragments by id when the upstream repeats it. Argument-only fragments belong
// to the most recently active call, matching Codeium's sequential stream form.
func accumulateToolDeltas(tools []*toolAcc, toolIndexes map[string]int, activeToolIndex int, deltas []toolDelta) ([]*toolAcc, int) {
	for _, delta := range deltas {
		if delta.id != "" {
			toolIndex, exists := toolIndexes[delta.id]
			if !exists {
				toolIndex = len(tools)
				toolIndexes[delta.id] = toolIndex
				tools = append(tools, &toolAcc{id: delta.id, name: delta.name})
			} else if delta.name != "" && tools[toolIndex].name == "" {
				tools[toolIndex].name = delta.name
			}
			activeToolIndex = toolIndex
		}
		if delta.args != "" && activeToolIndex >= 0 {
			tools[activeToolIndex].args.WriteString(delta.args)
		}
	}
	return tools, activeToolIndex
}

func openAICompletion(model, content, reasoning string, tools []*toolAcc) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	finish := "stop"
	if len(tools) > 0 {
		calls := make([]map[string]any, 0, len(tools))
		for i, t := range tools {
			calls = append(calls, map[string]any{
				"index":    i,
				"id":       t.id,
				"type":     "function",
				"function": map[string]any{"name": t.name, "arguments": t.args.String()},
			})
		}
		message["tool_calls"] = calls
		if content == "" {
			message["content"] = nil
		}
		finish = "tool_calls"
	}
	return map[string]any{
		"id":      "chatcmpl-" + nowID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
}

func sseChunk(id, model, role, content, reasoning, finish string) []byte {
	delta := map[string]any{}
	if role != "" {
		delta["role"] = role
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if content != "" {
		delta["content"] = content
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
	}
	b, _ := json.Marshal(obj)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// sseToolChunk emits an OpenAI streaming tool-call delta. On the start frame,
// tcID and tcName are set; subsequent frames carry only argsDelta.
func sseToolChunk(id, model, role string, index int, tcID, tcName, argsDelta string) []byte {
	fn := map[string]any{}
	if tcName != "" {
		fn["name"] = tcName
	}
	if argsDelta != "" || tcID != "" {
		fn["arguments"] = argsDelta
	}
	call := map[string]any{"index": index, "function": fn}
	if tcID != "" {
		call["id"] = tcID
		call["type"] = "function"
	}
	delta := map[string]any{"tool_calls": []map[string]any{call}}
	if role != "" {
		delta["role"] = role
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}},
	}
	b, _ := json.Marshal(obj)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

func nowID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// clientPool caches one *http.Client per proxy URL so connections are pooled and
// transports are not rebuilt (and leaked) on every request under concurrency.
var clientPool sync.Map // proxyURL string -> *http.Client

// buildHTTPClient honours a per-auth proxy override; no timeout after connect.
func buildHTTPClient(a *coreauth.Auth) *http.Client {
	if a == nil || strings.TrimSpace(a.ProxyURL) == "" {
		return http.DefaultClient
	}
	key := strings.TrimSpace(a.ProxyURL)
	if c, ok := clientPool.Load(key); ok {
		return c.(*http.Client)
	}
	u, err := url.Parse(key)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
		return http.DefaultClient
	}
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	actual, _ := clientPool.LoadOrStore(key, c)
	return actual.(*http.Client)
}

// statusErr carries an HTTP-like status for the auth manager.
type statusErr struct {
	code int
	msg  string
}

func (e *statusErr) Error() string   { return e.msg }
func (e *statusErr) StatusCode() int { return e.code }
