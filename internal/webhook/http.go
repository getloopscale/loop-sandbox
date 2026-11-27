// Package webhook delivers sandbox lifecycle events to a configured HTTP endpoint (M20
// agent-native DX). Delivery is best-effort and asynchronous — a slow or failing webhook must
// never block a sandbox operation, and a burst must not spawn unbounded goroutines. The target
// URL is operator-configured (not tenant-reachable); it is nonetheless dialed through the
// anti-SSRF egress policy so a compromised/misconfigured target cannot become an internal-network
// probe (DNS-rebinding-safe: every resolved IP is checked at connect time, not just literals).
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/egress"
)

const (
	queueCap   = 1024 // bounded backlog; excess events are dropped (best-effort contract)
	numWorkers = 8    // fixed delivery concurrency (no per-event goroutine fan-out)
)

// HTTPSink POSTs each event as JSON to the configured URL via a bounded worker pool.
type HTTPSink struct {
	url    string
	client *http.Client
	queue  chan app.WebhookEvent
	// counters for observability (the wiring triad's runtime metric): delivered / dropped.
	delivered atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
}

// NewHTTP builds a sink for url (returns nil when url is empty — webhooks are opt-in). Delivery
// dials through the egress policy so a resolved IP in a blocked range (IMDS/loopback/private) is
// refused at connect time.
func NewHTTP(url string) *HTTPSink {
	if url == "" {
		return nil
	}
	pol, _ := egress.New()
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: pol.ControlFunc()}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
	h := &HTTPSink{url: url, client: client, queue: make(chan app.WebhookEvent, queueCap)}
	for i := 0; i < numWorkers; i++ {
		go h.worker()
	}
	return h
}

// Emit enqueues the event for delivery (non-blocking). If the backlog is full the event is
// dropped (best-effort) and counted — never blocks the caller's hot path.
func (h *HTTPSink) Emit(_ context.Context, e app.WebhookEvent) {
	if h == nil {
		return
	}
	select {
	case h.queue <- e:
	default:
		h.dropped.Add(1)
	}
}

// Stats returns delivery counters (delivered, dropped, failed) for observability.
func (h *HTTPSink) Stats() (delivered, dropped, failed int64) {
	return h.delivered.Load(), h.dropped.Load(), h.failed.Load()
}

func (h *HTTPSink) worker() {
	for e := range h.queue {
		h.deliver(e)
	}
}

func (h *HTTPSink) deliver(e app.WebhookEvent) {
	body, err := json.Marshal(e)
	if err != nil {
		h.failed.Add(1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		h.failed.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LoopSandbox-Event", e.Type)
	resp, err := h.client.Do(req) // egress-dialed: a blocked IP fails here
	if err != nil {
		h.failed.Add(1)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		h.failed.Add(1) // non-2xx endpoint response is a delivery failure, not a success
		return
	}
	h.delivered.Add(1)
}

var _ app.WebhookSink = (*HTTPSink)(nil)
