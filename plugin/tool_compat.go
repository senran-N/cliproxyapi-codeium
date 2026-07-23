package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	toolCompatibilityDescriptionLimit = 1024
	toolCompatibilityNameLimit        = 64
	toolCompatibilitySchemaDepth      = 16
)

var invalidToolNameCharacters = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

type toolCompatibilityMode int

const (
	toolCompatibilityNormal toolCompatibilityMode = iota
	toolCompatibilityFallback
)

type toolCompatibility struct {
	originalNameByCompatibleName map[string]string
}

func (compatibility toolCompatibility) restoreName(compatibleName string) string {
	if originalName := compatibility.originalNameByCompatibleName[compatibleName]; originalName != "" {
		return originalName
	}
	return compatibleName
}

// prepareToolCompatibleRequest converts OpenAI and Cursor tool definitions to
// the conservative function-tool subset accepted by Codeium. It also keeps a
// reverse name map so sanitized names never leak back to the client.
func prepareToolCompatibleRequest(originalRequest oaiRequest, mode toolCompatibilityMode) (oaiRequest, toolCompatibility) {
	compatibleRequest := originalRequest
	compatibleRequest.Tools = nil
	compatibleRequest.Messages = append([]oaiMessage(nil), originalRequest.Messages...)
	for messageIndex := range compatibleRequest.Messages {
		compatibleRequest.Messages[messageIndex].ToolCalls = append(
			[]oaiToolCall(nil),
			originalRequest.Messages[messageIndex].ToolCalls...,
		)
	}

	compatibility := toolCompatibility{
		originalNameByCompatibleName: make(map[string]string),
	}
	compatibleNameByOriginalName := make(map[string]string)
	usedCompatibleNames := make(map[string]struct{})
	registerCompatibleName := func(originalName string) string {
		if existingName := compatibleNameByOriginalName[originalName]; existingName != "" {
			return existingName
		}
		compatibleName := makeCompatibleToolName(originalName, usedCompatibleNames)
		usedCompatibleNames[compatibleName] = struct{}{}
		compatibleNameByOriginalName[originalName] = compatibleName
		compatibility.originalNameByCompatibleName[compatibleName] = originalName
		return compatibleName
	}

	for _, originalTool := range originalRequest.Tools {
		if originalTool.Type != "" && !strings.EqualFold(originalTool.Type, "function") {
			continue
		}
		originalName := strings.TrimSpace(originalTool.Function.Name)
		if originalName == "" {
			continue
		}
		if _, duplicateDefinition := compatibleNameByOriginalName[originalName]; duplicateDefinition {
			continue
		}

		compatibleName := registerCompatibleName(originalName)

		compatibleTool := originalTool
		compatibleTool.Type = "function"
		compatibleTool.Function.Name = compatibleName
		compatibleTool.Function.Description = normalizeToolDescription(
			compatibleName,
			originalTool.Function.Description,
			mode,
		)
		compatibleTool.Function.Parameters = normalizeToolParameters(
			originalTool.Function.Parameters,
			mode,
		)
		compatibleRequest.Tools = append(compatibleRequest.Tools, compatibleTool)
	}

	for messageIndex := range compatibleRequest.Messages {
		for toolCallIndex := range compatibleRequest.Messages[messageIndex].ToolCalls {
			toolCall := &compatibleRequest.Messages[messageIndex].ToolCalls[toolCallIndex]
			originalName := strings.TrimSpace(toolCall.Function.Name)
			if originalName != "" {
				toolCall.Function.Name = registerCompatibleName(originalName)
			}
		}
	}

	choiceMode, selectedOriginalName := parseToolChoice(originalRequest.ToolChoice)
	compatibleRequest.LimitParallelToolCalls = originalRequest.ParallelToolCalls != nil && !*originalRequest.ParallelToolCalls
	switch choiceMode {
	case "none":
		compatibleRequest.Tools = nil
		compatibleRequest.ResolvedToolChoice = ""
	case "required":
		if len(compatibleRequest.Tools) > 0 {
			compatibleRequest.ResolvedToolChoice = "required"
		}
	case "function":
		selectedCompatibleName := compatibleNameByOriginalName[selectedOriginalName]
		if selectedCompatibleName == "" {
			compatibleRequest.Tools = nil
			compatibleRequest.ResolvedToolChoice = ""
			compatibleRequest.ToolCompatibilityError = fmt.Sprintf(
				"tool_choice selects unknown function %q",
				selectedOriginalName,
			)
			break
		}
		for _, compatibleTool := range compatibleRequest.Tools {
			if compatibleTool.Function.Name == selectedCompatibleName {
				compatibleRequest.Tools = []oaiTool{compatibleTool}
				break
			}
		}
		compatibleRequest.ResolvedToolChoice = "required"
	default:
		if len(compatibleRequest.Tools) > 0 {
			compatibleRequest.ResolvedToolChoice = "auto"
		}
	}

	return compatibleRequest, compatibility
}

