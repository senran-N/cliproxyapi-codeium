package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

var testPNGData = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44,
	0xae, 0x42, 0x60, 0x82,
}

func TestPrepareImageCompatibleRequestEncodesOpenAIAndMCPImages(t *testing.T) {
	encodedPNG := base64.StdEncoding.EncodeToString(testPNGData)
	requestJSON := `{
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"inspect"},
					{"type":"image_url","caption":"pixel","image_url":{"url":"data:image/png;base64,` + encodedPNG + `"}}
				]
			},
			{
				"role":"tool",
				"tool_call_id":"call-1",
				"content":[{"type":"image","mimeType":"image/png","data":"` + encodedPNG + `"}]
			}
		]
	}`
	var originalRequest oaiRequest
	if errDecode := json.Unmarshal([]byte(requestJSON), &originalRequest); errDecode != nil {
		t.Fatalf("decode image request: %v", errDecode)
	}
	preparedRequest, errPrepare := prepareImageCompatibleRequest(context.Background(), nil, originalRequest)
	if errPrepare != nil {
		t.Fatalf("prepare image request: %v", errPrepare)
	}
	if preparedRequest.Messages[0].contentString() != "inspect" ||
		len(preparedRequest.Messages[0].Images) != 1 ||
		len(preparedRequest.Messages[1].Images) != 1 {
		t.Fatalf("unexpected prepared request: %+v", preparedRequest.Messages)
	}

	requestMessage, _ := buildChatRequest(providerConfig{}, "synthetic-jwt", preparedRequest)
	imageMessage := findFirstEncodedImage(t, requestMessage)
	imageReader := newPR(imageMessage)
	imageFields := map[int]string{}
	for !imageReader.eof() {
		field, wire, value, _, errNext := imageReader.next()
		if errNext != nil {
			t.Fatalf("decode ImageData: %v", errNext)
		}
		if wire == 2 {
			imageFields[field] = string(value)
		}
	}
	if imageFields[1] != encodedPNG || imageFields[2] != "image/png" || imageFields[3] != "pixel" {
		t.Fatalf("encoded ImageData fields = %+v", imageFields)
	}
}

func TestPrepareImageCompatibleRequestRejectsMalformedImage(t *testing.T) {
	_, errPrepare := prepareImageCompatibleRequest(context.Background(), nil, oaiRequest{
		Messages: []oaiMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-base64!"}}]`),
		}},
	})
	if errPrepare == nil || !strings.Contains(errPrepare.Error(), "invalid base64 image data") {
		t.Fatalf("malformed image error = %v", errPrepare)
	}
}

func TestValidateRequestImageModelSupportRejectsKnownTextOnlyModel(t *testing.T) {
	const accountKey = "plugin-image-capability-account"
	const modelID = "text-only-model"
	storeDynamicCatalog(
		accountKey,
		map[string]string{modelID: "text-only-wire"},
		map[string]map[string]string{modelID: {"medium": "text-only-wire"}},
		[]modelDef{{
			ID:                modelID,
			Wire:              "text-only-wire",
			ImageSupportKnown: true,
		}},
	)
	request := oaiRequest{
		Model: modelID,
		Messages: []oaiMessage{{
			Role:   "user",
			Images: []codeiumImageInput{{MIMEType: "image/png"}},
		}},
	}
	errSupport := validateRequestImageModelSupport(accountKey, request)
	if errSupport == nil || !strings.Contains(errSupport.Error(), "does not support image input") {
		t.Fatalf("model image support error = %v", errSupport)
	}
}

func TestFetchRemoteImageConvertsHTTPSResponseToImageData(t *testing.T) {
	imageClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Type": []string{"image/png"}},
			Body:          io.NopCloser(strings.NewReader(string(testPNGData))),
			ContentLength: int64(len(testPNGData)),
			Request:       request,
		}, nil
	})}
	originalClientFactory := createRemoteImageHTTPClient
	createRemoteImageHTTPClient = func() *http.Client { return imageClient }
	defer func() { createRemoteImageHTTPClient = originalClientFactory }()
	image, errFetch := fetchRemoteImage(
		context.Background(),
		nil,
		"https://8.8.8.8/pixel.png",
		"remote pixel",
		maximumImageBytes,
	)
	if errFetch != nil {
		t.Fatalf("fetch remote image: %v", errFetch)
	}
	if image.Base64Data != base64.StdEncoding.EncodeToString(testPNGData) ||
		image.MIMEType != "image/png" ||
		image.Caption != "remote pixel" {
		t.Fatalf("remote ImageData = %+v", image)
	}
}

func findFirstEncodedImage(t *testing.T, requestMessage []byte) []byte {
	t.Helper()
	requestReader := newPR(requestMessage)
	for !requestReader.eof() {
		field, wire, messageValue, _, errNext := requestReader.next()
		if errNext != nil {
			t.Fatalf("decode GetChatMessageRequest: %v", errNext)
		}
		if field != 3 || wire != 2 {
			continue
		}
		messageReader := newPR(messageValue)
		for !messageReader.eof() {
			messageField, messageWire, nestedValue, _, errMessage := messageReader.next()
			if errMessage != nil {
				t.Fatalf("decode ChatMessagePrompt: %v", errMessage)
			}
			if messageField == 10 && messageWire == 2 {
				return nestedValue
			}
		}
	}
	t.Fatal("GetChatMessageRequest did not contain ChatMessagePrompt.images")
	return nil
}
