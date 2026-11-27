package webhook

import (
	"context"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
)

// The bounded worker pool processes every queued event (no unbounded goroutine fan-out): 3
// events all reach a terminal state (delivered/failed) — here they fail (unroutable TEST-NET
// target), proving the pool drains rather than leaking.
func TestHTTPSink_worker_pool_drains_all_events(t *testing.T) {
	h := NewHTTP("http://198.51.100.23/webhook") // TEST-NET-2 (not loopback/private) — dial fails
	for i := 0; i < 3; i++ {
		h.Emit(context.Background(), app.WebhookEvent{Type: "sandbox.created", TenantID: "t", SandboxID: "s"})
	}
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		if d, dr, f := h.Stats(); d+dr+f == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if d, dr, f := h.Stats(); d+dr+f != 3 {
		t.Errorf("stats sum = %d, want 3 (delivered=%d dropped=%d failed=%d)", d+dr+f, d, dr, f)
	}
}

// A blocked-IP webhook target (loopback) is refused at DIAL by the egress ControlFunc — the
// DNS-rebinding-safe SSRF guard — and counted failed, never delivered (M20 review HIGH).
func TestHTTPSink_ssrf_loopback_refused(t *testing.T) {
	h := NewHTTP("http://127.0.0.1:9/webhook")
	h.Emit(context.Background(), app.WebhookEvent{Type: "sandbox.created", TenantID: "t", SandboxID: "s"})
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, f := h.Stats(); f == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if d, _, f := h.Stats(); f != 1 || d != 0 {
		t.Errorf("loopback target: delivered=%d failed=%d, want delivered=0 failed=1 (SSRF blocked at dial)", d, f)
	}
}