func makeCompatibleToolName(originalName string, usedNames map[string]struct{}) string {
	compatibleName := invalidToolNameCharacters.ReplaceAllString(strings.TrimSpace(originalName), "_")
	compatibleName = strings.Trim(compatibleName, "_")
	if compatibleName == "" {
		compatibleName = "tool"
	}

	_, nameAlreadyUsed := usedNames[compatibleName]
	if len(compatibleName) <= toolCompatibilityNameLimit && !nameAlreadyUsed {
		return compatibleName
	}

	hashBytes := sha256.Sum256([]byte(originalName))
	hashSuffix := hex.EncodeToString(hashBytes[:4])
	maximumPrefixLength := toolCompatibilityNameLimit - len(hashSuffix) - 1
	if maximumPrefixLength < 1 {
		maximumPrefixLength = 1
	}
	if len(compatibleName) > maximumPrefixLength {
		compatibleName = compatibleName[:maximumPrefixLength]
	}
	compatibleName = strings.TrimRight(compatibleName, "_") + "_" + hashSuffix

	if _, collision := usedNames[compatibleName]; !collision {
		return compatibleName
	}
	for collisionIndex := 2; ; collisionIndex++ {
		collisionSuffix := fmt.Sprintf("_%d", collisionIndex)
		prefixLength := toolCompatibilityNameLimit - len(collisionSuffix)
		candidate := compatibleName
		if len(candidate) > prefixLength {
			candidate = candidate[:prefixLength]
		}
		candidate += collisionSuffix
		if _, collision := usedNames[candidate]; !collision {
			return candidate
		}
	}
}

func compatibleToolDescription(toolName, description string) string {
	return normalizeToolDescription(toolName, description, toolCompatibilityNormal)
}

func normalizeToolDescription(toolName, description string, mode toolCompatibilityMode) string {
	normalizedDescription := strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && !unicode.IsSpace(character) {
			return -1
		}
		return character
	}, description)
	normalizedDescription = strings.Join(strings.Fields(normalizedDescription), " ")

	if mode == toolCompatibilityFallback || descriptionTriggersMCPPolicy(toolName, normalizedDescription) {
		normalizedDescription = fmt.Sprintf(
			"Use %s when this capability is needed. Supply arguments matching the provided schema.",
			toolName,
		)
	}
	if normalizedDescription == "" {
		normalizedDescription = fmt.Sprintf("Use %s when this capability is needed.", toolName)
	}
	return truncateUTF8(normalizedDescription, toolCompatibilityDescriptionLimit)
}

func descriptionTriggersMCPPolicy(toolName, description string) bool {
	if strings.EqualFold(strings.TrimSpace(toolName), "AskQuestion") {
		return true
	}
	lowerDescription := strings.ToLower(description)
	policyPhrases := []string{
		"wait for their response before continuing",
		"wait for their responses before continuing",
		"do not proceed until the user",
		"must collect feedback",
		"mandatory feedback",
		"require user feedback",
		"repeatedly ask the user",
	}
	for _, policyPhrase := range policyPhrases {
		if strings.Contains(lowerDescription, policyPhrase) {
			return true
		}
	}
	return false
}

