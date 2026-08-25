package store

import (
	"os"
	"testing"
	"time"
)

func BenchmarkComputeUptimeFromCounters(b *testing.B) {
	dbPath := b.TempDir() + "/bench.db"
	st, err := Open(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	defer os.Remove(dbPath)

	serverID := "test-server-1"
	now := time.Now().UTC().Format(time.RFC3339)
	// Create a server
	_, err = st.db.Exec(`INSERT INTO servers (id, name, description, server_url, health_url, write_key_hash, status, probe_method, created_at, updated_at) VALUES (?, 'test', '', 'https://example.com', 'https://example.com', 'hash', 'active', 'GET', ?, ?)`, serverID, now, now)
	if err != nil {
		b.Fatal(err)
	}
	// Insert 1000 probes
	for i := 0; i < 1000; i++ {
		_ = st.RecordProbe(serverID, now, i%2 == 0, 50)
	}
	// Initialize counters
	_ = st.IncrementProbeCount(serverID, true)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = st.ComputeUptimeFromCounters(serverID)
	}
}

func BenchmarkGetUptime30d_TableScan(b *testing.B) {
	dbPath := b.TempDir() + "/bench.db"
	st, err := Open(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	defer os.Remove(dbPath)

	serverID := "test-server-1"
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = st.db.Exec(`INSERT INTO servers (id, name, description, server_url, health_url, write_key_hash, status, probe_method, created_at, updated_at) VALUES (?, 'test', '', 'https://example.com', 'https://example.com', 'hash', 'active', 'GET', ?, ?)`, serverID, now, now)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		_ = st.RecordProbe(serverID, now, i%2 == 0, 50)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = st.GetUptime30d(serverID)
	}
}