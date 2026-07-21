package main

import (
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
	session := mapAttr(attrs, meta, "session_token")
	id := deviceFingerprint(session)
	return providerConfig{
		endpoint:     mapAttrOr(attrs, meta, "endpoint", defaultEndpoint),
		clientName:   mapAttrOr(attrs, meta, "client_name", "windsurf"),
		extVersion:   mapAttrOr(attrs, meta, "ext_version", "1.48.2"),
		ideVersion:   mapAttrOr(attrs, meta, "ide_version", "3.3.18"),
		locale:       mapAttrOr(attrs, meta, "locale", "en"),
		sessionToken: session,
		teamID:       mapAttr(attrs, meta, "team_id"),
		userID:       mapAttr(attrs, meta, "user_id"),
		systemPrompt: mapAttrOr(attrs, meta, "system_prompt", defaultSystemPrompt),
		osJSON:       mapAttrOr(attrs, meta, "os_json", osJSON()),
		cpuJSON:      mapAttrOr(attrs, meta, "cpu_json", cpuJSON()),
		deviceID:     mapAttrOr(attrs, meta, "device_id", id.DeviceID),
		extPath:      mapAttrOr(attrs, meta, "ext_path", defaultExtPath()),
		hwHash:       mapAttrOr(attrs, meta, "hw_hash", id.HWHash),
		hash27:       mapAttrOr(attrs, meta, "hash27", id.Hash27),
		hex31:        mapAttr(attrs, meta, "hex31"),
	}
}

func defaultExtPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windsurf\resources\app\extensions\windsurf`
	}
	return "/opt/windsurf/resources/app/extensions/windsurf"
}
