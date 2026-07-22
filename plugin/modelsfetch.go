package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// dynamicCatalog contains one account's friendly-model resolution tables.
// Accounts can expose different models or map the same friendly id to different
// wire variants, so catalogues must never be shared across credentials.
type dynamicCatalog struct {
	baseWire map[string]string
	families map[string]map[string]string
}

var (
	dynamicCatalogsMu sync.RWMutex
	dynamicCatalogs   = map[string]dynamicCatalog{}
)

// resolveDynamic maps a friendly id + reasoning effort to a wire id. It composes
// a thinking/context variant when the request asks for one (e.g.
// "claude-opus-4.8" + effort "high" -> "claude-opus-4-8-high").
func resolveDynamic(accountKey, id, effort string) (string, bool) {
	dynamicCatalogsMu.RLock()
	defer dynamicCatalogsMu.RUnlock()
	catalog, catalogExists := dynamicCatalogs[accountKey]
	if !catalogExists {
		return "", false
	}
	fam, ok := catalog.families[id]
	if !ok {
		w, okBase := catalog.baseWire[id]
		return w, okBase
	}
	if tier := normalizeEffort(effort); tier != "" {
		if w, okTier := fam[tier]; okTier {
			return w, true
		}
	}
	if w, okBase := catalog.baseWire[id]; okBase {
		return w, true
	}
	return "", false
}

func storeDynamicCatalog(accountKey string, baseWire map[string]string, families map[string]map[string]string) {
	dynamicCatalogsMu.Lock()
	dynamicCatalogs[accountKey] = dynamicCatalog{baseWire: baseWire, families: families}
	dynamicCatalogsMu.Unlock()
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
	return fetchModelCatalogWithClient(ctx, http.DefaultClient, cfg)
}

func fetchModelCatalogWithClient(ctx context.Context, client *http.Client, cfg providerConfig) []modelDef {
	if client == nil {
		client = http.DefaultClient
	}
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

	list, baseWire, families := buildCatalog(entries)
	if len(list) == 0 {
		return nil
	}
	storeDynamicCatalog(cfg.sessionToken, baseWire, families)
	return list
}

// buildCatalog turns raw model entries into the client-facing model list plus the
// resolution tables (family/context key -> default wire, and key -> tier -> wire).
// Split out from fetchModelCatalog so the bucketing is unit-testable without a
// live account.
func buildCatalog(entries []modelEntry) (list []modelDef, baseWire map[string]string, families map[string]map[string]string) {
	// Pass 1: learn each base family's default (featured) tier + context window.
	// The featured entry is what the Devin picker shows by default — for GLM that
	// is the 200K "High", not the 1M variant — so we treat its context as the
	// family default rather than forcing the largest window.
	baseName := map[string]string{}     // family slug -> base display
	featuredTier := map[string]string{} // family slug -> featured entry's tier
	featuredCtx := map[string]uint64{}  // family slug -> featured entry's context
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
		if _, ok := baseName[fam]; !ok {
			baseName[fam] = base
		}
		if e.featured {
			if tier := tierFromDisplay(e.display, base); tier != "" {
				featuredTier[fam] = tier
				featuredCtx[fam] = e.context
			}
		}
	}

	// Pass 2: bucket every variant into a model key. Variants at the family's
	// default context land under the plain family id (e.g. "glm-5.2"); variants
	// with a *larger* window get a context-suffixed sibling id (e.g.
	// "glm-5.2-1m"). Within a key, the tier (thinking effort) selects the wire.
	families = map[string]map[string]string{} // model key -> tier -> wire
	keyDisplay := map[string]string{}         // model key -> display name
	keyFam := map[string]string{}             // model key -> base family slug
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
		if _, ok := featuredTier[fam]; !ok {
			continue // only surface families the picker features
		}
		tier := tierFromDisplay(e.display, base)
		if tier == "" {
			continue // skip fast/priority speed variants
		}
		defCtx := featuredCtx[fam]
		key := fam
		display := baseName[fam]
		if defCtx > 0 && e.context > defCtx {
			label := ctxLabel(e.context)
			key = fam + "-" + label
			display = baseName[fam] + " (" + label + " context)"
		} else if defCtx > 0 && e.context < defCtx {
			continue // smaller-than-default windows are not surfaced
		}
		if families[key] == nil {
			families[key] = map[string]string{}
			keyDisplay[key] = display
			keyFam[key] = fam
		}
		if _, ok := families[key][tier]; !ok {
			families[key][tier] = e.wire
		}
	}

	baseWire = map[string]string{}
	for key, tiers := range families {
		wire := tiers[featuredTier[keyFam[key]]]
		if wire == "" {
			for _, w := range tiers {
				wire = w
				break
			}
		}
		if wire == "" {
			continue
		}
		baseWire[key] = wire
		list = append(list, modelDef{ID: key, Display: keyDisplay[key], Wire: wire})
	}
	sort.Slice(list, func(firstIndex, secondIndex int) bool {
		return list[firstIndex].ID < list[secondIndex].ID
	})
	return list, baseWire, families
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
// variant). We surface the featured families (f11=1) as clean base ids
// (claude-opus-4.8, gpt-5.6-sol, glm-5.2, …) at their default context, plus one
// context-suffixed sibling per family that offers a larger window (e.g.
// glm-5.2-1m). The thinking tier is chosen at request time from reasoning_effort,
// so the picker stays small while every context/effort combination is reachable.
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

// ctxLabel renders a context-window length as a short id suffix: 200000 -> "200k",
// 1048576 -> "1m". Used to name the larger-context sibling of a base family.
func ctxLabel(n uint64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%dm", (n+500_000)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// slugify turns a display name into a friendly model id, e.g.
// "Claude Opus 4.8 High" -> "claude-opus-4.8-high".
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
