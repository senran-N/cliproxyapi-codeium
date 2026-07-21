package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/google/uuid"
)

// identity is a device fingerprint. It is derived deterministically from a
// per-account seed (the session token), which gives three important properties
// for a multi-credential deployment:
//
//   - Isolation: each imported account presents its OWN device id / hardware
//     hashes, so a backend that rate-limits or abuse-checks per device treats
//     the accounts independently (sharing one fingerprint across accounts would
//     defeat the point of importing several).
//   - Stability: the same account always yields the same fingerprint across
//     restarts and machines, with no file to persist or lock.
//   - No cross-account collision: distinct seeds produce distinct fingerprints.
//
// Nothing here is a secret; the session token is the real credential.
type identity struct {
	DeviceID string // metadata f10
	HWHash   string // metadata f24 (128 hex / 64 bytes)
	Hash27   string // metadata f27 (64 hex / 32 bytes)
}

// deviceFingerprint derives a stable per-account identity from a seed. The seed
// must be stable and unique per account; the session token is used.
func deviceFingerprint(seed string) identity {
	if seed == "" {
		seed = "codeium-anonymous"
	}
	return identity{
		DeviceID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("codeium-device|"+seed)).String(),
		HWHash:   sha512hex("codeium-hw|" + seed),
		Hash27:   sha256hex("codeium-h27|" + seed),
	}
}

func sha512hex(s string) string {
	sum := sha512.Sum512([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// osJSON reports the local OS the way the client encodes metadata f5 (chat).
// OS/CPU telemetry reflects the real host CPA runs on (shared across accounts,
// which is correct — it is the same machine).
func osJSON() string {
	name := map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"}[runtime.GOOS]
	if name == "" {
		name = runtime.GOOS
	}
	m := map[string]any{
		"Os":                 name,
		"Arch":               runtime.GOARCH,
		"Version":            "10.0",
		"ProductName":        name,
		"MajorVersionNumber": 10,
		"MinorVersionNumber": 0,
		"Build":              "0",
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// cpuJSON reports local CPU/topology for metadata f8 (chat). Best-effort.
func cpuJSON() string {
	threads := runtime.NumCPU()
	cores := threads / 2
	if cores < 1 {
		cores = threads
	}
	m := map[string]any{
		"NumSockets": 1,
		"NumCores":   cores,
		"NumThreads": threads,
		"VendorID":   "",
		"Family":     "0",
		"Model":      "",
		"ModelName":  fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		"Memory":     0,
	}
	b, _ := json.Marshal(m)
	return string(b)
}
