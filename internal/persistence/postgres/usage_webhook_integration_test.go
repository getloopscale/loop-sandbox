//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/metering"
	"github.com/getloopscale/loop-sandbox/internal/persistence/postgres"
)

// The usage a tenant generated reaches the HTTP receiver as events that add up
// to the same amounts, through the real outbox in PostgreSQL.
func TestUsageDeliveredToTheWebhookAddsUpToTheUsageRecorded(t *testing.T) {
	pool := setupPostgres(t)
	ctx := context.Background()
	store := postgres.New(pool)
	start, end := domain.CurrentPeriod(fixedNow)
	recorded := map[string]int64{}
	for _, u := range []struct {
		tenant string
		metric domain.CostMetric
		amount int64
	}{
		{"tenant-a", domain.MetricSandboxCreations, 1}, {"tenant-a", domain.MetricSandboxCreations, 1},
		{"tenant-a", domain.MetricSandboxCPUSecond, 4}, {"tenant-b", domain.MetricSandboxCreations, 1},
		{"tenant-b", domain.MetricSandboxCPUSecond, 2}, {"tenant-a", domain.MetricSandboxCPUSecond, 3},
	} {
		if err := store.Increment(ctx, u.tenant, u.metric, u.amount, start, end); err != nil {
			t.Fatalf("Increment: %v", err)
		}
		recorded[u.tenant+"/"+string(u.metric)] += u.amount
	}
	var (
		mu        sync.Mutex
		delivered = map[string]int64{}
		seen      = map[int64]bool{}
	)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got struct {
			Events []struct {
				EventID  int64  `json:"event_id"`
				TenantID string `json:"tenant_id"`
				Metric   string `json:"metric"`
				Amount   int64  `json:"amount"`
			} `json:"events"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, e := range got.Events {
			if seen[e.EventID] {
				continue // the receiver ignores an event it has seen
			}
			seen[e.EventID] = true
			delivered[e.TenantID+"/"+e.Metric] += e.Amount
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	sink, err := metering.NewHTTPSink(receiver.URL, "")
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}

	n, err := metering.NewEmitter(store, sink, 4).Drain(ctx)

	if err != nil || n != 6 {
		t.Fatalf("drain = %d (err %v), want the 6 events", n, err)
	}
	if len(delivered) != len(recorded) {
		t.Errorf("delivered %v, recorded %v", delivered, recorded)
	}
	for key, want := range recorded {
		if delivered[key] != want {
			t.Errorf("%s: delivered %d, recorded %d", key, delivered[key], want)
		}
	}
}
