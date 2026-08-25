package health

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trucore-ai/meshdns/internal/events"
	"github.com/trucore-ai/meshdns/internal/store"
)

// ProbeFunc performs a single health check for the given URL.
// ProbeResult carries the outcome of a single health check.
type ProbeResult struct {
	Up        bool
	Status    int // HTTP status code; 0 on transport error
	LatencyMs int
}

// ProbeFunc performs a single health check for the given URL using the given
// HTTP method. method "" is treated as GET.
type ProbeFunc func(context.Context, string, string, *http.Client) ProbeResult

type Pool struct {
	st     *store.Store
	client *http.Client
	jobs   chan job
	done   chan struct{}
	wg     sync.WaitGroup

	probeFn atomic.Value
}

type job struct {
	serverID string
	url      string
	probeMethod string
	dueAt    time.Time
}

type serverTarget struct {
	id   string
	url  string
	probeMethod string
	kind targetKind
}

type targetKind int

const (
	targetDeclaredHealthy targetKind = iota
	targetProbe
)

// Start launches the health-check scheduler and worker pool.
func Start(ctx context.Context, st *store.Store, interval, timeout time.Duration, workers int) *Pool {
	if interval <= 0 {
		interval = time.Second
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	if workers <= 0 {
		workers = 1
	}

	p := &Pool{
		st:     st,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		jobs:   make(chan job, workers*32),
		done:   make(chan struct{}),
	}
	p.SetProbeFunc(defaultProbe)

	p.wg.Add(2 + workers)
	go p.schedule(ctx, interval, timeout)
	for i := 0; i < workers; i++ {
		go p.worker(ctx)
	}
	go p.pruner(ctx, 10*time.Minute)
	go func() {
		p.wg.Wait()
		close(p.done)
	}()

	return p
}

// Done closes when the scheduler and all workers have exited.
func (p *Pool) Done() <-chan struct{} {
	return p.done
}

// Wait blocks until the scheduler and all workers have exited.
func (p *Pool) Wait() {
	<-p.done
}

// SetProbeFunc overrides the probe implementation used by the pool.
func (p *Pool) SetProbeFunc(fn ProbeFunc) {
	if fn == nil {
		fn = defaultProbe
	}
	p.probeFn.Store(fn)
}

func (p *Pool) probe(ctx context.Context, method, url string) ProbeResult {
	fn := p.probeFn.Load().(ProbeFunc)
	return fn(ctx, method, url, p.client)
}

func (p *Pool) schedule(ctx context.Context, interval, timeout time.Duration) {
	defer p.wg.Done()
	defer close(p.jobs)

	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		targets, err := p.targets()
		if err == nil {
			now := time.Now().UTC()
			for _, target := range targets {
				switch target.kind {
				case targetDeclaredHealthy:
					// Servers without a health URL are declared healthy so they remain available in listings.
					if err := p.declareHealthy(target.id, now); err != nil {
						continue
					}
				case targetProbe:
					select {
					case p.jobs <- job{
						serverID: target.id,
						url:      target.url,
						probeMethod: target.probeMethod,
						dueAt:    now.Add(interval + jitterOffset(interval, target.id, now)),
					}:
					case <-ctx.Done():
						return
					}
				}
			}
		}

		timer.Reset(interval)
	}
}

func (p *Pool) worker(ctx context.Context) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-p.jobs:
			if !ok {
				return
			}
			if wait := time.Until(j.dueAt); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			p.runProbe(j.serverID, j.url, j.probeMethod)
		}
	}
}

