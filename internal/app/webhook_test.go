package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// recSink records emitted webhook events.
type recSink struct {
	mu     sync.Mutex
	events []app.WebhookEvent
}

func (r *recSink) Emit(_ context.Context, e app.WebhookEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recSink) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

// Creating and destroying a sandbox emit the lifecycle webhooks (M20 agent-native DX).
func TestLifecycle_webhooks_emitted(t *testing.T) {
	ctx := context.Background()
	sink := &recSink{}
	svc := app.NewService(memory.New(), fake.New(), clock.Fixed{T: time.Unix(0, 0).UTC()}, &seqIDs{}, &fakeAudit{}, nil, nil)
	svc.SetWebhookSink(sink)

	sb, err := svc.CreateSandbox(ctx, actorA, "", time.Hour)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := svc.DestroySandbox(ctx, actorA, sb.ID); err != nil {
		t.Fatalf("DestroySandbox: %v", err)
	}

	got := sink.types()
	want := map[string]bool{"sandbox.created": false, "sandbox.destroyed": false}
	for _, ty := range got {
		if _, ok := want[ty]; ok {
			want[ty] = true
		}
	}
	if !want["sandbox.created"] || !want["sandbox.destroyed"] {
		t.Errorf("emitted %v, want both sandbox.created and sandbox.destroyed", got)
	}
	// The event carries tenant + sandbox identity.
	if len(sink.events) > 0 && (sink.events[0].TenantID != "tenant-a" || sink.events[0].SandboxID != sb.ID) {
		t.Errorf("event identity = %+v, want tenant-a / %s", sink.events[0], sb.ID)
	}
}
