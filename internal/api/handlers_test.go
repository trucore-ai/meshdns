package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trucore-ai/meshdns/internal/config"
	"github.com/trucore-ai/meshdns/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return NewServer(s, &config.Config{})
}

// registerHelper registers a server and returns the response.
func registerHelper(t *testing.T, srv *Server, body string) RegisterResponse {
	t.Helper()
	req := httptest.NewRequest("POST", "/v0/servers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var resp RegisterResponse
	json.NewDecoder(w.Body).Decode(&resp)
	return resp
}

// --- Health, llms.txt, Landing ---

func TestHealthEndpoint(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok"`) {
		t.Errorf("expected ok in body, got %s", w.Body.String())
	}
}

func TestLLMsTxtEndpoint(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("GET", "/llms.txt", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if len(w.Body.String()) < 10 {
		t.Errorf("expected content, got %d bytes", len(w.Body.String()))
	}
	// Should contain MeshDNS content
	if !strings.Contains(w.Body.String(), "MeshDNS") {
		t.Errorf("expected MeshDNS content in llms.txt, got: %s", w.Body.String()[:100])
	}
}

func TestLandingPage(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html, got %s", ct)
	}
	if !strings.Contains(w.Body.String(), "MeshDNS") {
		t.Errorf("expected MeshDNS in HTML body")
	}
}

func TestLandingPageWithServers(t *testing.T) {
	srv := testServer(t)

	// Register a server so stats are populated
	registerHelper(t, srv, `{"name":"land-srv","server_url":"https://l.com","capabilities":["land"],"owner_contact":"a@b.com"}`)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	// Should show active count
	if !strings.Contains(w.Body.String(), "1") {
		t.Errorf("expected server count in HTML")
	}
}

// --- Register ---

func TestRegisterServer(t *testing.T) {
	srv := testServer(t)
	body := `{"name":"weather-agent","description":"Weather MCP","server_url":"https://weather.example.com","health_url":"https://weather.example.com/health","capabilities":["weather","forecast"],"owner_contact":"ops@example.com"}`

	req := httptest.NewRequest("POST", "/v0/servers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp RegisterResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.ServerID == "" {
		t.Fatal("expected non-empty server_id")
	}
	if resp.WriteKey == "" {
		t.Fatal("expected non-empty write_key")
	}
}

func TestRegisterDuplicateName(t *testing.T) {
	srv := testServer(t)
	body := `{"name":"dup-srv","server_url":"https://a.example.com","capabilities":["weather"],"owner_contact":"a@b.com"}`

	// First registration
	req1 := httptest.NewRequest("POST", "/v0/servers", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)
	if w1.Code != 201 {
		t.Fatalf("first register: expected 201, got %d", w1.Code)
	}

	// Duplicate
	req2 := httptest.NewRequest("POST", "/v0/servers", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != 409 {
		t.Fatalf("expected 409 for duplicate, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestRegisterValidationErrors(t *testing.T) {
	srv := testServer(t)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty", `{}`, 422},
		{"bad name", `{"name":"INVALID","server_url":"https://x.com","capabilities":["a"]}`, 422},
		{"no caps", `{"name":"test","server_url":"https://x.com","capabilities":[]}`, 422},
		{"bad url", `{"name":"test2","server_url":"ftp://bad","capabilities":["a"]}`, 422},
		{"bad health url", `{"name":"test3","server_url":"https://x.com","health_url":"ftp://bad","capabilities":["a"]}`, 422},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v0/servers", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("expected %d, got %d: %s", tt.want, w.Code, w.Body.String())
			}
		})
	}
}

// --- Get Server ---

