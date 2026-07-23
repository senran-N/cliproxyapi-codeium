package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maximumImagesPerRequest  = 20
	maximumImageBytes        = 10 * 1024 * 1024
	maximumTotalImageBytes   = 20 * 1024 * 1024
	maximumImageCaptionBytes = 512
	remoteImageFetchTimeout  = 15 * time.Second
)

type codeiumImageInput struct {
	Base64Data  string
	MIMEType    string
	Caption     string
	decodedSize int
}

// prepareImageCompatibleRequest extracts multimodal content before tool
// normalization. Codeium accepts image bytes on each ChatMessagePrompt rather
// than OpenAI image URLs, so remote images are fetched and data URLs are
// validated and canonicalized here.
func prepareImageCompatibleRequest(
	ctx context.Context,
	client *http.Client,
	originalRequest oaiRequest,
) (oaiRequest, error) {
	compatibleRequest := originalRequest
	compatibleRequest.Messages = append([]oaiMessage(nil), originalRequest.Messages...)
	for messageIndex, message := range compatibleRequest.Messages {
		if (message.Role == "system" || message.Role == "developer") && messageContainsImageInput(message) {
			return oaiRequest{}, fmt.Errorf(
				"message %d (%s): images are not supported in system messages",
				messageIndex,
				message.Role,
			)
		}
	}

	totalImageCount := 0
	totalImageBytes := 0
	for messageIndex := range compatibleRequest.Messages {
		message := &compatibleRequest.Messages[messageIndex]
		message.ToolCalls = append([]oaiToolCall(nil), originalRequest.Messages[messageIndex].ToolCalls...)

		preparedText, images, errPrepare := prepareMessageContent(
			ctx,
			client,
			message.Content,
			maximumImagesPerRequest-totalImageCount,
			maximumTotalImageBytes-totalImageBytes,
		)
		if errPrepare != nil {
			return oaiRequest{}, fmt.Errorf(
				"message %d (%s): %w",
				messageIndex,
				message.Role,
				errPrepare,
			)
		}
		totalImageCount += len(images)
		if totalImageCount > maximumImagesPerRequest {
			return oaiRequest{}, fmt.Errorf(
				"image count exceeds the %d-image request limit",
				maximumImagesPerRequest,
			)
		}
		for _, image := range images {
			totalImageBytes += image.decodedSize
		}
		if totalImageBytes > maximumTotalImageBytes {
			return oaiRequest{}, fmt.Errorf(
				"total image data exceeds the %d-byte request limit",
				maximumTotalImageBytes,
			)
		}

		message.PreparedText = preparedText
		message.ContentPrepared = true
		message.Images = images
	}
	return compatibleRequest, nil
}

func validateRequestImageModelSupport(accountKey string, request oaiRequest) error {
	for _, message := range request.Messages {
		if messageContainsImageInput(message) {
			modelID := request.Model
			if strings.TrimSpace(modelID) == "" {
				modelID = "swe-1-7"
			}
			supportsImages, supportKnown := lookupModelImageSupport(
				accountKey,
				modelID,
				request.ReasoningEffort,
			)
			if supportKnown && !supportsImages {
				return fmt.Errorf("model %q does not support image input", modelID)
			}
			return nil
		}
	}
	return nil
}

func messageContainsImageInput(message oaiMessage) bool {
	if len(message.Images) > 0 {
		return true
	}
	var contentValue any
	if json.Unmarshal(message.Content, &contentValue) != nil {
		return false
	}
	return contentValueContainsImageInput(contentValue)
}

func contentValueContainsImageInput(contentValue any) bool {
	switch typedValue := contentValue.(type) {
	case []any:
		for _, item := range typedValue {
			if contentValueContainsImageInput(item) {
				return true
			}
		}
	case map[string]any:
		partType, _ := typedValue["type"].(string)
		return isImageContentPart(partType)
	}
	return false
}

