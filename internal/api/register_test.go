package api

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/trucore-ai/meshdns/internal/store"
	_ "modernc.org/sqlite"
)

type registerResponse struct {
	ServerID string `json:"server_id"`
	WriteKey string `json:"write_key"`
}

type apiErrorResponse struct {
	Error struct {
		Code   string            `json:"code"`
		Detail map[string]string `json:"detail"`
	} `json:"error"`
}

func TestRegisterHappyPathPersistsServerAndOnlyReturnsPlaintextKeyOnce(t *testing.T) {
	st, dbPath := newAPITestStore(t)
	router := New(st).Router()

	rec := performJSON(t, router, http.MethodPost, "/v0/servers", "", map[string]any{
		"name":          "alpha-01",
		"description":   "Alpha resolver",
		"server_url":    "https://alpha.example/dns-query",
		"health_url":    "https://alpha.example/health",
		"capabilities":  []string{"dns", "doh"},
		"owner_contact": "ops@example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got registerResponse
	decodeResponse(t, rec, &got)
	if got.ServerID == "" {
		t.Fatalf("server_id is empty")
	}
	if len(got.WriteKey) != 64 {
		t.Fatalf("write_key length = %d, want 64", len(got.WriteKey))
	}

	server, err := st.GetServerByName("alpha-01")
	if err != nil {
		t.Fatalf("GetServerByName = %v", err)
	}
	if server.ID != got.ServerID {
		t.Fatalf("stored server ID = %q, want %q", server.ID, got.ServerID)
	}
	if !reflect.DeepEqual(server.Capabilities, []string{"dns", "doh"}) {
		t.Fatalf("Capabilities = %#v, want dns,doh", server.Capabilities)
	}

	storedHash := readWriteKeyHash(t, dbPath, got.ServerID)
	if storedHash == got.WriteKey {
		t.Fatalf("stored write_key_hash contains plaintext write key")
	}
	if storedHash != hashWriteKey(got.WriteKey) {
		t.Fatalf("write_key_hash = %q, want sha256 hex of write key", storedHash)
	}
}

func TestRegisterDuplicateNameReturnsConflict(t *testing.T) {
	st, _ := newAPITestStore(t)
	router := New(st).Router()

	registerServer(t, router, "alpha-01")
	rec := performJSON(t, router, http.MethodPost, "/v0/servers", "", validRegisterBody("alpha-01"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeResponse(t, rec, &got)
	if got.Error.Code != "duplicate_name" {
		t.Fatalf("error code = %q, want duplicate_name", got.Error.Code)
	}
}

func TestRegisterValidationErrorsNameFields(t *testing.T) {
	tests := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{
			name:  "bad name uppercase",
			body:  validRegisterBodyWith("Alpha", "https://alpha.example/dns-query", []string{"dns"}),
			field: "name",
		},
		{
			name:  "missing server url",
			body:  validRegisterBodyWith("alpha-01", "", []string{"dns"}),
			field: "server_url",
		},
		{
			name:  "zero capabilities",
			body:  validRegisterBodyWith("alpha-01", "https://alpha.example/dns-query", []string{}),
			field: "capabilities",
		},
		{
			name:  "too many capabilities",
			body:  validRegisterBodyWith("alpha-01", "https://alpha.example/dns-query", numberedCapabilities(21)),
			field: "capabilities",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _ := newAPITestStore(t)
			router := New(st).Router()

			rec := performJSON(t, router, http.MethodPost, "/v0/servers", "", tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			var got apiErrorResponse
			decodeResponse(t, rec, &got)
			if got.Error.Code != "validation_failed" {
				t.Fatalf("error code = %q, want validation_failed", got.Error.Code)
			}
			if got.Error.Detail[tt.field] == "" {
				t.Fatalf("detail = %#v, want field %q named", got.Error.Detail, tt.field)
			}
		})
	}
}

func TestUpdateServerAuthAndValidation(t *testing.T) {
	st, _ := newAPITestStore(t)
	router := New(st).Router()
	registered := registerServer(t, router, "alpha-01")

	rec := performJSON(t, router, http.MethodPut, "/v0/servers/"+registered.ServerID, registered.WriteKey, map[string]any{
		"description":   "Updated resolver",
		"server_url":    "https://updated.example/dns-query",
		"health_url":    "https://updated.example/health",
		"owner_contact": "updated@example.com",
		"capabilities":  []string{"resolve", "dns"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var updated serverJSON
	decodeResponse(t, rec, &updated)
	if updated.Description != "Updated resolver" || updated.ServerURL != "https://updated.example/dns-query" {
		t.Fatalf("updated response = %+v, want changed description and server_url", updated)
	}
	if !reflect.DeepEqual(updated.Capabilities, []string{"dns", "resolve"}) {
		t.Fatalf("response capabilities = %#v, want sorted dns,resolve", updated.Capabilities)
	}

	stored, err := st.GetServer(registered.ServerID)
	if err != nil {
		t.Fatalf("GetServer = %v", err)
	}
	if stored.OwnerContact != "updated@example.com" || stored.HealthURL != "https://updated.example/health" {
		t.Fatalf("stored server = %+v, want updated owner_contact and health_url", stored)
	}
	if !reflect.DeepEqual(stored.Capabilities, []string{"dns", "resolve"}) {
		t.Fatalf("stored capabilities = %#v, want sorted dns,resolve", stored.Capabilities)
	}

	wrongKey := performJSON(t, router, http.MethodPut, "/v0/servers/"+registered.ServerID, "wrong", map[string]any{
		"description": "nope",
	})
	if wrongKey.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d, body = %s", wrongKey.Code, wrongKey.Body.String())
	}

	missingKey := performJSON(t, router, http.MethodPut, "/v0/servers/"+registered.ServerID, "", map[string]any{
		"description": "nope",
	})
	if missingKey.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, body = %s", missingKey.Code, missingKey.Body.String())
	}

	unknown := performJSON(t, router, http.MethodPut, "/v0/servers/unknown", registered.WriteKey, map[string]any{
		"description": "nope",
	})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown status = %d, body = %s", unknown.Code, unknown.Body.String())
	}
}

func TestDeleteServerDelistsAndLogsEvents(t *testing.T) {
	st, _ := newAPITestStore(t)
	router := New(st).Router()
	registered := registerServer(t, router, "alpha-01")

	rec := performJSON(t, router, http.MethodDelete, "/v0/servers/"+registered.ServerID, registered.WriteKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var ok map[string]bool
	decodeResponse(t, rec, &ok)
	if !ok["ok"] {
		t.Fatalf("ok response = %#v, want ok true", ok)
	}

	server, err := st.GetServer(registered.ServerID)
	if err != nil {
		t.Fatalf("GetServer = %v", err)
	}
	if server.Status != "delisted" {
		t.Fatalf("status = %q, want delisted", server.Status)
	}

	registerCount, err := st.CountEventsSince("register", "1970-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CountEventsSince register = %v", err)
	}
	delistCount, err := st.CountEventsSince("delist", "1970-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("CountEventsSince delist = %v", err)
	}
	if registerCount != 1 || delistCount != 1 {
		t.Fatalf("event counts register=%d delist=%d, want 1 each", registerCount, delistCount)
	}
}

func TestRegisterEventPayloadHashesSourceWithoutRawRemoteIP(t *testing.T) {
	st, dbPath := newAPITestStore(t)
	router := New(st).Router()

	rawIP := "203.0.113.9"
	rec := performJSONWithRemoteAddr(t, router, http.MethodPost, "/v0/servers", "", validRegisterBody("alpha-01"), rawIP+":4567")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	payloadJSON := readEventPayload(t, dbPath, "register")
	if strings.Contains(payloadJSON, rawIP) {
		t.Fatalf("register payload contains raw IP %q: %s", rawIP, payloadJSON)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("Unmarshal payload = %v", err)
	}
	if payload["source_hash"] != sourceHashForTest(rawIP) {
		t.Fatalf("source_hash = %#v, want hash of remote IP", payload["source_hash"])
	}
	if payload["server_id"] == "" {
		t.Fatalf("payload missing server_id: %#v", payload)
	}
	if payload["capability_count"] != float64(2) {
		t.Fatalf("capability_count = %#v, want 2", payload["capability_count"])
	}
}

func newAPITestStore(t *testing.T) (*store.Store, string) {
	t.Helper()

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

	return st, dbPath
}

func registerServer(t *testing.T, router http.Handler, name string) registerResponse {
	t.Helper()

	rec := performJSON(t, router, http.MethodPost, "/v0/servers", "", validRegisterBody(name))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got registerResponse
	decodeResponse(t, rec, &got)
	return got
}

func performJSON(t *testing.T, router http.Handler, method, path, writeKey string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return performJSONWithRemoteAddr(t, router, method, path, writeKey, body, "192.0.2.1:1234")
}

func performJSONWithRemoteAddr(t *testing.T, router http.Handler, method, path, writeKey string, body any, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()

	var reqBody bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&reqBody).Encode(body); err != nil {
			t.Fatalf("Encode body = %v", err)
		}
	}

	req := httptest.NewRequest(method, path, &reqBody)
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	if writeKey != "" {
		req.Header.Set("Authorization", "Bearer "+writeKey)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()

	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("Decode response %q = %v", rec.Body.String(), err)
	}
}

func validRegisterBody(name string) map[string]any {
	return validRegisterBodyWith(name, "https://"+name+".example/dns-query", []string{"dns", "doh"})
}

func validRegisterBodyWith(name, serverURL string, caps []string) map[string]any {
	return map[string]any{
		"name":          name,
		"description":   "Test resolver",
		"server_url":    serverURL,
		"health_url":    "https://" + strings.ToLower(name) + ".example/health",
		"capabilities":  caps,
		"owner_contact": "ops@example.com",
	}
}

func numberedCapabilities(count int) []string {
	caps := make([]string, count)
	for i := range caps {
		caps[i] = fmt.Sprintf("cap-%02d", i+1)
	}

	return caps
}

func readWriteKeyHash(t *testing.T, dbPath string, serverID string) string {
	t.Helper()

	db := openReadDB(t, dbPath)
	defer db.Close()

	var hash string
	if err := db.QueryRow(`SELECT write_key_hash FROM servers WHERE id = ?`, serverID).Scan(&hash); err != nil {
		t.Fatalf("query write_key_hash = %v", err)
	}

	return hash
}

func readEventPayload(t *testing.T, dbPath string, eventType string) string {
	t.Helper()

	db := openReadDB(t, dbPath)
	defer db.Close()

	var payload string
	if err := db.QueryRow(`SELECT payload FROM events WHERE type = ? ORDER BY id DESC LIMIT 1`, eventType).Scan(&payload); err != nil {
		t.Fatalf("query event payload = %v", err)
	}

	return payload
}

func openReadDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}

	return db
}

func sourceHashForTest(remoteIP string) string {
	sum := sha256.Sum256([]byte(remoteIP))
	return hex.EncodeToString(sum[:])[:16]
}