func (p *Pool) runProbe(serverID, url, probeMethod string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	result := p.probe(context.Background(), probeMethod, url)

	// Auto-detect POST-only endpoints. Three cases trigger a POST retry:
	//   1. 405 Method Not Allowed — explicit "this is a POST-only endpoint"
	//   2. 4xx (non-405) — many MCP servers reject GET with 404
	//   3. Transport errors (status=0) — server might only respond to POST
	// Only retry when probeMethod is not already POST (avoids infinite loop).
	if !result.Up && probeMethod != http.MethodPost {
		if result.Status == http.StatusMethodNotAllowed ||
			(result.Status >= 400 && result.Status < 500 && result.Status != http.StatusMethodNotAllowed) ||
			result.Status == 0 {
			post := p.probe(context.Background(), http.MethodPost, url)
			if post.Up {
				_ = p.st.SetServerProbeMethod(serverID, http.MethodPost)
				result = post
			}
		}
	}

	up, latencyMs := result.Up, result.LatencyMs

	if err := p.st.RecordProbe(serverID, ts, up, latencyMs); err != nil {
		return
	}
	_ = events.Log(p.st, "probe", map[string]any{
		"server_id":  serverID,
		"result":     up,
		"latency_ms": latencyMs,
	}, "")
	if err := p.st.IncrementProbeCount(serverID, up); err != nil {
		return
	}
	if err := p.st.UpdateLatencyStats(serverID, latencyMs); err != nil {
		return
	}
	uptime30d, err := p.st.ComputeUptimeFromCounters(serverID)
	if err != nil {
		return
	}
	_ = p.st.SetServerState(serverID, up, ts, uptime30d)
}

func (p *Pool) declareHealthy(serverID string, now time.Time) error {
	uptime30d, err := p.st.GetUptime30d(serverID)
	if err != nil {
		return err
	}
	return p.st.SetServerState(serverID, true, now.Format(time.RFC3339), uptime30d)
}

func (p *Pool) pruner(ctx context.Context, interval time.Duration) {
	defer p.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := p.st.PruneProbes(30)
			if err != nil {
				log.Printf("[WARN] [pruner] probe prune failed: %v", err)
			} else if deleted > 0 {
				log.Printf("[INFO] [pruner] pruned %d probes older than 30 days", deleted)
			}
			deleted, err = p.st.PruneEvents(90)
			if err != nil {
				log.Printf("[WARN] [pruner] event prune failed: %v", err)
			} else if deleted > 0 {
				log.Printf("[INFO] [pruner] pruned %d events older than 90 days", deleted)
			}
			if err := p.st.RebuildAllUptimeCounters(); err != nil {
				log.Printf("[WARN] [pruner] uptime rebuild failed: %v", err)
			}
		}
	}
}

func (p *Pool) targets() ([]serverTarget, error) {
	servers, _, err := p.st.ListServers("", "", "active", "", 10000)
	if err != nil {
		return nil, err
	}

	targets := make([]serverTarget, 0, len(servers))
	for _, server := range servers {
		if strings.TrimSpace(server.HealthURL) == "" {
			targets = append(targets, serverTarget{id: server.ID, kind: targetDeclaredHealthy})
			continue
		}
		targets = append(targets, serverTarget{id: server.ID, url: server.HealthURL, probeMethod: server.ProbeMethod, kind: targetProbe})
	}

	return targets, nil
}

// mcpInitializeBody is a minimal MCP initialize request used for POST probes
// of streamable-HTTP MCP endpoints.
const mcpInitializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"meshdns-health","version":"1.0.0"}}}`

func defaultProbe(ctx context.Context, method, url string, client *http.Client) ProbeResult {
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(mcpInitializeBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return ProbeResult{}
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	latency := int(time.Since(start) / time.Millisecond)
	if latency < 0 {
		latency = 0
	}

	if resp.StatusCode/100 != 2 {
		return ProbeResult{Up: false, Status: resp.StatusCode, LatencyMs: latency}
	}

	return ProbeResult{Up: true, Status: resp.StatusCode, LatencyMs: latency}
}

func jitterOffset(interval time.Duration, serverID string, now time.Time) time.Duration {
	if interval <= 0 {
		return 0
	}

	seed := sha1.Sum([]byte(serverID + now.UTC().Format(time.RFC3339Nano)))
	u := binary.BigEndian.Uint64(seed[:8])
	window := uint64(interval / 10)
	if window < 1 {
		window = 1
	}
	offset := int64(u%(2*window+1)) - int64(window)
	return time.Duration(offset)
}
