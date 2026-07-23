package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	devinManualLoginEndpoint = "https://app.devin.ai/auth/windsurf/show-auth-code"
	devinExchangeEndpoint    = "https://server.self-serve.windsurf.com"
	codeExchangePath         = "/exa.seat_management_pb.SeatManagementService/ExchangeDevinCode"
	loginLifetime            = 10 * time.Minute
	authValidationInterval   = 12 * time.Hour
)

type persistedCredentials struct {
	Type         string `json:"type"`
	SessionToken string `json:"session_token"`
	TeamID       string `json:"team_id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	OrgID        string `json:"org_id,omitempty"`
}

type pluginHostConfig struct {
	AuthDir string `json:"AuthDir"`
}

type pluginAuthData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID,omitempty"`
	FileName         string            `json:"FileName,omitempty"`
	Label            string            `json:"Label,omitempty"`
	StorageJSON      []byte            `json:"StorageJSON,omitempty"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter,omitempty"`
}

type authParseRequest struct {
	Provider string `json:"Provider"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
}

type authLoginStartRequest struct {
	HostCallbackID string           `json:"host_callback_id"`
	Provider       string           `json:"Provider"`
	BaseURL        string           `json:"BaseURL"`
	Host           pluginHostConfig `json:"Host"`
}

type authLoginPollRequest struct {
	HostCallbackID string           `json:"host_callback_id"`
	Provider       string           `json:"Provider"`
	State          string           `json:"State"`
	Host           pluginHostConfig `json:"Host"`
}

type authRefreshRequest struct {
	HostCallbackID string            `json:"host_callback_id"`
	AuthID         string            `json:"AuthID"`
	StorageJSON    []byte            `json:"StorageJSON"`
	Metadata       map[string]any    `json:"Metadata"`
	Attributes     map[string]string `json:"Attributes"`
}

type oauthCallbackPayload struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

type managementRequest struct {
	Method string              `json:"Method"`
	Path   string              `json:"Path"`
	Query  map[string][]string `json:"Query"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

func parseAuth(requestJSON []byte) (json.RawMessage, string, string) {
	var request authParseRequest
	if errDecode := json.Unmarshal(requestJSON, &request); errDecode != nil {
		return nil, "invalid_request", "decode auth parse request: " + errDecode.Error()
	}
	credentials, errCredentials := decodePersistedCredentials(request.RawJSON)
	if errCredentials != nil || !isCodeiumSessionToken(credentials.SessionToken) {
		responseJSON, _ := json.Marshal(map[string]any{"Handled": false})
		return responseJSON, "", ""
	}
	authData, errAuthData := buildAuthData(credentials, request.FileName, "")
	if errAuthData != nil {
		return nil, "invalid_auth", errAuthData.Error()
	}
	responseJSON, _ := json.Marshal(map[string]any{"Handled": true, "Auth": authData})
	return responseJSON, "", ""
}

func startLogin(requestJSON []byte) (json.RawMessage, string, string) {
	var request authLoginStartRequest
	if errDecode := json.Unmarshal(requestJSON, &request); errDecode != nil {
		return nil, "invalid_request", "decode auth login request: " + errDecode.Error()
	}

	state := uuid.NewString()
	helperURL := &url.URL{
		Path: "/v0/resource/plugins/" + pluginName + "/login",
	}
	helperQuery := helperURL.Query()
	helperQuery.Set("state", state)
	helperURL.RawQuery = helperQuery.Encode()

	responseJSON, _ := json.Marshal(map[string]any{
		"Provider":  providerKey,
		"URL":       helperURL.String(),
		"State":     state,
		"ExpiresAt": time.Now().UTC().Add(loginLifetime),
	})
	return responseJSON, "", ""
}

func pollLogin(requestJSON []byte) (json.RawMessage, string, string) {
	var request authLoginPollRequest
	if errDecode := json.Unmarshal(requestJSON, &request); errDecode != nil {
		return nil, "invalid_request", "decode auth poll request: " + errDecode.Error()
	}
	state, errState := validateLoginState(request.State)
	if errState != nil {
		return nil, "invalid_request", errState.Error()
	}
	authDirectory := strings.TrimSpace(request.Host.AuthDir)
	if authDirectory == "" {
		return nil, "invalid_request", "CPA did not provide an auth directory"
	}

	callbackPath := filepath.Join(authDirectory, ".oauth-"+providerKey+"-"+state+".oauth")
	callbackJSON, errRead := os.ReadFile(callbackPath)
	if errors.Is(errRead, os.ErrNotExist) {
		return loginStatusResponse("pending", "Waiting for the one-time Devin authentication token", nil), "", ""
	}
	if errRead != nil {
		return nil, "auth_failed", "read Codeium login callback: " + errRead.Error()
	}
	_ = os.Remove(callbackPath)

	var callback oauthCallbackPayload
	if errDecode := json.Unmarshal(callbackJSON, &callback); errDecode != nil {
		return loginStatusResponse("error", "The Codeium login callback was malformed", nil), "", ""
	}
	if strings.TrimSpace(callback.Error) != "" {
		return loginStatusResponse("error", strings.TrimSpace(callback.Error), nil), "", ""
	}
	if callback.State != state || strings.TrimSpace(callback.Code) == "" {
		return loginStatusResponse("error", "The Codeium login callback did not match this login attempt", nil), "", ""
	}

	exchangeContext, cancelExchange := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelExchange()
	httpClient := buildPluginHTTPClient(request.HostCallbackID)
	credentials, errExchange := exchangeDevinCode(exchangeContext, httpClient, devinExchangeEndpoint, callback.Code)
	if errExchange != nil {
		return loginStatusResponse("error", "Codeium rejected the one-time authentication token: "+errExchange.Error(), nil), "", ""
	}
	if errValidate := validateCredentials(exchangeContext, httpClient, &credentials); errValidate != nil {
		return loginStatusResponse("error", "Codeium session validation failed: "+errValidate.Error(), nil), "", ""
	}
	authData, errAuthData := buildAuthData(credentials, "", "")
	if errAuthData != nil {
		return nil, "auth_failed", errAuthData.Error()
	}
	return loginStatusResponse("success", "Codeium login completed", &authData), "", ""
}

func refreshAuth(requestJSON []byte) (json.RawMessage, string, string) {
	var request authRefreshRequest
	if errDecode := json.Unmarshal(requestJSON, &request); errDecode != nil {
		return nil, "invalid_request", "decode auth refresh request: " + errDecode.Error()
	}
	credentials, _ := decodePersistedCredentials(request.StorageJSON)
	providerConfig := configFromAuthData(request.StorageJSON, request.Attributes, request.Metadata)
	if credentials.SessionToken == "" {
		credentials.SessionToken = providerConfig.sessionToken
	}
	if !isCodeiumSessionToken(credentials.SessionToken) {
		return nil, "invalid_auth", "Codeium session token is missing or invalid"
	}

	validationContext, cancelValidation := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelValidation()
	if errValidate := validateCredentials(validationContext, buildPluginHTTPClient(request.HostCallbackID), &credentials); errValidate != nil {
		return nil, "refresh_failed", errValidate.Error()
	}
	authData, errAuthData := buildAuthData(credentials, "", request.AuthID)
	if errAuthData != nil {
		return nil, "refresh_failed", errAuthData.Error()
	}
	responseJSON, _ := json.Marshal(map[string]any{
		"Auth":             authData,
		"NextRefreshAfter": authData.NextRefreshAfter,
	})
	return responseJSON, "", ""
}

func loginStatusResponse(status, message string, authData *pluginAuthData) json.RawMessage {
	response := map[string]any{"Status": status, "Message": message}
	if authData != nil {
		response["Auth"] = *authData
	}
	responseJSON, _ := json.Marshal(response)
	return responseJSON
}

func exchangeDevinCode(ctx context.Context, client *http.Client, endpoint, oneTimeCode string) (persistedCredentials, error) {
	var requestBody pw
	requestBody.str(1, strings.TrimSpace(oneTimeCode))
	requestURL := strings.TrimRight(endpoint, "/") + codeExchangePath
	httpRequest, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(requestBody.bytes()))
	if errRequest != nil {
		return persistedCredentials{}, errRequest
	}
	httpRequest.Header.Set("Content-Type", "application/proto")
	httpRequest.Header.Set("Connect-Protocol-Version", "1")
	httpRequest.Header.Set("Origin", "https://windsurf.com")
	httpRequest.Header.Set("Referer", "https://windsurf.com/")
	httpRequest.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	httpResponse, errDo := client.Do(httpRequest)
	if errDo != nil {
		return persistedCredentials{}, errDo
	}
	defer func() { _ = httpResponse.Body.Close() }()
	responseBody, errRead := io.ReadAll(httpResponse.Body)
	if errRead != nil {
		return persistedCredentials{}, errRead
	}
	if httpResponse.StatusCode != http.StatusOK {
		return persistedCredentials{}, fmt.Errorf("ExchangeDevinCode HTTP %d: %s", httpResponse.StatusCode, truncate(responseBody, 200))
	}

	sessionToken, errSession := parseFirstStringField(responseBody, 1)
	if errSession != nil || !isCodeiumSessionToken(sessionToken) {
		return persistedCredentials{}, fmt.Errorf("ExchangeDevinCode response did not contain a valid session token")
	}
	accountID, _ := parseFirstStringField(responseBody, 3)
	orgID, _ := parseFirstStringField(responseBody, 4)
	return persistedCredentials{
		Type:         providerKey,
		SessionToken: sessionToken,
		AccountID:    accountID,
		OrgID:        orgID,
	}, nil
}

