package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// pluginName/Version/Author are surfaced to management clients and the registry.
const (
	pluginName    = "codeium"
	pluginVersion = "0.6.0"
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
	StorageJSON     []byte
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

type authModelRequest struct {
	HostCallbackID string            `json:"host_callback_id"`
	AuthID         string            `json:"AuthID"`
	StorageJSON    []byte            `json:"StorageJSON"`
	Metadata       map[string]any    `json:"Metadata"`
	Attributes     map[string]string `json:"Attributes"`
	Host           pluginHostConfig  `json:"Host"`
}

var modelCatalogRefreshes sync.Map

// handleMethod dispatches a host RPC. On success it returns the method result
// JSON; on failure it returns an error code + message.
func handleMethod(method string, request []byte) (result json.RawMessage, code, message string) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return json.RawMessage(registerJSON()), "", ""
	case "auth.identifier":
		return json.RawMessage(`{"identifier":"` + providerKey + `"}`), "", ""
	case "auth.parse":
		return parseAuth(request)
	case "auth.login.start":
		return startLogin(request)
	case "auth.login.poll":
		return pollLogin(request)
	case "auth.refresh":
		return refreshAuth(request)
	case "management.register":
		return json.RawMessage(`{"resources":[{"path":"/login","menu":"Codeium Login","description":"Complete Codeium authentication"}]}`), "", ""
	case "management.handle":
		return handleManagement(request)
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
	providerConfig := configFromAuthData(executionRequest.StorageJSON, executionRequest.AuthAttributes, executionRequest.AuthMetadata)
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
		"auth_provider":           true,
		"executor":                true,
		"executor_model_scope":    "both",
		"executor_input_formats":  []string{"chat-completions"},
		"executor_output_formats": []string{"chat-completions"},
		"management_api":          true,
		"model_provider":          true,
	}
	registrationJSON, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"metadata":       metadata,
		"capabilities":   capabilities,
	})
	return string(registrationJSON)
}

// modelsForAuth restores a persisted account catalogue without doing network
// work in the RPC. Some native hosts stop waiting for model.for_auth as soon as
// their registration context ends, so a network-bound response can be discarded
// even after discovery succeeds. Missing and stale catalogues are refreshed in
// the background and saved through the host, which triggers account reloading.
func modelsForAuth(request []byte) json.RawMessage {
	var modelRequest authModelRequest
	if errDecode := json.Unmarshal(request, &modelRequest); errDecode != nil {
		return json.RawMessage(modelsJSON(codeiumModels))
	}
	credentials, errCredentials := decodePersistedCredentials(modelRequest.StorageJSON)
	if errCredentials != nil || strings.TrimSpace(credentials.SessionToken) == "" {
		return json.RawMessage(modelsJSON(codeiumModels))
	}

	dynamicModels := restoreDynamicCatalog(credentials.SessionToken, credentials.ModelCatalog)
	if modelCatalogNeedsRefresh(credentials.ModelCatalog) {
		startAsyncModelCatalogRefresh(modelRequest, credentials)
	}
	if len(dynamicModels) == 0 {
		return json.RawMessage(modelsJSON(codeiumModels))
	}
	return json.RawMessage(modelsJSON(dynamicModels))
}

func startAsyncModelCatalogRefresh(modelRequest authModelRequest, credentials persistedCredentials) {
	accountKey := strings.TrimSpace(credentials.SessionToken)
	authID := strings.TrimSpace(modelRequest.AuthID)
	if accountKey == "" || authID == "" {
		return
	}
	if _, refreshAlreadyRunning := modelCatalogRefreshes.LoadOrStore(accountKey, struct{}{}); refreshAlreadyRunning {
		return
	}

	go func() {
		defer modelCatalogRefreshes.Delete(accountKey)
		directHTTPClient, errClient := buildDirectHTTPClient(modelRequest.Host.ProxyURL)
		if errClient != nil {
			return
		}
		refreshContext, cancelRefresh := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelRefresh()
		providerConfig := configFromAuthData(modelRequest.StorageJSON, modelRequest.Attributes, modelRequest.Metadata)
		refreshPersistedModelCatalogWithConfig(refreshContext, directHTTPClient, &credentials, providerConfig)
		if credentials.ModelCatalog == nil || len(credentials.ModelCatalog.Models) == 0 {
			return
		}

		authFileName := authID + ".json"
		var authLookup hostAuthGetResponse
		if errLookup := invokeHostCallback(hostAuthGetMethod, hostAuthGetRequest{AuthIndex: authID}, &authLookup); errLookup == nil {
			if candidateName := strings.TrimSpace(authLookup.Name); strings.HasSuffix(strings.ToLower(candidateName), ".json") {
				authFileName = candidateName
			}
		}
		updatedCredentialsJSON, errMarshal := json.Marshal(credentials)
		if errMarshal != nil {
			return
		}
		_ = invokeHostCallback(hostAuthSaveMethod, hostAuthSaveRequest{
			Name: authFileName,
			JSON: updatedCredentialsJSON,
		}, nil)
	}()
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
