package main

// modelDef maps a friendly, OpenAI-style model id (what clients send and what
// /v1/models advertises) to the upstream wire value placed in GetChatMessage
// field 21.
type modelDef struct {
	ID                string // friendly id (client-facing)
	Display           string
	Wire              string // upstream f21 wire id
	SupportsImages    bool
	ImageSupportKnown bool
}

// codeiumModels is only a minimal fallback used when the live model fetch fails.
// The real, current catalogue (Claude/GPT/GLM/Kimi/Gemini families, whatever the
// account offers today) is fetched dynamically at runtime — see modelsfetch.go —
// so no volatile model names are hardcoded here. The SWE ids are stable and
// always available regardless of account entitlements.
var codeiumModels = []modelDef{
	{ID: "swe-1-7", Display: "SWE-1.7", Wire: "swe-1-7"},
	{ID: "swe-1-6", Display: "SWE-1.6", Wire: "swe-1-6"},
}

// resolveModelWire maps a client-supplied model id to its upstream wire value.
// A value that already looks like a wire id (MODEL_*, swe-*) or an unknown id is
// passed through unchanged, so callers may also send the raw enum directly.
// resolveModelWire maps a client model id (+ requested reasoning effort) to the
// upstream wire id. A base family id composes the matching thinking/context
// variant; an exact wire id passes through unchanged.
func resolveModelWire(accountKey, id, effort string) string {
	if w, ok := resolveDynamic(accountKey, id, effort); ok {
		return w
	}
	for _, m := range codeiumModels {
		if m.ID == id {
			return m.Wire
		}
	}
	return id
}