func validateCredentials(ctx context.Context, client *http.Client, credentials *persistedCredentials) error {
	if credentials == nil || !isCodeiumSessionToken(credentials.SessionToken) {
		return fmt.Errorf("session token is missing or invalid")
	}
	providerConfig := configFromAuthData(mustMarshalCredentials(*credentials), nil, nil)
	jwt, errJWT := refreshJWT(ctx, client, providerConfig)
	if errJWT != nil {
		return errJWT
	}
	credentials.Type = providerKey
	if strings.TrimSpace(credentials.UserID) == "" {
		credentials.UserID = jwt.userID
	}
	if strings.TrimSpace(credentials.TeamID) == "" {
		credentials.TeamID = jwt.teamID
	}
	return nil
}

func buildAuthData(credentials persistedCredentials, fileName, authID string) (pluginAuthData, error) {
	storageJSON, errMarshal := json.Marshal(credentials)
	if errMarshal != nil {
		return pluginAuthData{}, fmt.Errorf("encode Codeium credentials: %w", errMarshal)
	}
	if strings.TrimSpace(authID) == "" {
		authID = credentialID(credentials.SessionToken)
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = authID + ".json"
	}
	label := "Codeium"
	if credentials.AccountID != "" {
		label += " (" + credentials.AccountID + ")"
	}
	metadata := map[string]any{
		"type":       providerKey,
		"account_id": credentials.AccountID,
		"org_id":     credentials.OrgID,
		"team_id":    credentials.TeamID,
		"user_id":    credentials.UserID,
	}
	return pluginAuthData{
		Provider:         providerKey,
		ID:               authID,
		FileName:         fileName,
		Label:            label,
		StorageJSON:      storageJSON,
		Metadata:         metadata,
		NextRefreshAfter: time.Now().UTC().Add(authValidationInterval),
	}, nil
}

