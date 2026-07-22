package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// pluginName/Version/Author are surfaced to management clients and the registry.
const (
	pluginName    = "codeium"
	pluginVersion = "0.2.0"
	pluginAuthor  = "senran-N"
	pluginRepo    = "https://github.com/senran-N/cliproxyapi-codeium"
)

// executorRequest mirrors the host's pluginapi.ExecutorRequest wire form (only
// the fields this plugin consumes). []byte fields arrive base64-encoded.
type executorRequest struct {
	HostCallbackID  string `json:"host_callback_id"`
	StreamID        string `json:"stream_id"`
	Model           string
	Stream          bool
	Payload         []byte
	OriginalRequest []byte
	SourceFormat    string
	Metadata        map[string]any
	AuthMetadata    map[string]any
	AuthAttributes  map[string]string
}

type execResult struct {
	Payload []byte              `json:"Payload"`
	Headers map[string][]string `json:"Headers,omitempty"`
}

type streamResult struct {
	Headers map[string][]string `json:"headers,omitempty"`
	Chunks  []streamChunk       `json:"chunks"`
}

type streamChunk struct {
	Payload []byte `json:"Payload"`
}

// handleMethod dispatches a host RPC. On success it returns the method result
// JSON; on failure it returns an error code + message.
func handleMethod(method string, request []byte) (result json.RawMessage, code, message string) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return json.RawMessage(registerJSON()), "", ""
	case "executor.identifier":
		return json.RawMessage(`{"identifier":"` + providerKey + `"}`), "", ""
	case "model.static":
		return json.RawMessage(modelsJSON(codeiumModels)), "", ""
	case "model.for_auth":
		return modelsForAuth(request), "", ""
	case "executor.execute":
		return runExecute(request, false)
	case "executor.execute_stream":
		return runExecute(request, true)
	case "executor.count_tokens":
		return nil, "unsupported", "count_tokens is not supported"
	case "executor.http_request":
		return nil, "unsupported", "http_request is not supported"
	default:
		return nil, "unknown_method", "unknown method: " + method
	}
}

func runExecute(request []byte, stream bool) (json.RawMessage, string, string) {
	var executionRequest executorRequest
	if err := json.Unmarshal(request, &executionRequest); err != nil {
		return nil, "invalid_request", "decode executor request: " + err.Error()
	}
	providerConfig := configFromMaps(executionRequest.AuthAttributes, executionRequest.AuthMetadata)
	payload := executionRequest.Payload
	if len(payload) == 0 {
		payload = executionRequest.OriginalRequest
	}
	var openAIRequest oaiRequest
	if err := json.Unmarshal(payload, &openAIRequest); err != nil {
		return nil, "invalid_request", "decode chat payload: " + err.Error()
	}
	if openAIRequest.Model == "" {
		openAIRequest.Model = executionRequest.Model
	}
	if openAIRequest.ReasoningEffort == "" {
		if reasoningEffort, ok := executionRequest.Metadata["reasoning_effort"].(string); ok {
			openAIRequest.ReasoningEffort = reasoningEffort
		}
	}

	executionContext := context.Background()
	httpClient := buildPluginHTTPClient(executionRequest.HostCallbackID)
	if !stream {
		body, err := executeNonStream(executionContext, httpClient, providerConfig, openAIRequest)
		if err != nil {
			return nil, "execute_failed", err.Error()
		}
		resultJSON, _ := json.Marshal(execResult{
			Payload: body,
			Headers: map[string][]string{"Content-Type": {"application/json"}},
		})
		return resultJSON, "", ""
	}

	if strings.TrimSpace(executionRequest.StreamID) != "" {
		startAsyncExecutionStream(executionContext, httpClient, providerConfig, openAIRequest, executionRequest.StreamID)
		resultJSON, _ := json.Marshal(streamResult{
			Headers: map[string][]string{"Content-Type": {"text/event-stream"}},
			Chunks:  []streamChunk{},
		})
		return resultJSON, "", ""
	}

	chunks, err := executeStream(executionContext, httpClient, providerConfig, openAIRequest)
	if err != nil {
		return nil, "execute_failed", err.Error()
	}
	streamChunks := make([]streamChunk, 0, len(chunks))
	for _, chunk := range chunks {
		streamChunks = append(streamChunks, streamChunk{Payload: chunk})
	}
	resultJSON, _ := json.Marshal(streamResult{
		Headers: map[string][]string{"Content-Type": {"text/event-stream"}},
		Chunks:  streamChunks,
	})
	return resultJSON, "", ""
}

// startAsyncExecutionStream bridges model chunks directly into the host-owned
// stream. Returning an empty chunk list tells CLIProxyAPI to keep the callback
// scope alive until host.stream.close, enabling real-time delivery and prompt
// cancellation when the downstream client disconnects.
func startAsyncExecutionStream(ctx context.Context, client *http.Client, cfg providerConfig, openAIRequest oaiRequest, streamID string) {
	go func() {
		errStream := executeStreamTo(ctx, client, cfg, openAIRequest, func(chunk []byte) error {
			return invokeHostCallback(hostStreamEmitMethod, hostStreamEmitRequest{
				StreamID: streamID,
				Payload:  chunk,
			}, nil)
		})
		errorMessage := ""
		if errStream != nil {
			errorMessage = errStream.Error()
		}
		_ = invokeHostCallback(hostStreamCloseMethod, hostStreamCloseRequest{
			StreamID: streamID,
			Error:    errorMessage,
		}, nil)
	}()
}

// registerJSON declares the plugin metadata + executor/model_provider capabilities.
func registerJSON() string {
	metadata := map[string]any{
		"Name":             pluginName,
		"Version":          pluginVersion,
		"Author":           pluginAuthor,
		"GitHubRepository": pluginRepo,
		"Logo":             "",
		"ConfigFields":     []any{},
	}
	capabilities := map[string]any{
		"executor":                true,
		"executor_model_scope":    "both",
		"executor_input_formats":  []string{"chat-completions"},
		"executor_output_formats": []string{"chat-completions"},
		"model_provider":          true,
	}
	registrationJSON, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"metadata":       metadata,
		"capabilities":   capabilities,
	})
	return string(registrationJSON)
}

// modelsForAuth returns the account's live model catalogue, fetched from the
// backend using the auth's session token, falling back to the static list.
func modelsForAuth(request []byte) json.RawMessage {
	var authRequest struct {
		HostCallbackID string            `json:"host_callback_id"`
		Metadata       map[string]any    `json:"Metadata"`
		Attributes     map[string]string `json:"Attributes"`
	}
	_ = json.Unmarshal(request, &authRequest)
	providerConfig := configFromMaps(authRequest.Attributes, authRequest.Metadata)
	fetchContext, cancelFetch := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFetch()
	models := fetchModelCatalogWithClient(fetchContext, buildPluginHTTPClient(authRequest.HostCallbackID), providerConfig)
	if len(models) == 0 {
		models = codeiumModels
	}
	return json.RawMessage(modelsJSON(models))
}

// modelsJSON renders a model list for model.static/for_auth.
func modelsJSON(list []modelDef) string {
	models := make([]map[string]any, 0, len(list))
	for _, model := range list {
		models = append(models, map[string]any{
			"ID":                         model.ID,
			"Object":                     "model",
			"OwnedBy":                    providerKey,
			"DisplayName":                model.Display,
			"SupportedGenerationMethods": []string{"chat"},
			"UserDefined":                true,
		})
	}
	modelsJSON, _ := json.Marshal(map[string]any{"Provider": providerKey, "Models": models})
	return string(modelsJSON)
}
