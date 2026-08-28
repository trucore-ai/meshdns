package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/trucore-ai/provengraph/internal/store"
)

func TestProbe_UpServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, _ := s.CreateServer(&store.Server{
		ID: "srv-1", Name: "test", ServerURL: ts.URL, HealthURL: ts.URL + "/health",
		WriteKeyHash: "h",
	})

	ok, latency := probeHTTP(ts.URL+"/health", 5*time.Second, "GET", s, id)
	if !ok {
		t.Fatal("expected probe to succeed")
	}
	if latency <= 0 {
		t.Error("expected positive latency")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	s.SetServerState(id, 1, now, 1.0)

	state, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	_ = state
}

func TestProbe_DownServer(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, _ := s.CreateServer(&store.Server{
		ID: "down-srv", Name: "down-test", ServerURL: "http://127.0.0.1:19999",
		HealthURL: "http://127.0.0.1:19999/health", WriteKeyHash: "h",
	})

	ok, _ := probeHTTP("http://127.0.0.1:19999/health", 500*time.Millisecond, "GET", s, id)
	if ok {
		t.Fatal("expected probe to fail on dead port")
	}
}

func TestProbe_Timeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer ts.Close()

	dir := t.TempDir()
	s, _ := store.Open(filepath.Join(dir, "test.db"))
	defer s.Close()
	id, _ := s.CreateServer(&store.Server{
		ID: "timeout-srv", Name: "timeout-test", ServerURL: ts.URL,
		HealthURL: ts.URL, WriteKeyHash: "h",
	})

	ok, _ := probeHTTP(ts.URL, 100*time.Millisecond, "GET", s, id)
	if ok {
		t.Fatal("expected probe to timeout")
	}
}

func TestProbe_POSTAutoDetect(t *testing.T) {
	// Server that only responds to POST with 200
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer ts.Close()

	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, _ := s.CreateServer(&store.Server{
		ID: "post-srv", Name: "post-test", ServerURL: ts.URL,
		HealthURL: ts.URL, WriteKeyHash: "h", ProbeMethod: "GET",
	})

	// First probe — GET should fail with 405, then auto-detect POST
	ok, _ := probeHTTP(ts.URL, 5*time.Second, "GET", s, id)
	if !ok {
		t.Fatal("expected POST auto-detect to succeed after GET 405")
	}

	// Verify probe_method was updated to POST
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProbeMethod != http.MethodPost {
		t.Errorf("expected probe_method=POST after auto-detect, got %q", got.ProbeMethod)
	}
}

func TestWorkerPool_StartAndStop(t *testing.T) {
	dir := t.TempDir()
	s, _ := store.Open(filepath.Join(dir, "test.db"))
	defer s.Close()

	// Register a server with a health URL that works
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	s.CreateServer(&store.Server{
		ID: "w1", Name: "worker-test", ServerURL: ts.URL, HealthURL: ts.URL + "/health",
		WriteKeyHash: "h",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{Interval: 100 * time.Millisecond, Timeout: 1 * time.Second, Workers: 2}
	go Start(ctx, s, cfg)

	// Let a few probe cycles run
	time.Sleep(350 * time.Millisecond)
	cancel()
}