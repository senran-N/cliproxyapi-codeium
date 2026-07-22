package main

import "testing"

// TestBuildCatalogContextSplit proves that a family with two context windows
// (GLM: 200K default + 1M) surfaces the plain id as the 200K model and a
// "-1m" sibling as the 1M model, and that reasoning-effort tiers still resolve
// within each context.
func TestBuildCatalogContextSplit(t *testing.T) {
	const accountKey = "account-context-split"
	entries := []modelEntry{
		// GLM-5.2 at 200K (the featured default context).
		{display: "GLM-5.2 High", wire: "glm-5-2-high", base: "GLM-5.2", featured: true, context: 200000},
		{display: "GLM-5.2 Max", wire: "glm-5-2-max", base: "GLM-5.2", context: 200000},
		// GLM-5.2 at 1M (larger window -> sibling id).
		{display: "GLM-5.2 High", wire: "glm-5-2-high-1m", base: "GLM-5.2", context: 1048576},
		{display: "GLM-5.2 Max", wire: "glm-5-2-max-1m", base: "GLM-5.2", context: 1048576},
		// A single-context family stays a lone base id with no sibling.
		{display: "Claude Opus 4.8 High", wire: "claude-opus-4-8-high", base: "Claude Opus 4.8", featured: true, context: 200000},
		{display: "Claude Opus 4.8 Max", wire: "claude-opus-4-8-max", base: "Claude Opus 4.8", context: 200000},
	}

	list, baseWire, families := buildCatalog(entries)

	ids := map[string]bool{}
	for _, m := range list {
		ids[m.ID] = true
	}
	for _, want := range []string{"glm-5.2", "glm-5.2-1m", "claude-opus-4.8"} {
		if !ids[want] {
			t.Errorf("expected model id %q in catalogue, got %v", want, list)
		}
	}
	if ids["claude-opus-4.8-1m"] {
		t.Errorf("single-context family must not get a context sibling: %v", list)
	}

	// Plain id defaults to the 200K featured wire; the sibling defaults to 1M.
	if got := baseWire["glm-5.2"]; got != "glm-5-2-high" {
		t.Errorf("glm-5.2 default wire = %q, want glm-5-2-high (200K)", got)
	}
	if got := baseWire["glm-5.2-1m"]; got != "glm-5-2-high-1m" {
		t.Errorf("glm-5.2-1m default wire = %q, want glm-5-2-high-1m (1M)", got)
	}

	// Effort composition resolves within each context window.
	cases := []struct{ id, effort, want string }{
		{"glm-5.2", "high", "glm-5-2-high"},
		{"glm-5.2", "max", "glm-5-2-max"},
		{"glm-5.2", "", "glm-5-2-high"}, // no effort -> featured default
		{"glm-5.2-1m", "high", "glm-5-2-high-1m"},
		{"glm-5.2-1m", "max", "glm-5-2-max-1m"},
	}
	storeDynamicCatalog(accountKey, baseWire, families)
	for _, c := range cases {
		if got := resolveModelWire(accountKey, c.id, c.effort); got != c.want {
			t.Errorf("resolveModelWire(%q, %q, %q) = %q, want %q", accountKey, c.id, c.effort, got, c.want)
		}
	}
}

func TestDynamicCatalogsAreIsolatedPerAccount(t *testing.T) {
	const modelID = "shared-friendly-model"
	storeDynamicCatalog("account-a", map[string]string{modelID: "wire-for-a"}, map[string]map[string]string{
		modelID: {"high": "wire-for-a-high"},
	})
	storeDynamicCatalog("account-b", map[string]string{modelID: "wire-for-b"}, map[string]map[string]string{
		modelID: {"high": "wire-for-b-high"},
	})

	if got := resolveModelWire("account-a", modelID, "high"); got != "wire-for-a-high" {
		t.Fatalf("account-a resolved to %q", got)
	}
	if got := resolveModelWire("account-b", modelID, "high"); got != "wire-for-b-high" {
		t.Fatalf("account-b resolved to %q", got)
	}
	if got := resolveModelWire("unknown-account", modelID, "high"); got != modelID {
		t.Fatalf("unknown account resolved to %q, want pass-through model id", got)
	}
}

func TestBuildCatalogReturnsStableModelOrder(t *testing.T) {
	entries := []modelEntry{
		{display: "Zulu High", wire: "zulu-high", base: "Zulu", featured: true, context: 200000},
		{display: "Alpha High", wire: "alpha-high", base: "Alpha", featured: true, context: 200000},
	}

	list, _, _ := buildCatalog(entries)
	if len(list) != 2 || list[0].ID != "alpha" || list[1].ID != "zulu" {
		t.Fatalf("catalogue order is not stable: %+v", list)
	}
}

func TestCtxLabel(t *testing.T) {
	for _, c := range []struct {
		n    uint64
		want string
	}{
		{200000, "200k"},
		{1048576, "1m"},
		{1000000, "1m"},
		{500000, "500k"},
	} {
		if got := ctxLabel(c.n); got != c.want {
			t.Errorf("ctxLabel(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}