func decodePersistedCredentials(rawJSON []byte) (persistedCredentials, error) {
	var credentials persistedCredentials
	if len(bytes.TrimSpace(rawJSON)) == 0 {
		return credentials, fmt.Errorf("credential JSON is empty")
	}
	if errDecode := json.Unmarshal(rawJSON, &credentials); errDecode != nil {
		return credentials, errDecode
	}
	credentials.Type = strings.TrimSpace(credentials.Type)
	credentials.SessionToken = strings.TrimSpace(credentials.SessionToken)
	return credentials, nil
}

func mustMarshalCredentials(credentials persistedCredentials) []byte {
	encoded, _ := json.Marshal(credentials)
	return encoded
}

func isCodeiumSessionToken(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), "devin-session-token$")
}

func credentialID(sessionToken string) string {
	digest := sha256.Sum256([]byte(sessionToken))
	return "codeium-" + hex.EncodeToString(digest[:8])
}

func validateLoginState(rawState string) (string, error) {
	state := strings.TrimSpace(rawState)
	if _, errParse := uuid.Parse(state); errParse != nil {
		return "", fmt.Errorf("invalid Codeium login state")
	}
	return state, nil
}

func handleManagement(requestJSON []byte) (json.RawMessage, string, string) {
	var request managementRequest
	if errDecode := json.Unmarshal(requestJSON, &request); errDecode != nil {
		return nil, "invalid_request", "decode management request: " + errDecode.Error()
	}
	stateValues := request.Query["state"]
	if len(stateValues) == 0 {
		return managementResponseJSON(http.StatusBadRequest, "text/plain; charset=utf-8", "Missing login state"), "", ""
	}
	state, errState := validateLoginState(stateValues[0])
	if errState != nil {
		return managementResponseJSON(http.StatusBadRequest, "text/plain; charset=utf-8", errState.Error()), "", ""
	}
	return managementResponseJSON(http.StatusOK, "text/html; charset=utf-8", loginHelperHTML(state)), "", ""
}

