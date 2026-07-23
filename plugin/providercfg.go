package main

import (
	"encoding/json"
	"runtime"
	"strings"
)

const (
	providerKey     = "codeium"
	defaultEndpoint = "https://server.codeium.com"

	defaultSystemPrompt = "You are Cascade, a powerful agentic AI coding assistant. Be terse, direct, and accurate. Answer the user's request; use tools when they are provided."
)

// providerConfig holds everything required to talk to the Codeium backend for one
// logged-in account. In the plugin build the values come from the host-supplied
// auth attributes / metadata rather than a coreauth.Auth.
type providerConfig struct {
	endpoint     string
	clientName   string
	extVersion   string
	ideVersion   string
	locale       string
	sessionToken string
	teamID       string
	userID       string
	systemPrompt string

	osJSON   string
	cpuJSON  string
	deviceID string
	extPath  string
	hwHash   string
	hash27   string
	hex31    string
}

// mapAttr reads a string value preferring attrs (immutable) then meta (mutable),
// mirroring the standalone provider's Attributes/Metadata lookup order.
func mapAttr(attrs map[string]string, meta map[string]any, key string) string {
	if attrs != nil {
		if v := strings.TrimSpace(attrs[key]); v != "" {
			return v
		}
	}
	if meta != nil {
		if v, ok := meta[key].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func mapAttrOr(attrs map[string]string, meta map[string]any, key, def string) string {
	if v := mapAttr(attrs, meta, key); v != "" {
		return v
	}
	return def
}

// configFromMaps builds a providerConfig from host auth attributes + metadata.
func configFromMaps(attrs map[string]string, meta map[string]any) providerConfig {
	return configFromAuthData(nil, attrs, meta)
}

// configFromAuthData combines provider-owned persisted credentials with host
// metadata and explicit attributes. Attributes remain the highest-priority
// source so administrators can override non-secret defaults when necessary.
func configFromAuthData(storageJSON []byte, attrs map[string]string, meta map[string]any) providerConfig {
	storageMetadata := make(map[string]any)
	if len(storageJSON) > 0 {
		_ = json.Unmarshal(storageJSON, &storageMetadata)
	}
	mergedMetadata := make(map[string]any, len(storageMetadata)+len(meta))
	for key, value := range storageMetadata {
		mergedMetadata[key] = value
	}
	for key, value := range meta {
		mergedMetadata[key] = value
	}

	// Older manually imported files sometimes nested their credential values
	// under "attributes". Accept that shape without allowing it to override
	// top-level metadata or explicit runtime attributes.
	if legacyAttributes, ok := mergedMetadata["attributes"].(map[string]any); ok {
		for key, value := range legacyAttributes {
			if _, alreadyDefined := mergedMetadata[key]; !alreadyDefined {
				mergedMetadata[key] = value
			}
		}
	}

	session := mapAttr(attrs, mergedMetadata, "session_token")
	id := deviceFingerprint(session)
	return providerConfig{
		endpoint:     mapAttrOr(attrs, mergedMetadata, "endpoint", defaultEndpoint),
		clientName:   mapAttrOr(attrs, mergedMetadata, "client_name", "windsurf"),
		extVersion:   mapAttrOr(attrs, mergedMetadata, "ext_version", "1.48.2"),
		ideVersion:   mapAttrOr(attrs, mergedMetadata, "ide_version", "3.3.18"),
		locale:       mapAttrOr(attrs, mergedMetadata, "locale", "en"),
		sessionToken: session,
		teamID:       mapAttr(attrs, mergedMetadata, "team_id"),
		userID:       mapAttr(attrs, mergedMetadata, "user_id"),
		systemPrompt: mapAttrOr(attrs, mergedMetadata, "system_prompt", defaultSystemPrompt),
		osJSON:       mapAttrOr(attrs, mergedMetadata, "os_json", osJSON()),
		cpuJSON:      mapAttrOr(attrs, mergedMetadata, "cpu_json", cpuJSON()),
		deviceID:     mapAttrOr(attrs, mergedMetadata, "device_id", id.DeviceID),
		extPath:      mapAttrOr(attrs, mergedMetadata, "ext_path", defaultExtPath()),
		hwHash:       mapAttrOr(attrs, mergedMetadata, "hw_hash", id.HWHash),
		hash27:       mapAttrOr(attrs, mergedMetadata, "hash27", id.Hash27),
		hex31:        mapAttr(attrs, mergedMetadata, "hex31"),
	}
}

func defaultExtPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windsurf\resources\app\extensions\windsurf`
	}
	return "/opt/windsurf/resources/app/extensions/windsurf"
}