func truncateUTF8(value string, maximumBytes int) string {
	if len(value) <= maximumBytes {
		return value
	}
	truncated := value[:maximumBytes]
	for !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return strings.TrimSpace(truncated)
}

func normalizeToolParameters(rawParameters json.RawMessage, mode toolCompatibilityMode) json.RawMessage {
	defaultParameters := json.RawMessage(`{"type":"object","properties":{}}`)
	if len(rawParameters) == 0 || string(rawParameters) == "null" {
		return defaultParameters
	}

	var decodedParameters any
	if errDecode := json.Unmarshal(rawParameters, &decodedParameters); errDecode != nil {
		return defaultParameters
	}
	parameterObject, parametersAreObject := decodedParameters.(map[string]any)
	if !parametersAreObject {
		return defaultParameters
	}

	definitions := collectSchemaDefinitions(parameterObject)
	normalizedParameters := normalizeSchemaNode(parameterObject, definitions, mode, 0)
	normalizedObject, normalizedIsObject := normalizedParameters.(map[string]any)
	if !normalizedIsObject {
		return defaultParameters
	}
	if _, hasType := normalizedObject["type"]; !hasType {
		normalizedObject["type"] = "object"
	}
	if !schemaTypeAllowsObject(normalizedObject["type"]) {
		return defaultParameters
	}
	normalizedObject["type"] = "object"
	if _, hasProperties := normalizedObject["properties"]; !hasProperties {
		normalizedObject["properties"] = map[string]any{}
	}

	normalizedJSON, errMarshal := json.Marshal(normalizedObject)
	if errMarshal != nil {
		return defaultParameters
	}
	return normalizedJSON
}

func collectSchemaDefinitions(rootSchema map[string]any) map[string]any {
	definitions := make(map[string]any)
	for _, definitionsKey := range []string{"$defs", "definitions"} {
		if definitionsObject, ok := rootSchema[definitionsKey].(map[string]any); ok {
			for definitionName, definition := range definitionsObject {
				definitions[definitionName] = definition
			}
		}
	}
	return definitions
}

func normalizeSchemaNode(node any, definitions map[string]any, mode toolCompatibilityMode, depth int) any {
	if depth > toolCompatibilitySchemaDepth {
		return map[string]any{}
	}
	switch typedNode := node.(type) {
	case bool:
		if typedNode {
			return map[string]any{}
		}
		return map[string]any{"not": map[string]any{}}
	case []any:
		normalizedItems := make([]any, 0, len(typedNode))
		for _, item := range typedNode {
			normalizedItems = append(normalizedItems, normalizeSchemaNode(item, definitions, mode, depth+1))
		}
		return normalizedItems
	case map[string]any:
		if mode == toolCompatibilityFallback {
			if reference, ok := typedNode["$ref"].(string); ok {
				const definitionPrefix = "#/$defs/"
				const legacyDefinitionPrefix = "#/definitions/"
				definitionName := ""
				switch {
				case strings.HasPrefix(reference, definitionPrefix):
					definitionName = strings.TrimPrefix(reference, definitionPrefix)
				case strings.HasPrefix(reference, legacyDefinitionPrefix):
					definitionName = strings.TrimPrefix(reference, legacyDefinitionPrefix)
				}
				if definition, found := definitions[definitionName]; found {
					resolvedDefinition := normalizeSchemaNode(definition, definitions, mode, depth+1)
					referenceSiblings := make(map[string]any, len(typedNode)-1)
					for siblingKey, siblingValue := range typedNode {
						if siblingKey != "$ref" {
							referenceSiblings[siblingKey] = siblingValue
						}
					}
					if len(referenceSiblings) == 0 {
						return resolvedDefinition
					}
					normalizedSiblings := normalizeSchemaNode(referenceSiblings, definitions, mode, depth+1)
					return map[string]any{"allOf": []any{resolvedDefinition, normalizedSiblings}}
				}
			}
		}

		normalizedObject := make(map[string]any)
		keys := make([]string, 0, len(typedNode))
		for key := range typedNode {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if shouldDropSchemaKeyword(key, mode) {
				continue
			}
			if key == "type" {
				if nullable, _ := typedNode["nullable"].(bool); nullable {
					normalizedObject["type"] = makeNullableSchemaType(typedNode["type"])
					continue
				}
			}
			value := typedNode[key]
			switch key {
			case "const":
				normalizedObject["enum"] = []any{value}
			case "enum":
				if constantValue, hasConstant := typedNode["const"]; hasConstant {
					normalizedObject["enum"] = []any{constantValue}
				} else {
					normalizedObject["enum"] = value
				}
			case "nullable":
				if nullable, ok := value.(bool); ok && nullable {
					normalizedObject["type"] = makeNullableSchemaType(typedNode["type"])
				}
			case "properties":
				properties, propertiesAreObject := value.(map[string]any)
				if !propertiesAreObject {
					continue
				}
				normalizedProperties := make(map[string]any, len(properties))
				for propertyName, propertySchema := range properties {
					normalizedProperties[propertyName] = normalizeSchemaNode(propertySchema, definitions, mode, depth+1)
				}
				normalizedObject[key] = normalizedProperties
			case "patternProperties", "dependentSchemas":
				schemaMap, schemaMapIsObject := value.(map[string]any)
				if !schemaMapIsObject {
					continue
				}
				normalizedSchemaMap := make(map[string]any, len(schemaMap))
				for schemaName, childSchema := range schemaMap {
					normalizedSchemaMap[schemaName] = normalizeSchemaNode(childSchema, definitions, mode, depth+1)
				}
				normalizedObject[key] = normalizedSchemaMap
			case "items", "additionalItems", "additionalProperties", "not", "contains", "propertyNames", "if", "then", "else":
				normalizedObject[key] = normalizeSchemaNode(value, definitions, mode, depth+1)
			case "allOf", "anyOf", "oneOf", "prefixItems":
				normalizedObject[key] = normalizeSchemaNode(value, definitions, mode, depth+1)
			default:
				normalizedObject[key] = value
			}
		}
		return normalizedObject
	default:
		return typedNode
	}
}

