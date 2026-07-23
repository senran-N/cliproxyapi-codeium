package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrepareToolCompatibleRequestNormalizesNamesSchemaAndChoice(t *testing.T) {
	var originalRequest oaiRequest
	requestJSON := []byte(`{
		"messages":[{"role":"assistant","tool_calls":[{
			"id":"call-1",
			"type":"function",
			"function":{"name":"mcp.server/read file","arguments":"{}"}
		}]}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"mcp.server/read file",
				"description":"Wait for their responses before continuing.",
				"parameters":{
					"$schema":"https://json-schema.org/draft/2020-12/schema",
					"type":"object",
					"properties":{"mode":{"const":"fast"}}
				}
			}
		}],
		"tool_choice":{"type":"function","function":{"name":"mcp.server/read file"}}
	}`)
	if errDecode := json.Unmarshal(requestJSON, &originalRequest); errDecode != nil {
		t.Fatalf("decode request: %v", errDecode)
	}

	compatibleRequest, compatibility := prepareToolCompatibleRequest(originalRequest, toolCompatibilityNormal)
	if len(compatibleRequest.Tools) != 1 || compatibleRequest.ResolvedToolChoice != "required" {
		t.Fatalf("unexpected compatible request: %+v", compatibleRequest)
	}
	compatibleName := compatibleRequest.Tools[0].Function.Name
	if compatibleName != "mcp_server_read_file" || compatibility.restoreName(compatibleName) != "mcp.server/read file" {
		t.Fatalf("tool name mapping failed: name=%q map=%+v", compatibleName, compatibility.originalNameByCompatibleName)
	}
	if compatibleRequest.Messages[0].ToolCalls[0].Function.Name != compatibleName {
		t.Fatalf("historical tool name was not mapped: %+v", compatibleRequest.Messages[0].ToolCalls[0])
	}
	if originalRequest.Messages[0].ToolCalls[0].Function.Name != "mcp.server/read file" {
		t.Fatal("compatibility preparation mutated the original request")
	}
	parameters := string(compatibleRequest.Tools[0].Function.Parameters)
	if strings.Contains(parameters, `"$schema"`) || strings.Contains(parameters, `"const"`) || !strings.Contains(parameters, `"enum"`) {
		t.Fatalf("schema was not normalized: %s", parameters)
	}
	if strings.Contains(compatibleRequest.Tools[0].Function.Description, "continuing") {
		t.Fatalf("policy-sensitive description was not rewritten: %q", compatibleRequest.Tools[0].Function.Description)
	}
}

func TestCompatibleMessageContentTextualizesStructuredResults(t *testing.T) {
	content := compatibleMessageContent(json.RawMessage(`[
		{"type":"text","text":"result"},
		{"type":"image_url","image_url":{"url":"https://example.test/image.png"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAABBBBB"}},
		{"type":"resource","resource":{"uri":"file:///tmp/data.json"}},
		{"type":"audio","mimeType":"audio/wav","data":"AAAAABBBBB"}
	]`))
	for _, expectedFragment := range []string{
		"result",
		"[image: https://example.test/image.png]",
		"[image: image/png data omitted",
		"[resource: file:///tmp/data.json]",
		"[audio: audio/wav, 10 encoded bytes omitted]",
	} {
		if !strings.Contains(content, expectedFragment) {
			t.Fatalf("content %q does not contain %q", content, expectedFragment)
		}
	}
	if strings.Contains(content, "AAAAABBBBB") {
		t.Fatalf("binary payload leaked into text content: %q", content)
	}
}

func TestPrepareToolCompatibleRequestMapsHistoricalToolWithoutDefinition(t *testing.T) {
	var originalRequest oaiRequest
	if errDecode := json.Unmarshal([]byte(`{
		"messages":[{"role":"assistant","tool_calls":[{
			"id":"call-old",
			"type":"function",
			"function":{"name":"old.mcp/read resource","arguments":"{}"}
		}]}]
	}`), &originalRequest); errDecode != nil {
		t.Fatalf("decode historical request: %v", errDecode)
	}
	compatibleRequest, compatibility := prepareToolCompatibleRequest(originalRequest, toolCompatibilityNormal)
	compatibleName := compatibleRequest.Messages[0].ToolCalls[0].Function.Name
	if compatibleName != "old_mcp_read_resource" || compatibility.restoreName(compatibleName) != "old.mcp/read resource" {
		t.Fatalf("historical name mapping failed: name=%q map=%+v", compatibleName, compatibility.originalNameByCompatibleName)
	}
	if !hasToolCompatibilityContext(originalRequest) {
		t.Fatal("historical tool context was not detected")
	}
}

