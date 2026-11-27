package domain_test

import (
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

var nt0 = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

func TestNode_stale_when_heartbeat_old(t *testing.T) {
	thr := 30 * time.Second
	fresh := &domain.Node{State: domain.NodeReady, LastHeartbeat: nt0.Add(-10 * time.Second)}
	stale := &domain.Node{State: domain.NodeReady, LastHeartbeat: nt0.Add(-2 * time.Minute)}
	if fresh.Stale(nt0, thr) {
		t.Errorf("fresh node (10s old, thr 30s) should NOT be stale")
	}
	if !stale.Stale(nt0, thr) {
		t.Errorf("stale node (2m old, thr 30s) should be stale")
	}
}

func TestNode_placeable_only_when_ready_and_fresh(t *testing.T) {
	thr := 30 * time.Second
	cases := []struct {
		name  string
		state domain.NodeState
		age   time.Duration
		want  bool
	}{
		{"ready+fresh", domain.NodeReady, 5 * time.Second, true},
		{"ready+stale", domain.NodeReady, 5 * time.Minute, false},
		{"draining+fresh", domain.NodeDraining, 5 * time.Second, false},
		{"unhealthy+fresh", domain.NodeUnhealthy, 5 * time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &domain.Node{State: c.state, LastHeartbeat: nt0.Add(-c.age)}
			if got := n.Placeable(nt0, thr); got != c.want {
				t.Errorf("Placeable(%s, age=%s) = %v, want %v", c.state, c.age, got, c.want)
			}
		})
	}
}
