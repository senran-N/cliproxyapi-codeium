package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareToolCompatibleRequestNormalizesCursorTools(t *testing.T) {
	originalRequest := decodeToolCompatibilityRequest(t, `{
		"model":"test-model",
		"messages":[{
			"role":"assistant",
			"tool_calls":[{
				"id":"call-1",
				"type":"function",
				"function":{"name":"mcp.server/read file","arguments":"{}"}
			}]
		}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"mcp.server/read file",
					"description":"Collect structured answers and wait for their responses before continuing.",
					"parameters":{
						"$schema":"https://json-schema.org/draft/2020-12/schema",
						"type":"object",
						"properties":{
							"mode":{"const":"fast"},
							"target":{"oneOf":[{"type":"string"},{"type":"integer"}]}
						}
					}
				}
			},
			{
				"type":"function",
				"function":{
					"name":"mcp.server read file",
					"description":"Read another resource.",
					"parameters":{"type":"object"}
				}
			}
		],
		"tool_choice":{"type":"function","function":{"name":"mcp.server/read file"}},
		"parallel_tool_calls":false
	}`)

	compatibleRequest, compatibility := prepareToolCompatibleRequest(originalRequest, toolCompatibilityNormal)
	if len(compatibleRequest.Tools) != 1 {
		t.Fatalf("compatible tool count = %d, want 1 selected tool", len(compatibleRequest.Tools))
	}
	compatibleName := compatibleRequest.Tools[0].Function.Name
	if compatibleName != "mcp_server_read_file" {
		t.Fatalf("compatible tool name = %q", compatibleName)
	}
	if compatibility.restoreName(compatibleName) != "mcp.server/read file" {
		t.Fatalf("tool name was not reversible: %+v", compatibility.originalNameByCompatibleName)
	}
	if len(compatibility.originalNameByCompatibleName) != 2 {
		t.Fatalf("colliding tool names were not preserved: %+v", compatibility.originalNameByCompatibleName)
	}
	if compatibleRequest.ResolvedToolChoice != "required" {
		t.Fatalf("resolved tool choice = %q", compatibleRequest.ResolvedToolChoice)
	}
	if !compatibleRequest.LimitParallelToolCalls {
		t.Fatal("parallel_tool_calls=false was not preserved")
	}
	if compatibleRequest.Messages[0].ToolCalls[0].Function.Name != compatibleName {
		t.Fatalf("historical tool call was not aliased: %+v", compatibleRequest.Messages[0].ToolCalls[0])
	}
	if originalRequest.Messages[0].ToolCalls[0].Function.Name != "mcp.server/read file" {
		t.Fatal("compatibility preparation mutated the original request")
	}
	if strings.Contains(compatibleRequest.Tools[0].Function.Description, "wait for their responses") {
		t.Fatalf("policy-sensitive description was preserved: %q", compatibleRequest.Tools[0].Function.Description)
	}

	var normalizedSchema map[string]any
	if errDecode := json.Unmarshal(compatibleRequest.Tools[0].Function.Parameters, &normalizedSchema); errDecode != nil {
		t.Fatalf("decode normalized schema: %v", errDecode)
	}
	if _, found := normalizedSchema["$schema"]; found {
		t.Fatalf("schema metadata was not removed: %+v", normalizedSchema)
	}
	properties := normalizedSchema["properties"].(map[string]any)
	modeSchema := properties["mode"].(map[string]any)
	if _, found := modeSchema["const"]; found || len(modeSchema["enum"].([]any)) != 1 {
		t.Fatalf("const was not normalized to enum: %+v", modeSchema)
	}
	targetSchema := properties["target"].(map[string]any)
	if len(targetSchema["oneOf"].([]any)) != 2 {
		t.Fatalf("oneOf alternatives were not preserved: %+v", targetSchema)
	}
}

func TestPrepareToolCompatibleRequestHonorsNoneChoice(t *testing.T) {
	originalRequest := decodeToolCompatibilityRequest(t, `{
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],
		"tool_choice":"none"
	}`)
	compatibleRequest, _ := prepareToolCompatibleRequest(originalRequest, toolCompatibilityNormal)
	if len(compatibleRequest.Tools) != 0 || compatibleRequest.ResolvedToolChoice != "" {
		t.Fatalf("none tool choice still exposed tools: %+v", compatibleRequest)
	}
}

func TestPrepareToolCompatibleRequestRejectsUnknownForcedFunction(t *testing.T) {
	originalRequest := decodeToolCompatibilityRequest(t, `{
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"missing_tool"}}
	}`)
	compatibleRequest, _ := prepareToolCompatibleRequest(originalRequest, toolCompatibilityNormal)
	if !strings.Contains(compatibleRequest.ToolCompatibilityError, "missing_tool") {
		t.Fatalf("unknown forced function error = %q", compatibleRequest.ToolCompatibilityError)
	}
}

