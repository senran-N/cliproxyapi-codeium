package main

// modelDef maps a friendly, OpenAI-style model id (what clients send and what
// /v1/models advertises) to the upstream wire value placed in GetChatMessage
// field 21.
//
// IMPORTANT: the backend does NOT accept display names. It expects an internal
// enum such as MODEL_CLAUDE_4_5_OPUS / MODEL_PRIVATE_2 / MODEL_GOOGLE_GEMINI_2_5_FLASH,
// or the SWE ids (swe-1-7). These pairings were read from the client's
// GetCliModelConfigs / GetCommandModelConfigs catalogs and verified live.
type modelDef struct {
	ID      string // friendly id (client-facing)
	Display string
	Wire    string // upstream f21 enum / id
}

var codeiumModels = []modelDef{
	// SWE (non-premium, always available)
	{"swe-1-7", "SWE-1.7", "swe-1-7"},
	{"swe-1-6", "SWE-1.6", "swe-1-6"},

	// Anthropic
	{"claude-opus-4.5", "Claude Opus 4.5", "MODEL_CLAUDE_4_5_OPUS"},
	{"claude-opus-4.5-thinking", "Claude Opus 4.5 Thinking", "MODEL_CLAUDE_4_5_OPUS_THINKING"},
	{"claude-sonnet-4.5", "Claude Sonnet 4.5", "MODEL_PRIVATE_2"},
	{"claude-sonnet-4.5-thinking", "Claude Sonnet 4.5 Thinking", "MODEL_PRIVATE_3"},
	{"claude-haiku-4.5", "Claude Haiku 4.5", "MODEL_PRIVATE_11"},

	// OpenAI (GPT-5.2 thinking tiers)
	{"gpt-5.2", "GPT-5.2 Medium Thinking", "MODEL_GPT_5_2_MEDIUM"},
	{"gpt-5.2-none", "GPT-5.2 No Thinking", "MODEL_GPT_5_2_NONE"},
	{"gpt-5.2-low", "GPT-5.2 Low Thinking", "MODEL_GPT_5_2_LOW"},
	{"gpt-5.2-high", "GPT-5.2 High Thinking", "MODEL_GPT_5_2_HIGH"},
	{"gpt-5.2-xhigh", "GPT-5.2 XHigh Thinking", "MODEL_GPT_5_2_XHIGH"},

	// Google Gemini
	{"gemini-3-flash", "Gemini 3 Flash Medium", "MODEL_GOOGLE_GEMINI_3_0_FLASH_MEDIUM"},
	{"gemini-3-flash-minimal", "Gemini 3 Flash Minimal", "MODEL_GOOGLE_GEMINI_3_0_FLASH_MINIMAL"},
	{"gemini-3-flash-low", "Gemini 3 Flash Low", "MODEL_GOOGLE_GEMINI_3_0_FLASH_LOW"},
	{"gemini-3-flash-high", "Gemini 3 Flash High", "MODEL_GOOGLE_GEMINI_3_0_FLASH_HIGH"},
	{"gemini-2.5-flash", "Gemini 2.5 Flash", "MODEL_GOOGLE_GEMINI_2_5_FLASH"},
}

// resolveModelWire maps a client-supplied model id to its upstream wire value.
// A value that already looks like a wire id (MODEL_*, swe-*) or an unknown id is
// passed through unchanged, so callers may also send the raw enum directly.
// resolveModelWire maps a client model id (+ requested reasoning effort) to the
// upstream wire id. A base family id composes the matching thinking/context
// variant; an exact wire id passes through unchanged.
func resolveModelWire(id, effort string) string {
	if w, ok := resolveDynamic(id, effort); ok {
		return w
	}
	for _, m := range codeiumModels {
		if m.ID == id {
			return m.Wire
		}
	}
	return id
}
