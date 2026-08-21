package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/trucore-ai/meshdns/internal/store"
)

func TestResolveReturnsUpActiveServersOrderedByUptime(t *testing.T) {
	st, _ := newAPITestStore(t)
	seedQueryServers(t, st)
	router := New(st).Router()

	rec := performJSON(t, router, http.MethodGet, "/v0/resolve?capability=sandbox", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got []serverJSON
	decodeResponse(t, rec, &got)
	if gotIDs(got) != "srv-a,srv-b" {
		t.Fatalf("resolved IDs = %s, want srv-a,srv-b", gotIDs(got))
	}
}

func TestResolveValidationAndUnknownCapability(t *testing.T) {
	st, _ := newAPITestStore(t)
	seedQueryServers(t, st)
	router := New(st).Router()

	missing := performJSON(t, router, http.MethodGet, "/v0/resolve", "", nil)
	if missing.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing capability status = %d, body = %s", missing.Code, missing.Body.String())
	}

	unknown := performJSON(t, router, http.MethodGet, "/v0/resolve?capability=unknown", "", nil)
	if unknown.Code != http.StatusOK {
		t.Fatalf("unknown capability status = %d, body = %s", unknown.Code, unknown.Body.String())
	}

	var got []serverJSON
	decodeResponse(t, unknown, &got)
	if len(got) != 0 {
		t.Fatalf("unknown capability result = %+v, want empty array", got)
	}
}

func TestResolveLogsPrivacyPreservingEvent(t *testing.T) {
	st, dbPath := newAPITestStore(t)
	seedQueryServers(t, st)
	router := New(st).Router()

	rawIP := "203.0.113.55"
	rawUA := "meshdns-python/0.1 full-client detail"
	rec := performGETWithHeaders(t, router, "/v0/resolve?capability=sandbox", rawIP+":9876", rawUA)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	payloadJSON := readEventPayload(t, dbPath, "resolve")
	if strings.Contains(payloadJSON, rawIP) {
		t.Fatalf("resolve payload contains raw IP %q: %s", rawIP, payloadJSON)
	}
	if strings.Contains(payloadJSON, "full-client") || strings.Contains(payloadJSON, "detail") {
		t.Fatalf("resolve payload contains full user-agent: %s", payloadJSON)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("Unmarshal payload = %v", err)
	}
	if payload["capability"] != "sandbox" {
		t.Fatalf("capability = %#v, want sandbox", payload["capability"])
	}
	if payload["result_count"] != float64(2) {
		t.Fatalf("result_count = %#v, want 2", payload["result_count"])
	}
	if payload["ua_tag"] != "meshdns-python/0.1" {
		t.Fatalf("ua_tag = %#v, want first token", payload["ua_tag"])
	}
	if payload["source_hash"] != sourceHashForTest(rawIP) {
		t.Fatalf("source_hash = %#v, want hash of remote IP", payload["source_hash"])
	}
}

func TestListServersFiltersAndPagination(t *testing.T) {
	st, _ := newAPITestStore(t)
	seedQueryServers(t, st)
	router := New(st).Router()

	query := performJSON(t, router, http.MethodGet, "/v0/servers?query=alpha", "", nil)
	assertListIDs(t, query, []string{"srv-a"})

	capability := performJSON(t, router, http.MethodGet, "/v0/servers?capability=metrics", "", nil)
	assertListIDs(t, capability, []string{"srv-b"})

	delisted := performJSON(t, router, http.MethodGet, "/v0/servers?status=delisted", "", nil)
	assertListIDs(t, delisted, []string{"srv-d"})

	var walked []string
	cursor := ""
	for {
		path := "/v0/servers?status=all&limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := performJSON(t, router, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("pagination status = %d, body = %s", rec.Code, rec.Body.String())
		}

		var got listServersResponse
		decodeResponse(t, rec, &got)
		for _, server := range got.Servers {
			walked = append(walked, server.ID)
		}
		if got.NextCursor == "" {
			break
		}
		cursor = got.NextCursor
	}
	if !reflect.DeepEqual(walked, []string{"srv-a", "srv-b", "srv-c", "srv-d"}) {
		t.Fatalf("paginated IDs = %#v, want all four in name order", walked)
	}
}

