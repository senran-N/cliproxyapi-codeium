// Command plugin builds the Codeium/Devin provider as a CLIProxyAPI C-ABI
// dynamic-library plugin (auth provider, executor, model provider, and
// management resource capabilities).
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

static cliproxy_host_api stored_host;
static int stored_host_available;

static void store_host_api(const cliproxy_host_api* host) {
	if (host == NULL) {
		stored_host_available = 0;
		return;
	}
	stored_host = *host;
	stored_host_available = 1;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (!stored_host_available || stored_host.call == NULL) {
		return 1;
	}
	return stored_host.call(stored_host.host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host_available && stored_host.free_buffer != NULL && ptr != NULL) {
		stored_host.free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

const abiVersion uint32 = 1

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || uint32(host.abi_version) != abiVersion {
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

// callHost invokes a CLIProxyAPI host callback and decodes its standard RPC
// envelope. The host owns the returned C buffer, so it must be released through
// the host's matching allocator rather than C.free.
func callHost(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var cRequest unsafe.Pointer
	if len(request) > 0 {
		cRequest = C.CBytes(request)
		if cRequest == nil {
			return nil, fmt.Errorf("codeium plugin: allocate host callback request")
		}
		defer C.free(cRequest)
	}

	var response C.cliproxy_buffer
	resultCode := C.call_host_api(
		cMethod,
		(*C.uint8_t)(cRequest),
		C.size_t(len(request)),
		&response,
	)
	if response.ptr != nil {
		defer C.free_host_buffer(response.ptr, response.len)
	}
	if resultCode != 0 {
		return nil, fmt.Errorf("codeium plugin: host callback %s failed with code %d", method, int(resultCode))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, fmt.Errorf("codeium plugin: host callback %s returned an empty response", method)
	}
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}