func prepareMessageContent(
	ctx context.Context,
	client *http.Client,
	rawContent json.RawMessage,
	remainingImageCount int,
	remainingImageBytes int,
) (string, []codeiumImageInput, error) {
	if len(rawContent) == 0 || string(rawContent) == "null" {
		return "", nil, nil
	}
	var textContent string
	if json.Unmarshal(rawContent, &textContent) == nil {
		return textContent, nil, nil
	}

	var contentValue any
	if errDecode := json.Unmarshal(rawContent, &contentValue); errDecode != nil {
		return "", nil, fmt.Errorf("decode message content: %w", errDecode)
	}
	contentParts, contentIsArray := contentValue.([]any)
	if !contentIsArray {
		compactJSON, errMarshal := json.Marshal(contentValue)
		if errMarshal != nil {
			return "", nil, fmt.Errorf("encode structured message content: %w", errMarshal)
		}
		return string(compactJSON), nil, nil
	}

	textParts := make([]string, 0, len(contentParts))
	images := make([]codeiumImageInput, 0)
	preparedImageBytes := 0
	for partIndex, contentPart := range contentParts {
		partObject, partIsObject := contentPart.(map[string]any)
		if !partIsObject {
			if renderedPart := renderMessageContentPart(contentPart); renderedPart != "" {
				textParts = append(textParts, renderedPart)
			}
			continue
		}

		partType, _ := partObject["type"].(string)
		if isImageContentPart(partType) {
			if len(images) >= remainingImageCount {
				return "", nil, fmt.Errorf(
					"image count exceeds the %d-image request limit",
					maximumImagesPerRequest,
				)
			}
			remainingBytesForImage := remainingImageBytes - preparedImageBytes
			if remainingBytesForImage <= 0 {
				return "", nil, fmt.Errorf(
					"total image data exceeds the %d-byte request limit",
					maximumTotalImageBytes,
				)
			}
			image, errImage := prepareImageContentPart(
				ctx,
				client,
				partObject,
				remainingBytesForImage,
			)
			if errImage != nil {
				return "", nil, fmt.Errorf("image part %d: %w", partIndex, errImage)
			}
			if image.decodedSize > remainingImageBytes-preparedImageBytes {
				return "", nil, fmt.Errorf(
					"total image data exceeds the %d-byte request limit",
					maximumTotalImageBytes,
				)
			}
			preparedImageBytes += image.decodedSize
			images = append(images, image)
			continue
		}
		if renderedPart := renderMessageContentPart(contentPart); renderedPart != "" {
			textParts = append(textParts, renderedPart)
		}
	}
	return strings.Join(textParts, "\n"), images, nil
}

func isImageContentPart(partType string) bool {
	switch strings.ToLower(strings.TrimSpace(partType)) {
	case "image", "image_url", "input_image":
		return true
	default:
		return false
	}
}

func prepareImageContentPart(
	ctx context.Context,
	client *http.Client,
	partObject map[string]any,
	maximumDecodedBytes int,
) (codeiumImageInput, error) {
	caption := firstStringValue(partObject, "caption", "alt_text", "alt")
	caption = truncateImageCaption(caption)

	if directData := firstStringValue(partObject, "data"); directData != "" {
		mimeType := firstStringValue(partObject, "mimeType", "mime_type", "media_type")
		return decodeBase64Image(directData, mimeType, caption, maximumDecodedBytes)
	}
	if sourceObject, sourceIsObject := partObject["source"].(map[string]any); sourceIsObject {
		if sourceData := firstStringValue(sourceObject, "data"); sourceData != "" {
			mimeType := firstStringValue(sourceObject, "mimeType", "mime_type", "media_type")
			return decodeBase64Image(sourceData, mimeType, caption, maximumDecodedBytes)
		}
		if sourceURL := firstStringValue(sourceObject, "url"); sourceURL != "" {
			return loadImageAddress(ctx, client, sourceURL, caption, maximumDecodedBytes)
		}
	}

	imageAddress := ""
	switch imageURL := partObject["image_url"].(type) {
	case string:
		imageAddress = imageURL
	case map[string]any:
		imageAddress = firstStringValue(imageURL, "url")
	}
	if imageAddress == "" {
		imageAddress = firstStringValue(partObject, "url")
	}
	if imageAddress == "" {
		return codeiumImageInput{}, fmt.Errorf("image has no data or URL")
	}
	return loadImageAddress(ctx, client, imageAddress, caption, maximumDecodedBytes)
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func loadImageAddress(
	ctx context.Context,
	client *http.Client,
	imageAddress,
	caption string,
	maximumDecodedBytes int,
) (codeiumImageInput, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(imageAddress)), "data:") {
		return decodeImageDataURL(imageAddress, caption, maximumDecodedBytes)
	}
	return fetchRemoteImage(ctx, client, imageAddress, caption, maximumDecodedBytes)
}

func decodeImageDataURL(dataURL, caption string, maximumDecodedBytes int) (codeiumImageInput, error) {
	header, encodedData, foundSeparator := strings.Cut(strings.TrimSpace(dataURL), ",")
	if !foundSeparator {
		return codeiumImageInput{}, fmt.Errorf("malformed image data URL")
	}
	headerParts := strings.Split(header, ";")
	if len(headerParts) < 2 || !strings.EqualFold(headerParts[len(headerParts)-1], "base64") {
		return codeiumImageInput{}, fmt.Errorf("image data URL must use base64 encoding")
	}
	mimeType := headerParts[0][len("data:"):]
	return decodeBase64Image(encodedData, mimeType, caption, maximumDecodedBytes)
}

