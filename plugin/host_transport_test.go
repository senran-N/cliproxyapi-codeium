package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPluginHostTransportRoutesBufferedRequestsThroughHost(t *testing.T) {
	originalCallback := invokeRawHostCallback
	defer func() { invokeRawHostCallback = originalCallback }()

	invokeRawHostCallback = func(method string, requestJSON []byte) ([]byte, error) {
		if method != hostHTTPDoMethod {
			t.Fatalf("callback method = %q, want %q", method, hostHTTPDoMethod)
		}
		var request hostHTTPRequest
		if errDecode := json.Unmarshal(requestJSON, &request); errDecode != nil {
			t.Fatalf("decode callback request: %v", errDecode)
		}
		if request.HostCallbackID != "callback-123" || request.Method != http.MethodPost {
			t.Fatalf("unexpected callback request: %+v", request)
		}
		if string(request.Body) != "request-body" || request.Headers.Get("X-Test") != "request-header" {
			t.Fatalf("callback did not preserve body and headers: %+v", request)
		}
		return makeHostCallbackEnvelope(t, hostHTTPResponse{
			StatusCode: http.StatusCreated,
			Headers:    map[string][]string{"X-Upstream": {"response-header"}},
			Body:       []byte("response-body"),
		}), nil
	}

	request, errCreate := http.NewRequest(http.MethodPost, "https://server.codeium.com/test", strings.NewReader("request-body"))
	if errCreate != nil {
		t.Fatalf("create request: %v", errCreate)
	}
	request.Header.Set("X-Test", "request-header")
	response, errDo := buildPluginHTTPClient("callback-123").Do(request)
	if errDo != nil {
		t.Fatalf("host-backed request failed: %v", errDo)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		t.Fatalf("read response: %v", errRead)
	}
	if response.StatusCode != http.StatusCreated || response.Header.Get("X-Upstream") != "response-header" || string(responseBody) != "response-body" {
		t.Fatalf("unexpected response: status=%d headers=%v body=%q", response.StatusCode, response.Header, responseBody)
	}
}

func TestPluginHostTransportReadsAndClosesHostStream(t *testing.T) {
	originalCallback := invokeRawHostCallback
	defer func() { invokeRawHostCallback = originalCallback }()

	readCount := 0
	closeCount := 0
	invokeRawHostCallback = func(method string, requestJSON []byte) ([]byte, error) {
		switch method {
		case hostHTTPDoStreamMethod:
			var request hostHTTPRequest
			if errDecode := json.Unmarshal(requestJSON, &request); errDecode != nil {
				t.Fatalf("decode stream request: %v", errDecode)
			}
			if request.HostCallbackID != "stream-callback" {
				t.Fatalf("host callback id = %q", request.HostCallbackID)
			}
			return makeHostCallbackEnvelope(t, hostHTTPStreamResponse{
				StatusCode: http.StatusOK,
				Headers:    map[string][]string{"Content-Type": {"application/connect+proto"}},
				StreamID:   "stream-1",
			}), nil
		case hostHTTPStreamReadMethod:
			readCount++
			if readCount == 1 {
				return makeHostCallbackEnvelope(t, hostHTTPStreamReadResponse{Payload: []byte("first-")}), nil
			}
			return makeHostCallbackEnvelope(t, hostHTTPStreamReadResponse{Payload: []byte("second"), Done: true}), nil
		case hostHTTPStreamCloseMethod:
			closeCount++
			return makeHostCallbackEnvelope(t, struct{}{}), nil
		default:
			t.Fatalf("unexpected callback method %q", method)
			return nil, nil
		}
	}

	request, errCreate := http.NewRequest(http.MethodPost, "https://server.codeium.com/GetChatMessage", strings.NewReader("stream-request"))
	if errCreate != nil {
		t.Fatalf("create request: %v", errCreate)
	}
	request.Header.Set("Content-Type", "application/connect+proto")
	response, errDo := buildPluginHTTPClient("stream-callback").Do(request)
	if errDo != nil {
		t.Fatalf("open stream: %v", errDo)
	}

	responseBody, errRead := io.ReadAll(response.Body)
	if errRead != nil {
		t.Fatalf("read stream: %v", errRead)
	}
	if string(responseBody) != "first-second" || readCount != 2 {
		t.Fatalf("stream body=%q readCount=%d", responseBody, readCount)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatalf("close stream: %v", errClose)
	}
	if closeCount != 1 {
		t.Fatalf("host stream close count = %d, want 1", closeCount)
	}
}

func makeHostCallbackEnvelope(t *testing.T, result any) []byte {
	t.Helper()
	resultJSON, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		t.Fatalf("marshal callback result: %v", errMarshal)
	}
	envelopeJSON, errMarshal := json.Marshal(hostCallbackEnvelope{OK: true, Result: resultJSON})
	if errMarshal != nil {
		t.Fatalf("marshal callback envelope: %v", errMarshal)
	}
	return envelopeJSON
}
