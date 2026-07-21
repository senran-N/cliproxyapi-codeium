package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// jwtEntry is a cached, short-lived api JWT for one account, plus the account
// identifiers parsed from its claims.
type jwtEntry struct {
	token  string
	exp    time.Time
	userID string
	teamID string
}

// jwtCache caches minted JWTs keyed by session token so concurrent requests do
// not each hit GetUserJwt. Entries refresh a minute before expiry.
type jwtCache struct {
	mu sync.Mutex
	m  map[string]jwtEntry
}

var jwts = &jwtCache{m: map[string]jwtEntry{}}

// refreshGroup collapses concurrent refreshes for the same account into a single
// GetUserJwt call (avoids a thundering herd when many requests find the cached
// JWT expired at once).
var refreshGroup singleflight.Group

func (c *jwtCache) get(key string) (jwtEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	return e, ok
}

func (c *jwtCache) put(key string, e jwtEntry) {
	c.mu.Lock()
	c.m[key] = e
	c.mu.Unlock()
}

// getValidJWT returns a non-expired api JWT (plus derived account ids), minting a
// new one when needed. Cache and refresh are keyed by session token, so distinct
// accounts never share state and only one refresh runs per account at a time.
func getValidJWT(ctx context.Context, client *http.Client, cfg providerConfig) (jwtEntry, error) {
	key := cfg.sessionToken
	if e, ok := jwts.get(key); ok && time.Until(e.exp) > time.Minute {
		return e, nil
	}

	v, err, _ := refreshGroup.Do(key, func() (any, error) {
		// Re-check: another goroutine may have refreshed while we waited.
		if e, ok := jwts.get(key); ok && time.Until(e.exp) > time.Minute {
			return e, nil
		}
		// Use a detached, bounded context for credential acquisition so one
		// caller cancelling does not poison the shared refresh for others.
		refreshCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		entry, errRefresh := refreshJWT(refreshCtx, client, cfg)
		if errRefresh != nil {
			return jwtEntry{}, errRefresh
		}
		jwts.put(key, entry)
		return entry, nil
	})
	if err != nil {
		return jwtEntry{}, err
	}
	return v.(jwtEntry), nil
}

// refreshJWT exchanges the persistent session token for a fresh api JWT via
// exa.auth_pb.AuthService/GetUserJwt (unary Connect, application/proto).
func refreshJWT(ctx context.Context, client *http.Client, cfg providerConfig) (jwtEntry, error) {
	if cfg.sessionToken == "" {
		return jwtEntry{}, fmt.Errorf("codeium auth: session_token is empty (not logged in)")
	}

	body := buildGetUserJwtRequest(cfg)

	url := strings.TrimRight(cfg.endpoint, "/") + "/exa.auth_pb.AuthService/GetUserJwt"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return jwtEntry{}, err
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("User-Agent", "connect-go/1.18.1 (go1.26.3)")

	resp, err := client.Do(httpReq)
	if err != nil {
		return jwtEntry{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return jwtEntry{}, fmt.Errorf("codeium auth: GetUserJwt HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}

	// GetUserJwtResponse { string jwt = 1 }
	token, err := parseFirstStringField(raw, 1)
	if err != nil || token == "" {
		return jwtEntry{}, fmt.Errorf("codeium auth: could not parse JWT from response: %v", err)
	}
	userID, teamID := jwtAccount(token)
	return jwtEntry{token: token, exp: jwtExpiry(token), userID: userID, teamID: teamID}, nil
}

// jwtAccount extracts the user id and team id from an api JWT's claims.
func jwtAccount(token string) (userID, teamID string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var claims struct {
		APIKey string `json:"api_key"`
		TeamID string `json:"team_id"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return
	}
	teamID = claims.TeamID
	// api_key looks like "devin-synthetic-apikey$account-...$user-...".
	if i := strings.LastIndex(claims.APIKey, "$user-"); i >= 0 {
		userID = claims.APIKey[i+1:]
	}
	return
}

// buildGetUserJwtRequest encodes GetUserJwtRequest { ClientMetadata metadata = 1 }.
func buildGetUserJwtRequest(cfg providerConfig) []byte {
	var req pw
	req.msg(1, metadataForJWT(cfg))
	return req.bytes()
}

// parseFirstStringField returns the first wire-type-2 value of the given field.
func parseFirstStringField(raw []byte, field int) (string, error) {
	r := newPR(raw)
	for !r.eof() {
		f, wire, sub, _, err := r.next()
		if err != nil {
			return "", err
		}
		if f == field && wire == 2 {
			return string(sub), nil
		}
	}
	return "", fmt.Errorf("field %d not found", field)
}

// jwtExpiry parses the exp claim of a JWT. Falls back to a short TTL on failure.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) >= 2 {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.Exp > 0 {
				return time.Unix(claims.Exp, 0)
			}
		}
	}
	return time.Now().Add(5 * time.Minute)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
