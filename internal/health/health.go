package health

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/trucore-ai/meshdns/internal/store"
)

var logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Config holds health-check configuration.
type Config struct {
	Interval time.Duration
	Timeout  time.Duration
	Workers  int
}

// Start launches the background health-check worker pool.
// It probes each active server's health_url at the configured interval.
// Servers without a health_url are treated as always UP.
func Start(ctx context.Context, s *store.Store, cfg *Config) {
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}

	jobs := make(chan probeJob, cfg.Workers*2)

	// Worker pool
	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				probeAndRecord(s, job, cfg.Timeout)
			}
		}()
	}

	// Ticker with jittered start
	jitter := time.Duration(rand.Int63n(int64(cfg.Interval / 10)))
	select {
	case <-ctx.Done():
		close(jobs)
		wg.Wait()
		return
	case <-time.After(jitter):
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	// Initial probe
	dispatchProbes(s, jobs)

	for {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case <-ticker.C:
			dispatchProbes(s, jobs)
		}
	}
}

type probeJob struct {
	ServerID    string
	HealthURL   string
	ProbeMethod string
}

// mcpInitializeBody is a minimal MCP initialize request used for POST probes
// of streamable-HTTP MCP endpoints.
const mcpInitializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"meshdns-health","version":"1.0.0"}}}`

// dispatchProbes fetches all active servers with health_urls and enqueues probe jobs.
func dispatchProbes(s *store.Store, jobs chan<- probeJob) {
	servers, _, err := s.ListServers("", "", "active", "", 10000)
	if err != nil {
		logger.Error("failed to list servers for probing", "error", err)
		return
	}

	for _, srv := range servers {
		if srv.HealthURL == "" {
			// Servers without health_url are declared healthy
			now := time.Now().UTC().Format(time.RFC3339)
			_ = s.SetServerState(srv.ID, 1, now, 0.0)
			continue
		}

		probeMethod := srv.ProbeMethod
		if probeMethod == "" {
			probeMethod = "GET"
		}

		select {
		case jobs <- probeJob{ServerID: srv.ID, HealthURL: srv.HealthURL, ProbeMethod: probeMethod}:
		default:
			logger.Warn("probe job queue full, dropping job", "server_id", srv.ID)
		}
	}
}

// probeAndRecord probes a single server and records the result.
func probeAndRecord(s *store.Store, job probeJob, timeout time.Duration) {
	start := time.Now()
	ok, latency := probeHTTP(job.HealthURL, timeout, job.ProbeMethod, s, job.ServerID)

	// Record probe
	_ = s.RecordProbe(job.ServerID, ok, int(latency.Milliseconds()))

	// Compute 30-day uptime
	uptime, _ := s.GetUptime30d(job.ServerID)
	now := time.Now().UTC().Format(time.RFC3339)

	up := 0
	if ok {
		up = 1
	}
	_ = s.SetServerState(job.ServerID, up, now, uptime)

	logger.Debug("probe",
		"server_id", job.ServerID,
		"up", ok,
		"latency_ms", latency.Milliseconds(),
		"uptime_30d", uptime,
	)

	elapsed := time.Since(start)
	_ = elapsed
}

// probeHTTP performs an HTTP health check with timeout.
// Supports GET by default, with POST auto-detect for POST-only endpoints.
// Returns (up bool, latency time.Duration).
func probeHTTP(url string, timeout time.Duration, method string, s *store.Store, serverID string) (bool, time.Duration) {
	if method == "" {
		method = "GET"
	}

	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	ok, latency, status := doProbe(client, method, url)

	// Auto-detect POST-only endpoints.
	// Three cases trigger a POST retry:
	//   1. 405 Method Not Allowed — explicit "this is a POST-only endpoint"
	//   2. 4xx (non-405) — many MCP servers reject GET with 404
	//   3. Transport errors (status=0) — server might only respond to POST
	// Only retry when method is not already POST (avoids infinite loop).
	if !ok && method != http.MethodPost {
		if status == http.StatusMethodNotAllowed ||
			(status >= 400 && status < 500 && status != http.StatusMethodNotAllowed) ||
			status == 0 {
			postOK, postLat, _ := doProbe(client, http.MethodPost, url)
			if postOK {
				_ = s.SetServerProbeMethod(serverID, http.MethodPost)
				return true, postLat
			}
		}
	}

	return ok, latency
}

// doProbe performs a single HTTP probe with the given method.
func doProbe(client *http.Client, method, url string) (ok bool, latency time.Duration, status int) {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(mcpInitializeBody)
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return false, 0, 0
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency = time.Since(start)

	if err != nil {
		return false, latency, 0
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, latency, resp.StatusCode
	}
	return false, latency, resp.StatusCode
}