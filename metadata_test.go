package main

import "testing"

// TestGetUserJwtRequestRoundTrip encodes a GetUserJwtRequest and decodes it back,
// asserting the ClientMetadata fields land on the exact wire field numbers the
// backend expects. Uses only synthetic values (no real credentials).
//
// End-to-end wire compatibility with the live backend is covered by
// TestFetchModelCatalogLive (guarded by CODEIUM_SESSION_TOKEN).
func TestGetUserJwtRequestRoundTrip(t *testing.T) {
	cfg := providerConfig{
		clientName:   "windsurf",
		extVersion:   "1.48.2",
		sessionToken: "devin-session-token$SYNTHETIC",
		locale:       "en",
		ideVersion:   "3.3.18",
		deviceID:     "device-uuid",
		extPath:      `C:\Windsurf\ext`,
		hwHash:       "hwhash",
		teamID:       "devin-team$account-synthetic",
	}

	raw := buildGetUserJwtRequest(cfg)

	// Outer message: field 1 = ClientMetadata.
	r := newPR(raw)
	f, wire, sub, _, err := r.next()
	if err != nil || f != 1 || wire != 2 {
		t.Fatalf("expected outer field 1 (metadata), got f=%d wire=%d err=%v", f, wire, err)
	}

	got := map[int]string{}
	mr := newPR(sub)
	for !mr.eof() {
		mf, mwire, msub, _, merr := mr.next()
		if merr != nil {
			t.Fatalf("metadata decode error: %v", merr)
		}
		if mwire == 2 {
			got[mf] = string(msub)
		}
	}

	want := map[int]string{
		1:  "windsurf",
		2:  "1.48.2",
		3:  "devin-session-token$SYNTHETIC",
		4:  "en",
		5:  "windows",
		7:  "3.3.18",
		10: "device-uuid",
		12: "windsurf",
		17: `C:\Windsurf\ext`,
		24: "hwhash",
		26: "Pro",
		32: "devin-team$account-synthetic",
	}
	for f, v := range want {
		if got[f] != v {
			t.Errorf("metadata field %d: got %q want %q", f, got[f], v)
		}
	}
}
