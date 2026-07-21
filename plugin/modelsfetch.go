package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
)

// dynamicCatalog caches the account's real model list (friendly id -> wire enum),
// fetched from the backend. It persists for the lifetime of the loaded library.
var (
	dynCatMu   sync.RWMutex
	dynCatalog map[string]string // friendly id -> wire enum
)

// resolveDynamic maps a friendly id to its wire enum using the fetched catalog.
func resolveDynamic(id string) (string, bool) {
	dynCatMu.RLock()
	defer dynCatMu.RUnlock()
	w, ok := dynCatalog[id]
	return w, ok
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
	out := parseModelEntries(raw)
	if len(out) == 0 {
		return nil
	}
	cat := make(map[string]string, len(out))
	for _, m := range out {
		cat[m.ID] = m.Wire
	}
	dynCatMu.Lock()
	dynCatalog = cat
	dynCatMu.Unlock()
	return out
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
// variant). We surface only the featured entries (f11=1) — the handful the Devin
// picker shows by default — named by their clean family name (glm-5.2,
// claude-opus-4.8, kimi-k2.7, …). Non-featured variants stay callable by passing
// their exact wire id (e.g. "claude-opus-4-8-high").
func parseModelEntries(raw []byte) []modelDef {
	var out []modelDef
	seen := map[string]bool{}
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
		if e.wire == "" || e.display == "" || !e.featured {
			continue
		}
		name := e.base
		if name == "" {
			name = e.display
		}
		id := slugify(name)
		if id == "" || seen[id] {
			id = slugify(e.display) // disambiguate collisions (e.g. two SWE variants)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, modelDef{ID: id, Display: e.display, Wire: e.wire})
	}
	return out
}

type modelEntry struct {
	display, wire, base string
	featured            bool
}

// parseModelEntry reads f1/f22/f11 and the nested f30.f1 base name.
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
