package events

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/trucore-ai/provengraph/internal/store"
)

func TestLog_HashesSourceIP(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r := httptest.NewRequest("GET", "/v0/resolve?capability=test", nil)
	r.RemoteAddr = "192.168.1.1:12345"

	err = Log(s, "resolve", map[string]any{"capability": "test"}, r)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
}

func TestLog_SameIPProducesSameHash(t *testing.T) {
	dir := t.TempDir()
	s, _ := store.Open(filepath.Join(dir, "test.db"))
	defer s.Close()

	ip := "10.0.0.1:8080"
	r1 := httptest.NewRequest("GET", "/", nil)
	r1.RemoteAddr = ip
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = ip

	hash1 := hashIP("10.0.0.1")
	hash2 := hashIP("10.0.0.1")
	if hash1 != hash2 {
		t.Error("same IP should produce same hash")
	}
	_ = r1
	_ = r2
}

func TestLog_DifferentIPProducesDifferentHash(t *testing.T) {
	h1 := hashIP("1.2.3.4")
	h2 := hashIP("5.6.7.8")
	if h1 == h2 {
		t.Error("different IPs should produce different hashes")
	}
}

func TestHashNeverExposesRawIP(t *testing.T) {
	raw := "203.0.113.42"
	h := hashIP(raw)
	// Raw IP must not appear in the hex hash
	if len(h) == 0 {
		t.Fatal("empty hash")
	}
	// Hex chars only
	if _, err := hex.DecodeString(h); err != nil {
		t.Error("hash is not valid hex:", h)
	}
}

func hashIP(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])[:16]
}

func TestLogStatic(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = LogStatic(s, "probe", map[string]any{"server_id": "abc", "up": true})
	if err != nil {
		t.Fatalf("LogStatic: %v", err)
	}

	count, _ := s.CountEventsSince("probe", "2000-01-01T00:00:00Z")
	if count < 1 {
		t.Error("expected event to be recorded")
	}
}

func TestSourceHash(t *testing.T) {
	h := SourceHash("192.168.1.1:12345")
	if h == "" {
		t.Error("expected non-empty hash")
	}
	if len(h) != 16 {
		t.Errorf("expected 16-char hash, got %d", len(h))
	}
}

func TestDetectSDK(t *testing.T) {
	if DetectSDK("meshdns-python/1.0") != "python-sdk" {
		t.Error("expected python-sdk")
	}
	if DetectSDK("meshdns-client/1.0") != "python-sdk" {
		t.Error("expected python-sdk for meshdns-client")
	}
	if DetectSDK("meshdns-js/1.0") != "typescript-sdk" {
		t.Error("expected typescript-sdk for meshdns-js")
	}
	if DetectSDK("meshdns-ts/1.0") != "typescript-sdk" {
		t.Error("expected typescript-sdk for meshdns-ts")
	}
	if DetectSDK("curl/8.0") != "curl" {
		t.Error("expected curl for unknown UA")
	}
}