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

	seen := map[string]bool{}
	var out []modelDef
	for _, method := range []string{"GetCliModelConfigs", "GetCommandModelConfigs"} {
		raw, err := modelConfigsRPC(ctx, client, cfg, meta, method)
		if err != nil {
			continue
		}
		for _, m := range parseModelEntries(raw) {
			if m.Wire == "" || seen[m.Wire] {
				continue
			}
			seen[m.Wire] = true
			out = append(out, m)
		}
	}
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

// parseModelEntries extracts model entries from a model-config protobuf
// response. Each top-level entry carries f1 = display name and f22 = the wire
// model id (an enum like "MODEL_CLAUDE_4_5_OPUS" for first-party models, or a
// plain slug like "glm-5-2" / "kimi-k2-7" / "claude-5-fable-medium" for others).
// The friendly id is derived from the display name; the wire id goes to f21 of
// GetChatMessage.
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
		display, wireID := entryDisplayWire(sub)
		if display == "" || wireID == "" || seen[wireID] {
			continue
		}
		seen[wireID] = true
		out = append(out, modelDef{ID: slugify(display), Display: display, Wire: wireID})
	}
	return out
}

// entryDisplayWire reads f1 (display) and f22 (wire id) from one model entry.
func entryDisplayWire(b []byte) (display, wire string) {
	r := newPR(b)
	for !r.eof() {
		f, w, sub, _, err := r.next()
		if err != nil {
			break
		}
		if w != 2 || !isPrintableText(sub) {
			continue
		}
		switch f {
		case 1:
			display = string(sub)
		case 22:
			wire = string(sub)
		}
	}
	return
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
