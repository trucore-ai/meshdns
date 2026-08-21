package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"time"

	"github.com/trucore-ai/meshdns/internal/store"
)

// Log appends an event with a UTC RFC3339 timestamp. The caller's payload is
// copied, then ts and source_hash are added without storing the raw IP.
func Log(st *store.Store, eventType string, payload map[string]any, remoteAddr string) error {
	ts := time.Now().UTC().Format(time.RFC3339)
	eventPayload := make(map[string]any, len(payload)+2)
	for k, v := range payload {
		eventPayload[k] = v
	}
	eventPayload["ts"] = ts
	eventPayload["source_hash"] = sourceHash(remoteAddr)

	payloadJSON, err := json.Marshal(eventPayload)
	if err != nil {
		return err
	}

	return st.AppendEvent(ts, eventType, string(payloadJSON))
}

func sourceHash(remoteAddr string) string {
	sum := sha256.Sum256([]byte(remoteIP(remoteAddr)))
	return hex.EncodeToString(sum[:])[:16]
}

func remoteIP(remoteAddr string) string {
	addr := strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return strings.Trim(host, "[]")
	}

	if strings.HasPrefix(addr, "[") {
		if end := strings.Index(addr, "]"); end > 0 {
			return addr[1:end]
		}
	}

	return addr
}
