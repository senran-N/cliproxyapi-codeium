package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

// TestFingerprintIsolationAndStability proves each account (seed) gets its own
// stable fingerprint and different accounts never collide.
func TestFingerprintIsolationAndStability(t *testing.T) {
	a1 := deviceFingerprint("devin-session-token$acctA")
	a2 := deviceFingerprint("devin-session-token$acctA")
	b := deviceFingerprint("devin-session-token$acctB")

	if a1 != a2 {
		t.Fatalf("same seed must be stable: %+v vs %+v", a1, a2)
	}
	if a1.DeviceID == b.DeviceID || a1.HWHash == b.HWHash || a1.Hash27 == b.Hash27 {
		t.Fatalf("different accounts must not share fingerprints: %+v vs %+v", a1, b)
	}
	if len(a1.DeviceID) != 36 || len(a1.HWHash) != 128 || len(a1.Hash27) != 64 {
		t.Fatalf("wrong fingerprint lengths: %+v", a1)
	}
}

// TestConfigPerAuthIsolation proves two auth records produce independent configs
// with independent fingerprints derived from their own tokens.
func TestConfigPerAuthIsolation(t *testing.T) {
	c1 := configFromAuth(newAuth("devin-session-token$one"))
	c2 := configFromAuth(newAuth("devin-session-token$two"))
	if c1.deviceID == c2.deviceID || c1.hwHash == c2.hwHash {
		t.Fatalf("configs must not share device identity across accounts")
	}
}

// TestJWTCacheConcurrent exercises the cache and cache-hit path of getValidJWT
// from many goroutines. Run with -race to catch data races.
func TestJWTCacheConcurrent(t *testing.T) {
	const token = "devin-session-token$cachehit"
	jwts.put(token, jwtEntry{token: "cached-jwt", exp: time.Now().Add(time.Hour), userID: "user-x", teamID: "team-y"})
	cfg := configFromAuth(newAuth(token))

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := getValidJWT(context.Background(), http.DefaultClient, cfg)
			if err != nil || e.token != "cached-jwt" || e.userID != "user-x" {
				t.Errorf("unexpected cache result: %+v err=%v", e, err)
			}
		}()
	}
	wg.Wait()
}

// TestSingleflightCollapses proves concurrent refreshes for one key run the
// expensive work exactly once (the mechanism used by getValidJWT).
func TestSingleflightCollapses(t *testing.T) {
	var g singleflight.Group
	var calls int32
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = g.Do("same-key", func() (any, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(20 * time.Millisecond) // simulate a slow GetUserJwt
				return 0, nil
			})
		}()
	}
	close(start)
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected exactly 1 collapsed refresh, got %d", n)
	}
}

func newAuth(sessionToken string) *coreauth.Auth {
	return &coreauth.Auth{
		Provider:   providerKey,
		Attributes: map[string]string{"session_token": sessionToken},
	}
}
