package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// toolAcc accumulates a streamed tool call. The id is Codeium's
// "functions.<name>:<idx>", reused verbatim as the OpenAI tool_call id.
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

// openUpstream mints a JWT and opens the streaming GetChatMessage request.
func openUpstream(ctx context.Context, client *http.Client, cfg providerConfig, oai oaiRequest) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	entry, err := getValidJWT(ctx, client, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.teamID == "" {
		cfg.teamID = entry.teamID
	}
	if cfg.userID == "" {
		cfg.userID = entry.userID
	}
	msg, _ := buildChatRequest(cfg, entry.token, oai)
	env, err := encodeEnvelope(msg, true)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(cfg.endpoint, "/") + "/exa.api_server_pb.ApiServerService/GetChatMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(env))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Content-Encoding", "gzip")
	req.Header.Set("Connect-Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "connect-go/1.18.1 (go1.26.3)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("codeium: GetChatMessage HTTP %d: %s", resp.StatusCode, truncate(raw, 300))
	}
	return resp, nil
}

// executeNonStream drains the upstream stream into a single OpenAI completion.
func executeNonStream(ctx context.Context, client *http.Client, cfg providerConfig, oai oaiRequest) ([]byte, error) {
	resp, err := openUpstream(ctx, client, cfg, oai)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var content, reasoning strings.Builder
	var tools []*toolAcc
	toolIndexes := map[string]int{}
	activeToolIndex := -1
	model := oai.Model
	er := newEnvelopeReader(resp.Body)
	for {
		fr, errRead := er.read()
		if errRead == io.EOF {
			break
		}
		if errRead != nil {
			return nil, errRead
		}
		if fr.end {
			if te := trailerError(fr.body); te != nil {
				return nil, te
			}
			continue
		}
		d := parseResponseFrame(fr.body)
		if d.model != "" {
			model = d.model
		}
		content.WriteString(d.content)
		reasoning.WriteString(d.reasoning)
		tools, activeToolIndex = accumulateToolDeltas(tools, toolIndexes, activeToolIndex, d.tools)
	}
	return json.Marshal(openAICompletion(model, content.String(), reasoning.String(), tools))
}

// executeStream is the compatibility path for hosts that do not provide an
// output stream id. Current hosts use executeStreamTo for immediate delivery.
func executeStream(ctx context.Context, client *http.Client, cfg providerConfig, oai oaiRequest) ([][]byte, error) {
	var chunks [][]byte
	err := executeStreamTo(ctx, client, cfg, oai, func(chunk []byte) error {
		chunks = append(chunks, append([]byte(nil), chunk...))
		return nil
	})
	return chunks, err
}

// executeStreamTo forwards each translated SSE chunk as soon as the upstream
// frame arrives. Dynamic-library execution uses this to avoid buffering the
// complete model response inside the plugin.
func executeStreamTo(ctx context.Context, client *http.Client, cfg providerConfig, oai oaiRequest, emit func([]byte) error) error {
	if emit == nil {
		return fmt.Errorf("codeium: stream emitter is nil")
	}
	resp, err := openUpstream(ctx, client, cfg, oai)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	id := "chatcmpl-" + nowID()
	model := oai.Model
	first := true
	roleFor := func() string {
		if first {
			first = false
			return "assistant"
		}
		return ""
	}
	toolIndexes := map[string]int{}
	activeToolIndex := -1
	sawTool := false
	er := newEnvelopeReader(resp.Body)
	for {
		fr, errRead := er.read()
		if errRead == io.EOF {
			break
		}
		if errRead != nil {
			return errRead
		}
		if fr.end {
			if te := trailerError(fr.body); te != nil {
				return te
			}
			continue
		}
		d := parseResponseFrame(fr.body)
		if d.model != "" {
			model = d.model
		}
		if d.content != "" || d.reasoning != "" {
			if errEmit := emit(sseChunk(id, model, roleFor(), d.content, d.reasoning, "")); errEmit != nil {
				return errEmit
			}
		}
		for _, tool := range d.tools {
			if tool.id != "" {
				toolIndex, alreadyStarted := toolIndexes[tool.id]
				if !alreadyStarted {
					toolIndex = len(toolIndexes)
					toolIndexes[tool.id] = toolIndex
					sawTool = true
					if errEmit := emit(sseToolChunk(id, model, roleFor(), toolIndex, tool.id, tool.name, "")); errEmit != nil {
						return errEmit
					}
				}
				activeToolIndex = toolIndex
			}
			if tool.args != "" && activeToolIndex >= 0 {
				if errEmit := emit(sseToolChunk(id, model, "", activeToolIndex, "", "", tool.args)); errEmit != nil {
					return errEmit
				}
			}
		}
	}
	finish := "stop"
	if sawTool {
		finish = "tool_calls"
	}
	if errEmit := emit(sseChunk(id, model, "", "", "", finish)); errEmit != nil {
		return errEmit
	}
	return emit([]byte("data: [DONE]\n\n"))
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
