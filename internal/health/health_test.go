package health

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
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
	pool.SetProbeFunc(func(context.Context, string, *http.Client) (bool, int) {
		probeCalls.Add(1)
		return false, 0
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

func (p *probeController) probe(_ context.Context, url string, _ *http.Client) (bool, int) {
	p.mu.RLock()
	state, ok := p.states[url]
	p.mu.RUnlock()
	if !ok {
		return false, 0
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
	return state.up, state.latency
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