func decodeBase64Image(encodedData, mimeType, caption string, maximumDecodedBytes int) (codeiumImageInput, error) {
	canonicalMIMEType, errMIME := normalizeImageMIMEType(mimeType)
	if errMIME != nil {
		return codeiumImageInput{}, errMIME
	}
	maximumDecodedBytes = min(maximumDecodedBytes, maximumImageBytes)
	if maximumDecodedBytes <= 0 {
		return codeiumImageInput{}, fmt.Errorf("image exceeds the remaining request byte budget")
	}
	maximumEncodedBytes := base64.StdEncoding.EncodedLen(maximumDecodedBytes)
	if len(encodedData) > maximumEncodedBytes+1024 {
		return codeiumImageInput{}, fmt.Errorf(
			"encoded image exceeds the %d-byte per-image limit",
			maximumImageBytes,
		)
	}
	encodedData = strings.Map(func(character rune) rune {
		if unicode.IsSpace(character) {
			return -1
		}
		return character
	}, encodedData)
	if len(encodedData) > maximumEncodedBytes {
		return codeiumImageInput{}, fmt.Errorf(
			"encoded image exceeds the %d-byte per-image limit",
			maximumImageBytes,
		)
	}
	decodedData, errDecode := base64.StdEncoding.DecodeString(encodedData)
	if errDecode != nil {
		decodedData, errDecode = base64.RawStdEncoding.DecodeString(encodedData)
	}
	if errDecode != nil {
		return codeiumImageInput{}, fmt.Errorf("invalid base64 image data: %w", errDecode)
	}
	if len(decodedData) == 0 {
		return codeiumImageInput{}, fmt.Errorf("image data is empty")
	}
	if len(decodedData) > maximumDecodedBytes {
		return codeiumImageInput{}, fmt.Errorf(
			"image exceeds the %d-byte per-image limit",
			maximumImageBytes,
		)
	}
	detectedMIMEType := http.DetectContentType(decodedData)
	normalizedDetectedType, errDetected := normalizeImageMIMEType(detectedMIMEType)
	if errDetected != nil {
		return codeiumImageInput{}, fmt.Errorf(
			"image bytes are not a supported PNG, JPEG, GIF, or WebP payload",
		)
	}
	if normalizedDetectedType != canonicalMIMEType {
		return codeiumImageInput{}, fmt.Errorf(
			"declared image type %s does not match detected type %s",
			canonicalMIMEType,
			normalizedDetectedType,
		)
	}
	return codeiumImageInput{
		Base64Data:  base64.StdEncoding.EncodeToString(decodedData),
		MIMEType:    canonicalMIMEType,
		Caption:     caption,
		decodedSize: len(decodedData),
	}, nil
}

func fetchRemoteImage(
	ctx context.Context,
	_ *http.Client,
	imageAddress,
	caption string,
	maximumDecodedBytes int,
) (codeiumImageInput, error) {
	parsedURL, errParse := url.Parse(strings.TrimSpace(imageAddress))
	if errParse != nil {
		return codeiumImageInput{}, fmt.Errorf("parse image URL: %w", errParse)
	}
	if errValidate := validateRemoteImageURLSyntax(parsedURL); errValidate != nil {
		return codeiumImageInput{}, errValidate
	}
	imageClient := createRemoteImageHTTPClient()
	defer imageClient.CloseIdleConnections()
	imageClient.CheckRedirect = func(request *http.Request, previousRequests []*http.Request) error {
		if len(previousRequests) >= 5 {
			return fmt.Errorf("too many image redirects")
		}
		if errValidate := validateRemoteImageURLSyntax(request.URL); errValidate != nil {
			return errValidate
		}
		return nil
	}

	fetchContext, cancelFetch := context.WithTimeout(ctx, remoteImageFetchTimeout)
	defer cancelFetch()
	request, errRequest := http.NewRequestWithContext(fetchContext, http.MethodGet, parsedURL.String(), nil)
	if errRequest != nil {
		return codeiumImageInput{}, fmt.Errorf("create image request: %w", errRequest)
	}
	request.Header.Set("Accept", "image/png,image/jpeg,image/gif,image/webp")
	response, errFetch := imageClient.Do(request)
	if errFetch != nil {
		return codeiumImageInput{}, fmt.Errorf("fetch image: %w", errFetch)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return codeiumImageInput{}, fmt.Errorf("fetch image: HTTP %d", response.StatusCode)
	}
	maximumDecodedBytes = min(maximumDecodedBytes, maximumImageBytes)
	if maximumDecodedBytes <= 0 {
		return codeiumImageInput{}, fmt.Errorf("image exceeds the remaining request byte budget")
	}
	if response.ContentLength > int64(maximumDecodedBytes) {
		return codeiumImageInput{}, fmt.Errorf(
			"remote image exceeds the %d-byte per-image limit",
			maximumImageBytes,
		)
	}
	limitedBody := io.LimitReader(response.Body, int64(maximumDecodedBytes)+1)
	imageData, errRead := io.ReadAll(limitedBody)
	if errRead != nil {
		return codeiumImageInput{}, fmt.Errorf("read image response: %w", errRead)
	}
	if len(imageData) > maximumDecodedBytes {
		return codeiumImageInput{}, fmt.Errorf(
			"remote image exceeds the %d-byte per-image limit",
			maximumImageBytes,
		)
	}
	mimeType := response.Header.Get("Content-Type")
	if parsedMIMEType, _, errMediaType := mime.ParseMediaType(mimeType); errMediaType == nil {
		mimeType = parsedMIMEType
	}
	if _, errMIME := normalizeImageMIMEType(mimeType); errMIME != nil {
		mimeType = http.DetectContentType(imageData)
	}
	return decodeBase64Image(
		base64.StdEncoding.EncodeToString(imageData),
		mimeType,
		caption,
		maximumDecodedBytes,
	)
}

