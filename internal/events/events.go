package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/trucore-ai/provengraph/internal/store"
)

var logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Log records an instrumentation event.
// It hashes the remote IP (NFR-005) and includes it as source_hash in the payload.
func Log(s *store.Store, eventType string, payload map[string]any, r *http.Request) error {
	// Extract IP from RemoteAddr (strip port)
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}

	// Hash the IP with SHA-256, take first 16 hex chars
	sourceHash := ""
	if ip != "" {
		sum := sha256.Sum256([]byte(ip))
		sourceHash = hex.EncodeToString(sum[:])[:16]
	}

	// Add source_hash and user-agent to payload
	payload["source_hash"] = sourceHash
	payload["ua_tag"] = r.Header.Get("User-Agent")

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// Log to structured logger
	logger.Info("event",
		"type", eventType,
		"source_hash", sourceHash,
		"path", r.URL.Path,
	)

	// Persist to SQLite events table
	return s.AppendEvent(eventType, string(jsonPayload))
}

// LogStatic records an event without an HTTP request (e.g., from health probes).
func LogStatic(s *store.Store, eventType string, payload map[string]any) error {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	logger.Info("event", "type", eventType)
	return s.AppendEvent(eventType, string(jsonPayload))
}

// SourceHash computes a source hash from an IP string.
func SourceHash(remoteAddr string) string {
	ip := remoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if ip == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])[:16]
}

// DetectSDK checks if the User-Agent header indicates an SDK client.
func DetectSDK(ua string) string {
	ua = strings.ToLower(ua)
	if strings.Contains(ua, "meshdns-python") || strings.Contains(ua, "meshdns-client") {
		return "python-sdk"
	}
	if strings.Contains(ua, "meshdns-js") || strings.Contains(ua, "meshdns-ts") {
		return "typescript-sdk"
	}
	return "curl"
}