func shouldDropSchemaKeyword(keyword string, mode toolCompatibilityMode) bool {
	alwaysDropped := map[string]struct{}{
		"$schema": {}, "$id": {}, "$anchor": {}, "$dynamicAnchor": {},
		"$comment": {}, "examples": {}, "deprecated": {}, "readOnly": {},
		"writeOnly": {}, "contentEncoding": {}, "contentMediaType": {},
	}
	if _, shouldDrop := alwaysDropped[keyword]; shouldDrop {
		return true
	}
	if mode != toolCompatibilityFallback {
		return false
	}
	fallbackDropped := map[string]struct{}{
		"$defs": {}, "definitions": {}, "$ref": {}, "$dynamicRef": {},
		"unevaluatedItems": {}, "unevaluatedProperties": {},
		"dependentRequired": {}, "dependentSchemas": {},
	}
	_, shouldDrop := fallbackDropped[keyword]
	return shouldDrop
}

func makeNullableSchemaType(schemaType any) any {
	switch typedSchemaType := schemaType.(type) {
	case string:
		if typedSchemaType == "null" {
			return typedSchemaType
		}
		return []any{typedSchemaType, "null"}
	case []any:
		for _, existingType := range typedSchemaType {
			if existingType == "null" {
				return typedSchemaType
			}
		}
		return append(typedSchemaType, "null")
	default:
		return []any{"object", "null"}
	}
}

func schemaTypeAllowsObject(schemaType any) bool {
	if schemaType == "object" {
		return true
	}
	if schemaTypes, ok := schemaType.([]any); ok {
		for _, candidateType := range schemaTypes {
			if candidateType == "object" {
				return true
			}
		}
	}
	return false
}

