package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFromAuthDataUsesMetadataAndAttributesOverStorageJSON(t *testing.T) {
	configuration := configFromAuthData(
		[]byte(`{"session_token":"storage-token","team_id":"storage-team"}`),
		map[string]string{"team_id": "attribute-team"},
		map[string]any{"session_token": "metadata-token"},
	)

	if configuration.sessionToken != "metadata-token" {
		t.Fatalf("session token = %q, want metadata-token", configuration.sessionToken)
	}
	if configuration.teamID != "attribute-team" {
		t.Fatalf("team ID = %q, want attribute-team", configuration.teamID)
	}
}

func TestParseAuthRecognizesCodeiumCredentialFile(t *testing.T) {
	requestJSON, errMarshal := json.Marshal(authParseRequest{
		Provider: providerKey,
		FileName: "codeium.json",
		RawJSON:  []byte(`{"type":"codeium","session_token":"devin-session-token$test"}`),
	})
	if errMarshal != nil {
		t.Fatalf("marshal parse request: %v", errMarshal)
	}

	result, errorCode, errorMessage := parseAuth(requestJSON)
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("parseAuth failed: code=%q message=%q", errorCode, errorMessage)
	}
	var response struct {
		Handled bool           `json:"Handled"`
		Auth    pluginAuthData `json:"Auth"`
	}
	if errDecode := json.Unmarshal(result, &response); errDecode != nil {
		t.Fatalf("decode parse response: %v", errDecode)
	}
	if !response.Handled || response.Auth.Provider != providerKey || !strings.Contains(string(response.Auth.StorageJSON), "session_token") {
		t.Fatalf("unexpected parse response: %+v", response)
	}
}

func TestStartLoginReturnsCPAResourceURL(t *testing.T) {
	requestJSON, errMarshal := json.Marshal(authLoginStartRequest{BaseURL: "http://127.0.0.1:8321/v0/management/oauth-callback"})
	if errMarshal != nil {
		t.Fatalf("marshal start request: %v", errMarshal)
	}

	result, errorCode, errorMessage := startLogin(requestJSON)
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("startLogin failed: code=%q message=%q", errorCode, errorMessage)
	}
	var response struct {
		URL   string `json:"URL"`
		State string `json:"State"`
	}
	if errDecode := json.Unmarshal(result, &response); errDecode != nil {
		t.Fatalf("decode start response: %v", errDecode)
	}
	if !strings.HasPrefix(response.URL, "http://127.0.0.1:8321/v0/resource/plugins/codeium/login?state=") || response.State == "" {
		t.Fatalf("unexpected login start response: %+v", response)
	}
}

func TestExchangeDevinCodeEncodesOneTimeCodeAndParsesCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		requestBody, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			t.Fatalf("read exchange request: %v", errRead)
		}
		requestReader := newPR(requestBody)
		field, wire, value, _, errNext := requestReader.next()
		if errNext != nil || field != 1 || wire != 2 || string(value) != "one-time-code" {
			t.Fatalf("unexpected exchange request: field=%d wire=%d value=%q error=%v", field, wire, value, errNext)
		}
		var responseBody pw
		responseBody.str(1, "devin-session-token$session")
		responseBody.str(3, "account-id")
		responseBody.str(4, "org-id")
		responseWriter.Header().Set("Content-Type", "application/proto")
		_, _ = responseWriter.Write(responseBody.bytes())
	}))
	defer server.Close()

	credentials, errExchange := exchangeDevinCode(t.Context(), server.Client(), server.URL, "one-time-code")
	if errExchange != nil {
		t.Fatalf("exchangeDevinCode failed: %v", errExchange)
	}
	if credentials.SessionToken != "devin-session-token$session" || credentials.AccountID != "account-id" || credentials.OrgID != "org-id" {
		t.Fatalf("unexpected credentials: %+v", credentials)
	}
}

func TestPollLoginReadsCPACallbackAndRejectsMismatchedStateBeforeExchange(t *testing.T) {
	authDirectory := t.TempDir()
	state := "550e8400-e29b-41d4-a716-446655440000"
	callbackPath := filepath.Join(authDirectory, ".oauth-codeium-"+state+".oauth")
	if errWrite := os.WriteFile(callbackPath, []byte(`{"code":"one-time-code","state":"different-state"}`), 0o600); errWrite != nil {
		t.Fatalf("write callback file: %v", errWrite)
	}
	requestJSON, errMarshal := json.Marshal(authLoginPollRequest{
		State: state,
		Host:  pluginHostConfig{AuthDir: authDirectory},
	})
	if errMarshal != nil {
		t.Fatalf("marshal poll request: %v", errMarshal)
	}

	result, errorCode, errorMessage := pollLogin(requestJSON)
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("pollLogin failed: code=%q message=%q", errorCode, errorMessage)
	}
	var response struct {
		Status string `json:"Status"`
	}
	if errDecode := json.Unmarshal(result, &response); errDecode != nil {
		t.Fatalf("decode poll response: %v", errDecode)
	}
	if response.Status != "error" {
		t.Fatalf("poll status = %q, want error", response.Status)
	}
}
