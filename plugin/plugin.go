// Command plugin builds the Codeium/Devin provider as a CLIProxyAPI C-ABI
// dynamic-library plugin (executor + model_provider capabilities).
//
// Build (needs a C toolchain / cgo):
//
//	go build -buildmode=c-shared -o cliproxy-codeium.so .
//
// The executor declares chat-completions in/out; the host translates the
// Anthropic (/v1/messages) and Responses (/v1/responses) protocols around it.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"unsafe"
)

const abiVersion uint32 = 1

// pluginName/Version/Author are surfaced to management clients and the registry.
const (
	pluginName    = "codeium"
	pluginVersion = "0.1.0"
	pluginAuthor  = "senran-N"
	pluginRepo    = "https://github.com/senran-N/cliproxyapi-codeium"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// executorRequest mirrors the host's pluginapi.ExecutorRequest wire form (only
// the fields this plugin consumes). []byte fields arrive base64-encoded.
type executorRequest struct {
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

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, mustEnvelope(false, nil, "invalid_method", "method is required"))
		return 1
	}
	var reqBytes []byte
	if request != nil && requestLen > 0 {
		reqBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	result, code, message := handleMethod(C.GoString(method), reqBytes)
	if code != "" {
		writeResponse(response, mustEnvelope(false, nil, code, message))
		return 1
	}
	writeResponse(response, mustEnvelope(true, result, "", ""))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

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
	var er executorRequest
	if err := json.Unmarshal(request, &er); err != nil {
		return nil, "invalid_request", "decode executor request: " + err.Error()
	}
	cfg := configFromMaps(er.AuthAttributes, er.AuthMetadata)
	payload := er.Payload
	if len(payload) == 0 {
		payload = er.OriginalRequest
	}
	var oai oaiRequest
	if err := json.Unmarshal(payload, &oai); err != nil {
		return nil, "invalid_request", "decode chat payload: " + err.Error()
	}
	if oai.Model == "" {
		oai.Model = er.Model
	}
	// Reasoning effort (used to compose thinking variants) may come in the payload
	// or in the host execution metadata.
	if oai.ReasoningEffort == "" {
		if v, ok := er.Metadata["reasoning_effort"].(string); ok {
			oai.ReasoningEffort = v
		}
	}
	ctx := context.Background()
	if !stream {
		body, err := executeNonStream(ctx, cfg, oai)
		if err != nil {
			return nil, "execute_failed", err.Error()
		}
		out, _ := json.Marshal(execResult{Payload: body, Headers: map[string][]string{"Content-Type": {"application/json"}}})
		return out, "", ""
	}
	chunks, err := executeStream(ctx, cfg, oai)
	if err != nil {
		return nil, "execute_failed", err.Error()
	}
	sc := make([]streamChunk, 0, len(chunks))
	for _, c := range chunks {
		sc = append(sc, streamChunk{Payload: c})
	}
	out, _ := json.Marshal(streamResult{Headers: map[string][]string{"Content-Type": {"text/event-stream"}}, Chunks: sc})
	return out, "", ""
}

// registerJSON declares the plugin metadata + executor/model_provider capabilities.
func registerJSON() string {
	meta := map[string]any{
		"Name":             pluginName,
		"Version":          pluginVersion,
		"Author":           pluginAuthor,
		"GitHubRepository": pluginRepo,
		"Logo":             "",
		"ConfigFields":     []any{},
	}
	caps := map[string]any{
		"executor":                true,
		"executor_model_scope":    "both",
		"executor_input_formats":  []string{"chat-completions"},
		"executor_output_formats": []string{"chat-completions"},
		"model_provider":          true,
	}
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "metadata": meta, "capabilities": caps})
	return string(b)
}

// modelsForAuth returns the account's live model catalogue, fetched from the
// backend using the auth's session token, falling back to the static list.
func modelsForAuth(request []byte) json.RawMessage {
	var r struct {
		Metadata   map[string]any    `json:"Metadata"`
		Attributes map[string]string `json:"Attributes"`
	}
	_ = json.Unmarshal(request, &r)
	cfg := configFromMaps(r.Attributes, r.Metadata)
	models := fetchModelCatalog(context.Background(), cfg)
	if len(models) == 0 {
		models = codeiumModels
	}
	return json.RawMessage(modelsJSON(models))
}

// modelsJSON renders a model list for model.static/for_auth.
func modelsJSON(list []modelDef) string {
	models := make([]map[string]any, 0, len(list))
	for _, m := range list {
		models = append(models, map[string]any{
			"ID":                         m.ID,
			"Object":                     "model",
			"OwnedBy":                    providerKey,
			"DisplayName":                m.Display,
			"SupportedGenerationMethods": []string{"chat"},
			"UserDefined":                true,
		})
	}
	b, _ := json.Marshal(map[string]any{"Provider": providerKey, "Models": models})
	return string(b)
}

func mustEnvelope(ok bool, result json.RawMessage, code, message string) []byte {
	e := envelope{OK: ok, Result: result}
	if !ok {
		e.Error = &envelopeError{Code: code, Message: message}
	}
	raw, _ := json.Marshal(e)
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
