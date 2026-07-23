package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			AuthProvider  bool `json:"auth_provider"`
			Executor      bool `json:"executor"`
			ManagementAPI bool `json:"management_api"`
			ModelProvider bool `json:"model_provider"`
		} `json:"capabilities"`
	}
	if errDecode := json.Unmarshal(result, &registration); errDecode != nil {
		t.Fatalf("decode registration: %v", errDecode)
	}
	if registration.SchemaVersion != 1 ||
		registration.Metadata.Version != pluginVersion ||
		!registration.Capabilities.AuthProvider ||
		!registration.Capabilities.Executor ||
		!registration.Capabilities.ManagementAPI ||
		!registration.Capabilities.ModelProvider {
		t.Fatalf("unexpected registration: %+v", registration)
	}
}

func TestCompatibleToolDescriptionRewritesCursorAskQuestion(t *testing.T) {
	originalDescription := "Collect structured answers from the user and wait for their responses before continuing."
	compatibleDescription := compatibleToolDescription("AskQuestion", originalDescription)

	if compatibleDescription == originalDescription {
		t.Fatal("Cursor AskQuestion description was not rewritten")
	}
	if compatibleDescription == "" {
		t.Fatal("Cursor AskQuestion description was removed")
	}
	safeShellDescription := "Execute a command in a shell session."
	if unchangedDescription := compatibleToolDescription("Shell", safeShellDescription); unchangedDescription != safeShellDescription {
		t.Fatalf("unrelated tool description changed to %q", unchangedDescription)
	}
}

func TestModelsForAuthFallsBackToStaticCatalogueWithoutCredentials(t *testing.T) {
	result, errorCode, errorMessage := handleMethod("model.for_auth", []byte(`{"StorageJSON":"invalid"}`))
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("model.for_auth failed: code=%q message=%q", errorCode, errorMessage)
	}
	var response struct {
		Provider string `json:"Provider"`
		Models   []struct {
			ID string `json:"ID"`
		} `json:"Models"`
	}
	if errDecode := json.Unmarshal(result, &response); errDecode != nil {
		t.Fatalf("decode model.for_auth response: %v", errDecode)
	}
	if response.Provider != providerKey || len(response.Models) != len(codeiumModels) {
		t.Fatalf("unexpected model.for_auth response: %+v", response)
	}
}

func TestModelsForAuthLoadsPersistedDynamicCatalogueWithoutHostCall(t *testing.T) {
	originalCallback := invokeRawHostCallback
	defer func() { invokeRawHostCallback = originalCallback }()
	invokeRawHostCallback = func(method string, requestJSON []byte) ([]byte, error) {
		t.Fatalf("persisted catalogue unexpectedly called host method %q with %q", method, requestJSON)
		return nil, nil
	}

	const sessionToken = "runtime-model-test-token"
	dynamicModels := []modelDef{{ID: "claude-opus-4.8", Display: "Claude Opus 4.8", Wire: "MODEL_CLAUDE_OPUS_4_8_HIGH"}}
	modelCatalog := &persistedModelCatalog{
		Models:    dynamicModels,
		BaseWire:  map[string]string{"claude-opus-4.8": "MODEL_CLAUDE_OPUS_4_8_HIGH"},
		Families:  map[string]map[string]string{"claude-opus-4.8": {"high": "MODEL_CLAUDE_OPUS_4_8_HIGH"}},
		FetchedAt: time.Now().UTC(),
	}
	credentialsJSON, errMarshal := json.Marshal(persistedCredentials{
		Type:         providerKey,
		SessionToken: sessionToken,
		ModelCatalog: modelCatalog,
	})
	if errMarshal != nil {
		t.Fatalf("marshal persisted catalogue: %v", errMarshal)
	}

	requestJSON, errMarshal := json.Marshal(authModelRequest{
		AuthID:      "codeium-model-test",
		StorageJSON: credentialsJSON,
	})
	if errMarshal != nil {
		t.Fatalf("marshal model request: %v", errMarshal)
	}
	result, errorCode, errorMessage := handleMethod("model.for_auth", requestJSON)
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("model.for_auth failed: code=%q message=%q", errorCode, errorMessage)
	}
	var response struct {
		Provider string `json:"Provider"`
		Models   []struct {
			ID string `json:"ID"`
		} `json:"Models"`
	}
	if errDecode := json.Unmarshal(result, &response); errDecode != nil {
		t.Fatalf("decode model.for_auth response: %v", errDecode)
	}
	if response.Provider != providerKey || len(response.Models) != 1 || response.Models[0].ID != "claude-opus-4.8" {
		t.Fatalf("unexpected dynamic model response: %+v", response)
	}
	if resolvedModel := resolveModelWire(sessionToken, "claude-opus-4.8", "high"); resolvedModel != "MODEL_CLAUDE_OPUS_4_8_HIGH" {
		t.Fatalf("resolved model = %q", resolvedModel)
	}
}

