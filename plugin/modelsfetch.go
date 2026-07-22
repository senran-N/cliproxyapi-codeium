package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
)

// The fetched catalogue, keyed by friendly base-family id (e.g. "claude-opus-4.8").
// dynBaseWire is the featured/default wire id; dynFamilies maps a reasoning-effort
// tier (low/medium/high/xhigh/max/none/minimal) to the matching variant wire id.
// Persist for the lifetime of the loaded library.
var (
	dynCatMu    sync.RWMutex
	dynBaseWire map[string]string            // family id -> default wire id
	dynFamilies map[string]map[string]string // family id -> tier -> wire id
)

// resolveDynamic maps a friendly id + reasoning effort to a wire id. It composes
// a thinking/context variant when the request asks for one (e.g.
// "claude-opus-4.8" + effort "high" -> "claude-opus-4-8-high").
func resolveDynamic(id, effort string) (string, bool) {
	dynCatMu.RLock()
	defer dynCatMu.RUnlock()
	fam, ok := dynFamilies[id]
	if !ok {
		w, okBase := dynBaseWire[id]
		return w, okBase
	}
	if tier := normalizeEffort(effort); tier != "" {
		if w, okTier := fam[tier]; okTier {
			return w, true
		}
	}
	if w, okBase := dynBaseWire[id]; okBase {
		return w, true
	}
	return "", false
}

// normalizeEffort maps an OpenAI reasoning_effort / thinking level to a tier key.
func normalizeEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return "minimal"
	case "low":
		return "low"
	case "medium", "auto", "default":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "very-high", "veryhigh":
		return "xhigh"
	case "max", "maximum":
		return "max"
	case "none", "off", "no", "disabled":
		return "none"
	default:
		return ""
	}
}

// tierFromDisplay extracts a tier key from a variant display name given its base
// family name, e.g. base "GPT-5.6 Sol", display "GPT-5.6 Sol High Thinking" -> "high".
// "Fast"/priority speed variants are ignored (returns "").
func tierFromDisplay(display, base string) string {
	suffix := strings.TrimSpace(strings.TrimPrefix(display, base))
	low := strings.ToLower(suffix)
	if strings.Contains(low, "fast") || strings.Contains(low, "priority") {
		return ""
	}
	switch {
	case low == "":
		return "medium"
	case strings.Contains(low, "no thinking"), low == "none":
		return "none"
	case strings.Contains(low, "minimal"):
		return "minimal"
	case strings.Contains(low, "x-high"), strings.Contains(low, "xhigh"):
		return "xhigh"
	case strings.Contains(low, "high"):
		return "high"
	case strings.Contains(low, "medium"):
		return "medium"
	case strings.Contains(low, "low"):
		return "low"
	case strings.Contains(low, "max"):
		return "max"
	default:
		return ""
	}
}

// fetchModelCatalog queries the backend for the account's full model catalogue
// (chat + command model configs) and returns friendly model definitions. On any
// error it returns nil so callers fall back to the static list.
func fetchModelCatalog(ctx context.Context, cfg providerConfig) []modelDef {
	client := http.DefaultClient
	entry, err := getValidJWT(ctx, client, cfg)
	if err != nil {
		return nil
	}
	if cfg.teamID == "" {
		cfg.teamID = entry.teamID
	}
	if cfg.userID == "" {
		cfg.userID = entry.userID
	}
	meta := metadataForChat(cfg, entry.token)

	raw, err := modelConfigsRPC(ctx, client, cfg, meta, "GetCliModelConfigs")
	if err != nil {
		return nil
	}
	entries := parseModelEntries(raw)
	if len(entries) == 0 {
		return nil
	}

	families := map[string]map[string]string{} // family id -> tier -> wire
	bestCtx := map[string]map[string]uint64{}  // family id -> tier -> best context seen
	baseName := map[string]string{}            // family id -> base display
	featuredTier := map[string]string{}        // family id -> featured entry's tier
	for _, e := range entries {
		if e.wire == "" {
			continue
		}
		base := e.base
		if base == "" {
			base = e.display
		}
		fam := slugify(base)
		if fam == "" {
			continue
		}
		tier := tierFromDisplay(e.display, base)
		if tier == "" {
			continue // skip fast/priority speed variants
		}
		if families[fam] == nil {
			families[fam] = map[string]string{}
			bestCtx[fam] = map[string]uint64{}
			baseName[fam] = base
		}
		// Prefer the largest-context variant for each tier (default = max context).
		if cur, ok := families[fam][tier]; !ok || e.context >= bestCtx[fam][tier] {
			_ = cur
			bestCtx[fam][tier] = e.context
			families[fam][tier] = e.wire
		}
		if e.featured {
			featuredTier[fam] = tier
		}
	}

	baseWire := map[string]string{}
	var list []modelDef
	for fam, ft := range featuredTier {
		wire := families[fam][ft]
		if wire == "" {
			continue
		}
		baseWire[fam] = wire
		list = append(list, modelDef{ID: fam, Display: baseName[fam], Wire: wire})
	}
	if len(list) == 0 {
		return nil
	}
	dynCatMu.Lock()
	dynBaseWire = baseWire
	dynFamilies = families
	dynCatMu.Unlock()
	return list
}

