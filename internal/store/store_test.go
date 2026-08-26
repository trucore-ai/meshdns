package store

import (
	"path/filepath"
	"testing"
	"time"
)

func testDB(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetServer(t *testing.T) {
	s := testDB(t)
	srv := &Server{
		ID:           "test-id-001",
		Name:         "test-server",
		Description:  "a test server",
		ServerURL:    "https://example.com",
		HealthURL:    "https://example.com/health",
		WriteKeyHash: "hash123",
		OwnerContact: "ops@example.com",
	}
	id, err := s.CreateServer(srv)
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty server id")
	}

	got, err := s.GetServer(id)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.Name != srv.Name {
		t.Errorf("Name = %q, want %q", got.Name, srv.Name)
	}
	if got.Status != "active" {
		t.Errorf("Status = %q, want active", got.Status)
	}
}

func TestCreateDuplicateName(t *testing.T) {
	s := testDB(t)
	srv := &Server{ID: "id-1", Name: "dup", ServerURL: "https://a.example.com", WriteKeyHash: "h1"}
	_, err := s.CreateServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateServer(&Server{ID: "id-2", Name: "dup", ServerURL: "https://b.example.com", WriteKeyHash: "h2"})
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
}

func TestUpdateAndDelist(t *testing.T) {
	s := testDB(t)
	id, _ := s.CreateServer(&Server{ID: "id-upd", Name: "srv", ServerURL: "https://x.com", WriteKeyHash: "h"})

	err := s.UpdateServer(id, "hash_wrong", &Server{Name: "new-name"})
	if err == nil {
		t.Fatal("expected auth error for wrong write key")
	}

	err = s.UpdateServer(id, "h", &Server{Name: "new-name", Description: "updated"})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}

	got, _ := s.GetServer(id)
	if got.Name != "new-name" {
		t.Errorf("Name = %q, want new-name", got.Name)
	}

	err = s.DelistServer(id, "wrong")
	if err == nil {
		t.Fatal("expected auth error for delist")
	}
	err = s.DelistServer(id, "h")
	if err != nil {
		t.Fatalf("DelistServer: %v", err)
	}
	got, _ = s.GetServer(id)
	if got.Status != "delisted" {
		t.Errorf("Status = %q, want delisted", got.Status)
	}
}

func TestCapabilities(t *testing.T) {
	s := testDB(t)
	id, _ := s.CreateServer(&Server{ID: "cap-1", Name: "cap-srv", ServerURL: "https://c.com", WriteKeyHash: "h"})

	err := s.SetCapabilities(id, []string{"weather", "forecast"})
	if err != nil {
		t.Fatalf("SetCapabilities: %v", err)
	}

	caps, err := s.GetCapabilities(id)
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if len(caps) != 2 {
		t.Errorf("got %d capabilities, want 2", len(caps))
	}
}

func TestResolveByCapability(t *testing.T) {
	s := testDB(t)

	s1, _ := s.CreateServer(&Server{ID: "rs1", Name: "srv1", ServerURL: "https://1.com", WriteKeyHash: "h", HealthURL: "https://1.com/h"})
	s2, _ := s.CreateServer(&Server{ID: "rs2", Name: "srv2", ServerURL: "https://2.com", WriteKeyHash: "h"})
	s3, _ := s.CreateServer(&Server{ID: "rs3", Name: "srv3", ServerURL: "https://3.com", WriteKeyHash: "h", HealthURL: "https://3.com/h"})

	s.SetCapabilities(s1, []string{"weather"})
	s.SetCapabilities(s2, []string{"weather"})
	s.SetCapabilities(s3, []string{"news"})

	now := time.Now().UTC().Format(time.RFC3339)
	s.SetServerState(s1, 1, now, 0.95)
	s.SetServerState(s2, 0, now, 0.50)
	s.SetServerState(s3, 1, now, 0.90)

	// Override s2 to UP (so both weather servers are up but s1 has higher uptime)
	s.SetServerState(s2, 1, now, 0.50)

	results, err := s.GetUpServersByCapability("weather")
	if err != nil {
		t.Fatalf("GetUpServersByCapability: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 UP servers for weather, got %d", len(results))
	}
	// s1 (0.95) should come before s2 (0.50)
	if results[0].Name != "srv1" {
		t.Errorf("first result = %q, want srv1 (higher uptime)", results[0].Name)
	}

	empty, _ := s.GetUpServersByCapability("unknown")
	if len(empty) != 0 {
		t.Errorf("expected empty for unknown capability, got %d", len(empty))
	}
}