func validateRemoteImageURL(ctx context.Context, parsedURL *url.URL) error {
	if errSyntax := validateRemoteImageURLSyntax(parsedURL); errSyntax != nil {
		return errSyntax
	}
	_, errResolve := resolvePublicImageAddresses(ctx, parsedURL.Hostname())
	return errResolve
}

func validateRemoteImageURLSyntax(parsedURL *url.URL) error {
	if parsedURL == nil || !strings.EqualFold(parsedURL.Scheme, "https") {
		return fmt.Errorf("remote image URL must use HTTPS")
	}
	if parsedURL.User != nil {
		return fmt.Errorf("remote image URL must not contain credentials")
	}
	hostname := strings.TrimSpace(parsedURL.Hostname())
	if hostname == "" {
		return fmt.Errorf("remote image URL has no hostname")
	}
	return nil
}

func resolvePublicImageAddresses(ctx context.Context, hostname string) ([]netip.Addr, error) {
	addresses, errLookup := net.DefaultResolver.LookupNetIP(ctx, "ip", hostname)
	if errLookup != nil {
		return nil, fmt.Errorf("resolve remote image hostname: %w", errLookup)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("remote image hostname resolved to no addresses")
	}
	for _, address := range addresses {
		if !isPublicImageAddress(address) {
			return nil, fmt.Errorf("remote image URL resolves to a non-public address")
		}
	}
	return addresses, nil
}

func isPublicImageAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	for _, blockedPrefix := range blockedRemoteImagePrefixes {
		if blockedPrefix.Contains(address) {
			return false
		}
	}
	return true
}

var blockedRemoteImagePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

var createRemoteImageHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = dialPublicImageAddress
	return &http.Client{Transport: transport}
}

func dialPublicImageAddress(ctx context.Context, network, address string) (net.Conn, error) {
	hostname, port, errSplit := net.SplitHostPort(address)
	if errSplit != nil {
		return nil, fmt.Errorf("parse remote image address: %w", errSplit)
	}
	addresses, errResolve := resolvePublicImageAddresses(ctx, hostname)
	if errResolve != nil {
		return nil, errResolve
	}
	dialer := net.Dialer{Timeout: remoteImageFetchTimeout}
	var lastDialError error
	for _, resolvedAddress := range addresses {
		connection, errDial := dialer.DialContext(
			ctx,
			network,
			net.JoinHostPort(resolvedAddress.String(), port),
		)
		if errDial == nil {
			return connection, nil
		}
		lastDialError = errDial
	}
	return nil, fmt.Errorf("connect to remote image host: %w", lastDialError)
}

func normalizeImageMIMEType(mimeType string) (string, error) {
	if parsedType, _, errParse := mime.ParseMediaType(strings.TrimSpace(mimeType)); errParse == nil {
		mimeType = parsedType
	}
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return "image/png", nil
	case "image/jpeg", "image/jpg":
		return "image/jpeg", nil
	case "image/gif":
		return "image/gif", nil
	case "image/webp":
		return "image/webp", nil
	default:
		return "", fmt.Errorf("unsupported image MIME type %q", mimeType)
	}
}

func truncateImageCaption(caption string) string {
	caption = strings.TrimSpace(caption)
	if len(caption) <= maximumImageCaptionBytes {
		return caption
	}
	caption = caption[:maximumImageCaptionBytes]
	for !utf8.ValidString(caption) {
		caption = caption[:len(caption)-1]
	}
	return strings.TrimSpace(caption)
}
