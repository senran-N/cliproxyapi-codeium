package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const (
	hostHTTPDoMethod          = "host.http.do"
	hostHTTPDoStreamMethod    = "host.http.do_stream"
	hostHTTPStreamReadMethod  = "host.http.stream_read"
	hostHTTPStreamCloseMethod = "host.http.stream_close"
	hostStreamEmitMethod      = "host.stream.emit"
	hostStreamCloseMethod     = "host.stream.close"
)

var invokeRawHostCallback = callHost

type hostCallbackEnvelope struct {
	OK     bool                     `json:"ok"`
	Result json.RawMessage          `json:"result"`
	Error  *hostCallbackEnvelopeErr `json:"error"`
}

type hostCallbackEnvelopeErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type hostHTTPResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type hostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type hostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type hostStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type pluginHostTransport struct {
	hostCallbackID string
}

// buildPluginHTTPClient routes all plugin traffic through CLIProxyAPI's host
// transport. This preserves configured proxies, request logging, and the host's
// cancellation context instead of bypassing them through http.DefaultClient.
func buildPluginHTTPClient(hostCallbackID string) *http.Client {
	return &http.Client{Transport: &pluginHostTransport{hostCallbackID: strings.TrimSpace(hostCallbackID)}}
}

func (transport *pluginHostTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("codeium plugin: nil HTTP request")
	}
	requestBody, errReadBody := readAndRestoreRequestBody(request)
	if errReadBody != nil {
		return nil, fmt.Errorf("codeium plugin: read HTTP request body: %w", errReadBody)
	}
	hostRequest := hostHTTPRequest{
		HostCallbackID: transport.hostCallbackID,
		Method:         request.Method,
		URL:            request.URL.String(),
		Headers:        request.Header.Clone(),
		Body:           requestBody,
	}

	if isStreamingUpstreamRequest(request) {
		return transport.roundTripStream(request, hostRequest)
	}
	return transport.roundTripBuffered(request, hostRequest)
}

func (transport *pluginHostTransport) roundTripBuffered(request *http.Request, hostRequest hostHTTPRequest) (*http.Response, error) {
	var hostResponse hostHTTPResponse
	if errCall := invokeHostCallback(hostHTTPDoMethod, hostRequest, &hostResponse); errCall != nil {
		return nil, errCall
	}
	return &http.Response{
		StatusCode: hostResponse.StatusCode,
		Status:     formatHTTPStatus(hostResponse.StatusCode),
		Header:     hostResponse.Headers,
		Body:       io.NopCloser(bytes.NewReader(hostResponse.Body)),
		Request:    request,
	}, nil
}

func (transport *pluginHostTransport) roundTripStream(request *http.Request, hostRequest hostHTTPRequest) (*http.Response, error) {
	var hostResponse hostHTTPStreamResponse
	if errCall := invokeHostCallback(hostHTTPDoStreamMethod, hostRequest, &hostResponse); errCall != nil {
		return nil, errCall
	}
	if strings.TrimSpace(hostResponse.StreamID) == "" {
		return nil, fmt.Errorf("codeium plugin: host returned an empty HTTP stream id")
	}
	return &http.Response{
		StatusCode: hostResponse.StatusCode,
		Status:     formatHTTPStatus(hostResponse.StatusCode),
		Header:     hostResponse.Headers,
		Body:       &hostHTTPStreamBody{streamID: hostResponse.StreamID},
		Request:    request,
	}, nil
}

func formatHTTPStatus(statusCode int) string {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		return fmt.Sprintf("%d", statusCode)
	}
	return fmt.Sprintf("%d %s", statusCode, statusText)
}

func readAndRestoreRequestBody(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	requestBody, errRead := io.ReadAll(request.Body)
	if errRead != nil {
		return nil, errRead
	}
	_ = request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(requestBody))
	return requestBody, nil
}

func isStreamingUpstreamRequest(request *http.Request) bool {
	return strings.Contains(request.Header.Get("Content-Type"), "application/connect+proto") ||
		strings.HasSuffix(request.URL.Path, "/GetChatMessage")
}

func invokeHostCallback(method string, request any, response any) error {
	requestJSON, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return fmt.Errorf("codeium plugin: encode %s request: %w", method, errMarshal)
	}
	rawEnvelope, errCall := invokeRawHostCallback(method, requestJSON)
	if errCall != nil {
		return errCall
	}
	var callbackEnvelope hostCallbackEnvelope
	if errDecode := json.Unmarshal(rawEnvelope, &callbackEnvelope); errDecode != nil {
		return fmt.Errorf("codeium plugin: decode %s envelope: %w", method, errDecode)
	}
	if !callbackEnvelope.OK {
		if callbackEnvelope.Error != nil && strings.TrimSpace(callbackEnvelope.Error.Message) != "" {
			return fmt.Errorf("codeium plugin: %s: %s", method, callbackEnvelope.Error.Message)
		}
		return fmt.Errorf("codeium plugin: %s failed", method)
	}
	if response == nil || len(callbackEnvelope.Result) == 0 {
		return nil
	}
	if errDecode := json.Unmarshal(callbackEnvelope.Result, response); errDecode != nil {
		return fmt.Errorf("codeium plugin: decode %s result: %w", method, errDecode)
	}
	return nil
}

type hostHTTPStreamBody struct {
	streamID string

	mutex     sync.Mutex
	remainder []byte
	done      bool
	closed    bool
}

func (body *hostHTTPStreamBody) Read(destination []byte) (int, error) {
	body.mutex.Lock()
	defer body.mutex.Unlock()

	if len(destination) == 0 {
		return 0, nil
	}
	if len(body.remainder) > 0 {
		bytesCopied := copy(destination, body.remainder)
		body.remainder = body.remainder[bytesCopied:]
		return bytesCopied, nil
	}
	if body.done || body.closed {
		return 0, io.EOF
	}

	for {
		var streamResponse hostHTTPStreamReadResponse
		errRead := invokeHostCallback(hostHTTPStreamReadMethod, hostHTTPStreamReadRequest{StreamID: body.streamID}, &streamResponse)
		if errRead != nil {
			body.done = true
			return 0, errRead
		}
		if streamResponse.Error != "" {
			body.done = true
			return 0, fmt.Errorf("codeium plugin: host HTTP stream: %s", streamResponse.Error)
		}
		body.done = streamResponse.Done
		if len(streamResponse.Payload) > 0 {
			bytesCopied := copy(destination, streamResponse.Payload)
			if bytesCopied < len(streamResponse.Payload) {
				body.remainder = append(body.remainder[:0], streamResponse.Payload[bytesCopied:]...)
			}
			return bytesCopied, nil
		}
		if body.done {
			return 0, io.EOF
		}
	}
}

func (body *hostHTTPStreamBody) Close() error {
	body.mutex.Lock()
	defer body.mutex.Unlock()
	if body.closed {
		return nil
	}
	body.closed = true
	body.done = true
	body.remainder = nil
	return invokeHostCallback(hostHTTPStreamCloseMethod, hostHTTPStreamCloseRequest{StreamID: body.streamID}, nil)
}