func parseToolChoice(rawChoice json.RawMessage) (mode, selectedFunctionName string) {
	if len(rawChoice) == 0 || string(rawChoice) == "null" {
		return "auto", ""
	}
	var stringChoice string
	if json.Unmarshal(rawChoice, &stringChoice) == nil {
		switch strings.ToLower(strings.TrimSpace(stringChoice)) {
		case "none":
			return "none", ""
		case "required", "any":
			return "required", ""
		default:
			return "auto", ""
		}
	}
	var objectChoice struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(rawChoice, &objectChoice) != nil {
		return "auto", ""
	}
	selectedFunctionName = strings.TrimSpace(objectChoice.Function.Name)
	if selectedFunctionName == "" {
		selectedFunctionName = strings.TrimSpace(objectChoice.Name)
	}
	if selectedFunctionName != "" {
		return "function", selectedFunctionName
	}
	return "auto", ""
}

func compatibleMessageContent(rawContent json.RawMessage) string {
	if len(rawContent) == 0 || string(rawContent) == "null" {
		return ""
	}
	var textContent string
	if json.Unmarshal(rawContent, &textContent) == nil {
		return textContent
	}
	var contentValue any
	if json.Unmarshal(rawContent, &contentValue) != nil {
		return ""
	}
	if contentParts, ok := contentValue.([]any); ok {
		var textParts []string
		for _, contentPart := range contentParts {
			if renderedPart := renderMessageContentPart(contentPart); renderedPart != "" {
				textParts = append(textParts, renderedPart)
			}
		}
		return strings.Join(textParts, "\n")
	}
	compactJSON, errMarshal := json.Marshal(contentValue)
	if errMarshal != nil {
		return ""
	}
	return string(compactJSON)
}

func renderMessageContentPart(contentPart any) string {
	partObject, partIsObject := contentPart.(map[string]any)
	if !partIsObject {
		compactJSON, _ := json.Marshal(contentPart)
		return string(compactJSON)
	}
	for _, textKey := range []string{"text", "input_text", "output_text"} {
		if text, ok := partObject[textKey].(string); ok && text != "" {
			return text
		}
	}
	if imageURL, ok := partObject["image_url"].(map[string]any); ok {
		if imageAddress, ok := imageURL["url"].(string); ok && imageAddress != "" {
			return renderImageAddress(imageAddress)
		}
	}
	if imageAddress, ok := partObject["image_url"].(string); ok && imageAddress != "" {
		return renderImageAddress(imageAddress)
	}
	partType, _ := partObject["type"].(string)
	if partType == "image" || partType == "audio" {
		mediaType, _ := partObject["mimeType"].(string)
		if mediaType == "" {
			mediaType, _ = partObject["mime_type"].(string)
		}
		if mediaType == "" {
			mediaType = "unknown media type"
		}
		encodedData, _ := partObject["data"].(string)
		return fmt.Sprintf("[%s: %s, %d encoded bytes omitted]", partType, mediaType, len(encodedData))
	}
	if resource, ok := partObject["resource"].(map[string]any); ok {
		if resourceText, ok := resource["text"].(string); ok && resourceText != "" {
			return resourceText
		}
		if resourceURI, ok := resource["uri"].(string); ok && resourceURI != "" {
			return "[resource: " + resourceURI + "]"
		}
	}
	compactJSON, _ := json.Marshal(partObject)
	return string(compactJSON)
}

func renderImageAddress(imageAddress string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(imageAddress)), "data:") {
		mediaType := "embedded image"
		if separatorIndex := strings.IndexByte(imageAddress, ';'); separatorIndex > len("data:") {
			mediaType = imageAddress[len("data:"):separatorIndex]
		}
		return fmt.Sprintf("[image: %s data omitted, %d encoded bytes]", mediaType, len(imageAddress))
	}
	return "[image: " + imageAddress + "]"
}

func hasToolCompatibilityContext(request oaiRequest) bool {
	if len(request.Tools) > 0 {
		return true
	}
	for _, message := range request.Messages {
		if len(message.ToolCalls) > 0 || message.Role == "tool" {
			return true
		}
	}
	return false
}

func isMCPConfigurationError(err error) bool {
	if err == nil {
		return false
	}
	lowerMessage := strings.ToLower(err.Error())
	return strings.Contains(lowerMessage, "mcp configuration issue") ||
		(strings.Contains(lowerMessage, "permission_denied") && strings.Contains(lowerMessage, "mcp"))
}