func TestPrepareToolCompatibleRequestMapsHistoricalToolsWithoutDefinitions(t *testing.T) {
	originalRequest := decodeToolCompatibilityRequest(t, `{
		"messages":[{"role":"assistant","tool_calls":[{
			"id":"call-old",
			"type":"function",
			"function":{"name":"old.mcp/read resource","arguments":"{}"}
		}]}]
	}`)
	compatibleRequest, compatibility := prepareToolCompatibleRequest(originalRequest, toolCompatibilityNormal)
	compatibleName := compatibleRequest.Messages[0].ToolCalls[0].Function.Name
	if compatibleName != "old_mcp_read_resource" {
		t.Fatalf("historical compatible name = %q", compatibleName)
	}
	if compatibility.restoreName(compatibleName) != "old.mcp/read resource" {
		t.Fatalf("historical name was not reversible: %+v", compatibility.originalNameByCompatibleName)
	}
	if !hasToolCompatibilityContext(originalRequest) {
		t.Fatal("historical tool context was not detected")
	}
}

func TestNormalizeToolParametersPreservesNullableObjectProperties(t *testing.T) {
	normalizedSchemaJSON := normalizeToolParameters(
		json.RawMessage(`{
			"type":["object","null"],
			"properties":{"path":{"type":"string"}},
			"required":["path"]
		}`),
		toolCompatibilityNormal,
	)
	if !strings.Contains(string(normalizedSchemaJSON), `"path"`) || !strings.Contains(string(normalizedSchemaJSON), `"required"`) {
		t.Fatalf("nullable object constraints were removed: %s", normalizedSchemaJSON)
	}
}

func TestNormalizeToolParametersKeepsConstNarrowerThanEnum(t *testing.T) {
	normalizedSchemaJSON := normalizeToolParameters(
		json.RawMessage(`{
			"type":"object",
			"properties":{"mode":{"const":"safe","enum":["safe","dangerous"]}}
		}`),
		toolCompatibilityNormal,
	)
	if strings.Contains(string(normalizedSchemaJSON), "dangerous") {
		t.Fatalf("const plus enum was broadened: %s", normalizedSchemaJSON)
	}
}

func TestFallbackToolSchemaResolvesLocalReferences(t *testing.T) {
	rawSchema := json.RawMessage(`{
		"type":"object",
		"$defs":{"path":{"type":"string","description":"A file path"}},
		"properties":{"path":{"$ref":"#/$defs/path"}}
	}`)
	normalizedSchemaJSON := normalizeToolParameters(rawSchema, toolCompatibilityFallback)
	if strings.Contains(string(normalizedSchemaJSON), `"$ref"`) || strings.Contains(string(normalizedSchemaJSON), `"$defs"`) {
		t.Fatalf("fallback schema retained references: %s", normalizedSchemaJSON)
	}
	var normalizedSchema map[string]any
	if errDecode := json.Unmarshal(normalizedSchemaJSON, &normalizedSchema); errDecode != nil {
		t.Fatalf("decode fallback schema: %v", errDecode)
	}
	if !strings.Contains(string(normalizedSchemaJSON), `"type":"string"`) {
		t.Fatalf("local reference was not resolved: %s", normalizedSchemaJSON)
	}
}

func TestCompatibleMessageContentTextualizesToolResults(t *testing.T) {
	rawContent := json.RawMessage(`[
		{"type":"text","text":"result text"},
		{"type":"image_url","image_url":{"url":"https://example.test/image.png"}},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAABBBBB"}},
		{"type":"resource","resource":{"uri":"file:///tmp/result.json"}},
		{"type":"audio","mimeType":"audio/wav","data":"AAAAABBBBB"}
	]`)
	content := compatibleMessageContent(rawContent)
	for _, expectedFragment := range []string{
		"result text",
		"[image: https://example.test/image.png]",
		"[image: image/png data omitted",
		"[resource: file:///tmp/result.json]",
		"[audio: audio/wav, 10 encoded bytes omitted]",
	} {
		if !strings.Contains(content, expectedFragment) {
			t.Fatalf("textualized content %q does not contain %q", content, expectedFragment)
		}
	}
	if strings.Contains(content, "AAAAABBBBB") {
		t.Fatalf("binary payload leaked into text content: %q", content)
	}

	objectContent := compatibleMessageContent(json.RawMessage(`{"ok":true,"count":2}`))
	if objectContent != `{"count":2,"ok":true}` {
		t.Fatalf("object tool result = %q", objectContent)
	}
}

func TestIsMCPConfigurationErrorIsSpecific(t *testing.T) {
	if !isMCPConfigurationError(assertionError("codeium upstream error [permission_denied]: Unable to process request due to an MCP configuration issue.")) {
		t.Fatal("MCP configuration error was not recognized")
	}
	if isMCPConfigurationError(assertionError("codeium upstream error [permission_denied]: account disabled")) {
		t.Fatal("unrelated permission error was treated as an MCP configuration error")
	}
}

type assertionError string

func (message assertionError) Error() string { return string(message) }

func decodeToolCompatibilityRequest(t *testing.T, requestJSON string) oaiRequest {
	t.Helper()
	var request oaiRequest
	if errDecode := json.Unmarshal([]byte(requestJSON), &request); errDecode != nil {
		t.Fatalf("decode tool compatibility request: %v", errDecode)
	}
	return request
}
