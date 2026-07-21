package main

import "time"

// Metadata builders for the shared Codeium "ClientMetadata" sub-message (field 1
// of both GetUserJwtRequest and GetChatMessageRequest).
//
// Field numbers were reverse-engineered from captured traffic:
//
//	f1  string  client name            ("windsurf")
//	f2  string  extension version      ("1.48.2")
//	f3  string  session token          ("devin-session-token$<jwt>")
//	f4  string  locale                 ("en")
//	f5  string  os info                ("windows" for auth; OS JSON for chat)
//	f7  string  ide version            ("3.3.18")
//	f8  string  cpu JSON               (chat only)
//	f9  varint  editor kind enum       (auth only, observed 1266)
//	f10 string  device UUID            (auth only)
//	f12 string  ide name               ("windsurf")
//	f17 string  extension path         (auth only)
//	f20 string  user id                (chat only)
//	f21 string  fresh api JWT          (chat only)
//	f24 string  hardware hash          (auth only)
//	f26 string  plan                   ("Pro", auth only)
//	f27 string  fingerprint hash       (chat only)
//	f31 string  fingerprint hex        (chat only)
//	f32 string  team id                ("devin-team$account-...")

// metadataForJWT builds the ClientMetadata used by the GetUserJwt refresh call.
// It authenticates using the persistent session token only (no api JWT yet).
func metadataForJWT(cfg providerConfig) []byte {
	var w pw
	w.str(1, cfg.clientName)
	w.str(2, cfg.extVersion)
	w.str(3, cfg.sessionToken)
	w.str(4, cfg.locale)
	w.str(5, "windows")
	w.str(7, cfg.ideVersion)
	w.varintAlways(9, 1266)
	w.str(10, cfg.deviceID)
	w.str(12, cfg.clientName)
	w.str(17, cfg.extPath)
	w.str(24, cfg.hwHash)
	w.str(26, "Pro")
	w.str(32, cfg.teamID)
	return w.bytes()
}

// metadataForChat builds the ClientMetadata used by GetChatMessage. It carries
// the freshly minted api JWT plus the session token.
func metadataForChat(cfg providerConfig, jwt string) []byte {
	var w pw
	w.str(1, cfg.clientName)
	w.str(2, cfg.extVersion)
	w.str(3, cfg.sessionToken)
	w.str(4, cfg.locale)
	w.str(5, cfg.osJSON)
	w.str(7, cfg.ideVersion)
	w.str(8, cfg.cpuJSON)
	w.str(12, cfg.clientName)
	// f16: client timestamp { sec, nanos }.
	now := time.Now()
	var ts pw
	ts.varintAlways(1, uint64(now.Unix()))
	ts.varintAlways(2, uint64(now.Nanosecond()))
	w.msg(16, ts.bytes())
	w.str(20, cfg.userID)
	w.str(21, jwt)
	w.str(27, cfg.hash27)
	// f30: static client flags observed on every request.
	w.bytesField(30, []byte{0x00, 0x01, 0x03, 0x04})
	w.str(31, cfg.hex31) // omitted unless a captured fingerprint is supplied
	w.str(32, cfg.teamID)
	return w.bytes()
}
