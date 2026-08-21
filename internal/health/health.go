package health

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trucore-ai/meshdns/internal/events"
	"github.com/trucore-ai/meshdns/internal/store"
)

// ProbeFunc performs a single health check for the given URL.
type ProbeFunc func(context.Context, string, *http.Client) (bool, int)

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
	dueAt    time.Time
}

type serverTarget struct {
	id   string
	url  string
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
		client: &http.Client{Timeout: timeout},
		jobs:   make(chan job, workers*32),
		done:   make(chan struct{}),
	}
	p.SetProbeFunc(defaultProbe)

	p.wg.Add(1 + workers)
	go p.schedule(ctx, interval, timeout)
	for i := 0; i < workers; i++ {
		go p.worker(ctx)
	}
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

func (p *Pool) probe(ctx context.Context, url string) (bool, int) {
	fn := p.probeFn.Load().(ProbeFunc)
	return fn(ctx, url, p.client)
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
			p.runProbe(j.serverID, j.url)
		}
	}
}

func (p *Pool) runProbe(serverID, url string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	up, latencyMs := p.probe(context.Background(), url)

	if err := p.st.RecordProbe(serverID, ts, up, latencyMs); err != nil {
		return
	}
	_ = events.Log(p.st, "probe", map[string]any{
		"server_id":  serverID,
		"result":     up,
		"latency_ms": latencyMs,
	}, "")
	uptime30d, err := p.st.GetUptime30d(serverID)
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
		targets = append(targets, serverTarget{id: server.ID, url: server.HealthURL, kind: targetProbe})
	}

	return targets, nil
}

func defaultProbe(ctx context.Context, url string, client *http.Client) (bool, int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, 0
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode/100 != 2 {
		return false, 0
	}

	latency := int(time.Since(start) / time.Millisecond)
	if latency < 0 {
		latency = 0
	}
	return true, latency
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