func managementResponseJSON(statusCode int, contentType, body string) json.RawMessage {
	responseJSON, _ := json.Marshal(managementResponse{
		StatusCode: statusCode,
		Headers:    map[string][]string{"Content-Type": {contentType}, "Cache-Control": {"no-store"}},
		Body:       []byte(body),
	})
	return responseJSON
}

func loginHelperHTML(state string) string {
	upstreamURL, _ := url.Parse(devinManualLoginEndpoint)
	upstreamQuery := upstreamURL.Query()
	upstreamQuery.Set("from", "redirect")
	upstreamQuery.Set("state", state)
	upstreamURL.RawQuery = upstreamQuery.Encode()
	escapedUpstreamURL := html.EscapeString(upstreamURL.String())
	escapedState := html.EscapeString(state)
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Codeium Login</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 680px; margin: 48px auto; padding: 0 20px; color: #202124; }
    .card { border: 1px solid #dadce0; border-radius: 12px; padding: 24px; }
    input, button, a.button { box-sizing: border-box; width: 100%; padding: 12px; margin-top: 12px; border-radius: 8px; font-size: 15px; }
    input { border: 1px solid #9aa0a6; }
    button, a.button { border: 0; background: #1a73e8; color: white; cursor: pointer; text-decoration: none; display: block; text-align: center; }
    #status { min-height: 24px; margin-top: 12px; }
    code { overflow-wrap: anywhere; }
  </style>
</head>
<body>
  <div class="card">
    <h1>Sign in to Codeium</h1>
    <p>1. Open Devin's official one-time authentication page.</p>
    <a class="button" target="_blank" rel="noopener" href="` + escapedUpstreamURL + `">Open official login page</a>
    <p>2. Copy the one-time token shown there, paste it below, and submit it before it expires.</p>
    <form id="login-form">
      <input id="code" autocomplete="off" required placeholder="One-time authentication token">
      <button type="submit">Complete login</button>
    </form>
    <p id="status"></p>
    <small>Login state: <code>` + escapedState + `</code></small>
  </div>
  <script>
    const state = ` + fmt.Sprintf("%q", state) + `;
    const form = document.getElementById('login-form');
    const status = document.getElementById('status');
    form.addEventListener('submit', async (event) => {
      event.preventDefault();
      status.textContent = 'Submitting...';
      const response = await fetch('/v0/management/oauth-callback', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({provider: 'codeium', state, code: document.getElementById('code').value.trim()})
      });
      const result = await response.json().catch(() => ({}));
      if (!response.ok) {
        status.textContent = result.error || 'Login submission failed.';
        return;
      }
      status.textContent = 'Token accepted. You can close this page.';
      form.hidden = true;
    });
  </script>
</body>
</html>`
}