func TestExecuteNonStreamRetriesMCPConfigurationFailure(t *testing.T) {
	const sessionToken = "mcp-retry-non-stream-token"
	httpClient, attemptCount := newMCPRetryHTTPClient(t, sessionToken, "RETRY_OK")
	request := makeMCPRetryRequest()

	responseJSON, errExecute := executeNonStream(
		context.Background(),
		httpClient,
		providerConfig{sessionToken: sessionToken, endpoint: "https://codeium.test"},
		request,
	)
	if errExecute != nil {
		t.Fatalf("execute with fallback retry: %v", errExecute)
	}
	if attemptCount.Load() != 2 {
		t.Fatalf("upstream attempt count = %d, want 2", attemptCount.Load())
	}
	if !bytes.Contains(responseJSON, []byte("RETRY_OK")) {
		t.Fatalf("fallback response = %s", responseJSON)
	}
}

func TestExecuteStreamRetriesBeforeEmittingOutput(t *testing.T) {
	const sessionToken = "mcp-retry-stream-token"
	httpClient, attemptCount := newMCPRetryHTTPClient(t, sessionToken, "STREAM_RETRY_OK")
	var emittedChunks [][]byte

	errExecute := executeStreamTo(
		context.Background(),
		httpClient,
		providerConfig{sessionToken: sessionToken, endpoint: "https://codeium.test"},
		makeMCPRetryRequest(),
		func(chunk []byte) error {
			emittedChunks = append(emittedChunks, append([]byte(nil), chunk...))
			return nil
		},
	)
	if errExecute != nil {
		t.Fatalf("stream with fallback retry: %v", errExecute)
	}
	if attemptCount.Load() != 2 {
		t.Fatalf("upstream attempt count = %d, want 2", attemptCount.Load())
	}
	if !bytes.Contains(bytes.Join(emittedChunks, nil), []byte("STREAM_RETRY_OK")) {
		t.Fatalf("fallback stream chunks = %q", emittedChunks)
	}
}

func makeMCPRetryRequest() oaiRequest {
	var request oaiRequest
	_ = json.Unmarshal([]byte(`{
		"model":"swe-1-7",
		"messages":[{"role":"user","content":"test retry"}],
		"tools":[{"type":"function","function":{
			"name":"read.resource",
			"description":"Read a resource.",
			"parameters":{"type":"object","properties":{"path":{"type":"string"}}}
		}}]
	}`), &request)
	return request
}

func newMCPRetryHTTPClient(t *testing.T, sessionToken, successfulContent string) (*http.Client, *atomic.Int32) {
	t.Helper()
	jwts.put(sessionToken, jwtEntry{
		token:  "cached-api-jwt",
		exp:    time.Now().Add(time.Hour),
		userID: "user-test",
		teamID: "team-test",
	})

	var attemptCount atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		currentAttempt := attemptCount.Add(1)
		var responseBody []byte
		if currentAttempt == 1 {
			responseBody = makeConnectEndFrame([]byte(`{
				"error":{
					"code":"permission_denied",
					"message":"Unable to process request due to an MCP configuration issue."
				}
			}`))
		} else {
			var contentFrame pw
			contentFrame.str(3, successfulContent)
			messageEnvelope, errEnvelope := encodeEnvelope(contentFrame.bytes(), false)
			if errEnvelope != nil {
				t.Fatalf("encode successful response: %v", errEnvelope)
			}
			responseBody = append(messageEnvelope, makeConnectEndFrame([]byte(`{}`))...)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
			Request:    request,
		}, nil
	})}
	return client, &attemptCount
}

func makeConnectEndFrame(body []byte) []byte {
	frame := make([]byte, 5+len(body))
	frame[0] = connectFlagEndStream
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
	copy(frame[5:], body)
	return frame
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}
