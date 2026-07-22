package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestFetchModelCatalogLive queries the real backend for the account's model
// catalogue. Skipped unless CODEIUM_SESSION_TOKEN is set.
func TestFetchModelCatalogLive(t *testing.T) {
	tok := strings.TrimSpace(os.Getenv("CODEIUM_SESSION_TOKEN"))
	if tok == "" {
		t.Skip("set CODEIUM_SESSION_TOKEN")
	}
	cfg := configFromAuth(&coreauth.Auth{Attributes: map[string]string{"session_token": tok}})
	models := fetchModelCatalog(context.Background(), cfg)
	if len(models) == 0 {
		t.Fatal("fetchModelCatalog returned no models")
	}
	fmt.Printf("\n===== FETCHED %d MODELS =====\n", len(models))
	for _, m := range models {
		fmt.Printf("  %-34s -> %s\n", m.ID, m.Wire)
	}
	fmt.Println("----- variant composition (base + effort) -----")
	for _, id := range []string{"claude-opus-4.8", "gpt-5.6-sol", "glm-5.2"} {
		for _, eff := range []string{"", "low", "high", "xhigh", "max"} {
			fmt.Printf("  %-16s effort=%-6s -> %s\n", id, eff, resolveModelWire(cfg.sessionToken, id, eff))
		}
	}
	fmt.Println("=============================")

	// Verify f22 is the correct GetChatMessage wire id by executing an arena model.
	var target string
	for _, m := range models {
		if strings.Contains(m.ID, "glm") || strings.Contains(m.ID, "fable") || strings.Contains(m.ID, "kimi") {
			target = m.ID
			break
		}
	}
	if target == "" {
		return
	}
	entry, err := getValidJWT(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}
	if cfg.teamID == "" {
		cfg.teamID = entry.teamID
	}
	if cfg.userID == "" {
		cfg.userID = entry.userID
	}
	fmt.Printf("executing arena model %q ...\n", target)
	oai := oaiRequest{Model: target, Messages: []oaiMessage{{Role: "user", Content: []byte(`"reply with exactly: pong"`)}}}
	msg, _ := buildChatRequest(cfg, entry.token, oai)
	env, _ := encodeEnvelope(msg, true)
	url := strings.TrimRight(cfg.endpoint, "/") + "/exa.api_server_pb.ApiServerService/GetChatMessage"
	hr, _ := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(env))
	hr.Header.Set("Content-Type", "application/connect+proto")
	hr.Header.Set("Connect-Protocol-Version", "1")
	hr.Header.Set("Connect-Content-Encoding", "gzip")
	hr.Header.Set("Connect-Accept-Encoding", "gzip")
	hr.Header.Set("User-Agent", "connect-go/1.18.1 (go1.26.3)")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var content, reasoning strings.Builder
	er := newEnvelopeReader(resp.Body)
	for {
		fr, e := er.read()
		if e != nil {
			break
		}
		if fr.end {
			continue
		}
		d := parseResponseFrame(fr.body)
		content.WriteString(d.content)
		reasoning.WriteString(d.reasoning)
	}
	fmt.Printf("arena model %q -> content=%q reasoning=%.60q\n", target, content.String(), reasoning.String())
}