func modelConfigsRPC(ctx context.Context, client *http.Client, cfg providerConfig, meta []byte, method string) ([]byte, error) {
	var req pw
	req.msg(1, meta)
	url := strings.TrimRight(cfg.endpoint, "/") + "/exa.api_server_pb.ApiServerService/" + method
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.bytes()))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("User-Agent", "connect-go/1.18.1 (go1.26.3)")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, io.EOF
	}
	return io.ReadAll(resp.Body)
}

// parseModelEntries extracts the featured (primary) model entries from a
// model-config protobuf response. Each entry carries f1 = full display name,
// f22 = wire model id (sent as GetChatMessage f21), f11 = featured flag, and
// f30.f1 = the base family display name.
//
// The backend returns ~150 entries (every model x thinking tier x fast x context
// variant). We surface the featured entries (f11=1) collapsed down to ONE entry
// per base model family (f30.f1) — the ~10 clean names the Devin picker shows by
// default (claude-opus-4.5, gpt-5.2, gemini-3-flash, …). The extra thinking/
// context variants of a family are dropped from the list but stay callable by
// passing their exact wire id (e.g. "claude-opus-4-8-high").
func parseModelEntries(raw []byte) []modelEntry {
	var out []modelEntry
	r := newPR(raw)
	for !r.eof() {
		_, wire, sub, _, err := r.next()
		if err != nil {
			break
		}
		if wire != 2 {
			continue
		}
		e := parseModelEntry(sub)
		if e.wire == "" || e.display == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

type modelEntry struct {
	display, wire, base string
	featured            bool
	context             uint64 // f18 = context window length
}

// parseModelEntry reads f1/f22/f11/f18 and the nested f30.f1 base name.
func parseModelEntry(b []byte) modelEntry {
	var e modelEntry
	r := newPR(b)
	for !r.eof() {
		f, w, sub, v, err := r.next()
		if err != nil {
			break
		}
		switch {
		case w == 0 && f == 11:
			e.featured = v == 1
		case w == 0 && f == 18:
			e.context = v
		case w == 2 && f == 1 && isPrintableText(sub):
			e.display = string(sub)
		case w == 2 && f == 22 && isPrintableText(sub):
			e.wire = string(sub)
		case w == 2 && f == 30:
			if base, err := parseFirstStringField(sub, 1); err == nil {
				e.base = base
			}
		}
	}
	return e
}

func isPrintableText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c >= 32 && c < 127 || c == 9 || c == 10 || c == 13 {
			printable++
		}
	}
	return float64(printable)/float64(len(b)) > 0.9
}

// slugify turns a display name into a friendly model id, e.g.
// "Claude Opus 4.5 Thinking" -> "claude-opus-4.5-thinking".
func slugify(display string) string {
	var b strings.Builder
	prevDash := false
	for _, c := range strings.ToLower(strings.TrimSpace(display)) {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.':
			b.WriteRune(c)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