func TestListServersWithFilters(t *testing.T) {
	s := testDB(t)
	s1, _ := s.CreateServer(&Server{ID: "ls1", Name: "alpha", ServerURL: "https://a.com", WriteKeyHash: "h"})
	s2, _ := s.CreateServer(&Server{ID: "ls2", Name: "beta", ServerURL: "https://b.com", WriteKeyHash: "h"})
	s.DelistServer(s2, "h")

	// filter by status=active
	servers, cursor, err := s.ListServers("", "", "active", "", 10)
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 1 {
		t.Errorf("expected 1 active server, got %d", len(servers))
	}
	if servers[0].ID != s1 {
		t.Errorf("expected alpha")
	}
	if cursor != "" {
		t.Errorf("expected empty cursor, got %q", cursor)
	}

	// filter by query
	servers, _, _ = s.ListServers("bet", "", "all", "", 10)
	if len(servers) != 1 || servers[0].Name != "beta" {
		t.Errorf("expected beta from name search")
	}
}

func TestPagination(t *testing.T) {
	s := testDB(t)
	for i := 0; i < 5; i++ {
		n := string(rune('a' + i))
		s.CreateServer(&Server{ID: "pg-" + n, Name: "srv-" + n, ServerURL: "https://" + n + ".com", WriteKeyHash: "h"})
	}

	// Page 1: limit=2, no cursor
	page1, cursor, err := s.ListServers("", "", "all", "", 2)
	if err != nil {
		t.Fatalf("ListServers page 1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1))
	}
	if cursor == "" {
		t.Fatal("expected non-empty cursor")
	}

	// Page 2
	page2, cursor2, _ := s.ListServers("", "", "all", cursor, 2)
	if len(page2) != 2 {
		t.Errorf("page2 len = %d, want 2", len(page2))
	}

	// Page 3
	page3, cursor3, _ := s.ListServers("", "", "all", cursor2, 2)
	if len(page3) != 1 {
		t.Errorf("page3 len = %d, want 1", len(page3))
	}
	if cursor3 != "" {
		t.Errorf("expected empty cursor on last page, got %q", cursor3)
	}
}

func TestExportAll(t *testing.T) {
	s := testDB(t)
	s.CreateServer(&Server{ID: "ex1", Name: "e1", ServerURL: "https://e1.com", WriteKeyHash: "h"})
	s.CreateServer(&Server{ID: "ex2", Name: "e2", ServerURL: "https://e2.com", WriteKeyHash: "h"})

	export, err := s.ExportAll()
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if export.ExportedAt == "" {
		t.Error("export missing exported_at")
	}
	if len(export.Servers) != 2 {
		t.Errorf("export servers = %d, want 2", len(export.Servers))
	}
}

func TestEvents(t *testing.T) {
	s := testDB(t)

	err := s.AppendEvent("register", `{"server_id":"abc"}`)
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	count, err := s.CountEventsSince("register", "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CountEventsSince: %v", err)
	}
	if count < 1 {
		t.Errorf("expected >= 1 event, got %d", count)
	}
}

func TestProbes(t *testing.T) {
	s := testDB(t)
	id, _ := s.CreateServer(&Server{ID: "pr1", Name: "p", ServerURL: "https://p.com", WriteKeyHash: "h"})

	err := s.RecordProbe(id, true, 42)
	if err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}
	err = s.RecordProbe(id, false, 5001)
	if err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}

	uptime, err := s.GetUptime30d(id)
	if err != nil {
		t.Fatalf("GetUptime30d: %v", err)
	}
	// 1 up, 1 down = 0.5
	if uptime < 0.49 || uptime > 0.51 {
		t.Errorf("uptime = %f, want ~0.5", uptime)
	}
}

func TestGetStats(t *testing.T) {
	s := testDB(t)
	s.CreateServer(&Server{ID: "st1", Name: "stat-srv", ServerURL: "https://s.com", WriteKeyHash: "h"})

	stats, err := s.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.ServersActive < 1 {
		t.Errorf("expected >= 1 active server, got %d", stats.ServersActive)
	}
	if stats.ServersTotal < 1 {
		t.Errorf("expected >= 1 total server, got %d", stats.ServersTotal)
	}
}