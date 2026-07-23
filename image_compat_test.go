package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

var testPNGData = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44,
	0xae, 0x42, 0x60, 0x82,
}

func TestPrepareImageCompatibleRequestExtractsOpenAIAndMCPImages(t *testing.T) {
	encodedPNG := base64.StdEncoding.EncodeToString(testPNGData)
	requestJSON := `{
		"messages":[
			{
				"role":"user",
				"content":[
					{"type":"text","text":"inspect both images"},
					{"type":"image_url","caption":"first image","image_url":{"url":"data:image/png;base64,` + encodedPNG + `"}}
				]
			},
			{
				"role":"tool",
				"tool_call_id":"call-1",
				"content":[
					{"type":"image","mimeType":"image/png","data":"` + encodedPNG + `"}
				]
			}
		]
	}`
	var originalRequest oaiRequest
	if errDecode := json.Unmarshal([]byte(requestJSON), &originalRequest); errDecode != nil {
		t.Fatalf("decode image request: %v", errDecode)
	}

	preparedRequest, errPrepare := prepareImageCompatibleRequest(
		context.Background(),
		nil,
		originalRequest,
	)
	if errPrepare != nil {
		t.Fatalf("prepare image request: %v", errPrepare)
	}
	if preparedRequest.Messages[0].contentString() != "inspect both images" {
		t.Fatalf("prepared user text = %q", preparedRequest.Messages[0].contentString())
	}
	if len(preparedRequest.Messages[0].Images) != 1 || len(preparedRequest.Messages[1].Images) != 1 {
		t.Fatalf("prepared image counts = %d and %d", len(preparedRequest.Messages[0].Images), len(preparedRequest.Messages[1].Images))
	}
	userImage := preparedRequest.Messages[0].Images[0]
	if userImage.Base64Data != encodedPNG || userImage.MIMEType != "image/png" || userImage.Caption != "first image" {
		t.Fatalf("unexpected prepared user image: %+v", userImage)
	}
	if originalRequest.Messages[0].ContentPrepared || len(originalRequest.Messages[0].Images) != 0 {
		t.Fatal("image preparation mutated the original request")
	}
}

func TestBuildChatRequestEncodesImageDataOnMessageFieldTen(t *testing.T) {
	encodedPNG := base64.StdEncoding.EncodeToString(testPNGData)
	originalRequest := oaiRequest{
		Model: "swe-1-7",
		Messages: []oaiMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"inspect"},{"type":"image_url","caption":"pixel","image_url":{"url":"data:image/png;base64,` + encodedPNG + `"}}]`),
		}},
	}
	preparedRequest, errPrepare := prepareImageCompatibleRequest(context.Background(), nil, originalRequest)
	if errPrepare != nil {
		t.Fatalf("prepare image request: %v", errPrepare)
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

func TestPrepareImageCompatibleRequestRejectsInvalidInputs(t *testing.T) {
	testCases := []struct {
		name      string
		content   string
		wantError string
	}{
		{
			name:      "invalid base64",
			content:   `[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-base64!"}}]`,
			wantError: "invalid base64 image data",
		},
		{
			name:      "unsupported media type",
			content:   `[{"type":"image","mimeType":"image/svg+xml","data":"PHN2Zy8+"}]`,
			wantError: "unsupported image MIME type",
		},
		{
			name:      "invalid image bytes",
			content:   `[{"type":"image","mimeType":"image/png","data":"aGVsbG8="}]`,
			wantError: "image bytes are not a supported",
		},
		{
			name:      "system image",
			content:   `[{"type":"image","mimeType":"image/png","data":"` + base64.StdEncoding.EncodeToString(testPNGData) + `"}]`,
			wantError: "images are not supported in system messages",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			role := "user"
			if testCase.name == "system image" {
				role = "system"
			}
			_, errPrepare := prepareImageCompatibleRequest(context.Background(), nil, oaiRequest{
				Messages: []oaiMessage{{Role: role, Content: json.RawMessage(testCase.content)}},
			})
			if errPrepare == nil || !strings.Contains(errPrepare.Error(), testCase.wantError) {
				t.Fatalf("prepare error = %v, want fragment %q", errPrepare, testCase.wantError)
			}
		})
	}
}

func TestPrepareImageCompatibleRequestStopsBeforeFetchingTwentyFirstImage(t *testing.T) {
	fetchCount := 0
	imageClient := &http.Client{Transport: imageRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		fetchCount++
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

	contentParts := make([]map[string]any, maximumImagesPerRequest+1)
	for imageIndex := range contentParts {
		contentParts[imageIndex] = map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "https://8.8.8.8/pixel.png"},
		}
	}
	contentJSON, errMarshal := json.Marshal(contentParts)
	if errMarshal != nil {
		t.Fatalf("encode image parts: %v", errMarshal)
	}
	_, errPrepare := prepareImageCompatibleRequest(context.Background(), nil, oaiRequest{
		Messages: []oaiMessage{{Role: "user", Content: contentJSON}},
	})
	if errPrepare == nil || !strings.Contains(errPrepare.Error(), "image count exceeds") {
		t.Fatalf("image count error = %v", errPrepare)
	}
	if fetchCount != maximumImagesPerRequest {
		t.Fatalf("remote image fetch count = %d, want %d", fetchCount, maximumImagesPerRequest)
	}
}

func TestValidateRemoteImageURLRejectsPrivateAddresses(t *testing.T) {
	privateURL, errParse := url.Parse("https://127.0.0.1/private.png")
	if errParse != nil {
		t.Fatalf("parse private URL: %v", errParse)
	}
	errValidate := validateRemoteImageURL(context.Background(), privateURL)
	if errValidate == nil || !strings.Contains(errValidate.Error(), "non-public") {
		t.Fatalf("private image URL validation error = %v", errValidate)
	}
}

func TestValidateRequestImageModelSupportRejectsKnownTextOnlyModel(t *testing.T) {
	const accountKey = "image-capability-account"
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
	imageClient := &http.Client{Transport: imageRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://8.8.8.8/pixel.png" {
			t.Fatalf("remote image URL = %q", request.URL.String())
		}
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

type imageRoundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip imageRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
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