func TestModelsForAuthRefreshesMissingCatalogueAndSavesAuth(t *testing.T) {
	originalCallback := invokeRawHostCallback
	defer func() { invokeRawHostCallback = originalCallback }()

	const sessionToken = "runtime-model-refresh-token"
	modelCatalogRefreshes.Delete(sessionToken)
	defer modelCatalogRefreshes.Delete(sessionToken)
	jwts.put(sessionToken, jwtEntry{
		token:  "cached-api-jwt",
		exp:    time.Now().Add(time.Hour),
		userID: "user-test",
		teamID: "team-test",
	})

	var baseFamily pw
	baseFamily.str(1, "Claude Opus 4.8")
	var modelEntry pw
	modelEntry.str(1, "Claude Opus 4.8 High Thinking")
	modelEntry.varint(11, 1)
	modelEntry.varint(18, 200_000)
	modelEntry.str(22, "MODEL_CLAUDE_OPUS_4_8_HIGH")
	modelEntry.msg(30, baseFamily.bytes())
	var modelResponse pw
	modelResponse.msg(1, modelEntry.bytes())

	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/exa.api_server_pb.ApiServerService/GetCliModelConfigs") {
			t.Errorf("unexpected discovery path: %q", request.URL.Path)
			responseWriter.WriteHeader(http.StatusNotFound)
			return
		}
		responseWriter.Header().Set("Content-Type", "application/proto")
		_, _ = responseWriter.Write(modelResponse.bytes())
	}))
	defer server.Close()

	savedCredentials := make(chan hostAuthSaveRequest, 1)
	invokeRawHostCallback = func(method string, requestJSON []byte) ([]byte, error) {
		switch method {
		case hostAuthGetMethod:
			return makeHostCallbackEnvelope(t, hostAuthGetResponse{
				AuthIndex: "codeium-model-refresh",
				Name:      "existing-codeium.json",
			}), nil
		case hostAuthSaveMethod:
			var saveRequest hostAuthSaveRequest
			if errDecode := json.Unmarshal(requestJSON, &saveRequest); errDecode != nil {
				t.Errorf("decode auth save request: %v", errDecode)
			}
			savedCredentials <- saveRequest
			return makeHostCallbackEnvelope(t, struct{}{}), nil
		default:
			t.Errorf("unexpected host callback method %q", method)
			return makeHostCallbackEnvelope(t, struct{}{}), nil
		}
	}

	credentialsJSON := []byte(`{"type":"codeium","session_token":"` + sessionToken + `"}`)
	requestJSON, errMarshal := json.Marshal(authModelRequest{
		AuthID:      "codeium-model-refresh",
		StorageJSON: credentialsJSON,
		Metadata:    map[string]any{"endpoint": server.URL},
	})
	if errMarshal != nil {
		t.Fatalf("marshal model refresh request: %v", errMarshal)
	}
	initialResult := modelsForAuth(requestJSON)
	if !strings.Contains(string(initialResult), "swe-1-7") {
		t.Fatalf("initial response did not use fallback catalogue: %s", initialResult)
	}

	var saveRequest hostAuthSaveRequest
	select {
	case saveRequest = <-savedCredentials:
	case <-time.After(3 * time.Second):
		t.Fatal("background model discovery did not save refreshed credentials")
	}
	if saveRequest.Name != "existing-codeium.json" {
		t.Fatalf("saved auth file name = %q", saveRequest.Name)
	}
	var refreshedCredentials persistedCredentials
	if errDecode := json.Unmarshal(saveRequest.JSON, &refreshedCredentials); errDecode != nil {
		t.Fatalf("decode refreshed credentials: %v", errDecode)
	}
	if refreshedCredentials.ModelCatalog == nil || len(refreshedCredentials.ModelCatalog.Models) != 1 {
		t.Fatalf("refreshed credentials have no dynamic catalogue: %+v", refreshedCredentials.ModelCatalog)
	}

	refreshedRequestJSON, errMarshal := json.Marshal(authModelRequest{
		AuthID:      "codeium-model-refresh",
		StorageJSON: saveRequest.JSON,
	})
	if errMarshal != nil {
		t.Fatalf("marshal refreshed model request: %v", errMarshal)
	}
	refreshedResult := modelsForAuth(refreshedRequestJSON)
	if !strings.Contains(string(refreshedResult), "claude-opus-4.8") {
		t.Fatalf("refreshed response did not use dynamic catalogue: %s", refreshedResult)
	}
}

func TestOpenAIStreamChunkLeavesSSEFramingToHost(t *testing.T) {
	chunk := openAIStreamChunk("chatcmpl-test", "swe-1-7", "assistant", "STREAM_OK", "", "")
	if strings.HasPrefix(string(chunk), "data:") {
		t.Fatalf("executor chunk unexpectedly contains SSE framing: %q", chunk)
	}
	if !json.Valid(chunk) {
		t.Fatalf("executor chunk is not valid JSON: %q", chunk)
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