func TestExportIncludesAllServers(t *testing.T) {
	st, _ := newAPITestStore(t)
	seedQueryServers(t, st)
	router := New(st).Router()

	rec := performJSON(t, router, http.MethodGet, "/v0/export", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got exportResponse
	decodeResponse(t, rec, &got)
	if _, err := time.Parse(time.RFC3339, got.ExportedAt); err != nil {
		t.Fatalf("exported_at = %q, want RFC3339", got.ExportedAt)
	}
	if gotIDs(got.Servers) != "srv-a,srv-b,srv-c,srv-d" {
		t.Fatalf("exported IDs = %s, want all four", gotIDs(got.Servers))
	}

	statuses := make(map[string]string, len(got.Servers))
	for _, server := range got.Servers {
		statuses[server.ID] = server.Status
	}
	if statuses["srv-d"] != "delisted" {
		t.Fatalf("srv-d status = %q, want delisted", statuses["srv-d"])
	}
}

func TestStatsCountsServersAndRecentEvents(t *testing.T) {
	st, _ := newAPITestStore(t)
	seedQueryServers(t, st)
	router := New(st).Router()

	if err := st.AppendEvent(time.Now().UTC().Format(time.RFC3339), "probe", `{"server_id":"srv-a"}`); err != nil {
		t.Fatalf("AppendEvent probe = %v", err)
	}
	for _, path := range []string{
		"/v0/resolve?capability=sandbox",
		"/v0/resolve?capability=unknown",
	} {
		rec := performJSON(t, router, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("resolve %s status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
	}

	rec := performJSON(t, router, http.MethodGet, "/v0/stats", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got statsResponse
	decodeResponse(t, rec, &got)
	if got.ServersActive != 3 || got.ServersTotal != 4 || got.UpCount != 2 || got.Resolutions24 != 2 || got.Probes24 != 1 {
		t.Fatalf("stats = %+v, want active=3 total=4 up=2 resolutions=2 probes=1", got)
	}
}

func seedQueryServers(t *testing.T, st *store.Store) {
	t.Helper()

	servers := []store.Server{
		{
			ID:           "srv-a",
			Name:         "alpha",
			Description:  "Alpha sandbox server",
			ServerURL:    "https://alpha.example/dns-query",
			HealthURL:    "https://alpha.example/health",
			OwnerContact: "ops-alpha@example.com",
			Capabilities: []string{"sandbox"},
		},
		{
			ID:           "srv-b",
			Name:         "bravo",
			Description:  "Bravo sandbox server",
			ServerURL:    "https://bravo.example/dns-query",
			HealthURL:    "https://bravo.example/health",
			OwnerContact: "ops-bravo@example.com",
			Capabilities: []string{"metrics", "sandbox"},
		},
		{
			ID:           "srv-c",
			Name:         "charlie",
			Description:  "Charlie sandbox server",
			ServerURL:    "https://charlie.example/dns-query",
			HealthURL:    "https://charlie.example/health",
			OwnerContact: "ops-charlie@example.com",
			Capabilities: []string{"sandbox"},
		},
		{
			ID:           "srv-d",
			Name:         "delta",
			Description:  "Delta sandbox server",
			ServerURL:    "https://delta.example/dns-query",
			HealthURL:    "https://delta.example/health",
			OwnerContact: "ops-delta@example.com",
			Capabilities: []string{"sandbox"},
		},
	}
	for _, server := range servers {
		if err := st.CreateServer(server, "hash-"+server.ID); err != nil {
			t.Fatalf("CreateServer %s = %v", server.ID, err)
		}
	}

	checkedAt := time.Now().UTC().Format(time.RFC3339)
	states := []struct {
		id        string
		up        bool
		uptime30d float64
	}{
		{id: "srv-a", up: true, uptime30d: 0.9},
		{id: "srv-b", up: true, uptime30d: 0.7},
		{id: "srv-c", up: false, uptime30d: 0.99},
		{id: "srv-d", up: true, uptime30d: 1},
	}
	for _, state := range states {
		if err := st.SetServerState(state.id, state.up, checkedAt, state.uptime30d); err != nil {
			t.Fatalf("SetServerState %s = %v", state.id, err)
		}
	}
	if err := st.DelistServer("srv-d"); err != nil {
		t.Fatalf("DelistServer srv-d = %v", err)
	}
}

func performGETWithHeaders(t *testing.T, router http.Handler, path, remoteAddr, userAgent string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func assertListIDs(t *testing.T, rec *httptest.ResponseRecorder, want []string) {
	t.Helper()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got listServersResponse
	decodeResponse(t, rec, &got)
	ids := make([]string, 0, len(got.Servers))
	for _, server := range got.Servers {
		ids = append(ids, server.ID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("list IDs = %#v, want %#v", ids, want)
	}
}

func gotIDs(servers []serverJSON) string {
	ids := make([]string, 0, len(servers))
	for _, server := range servers {
		ids = append(ids, server.ID)
	}

	return strings.Join(ids, ",")
}
