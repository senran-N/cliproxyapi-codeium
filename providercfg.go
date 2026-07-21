package main

import (
	"runtime"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// providerKey is the upstream provider identifier used across the SDK.
const providerKey = "codeium"

// defaultEndpoint is the Codeium/Windsurf backend base URL.
const defaultEndpoint = "https://server.codeium.com"

// providerConfig holds everything required to talk to the Codeium backend on
// behalf of one logged-in account. All values are sourced from the auth record's
// immutable Attributes so the user simply drops their credentials into a JSON
// auth file (see README).
//
// The opaque fingerprint fields (osJSON, cpuJSON, deviceID, extPath, hwHash,
// hash27, hex31) are machine-specific values captured from the real client. They
// identify the local install to the backend; using the values from your own
// machine keeps requests indistinguishable from the official app.
type providerConfig struct {
	endpoint     string
	clientName   string // "windsurf"
	extVersion   string // e.g. "1.48.2"
	ideVersion   string // e.g. "3.3.18"
	locale       string // e.g. "en"
	sessionToken string // "devin-session-token$<jwt>"  (persistent login secret)
	teamID       string // "devin-team$account-..."
	userID       string // "user-..."
	systemPrompt string // Cascade system prompt (optional override)

	// Opaque device/fingerprint fields.
	osJSON   string
	cpuJSON  string
	deviceID string
	extPath  string
	hwHash   string // metadata f24 (GetUserJwt)
	hash27   string // metadata f27 (GetChatMessage)
	hex31    string // metadata f31 (GetChatMessage)
}

// attr reads a configuration value for the auth, preferring Attributes (set
// programmatically) and falling back to Metadata (populated verbatim from the
// auth JSON file by the file token store).
func attr(a *coreauth.Auth, key string) string {
	if a == nil {
		return ""
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes[key]); v != "" {
			return v
		}
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata[key].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func attrOr(a *coreauth.Auth, key, def string) string {
	if v := attr(a, key); v != "" {
		return v
	}
	return def
}

// configFromAuth extracts a providerConfig from an auth record. Device and
// hardware fingerprints default to a locally generated, persisted install
// identity (see fingerprint.go) so the provider is usable on any machine without
// pasting captured values. Only session_token is required from the user;
// team_id / user_id are derived from the minted JWT when left empty.
func configFromAuth(a *coreauth.Auth) providerConfig {
	session := attr(a, "session_token")
	// Per-account fingerprint keyed by the session token (see fingerprint.go).
	id := deviceFingerprint(session)
	return providerConfig{
		endpoint:     attrOr(a, "endpoint", defaultEndpoint),
		clientName:   attrOr(a, "client_name", "windsurf"),
		extVersion:   attrOr(a, "ext_version", "1.48.2"),
		ideVersion:   attrOr(a, "ide_version", "3.3.18"),
		locale:       attrOr(a, "locale", "en"),
		sessionToken: session,
		teamID:       attr(a, "team_id"),
		userID:       attr(a, "user_id"),
		systemPrompt: attrOr(a, "system_prompt", defaultSystemPrompt),
		osJSON:       attrOr(a, "os_json", osJSON()),
		cpuJSON:      attrOr(a, "cpu_json", cpuJSON()),
		deviceID:     attrOr(a, "device_id", id.DeviceID),
		extPath:      attrOr(a, "ext_path", defaultExtPath()),
		hwHash:       attrOr(a, "hw_hash", id.HWHash),
		hash27:       attrOr(a, "hash27", id.Hash27),
		hex31:        attr(a, "hex31"), // structured blob; omitted unless captured value supplied
	}
}

// defaultExtPath returns a plausible extension path for the current OS. It is
// telemetry only; the exact value is not validated by the backend.
func defaultExtPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windsurf\resources\app\extensions\windsurf`
	}
	return "/opt/windsurf/resources/app/extensions/windsurf"
}

// defaultSystemPrompt is a compact Cascade-style system prompt used when the
// caller does not supply one. The backend expects an agentic coding assistant
// persona; keeping this short avoids surprising behaviour while remaining valid.
const defaultSystemPrompt = "You are Cascade, a powerful agentic AI coding assistant. Be terse, direct, and accurate. Answer the user's request; use tools when they are provided."
