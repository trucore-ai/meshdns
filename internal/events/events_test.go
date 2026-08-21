package events

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trucore-ai/meshdns/internal/store"
	_ "modernc.org/sqlite"
)

func TestSourceHashSameIPSameHashDifferentIPsDifferentHashes(t *testing.T) {
	one := sourceHash("1.2.3.4")
	alsoOne := sourceHash("1.2.3.4")
	two := sourceHash("1.2.3.5")

	if one != alsoOne {
		t.Fatalf("same IP hashes differ: %q != %q", one, alsoOne)
	}
	if one == two {
		t.Fatalf("different IPs produced same hash: %q", one)
	}
}

func TestSourceHashStripsHostPort(t *testing.T) {
	withPort := sourceHash("1.2.3.4:5678")
	bare := sourceHash("1.2.3.4")

	if withPort != bare {
		t.Fatalf("host:port hash = %q, want bare IP hash %q", withPort, bare)
	}
}

func TestSourceHashHandlesBracketedIPv6Port(t *testing.T) {
	withPort := sourceHash("[::1]:8080")
	bare := sourceHash("::1")

	if withPort != bare {
		t.Fatalf("bracketed IPv6 host:port hash = %q, want bare IPv6 hash %q", withPort, bare)
	}
}

func TestLogStoresHashWithoutRawIPAndPreservesPayload(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "meshdns.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
	})

	rawIP := "1.2.3.4"
	payload := map[string]any{
		"server_id": "srv-1",
		"up":        true,
	}
	if err := Log(st, "server.up", payload, rawIP+":5678"); err != nil {
		t.Fatalf("Log = %v", err)
	}

	payloadJSON := readOnlyEventPayload(t, dbPath)
	if strings.Contains(payloadJSON, rawIP) {
		t.Fatalf("stored payload contains raw IP %q: %s", rawIP, payloadJSON)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &got); err != nil {
		t.Fatalf("Unmarshal payload = %v", err)
	}

	if got["server_id"] != "srv-1" {
		t.Fatalf("server_id = %#v, want srv-1", got["server_id"])
	}
	if got["up"] != true {
		t.Fatalf("up = %#v, want true", got["up"])
	}
	if got["source_hash"] != sourceHash(rawIP) {
		t.Fatalf("source_hash = %#v, want %q", got["source_hash"], sourceHash(rawIP))
	}
	ts, ok := got["ts"].(string)
	if !ok {
		t.Fatalf("ts = %#v, want string", got["ts"])
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Fatalf("ts is not RFC3339: %#v", got["ts"])
	}
	if _, ok := payload["source_hash"]; ok {
		t.Fatalf("Log mutated caller payload with source_hash")
	}
	if _, ok := payload["ts"]; ok {
		t.Fatalf("Log mutated caller payload with ts")
	}
}

func readOnlyEventPayload(t *testing.T, dbPath string) string {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	defer db.Close()

	var payload string
	if err := db.QueryRow(`SELECT payload FROM events ORDER BY id DESC LIMIT 1`).Scan(&payload); err != nil {
		t.Fatalf("query event payload = %v", err)
	}

	return payload
}
