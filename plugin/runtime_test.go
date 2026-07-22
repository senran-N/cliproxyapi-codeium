package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHandleMethodReturnsPluginRegistration(t *testing.T) {
	result, errorCode, errorMessage := handleMethod("plugin.register", nil)
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("plugin.register failed: code=%q message=%q", errorCode, errorMessage)
	}
	var registration struct {
		SchemaVersion int `json:"schema_version"`
		Metadata      struct {
			Version string `json:"Version"`
		} `json:"metadata"`
		Capabilities struct {
			Executor      bool `json:"executor"`
			ModelProvider bool `json:"model_provider"`
		} `json:"capabilities"`
	}
	if errDecode := json.Unmarshal(result, &registration); errDecode != nil {
		t.Fatalf("decode registration: %v", errDecode)
	}
	if registration.SchemaVersion != 1 || registration.Metadata.Version != pluginVersion || !registration.Capabilities.Executor || !registration.Capabilities.ModelProvider {
		t.Fatalf("unexpected registration: %+v", registration)
	}
}

func TestRunExecuteUsesAsynchronousHostStream(t *testing.T) {
	originalCallback := invokeRawHostCallback
	defer func() { invokeRawHostCallback = originalCallback }()

	streamClosed := make(chan hostStreamCloseRequest, 1)
	invokeRawHostCallback = func(method string, requestJSON []byte) ([]byte, error) {
		if method != hostStreamCloseMethod {
			t.Fatalf("unexpected host callback %q", method)
		}
		var closeRequest hostStreamCloseRequest
		if errDecode := json.Unmarshal(requestJSON, &closeRequest); errDecode != nil {
			t.Fatalf("decode close request: %v", errDecode)
		}
		streamClosed <- closeRequest
		return makeHostCallbackEnvelope(t, struct{}{}), nil
	}

	executionRequestJSON, errMarshal := json.Marshal(executorRequest{
		HostCallbackID: "callback-1",
		StreamID:       "output-stream-1",
		Model:          "swe-1-7",
		Payload:        []byte(`{"model":"swe-1-7","messages":[{"role":"user","content":"ping"}]}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal execution request: %v", errMarshal)
	}

	result, errorCode, errorMessage := runExecute(executionRequestJSON, true)
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("runExecute failed synchronously: code=%q message=%q", errorCode, errorMessage)
	}
	var streamResponse streamResult
	if errDecode := json.Unmarshal(result, &streamResponse); errDecode != nil {
		t.Fatalf("decode stream response: %v", errDecode)
	}
	if len(streamResponse.Chunks) != 0 {
		t.Fatalf("asynchronous response contained %d buffered chunks", len(streamResponse.Chunks))
	}

	select {
	case closeRequest := <-streamClosed:
		if closeRequest.StreamID != "output-stream-1" {
			t.Fatalf("closed stream %q", closeRequest.StreamID)
		}
		if !strings.Contains(closeRequest.Error, "session_token is empty") {
			t.Fatalf("close error = %q", closeRequest.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("asynchronous execution did not close the host stream")
	}
}