func TestGetServer(t *testing.T) {
	srv := testServer(t)

	resp := registerHelper(t, srv, `{"name":"get-me","server_url":"https://g.com","capabilities":["get"],"owner_contact":"a@b.com"}`)

	req := httptest.NewRequest("GET", "/v0/servers/"+resp.ServerID, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ServerWithState
	json.NewDecoder(w.Body).Decode(&result)
	if result.Name != "get-me" {
		t.Errorf("Name = %q, want get-me", result.Name)
	}
	if result.Status != "active" {
		t.Errorf("Status = %q, want active", result.Status)
	}
}

func TestGetServerNotFound(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest("GET", "/v0/servers/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// --- Update ---

func TestUpdateServer(t *testing.T) {
	srv := testServer(t)

	resp := registerHelper(t, srv, `{"name":"update-me","server_url":"https://u.example.com","capabilities":["old"],"owner_contact":"a@b.com"}`)

	// Update with write key via X-Write-Key header
	updateBody := `{"name":"updated-name","capabilities":["new1","new2"]}`
	upReq := httptest.NewRequest("PUT", "/v0/servers/"+resp.ServerID, strings.NewReader(updateBody))
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("X-Write-Key", resp.WriteKey)
	upW := httptest.NewRecorder()
	srv.ServeHTTP(upW, upReq)
	if upW.Code != 200 {
		t.Fatalf("update: expected 200, got %d: %s", upW.Code, upW.Body.String())
	}

	// Verify the update
	getReq := httptest.NewRequest("GET", "/v0/servers/"+resp.ServerID, nil)
	getW := httptest.NewRecorder()
	srv.ServeHTTP(getW, getReq)
	var updated ServerWithState
	json.NewDecoder(getW.Body).Decode(&updated)
	if updated.Name != "updated-name" {
		t.Errorf("Name = %q, want updated-name", updated.Name)
	}
	if len(updated.Capabilities) != 2 {
		t.Errorf("expected 2 caps, got %d", len(updated.Capabilities))
	}
}

func TestUpdateServerWithBearerToken(t *testing.T) {
	srv := testServer(t)

	resp := registerHelper(t, srv, `{"name":"bearer-upd","server_url":"https://b.example.com","capabilities":["a"],"owner_contact":"a@b.com"}`)

	updateBody := `{"description":"updated via bearer"}`
	upReq := httptest.NewRequest("PUT", "/v0/servers/"+resp.ServerID, strings.NewReader(updateBody))
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+resp.WriteKey)
	upW := httptest.NewRecorder()
	srv.ServeHTTP(upW, upReq)
	if upW.Code != 200 {
		t.Fatalf("update with bearer: expected 200, got %d: %s", upW.Code, upW.Body.String())
	}
}

func TestUpdateServerUnauthorized(t *testing.T) {
	srv := testServer(t)

	resp := registerHelper(t, srv, `{"name":"auth-test","server_url":"https://a.example.com","capabilities":["a"],"owner_contact":"a@b.com"}`)

	// Try update with wrong key
	updateBody := `{"name":"hacked"}`
	upReq := httptest.NewRequest("PUT", "/v0/servers/"+resp.ServerID, strings.NewReader(updateBody))
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("X-Write-Key", "wrong-key")
	upW := httptest.NewRecorder()
	srv.ServeHTTP(upW, upReq)
	if upW.Code != 401 {
		t.Errorf("expected 401 for wrong key, got %d", upW.Code)
	}

	// Try update with no key
	upReq2 := httptest.NewRequest("PUT", "/v0/servers/"+resp.ServerID, strings.NewReader(updateBody))
	upReq2.Header.Set("Content-Type", "application/json")
	upW2 := httptest.NewRecorder()
	srv.ServeHTTP(upW2, upReq2)
	if upW2.Code != 401 {
		t.Errorf("expected 401 for no key, got %d", upW2.Code)
	}
}

// --- Delist ---

func TestRegisterThenDelist(t *testing.T) {
	srv := testServer(t)

	resp := registerHelper(t, srv, `{"name":"del-me","server_url":"https://d.example.com","capabilities":["del"],"owner_contact":"a@b.com"}`)

	// Delist with correct key via X-Write-Key header
	delReq := httptest.NewRequest("DELETE", "/v0/servers/"+resp.ServerID, nil)
	delReq.Header.Set("X-Write-Key", resp.WriteKey)
	delW := httptest.NewRecorder()
	srv.ServeHTTP(delW, delReq)
	if delW.Code != 200 {
		t.Fatalf("delist: expected 200, got %d: %s", delW.Code, delW.Body.String())
	}

	// Delist with wrong key should fail
	delReq2 := httptest.NewRequest("DELETE", "/v0/servers/"+resp.ServerID, nil)
	delReq2.Header.Set("X-Write-Key", "wrong-key")
	delW2 := httptest.NewRecorder()
	srv.ServeHTTP(delW2, delReq2)
	if delW2.Code != 401 {
		t.Errorf("expected 401 for wrong key, got %d", delW2.Code)
	}
}

func TestDelistWithBearerToken(t *testing.T) {
	srv := testServer(t)

	resp := registerHelper(t, srv, `{"name":"bearer-del","server_url":"https://bd.example.com","capabilities":["del"],"owner_contact":"a@b.com"}`)

	delReq := httptest.NewRequest("DELETE", "/v0/servers/"+resp.ServerID, nil)
	delReq.Header.Set("Authorization", "Bearer "+resp.WriteKey)
	delW := httptest.NewRecorder()
	srv.ServeHTTP(delW, delReq)
	if delW.Code != 200 {
		t.Fatalf("delist with bearer: expected 200, got %d: %s", delW.Code, delW.Body.String())
	}
}

func TestDelistNonExistent(t *testing.T) {
	srv := testServer(t)

	delReq := httptest.NewRequest("DELETE", "/v0/servers/nonexistent", nil)
	delReq.Header.Set("X-Write-Key", "some-key")
	delW := httptest.NewRecorder()
	srv.ServeHTTP(delW, delReq)
	if delW.Code != 404 {
		t.Errorf("expected 404 for nonexistent, got %d", delW.Code)
	}
}

// --- Resolve ---

func TestResolveReturnsOnlyUpServers(t *testing.T) {
	srv := testServer(t)

	reg1 := registerHelper(t, srv, `{"name":"w-up","server_url":"https://up.com","health_url":"https://up.com/h","capabilities":["weather"],"owner_contact":"a@b.com"}`)
	reg2 := registerHelper(t, srv, `{"name":"w-down","server_url":"https://down.com","health_url":"https://down.com/h","capabilities":["weather"],"owner_contact":"a@b.com"}`)

	// Mark s1 UP, s2 DOWN
	now := time.Now().UTC().Format(time.RFC3339)
	srv.Store.SetServerState(reg1.ServerID, 1, now, 0.95)
	srv.Store.SetServerState(reg2.ServerID, 0, now, 0.50)

	req := httptest.NewRequest("GET", "/v0/resolve?capability=weather", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var results []ServerWithState
	json.NewDecoder(w.Body).Decode(&results)
	if len(results) != 1 {
		t.Fatalf("expected 1 UP server, got %d", len(results))
	}
	if results[0].Name != "w-up" {
		t.Errorf("expected w-up, got %s", results[0].Name)
	}
}

func TestResolveUnknownCapabilityReturnsEmpty(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest("GET", "/v0/resolve?capability=nonexistent", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var results []ServerWithState
	json.NewDecoder(w.Body).Decode(&results)
	if len(results) != 0 {
		t.Errorf("expected empty, got %d", len(results))
	}
}

func TestResolveMissingCapability(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest("GET", "/v0/resolve", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for missing capability, got %d", w.Code)
	}
}

func TestResolveRankedByUptime(t *testing.T) {
	srv := testServer(t)

	reg1 := registerHelper(t, srv, `{"name":"low-uptime","server_url":"https://low.com","health_url":"https://low.com/h","capabilities":["rank"],"owner_contact":"a@b.com"}`)
	reg2 := registerHelper(t, srv, `{"name":"high-uptime","server_url":"https://high.com","health_url":"https://high.com/h","capabilities":["rank"],"owner_contact":"a@b.com"}`)

	now := time.Now().UTC().Format(time.RFC3339)
	srv.Store.SetServerState(reg1.ServerID, 1, now, 0.30)
	srv.Store.SetServerState(reg2.ServerID, 1, now, 0.99)

	req := httptest.NewRequest("GET", "/v0/resolve?capability=rank", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var results []ServerWithState
	json.NewDecoder(w.Body).Decode(&results)
	if len(results) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(results))
	}
	// Higher uptime should be first
	if results[0].Name != "high-uptime" {
		t.Errorf("first should be high-uptime, got %s", results[0].Name)
	}
	if results[1].Name != "low-uptime" {
		t.Errorf("second should be low-uptime, got %s", results[1].Name)
	}
}

// --- List Servers ---

func TestListServers(t *testing.T) {
	srv := testServer(t)

	registerHelper(t, srv, `{"name":"srv-a","server_url":"https://a.com","capabilities":["list"],"owner_contact":"a@b.com"}`)
	registerHelper(t, srv, `{"name":"srv-b","server_url":"https://b.com","capabilities":["list"],"owner_contact":"a@b.com"}`)
	registerHelper(t, srv, `{"name":"srv-c","server_url":"https://c.com","capabilities":["list"],"owner_contact":"a@b.com"}`)

	req := httptest.NewRequest("GET", "/v0/servers", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Servers    []ServerWithState `json:"servers"`
		NextCursor string            `json:"next_cursor"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Servers) < 3 {
		t.Errorf("expected >= 3 servers, got %d", len(resp.Servers))
	}
}

func TestListServersWithQueryFilter(t *testing.T) {
	srv := testServer(t)

	registerHelper(t, srv, `{"name":"alpha-srv","server_url":"https://a.com","capabilities":["q"],"owner_contact":"a@b.com"}`)
	registerHelper(t, srv, `{"name":"beta-srv","server_url":"https://b.com","capabilities":["q"],"owner_contact":"a@b.com"}`)

	req := httptest.NewRequest("GET", "/v0/servers?query=alpha", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var resp struct {
		Servers    []ServerWithState `json:"servers"`
		NextCursor string            `json:"next_cursor"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Servers) != 1 || resp.Servers[0].Name != "alpha-srv" {
		t.Errorf("expected only alpha-srv, got %+v", resp.Servers)
	}
}

func TestListServersWithPagination(t *testing.T) {
	srv := testServer(t)

	for i := 0; i < 5; i++ {
		n := string(rune('a' + i))
		registerHelper(t, srv, `{"name":"pg-`+n+`","server_url":"https://`+n+`.com","capabilities":["pg"],"owner_contact":"a@b.com"}`)
	}

	// Page 1: limit=2
	req1 := httptest.NewRequest("GET", "/v0/servers?limit=2", nil)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)

	var resp1 struct {
		Servers    []ServerWithState `json:"servers"`
		NextCursor string            `json:"next_cursor"`
	}
	json.NewDecoder(w1.Body).Decode(&resp1)
	if len(resp1.Servers) != 2 {
		t.Errorf("page1 len = %d, want 2", len(resp1.Servers))
	}
	if resp1.NextCursor == "" {
		t.Fatal("expected non-empty next_cursor")
	}

	// Page 2
	req2 := httptest.NewRequest("GET", "/v0/servers?limit=2&cursor="+resp1.NextCursor, nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)

	var resp2 struct {
		Servers    []ServerWithState `json:"servers"`
		NextCursor string            `json:"next_cursor"`
	}
	json.NewDecoder(w2.Body).Decode(&resp2)
	if len(resp2.Servers) != 2 {
		t.Errorf("page2 len = %d, want 2", len(resp2.Servers))
	}
}

// --- Export ---

func TestExport(t *testing.T) {
	srv := testServer(t)
	registerHelper(t, srv, `{"name":"exp-srv","server_url":"https://e.com","capabilities":["export"],"owner_contact":"a@b.com"}`)

	req := httptest.NewRequest("GET", "/v0/export", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var export map[string]any
	json.NewDecoder(w.Body).Decode(&export)
	if _, ok := export["exported_at"]; !ok {
		t.Error("export missing exported_at")
	}
	if servers, ok := export["servers"].([]any); !ok || len(servers) < 1 {
		t.Errorf("export servers empty or missing")
	}
}

func TestExportEmpty(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest("GET", "/v0/export", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// --- Stats ---

func TestStats(t *testing.T) {
	srv := testServer(t)
	registerHelper(t, srv, `{"name":"stat-srv","server_url":"https://s.com","capabilities":["stats"],"owner_contact":"a@b.com"}`)

	req := httptest.NewRequest("GET", "/v0/stats", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var stats map[string]any
	json.NewDecoder(w.Body).Decode(&stats)
	if v, ok := stats["servers_active"].(float64); !ok || v < 1 {
		t.Errorf("expected servers_active >= 1, got %v", stats["servers_active"])
	}
}

// --- CORS preflight ---

func TestCORSPreflight(t *testing.T) {
	srv := testServer(t)

	req := httptest.NewRequest("OPTIONS", "/v0/servers", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Errorf("expected 204 for OPTIONS, got %d", w.Code)
	}
}

// --- Write key hashing ---

func TestHashWriteKey(t *testing.T) {
	key := "test-key-12345"
	hash1 := hashWriteKey(key)
	hash2 := hashWriteKey(key)

	if hash1 != hash2 {
		t.Error("same key should produce same hash")
	}
	if hash1 == key {
		t.Error("hash should not equal original key")
	}
	if len(hash1) != 64 {
		t.Errorf("expected 64 hex chars (SHA-256), got %d", len(hash1))
	}

	// Different keys should produce different hashes
	hash3 := hashWriteKey("different-key")
	if hash1 == hash3 {
		t.Error("different keys should produce different hashes")
	}
}

// --- Write key generation ---

func TestGenerateWriteKey(t *testing.T) {
	key1 := generateWriteKey()
	key2 := generateWriteKey()

	if key1 == key2 {
		t.Error("generated keys should be unique")
	}
	if len(key1) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(key1))
	}
}

// --- JSON round-trip ---

func TestServerRoundTrip(t *testing.T) {
	srv := testServer(t)

	// Register
	resp := registerHelper(t, srv, `{"name":"round-trip","description":"testing","server_url":"https://rt.com","health_url":"https://rt.com/h","capabilities":["rt"],"owner_contact":"rt@test.com"}`)

	// Get
	req := httptest.NewRequest("GET", "/v0/servers/"+resp.ServerID, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	var result ServerWithState
	json.NewDecoder(w.Body).Decode(&result)

	if result.Name != "round-trip" {
		t.Errorf("Name = %q, want round-trip", result.Name)
	}
	if result.Description != "testing" {
		t.Errorf("Description = %q, want testing", result.Description)
	}
	if result.ServerURL != "https://rt.com" {
		t.Errorf("ServerURL = %q", result.ServerURL)
	}
	if result.HealthURL != "https://rt.com/h" {
		t.Errorf("HealthURL = %q", result.HealthURL)
	}
	if result.OwnerContact != "rt@test.com" {
		t.Errorf("OwnerContact = %q", result.OwnerContact)
	}
	if len(result.Capabilities) != 1 || result.Capabilities[0] != "rt" {
		t.Errorf("Capabilities = %v", result.Capabilities)
	}
	if result.Status != "active" {
		t.Errorf("Status = %q", result.Status)
	}
}

// --- Test helper sinks ---

func TestRegisterHelper(t *testing.T) {
	srv := testServer(t)
	resp := registerHelper(t, srv, `{"name":"helper-test","server_url":"https://h.com","capabilities":["helper"],"owner_contact":"a@b.com"}`)
	if resp.ServerID == "" || resp.WriteKey == "" {
		t.Error("registerHelper should return valid response")
	}
}

// Prevent unused imports
var _ = bytes.NewReader
var _ = time.Now