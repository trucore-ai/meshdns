package health

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trucore-ai/meshdns/internal/store"
)

func TestPoolHealthyServerRecordsProbeAndState(t *testing.T) {
	st, dbPath := newTestStore(t)
	ctrl := newProbeController()
	ctrl.set("https://healthy.example.test/health", true, 12)

	mustCreate(t, st, store.Server{
		ID:        "srv-healthy",
		Name:      "healthy",
		ServerURL: "https://healthy.example.test",
		HealthURL: "https://healthy.example.test/health",
	})

	ctx, cancel := context.WithCancel(context.Background())
	pool := Start(ctx, st, 50*time.Millisecond, 200*time.Millisecond, 2)
	pool.SetProbeFunc(ctrl.probe)
	defer func() {
		cancel()
		<-pool.Done()
	}()

	if err := waitForState(t, st, "srv-healthy", func(s store.Server) bool {
		return s.Up && s.LastCheckedAt != "" && s.Uptime30d == 1
	}); err != nil {
		t.Fatal(err)
	}
	if got := probeCount(t, dbPath, "srv-healthy"); got == 0 {
		t.Fatalf("probe count = %d, want >0", got)
	}
}

func TestPoolMarksFailedServerDown(t *testing.T) {
	st, _ := newTestStore(t)
	ctrl := newProbeController()
	ctrl.set("https://down.example.test/health", false, 0)

	mustCreate(t, st, store.Server{
		ID:        "srv-down",
		Name:      "down",
		ServerURL: "https://down.example.test",
		HealthURL: "https://down.example.test/health",
	})

	ctx, cancel := context.WithCancel(context.Background())
	pool := Start(ctx, st, 50*time.Millisecond, 200*time.Millisecond, 2)
	pool.SetProbeFunc(ctrl.probe)
	defer func() {
		cancel()
		<-pool.Done()
	}()

	if err := waitForState(t, st, "srv-down", func(s store.Server) bool {
		return !s.Up && s.LastCheckedAt != ""
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolRecoversServerAndUptimeTracksHistory(t *testing.T) {
	st, _ := newTestStore(t)
	ctrl := newProbeController()
	ctrl.set("https://recover.example.test/health", false, 0)

	mustCreate(t, st, store.Server{
		ID:        "srv-recover",
		Name:      "recover",
		ServerURL: "https://recover.example.test",
		HealthURL: "https://recover.example.test/health",
	})

	ctx, cancel := context.WithCancel(context.Background())
	pool := Start(ctx, st, 50*time.Millisecond, 200*time.Millisecond, 2)
	pool.SetProbeFunc(ctrl.probe)
	defer func() {
		cancel()
		<-pool.Done()
	}()

	if err := waitForState(t, st, "srv-recover", func(s store.Server) bool {
		return !s.Up && s.LastCheckedAt != ""
	}); err != nil {
		t.Fatal(err)
	}

	ctrl.set("https://recover.example.test/health", true, 9)

	if err := waitForState(t, st, "srv-recover", func(s store.Server) bool {
		return s.Up && s.LastCheckedAt != "" && s.Uptime30d > 0 && s.Uptime30d < 1
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolDeclaresServerWithoutHealthURLHealthy(t *testing.T) {
	st, dbPath := newTestStore(t)
	var probeCalls atomic.Int32

	mustCreate(t, st, store.Server{
		ID:        "srv-declared",
		Name:      "declared",
		ServerURL: "https://declared.example.test",
		HealthURL: "",
	})

	ctx, cancel := context.WithCancel(context.Background())
	pool := Start(ctx, st, 50*time.Millisecond, 200*time.Millisecond, 2)
	pool.SetProbeFunc(func(context.Context, string, string, *http.Client) ProbeResult {
		probeCalls.Add(1)
		return ProbeResult{}
	})
	defer func() {
		cancel()
		<-pool.Done()
	}()

	if err := waitForState(t, st, "srv-declared", func(s store.Server) bool {
		return s.Up && s.LastCheckedAt != "" && s.Uptime30d == 1
	}); err != nil {
		t.Fatal(err)
	}
	if got := probeCount(t, dbPath, "srv-declared"); got != 0 {
		t.Fatalf("probe count = %d, want 0", got)
	}
	if got := probeCalls.Load(); got != 0 {
		t.Fatalf("probe calls = %d, want 0", got)
	}
}

func TestPoolStopsOnCancelAfterInFlightProbe(t *testing.T) {
	st, _ := newTestStore(t)
	ctrl := newProbeController()
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	ctrl.setBlocking("https://cancel.example.test/health", started, release, true, 20)

	mustCreate(t, st, store.Server{
		ID:        "srv-cancel",
		Name:      "cancel",
		ServerURL: "https://cancel.example.test",
		HealthURL: "https://cancel.example.test/health",
	})

	ctx, cancel := context.WithCancel(context.Background())
	pool := Start(ctx, st, 50*time.Millisecond, 2*time.Second, 1)
	pool.SetProbeFunc(ctrl.probe)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never started")
	}

	cancel()

	select {
	case <-pool.Done():
		t.Fatal("pool exited before in-flight probe completed")
	default:
	}

	close(release)

	select {
	case <-pool.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not exit after cancel and probe completion")
	}
}

type probeController struct {
	mu     sync.RWMutex
	states map[string]probeState
}

type probeState struct {
	up      bool
	status  int
	latency int
	blocked chan struct{}
	started chan struct{}
}

func newProbeController() *probeController {
	return &probeController{states: map[string]probeState{}}
}

func (p *probeController) set(url string, up bool, latency int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.states[url]
	state.up = up
	state.latency = latency
	p.states[url] = state
}

// setStatus registers a probe outcome with an explicit HTTP status code
// (used to simulate e.g. 405 responses). Key may be "METHOD url" for
// method-specific behavior or a bare URL for any-method behavior.
func (p *probeController) setStatus(key string, up bool, status, latency int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.states[key] = probeState{up: up, status: status, latency: latency}
}

func (p *probeController) setBlocking(url string, started, release chan struct{}, up bool, latency int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.states[url] = probeState{
		up:      up,
		latency: latency,
		blocked: release,
		started: started,
	}
}

func (p *probeController) probe(_ context.Context, method, url string, _ *http.Client) ProbeResult {
	p.mu.RLock()
	state, ok := p.states[method+" "+url]
	if !ok {
		state, ok = p.states[url]
	}
	p.mu.RUnlock()
	if !ok {
		return ProbeResult{}
	}
	if state.started != nil {
		select {
		case state.started <- struct{}{}:
		default:
		}
	}
	if state.blocked != nil {
		<-state.blocked
	}
	if state.up {
		return ProbeResult{Up: true, Status: http.StatusOK, LatencyMs: state.latency}
	}
	return ProbeResult{Up: false, Status: state.status, LatencyMs: state.latency}
}

func TestPoolAutoDetectsPOSTOnlyEndpoint(t *testing.T) {
	st, _ := newTestStore(t)
	ctrl := newProbeController()
	const healthURL = "https://post-only.example.test/mcp"
	// First probe uses method "" (defaults to GET) — the bare-URL key is the
	// fallback for any method, so it simulates the 405 GET response.
	ctrl.setStatus(healthURL, false, http.StatusMethodNotAllowed, 3)
	ctrl.setStatus("POST "+healthURL, true, http.StatusOK, 7)

	mustCreate(t, st, store.Server{
		ID:        "srv-post-only",
		Name:      "post-only",
		ServerURL: "https://post-only.example.test",
		HealthURL: healthURL,
	})

	ctx, cancel := context.WithCancel(context.Background())
	pool := Start(ctx, st, 50*time.Millisecond, 200*time.Millisecond, 2)
	pool.SetProbeFunc(ctrl.probe)
	defer func() {
		cancel()
		<-pool.Done()
	}()

	if err := waitForState(t, st, "srv-post-only", func(s store.Server) bool {
		return s.Up && s.LastCheckedAt != ""
	}); err != nil {
		t.Fatal(err)
	}

	server, err := st.GetServer("srv-post-only")
	if err != nil {
		t.Fatalf("GetServer = %v", err)
	}
	if server.ProbeMethod != http.MethodPost {
		t.Fatalf("ProbeMethod = %q, want %q (auto-detected and persisted)", server.ProbeMethod, http.MethodPost)
	}
}

func TestDefaultProbePOSTSendsMCPInitialize(t *testing.T) {
	var gotMethod, gotContentType, gotAccept, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		body := make([]byte, 512)
		n, _ := r.Body.Read(body)
		gotBody = string(body[:n])
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	client := srv.Client()

	get := defaultProbe(context.Background(), http.MethodGet, srv.URL, client)
	if get.Up {
		t.Fatal("GET probe Up = true, want false (405 endpoint)")
	}
	if get.Status != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe Status = %d, want 405", get.Status)
	}

	post := defaultProbe(context.Background(), http.MethodPost, srv.URL, client)
	if !post.Up {
		t.Fatalf("POST probe Up = false, want true (status=%d)", post.Status)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("server saw method %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotAccept == "" {
		t.Fatal("Accept header missing on POST probe")
	}
	if !strings.Contains(gotBody, `"method":"initialize"`) {
		t.Fatalf("POST body does not look like an MCP initialize request: %q", gotBody)
	}
}

func waitForState(t *testing.T, st *store.Store, id string, predicate func(store.Server) bool) error {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		server, err := st.GetServer(id)
		if err == nil && predicate(server) {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("GetServer %s = %w", id, err)
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timeout waiting for %s: last error = %v", id, err)
			}
			return fmt.Errorf("timeout waiting for %s: last state = %+v", id, server)
		}
		<-ticker.C
	}
}

func probeCount(t *testing.T, dbPath, serverID string) int {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Clean(dbPath))
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM probes WHERE server_id = ?`, serverID).Scan(&count); err != nil {
		t.Fatalf("probe count query = %v", err)
	}

	return count
}

func newTestStore(t *testing.T) (*store.Store, string) {
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

func mustCreate(t *testing.T, st *store.Store, server store.Server) {
	t.Helper()

	if err := st.CreateServer(server, "write-key-hash"); err != nil {
		t.Fatalf("CreateServer %s = %v", server.ID, err)
	}
}
