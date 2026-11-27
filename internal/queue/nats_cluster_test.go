package queue_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/getloopscale/loop-sandbox/internal/queue"
)

// freePort returns a currently-free TCP port on loopback (small TOCTOU window, fine for a
// test harness).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

// embeddedCluster starts an n-node NATS JetStream cluster in-process (real clustering +
// RAFT, no external broker) and returns the node handles + all client URLs. A clustered
// JetStream node requires the FULL route set up front, so cluster ports are pre-picked and
// every node is given all routes.
func embeddedCluster(t *testing.T, n int) ([]*natsserver.Server, []string) {
	t.Helper()
	clusterPorts := make([]int, n)
	routeStrs := make([]string, n)
	for i := 0; i < n; i++ {
		clusterPorts[i] = freePort(t)
		routeStrs[i] = fmt.Sprintf("nats://127.0.0.1:%d", clusterPorts[i])
	}
	routes := natsserver.RoutesFromStr(strings.Join(routeStrs, ","))

	servers := make([]*natsserver.Server, 0, n)
	var urls []string
	for i := 0; i < n; i++ {
		opts := &natsserver.Options{
			ServerName: fmt.Sprintf("n%d", i),
			Host:       "127.0.0.1",
			Port:       -1,
			JetStream:  true,
			StoreDir:   t.TempDir(),
			Cluster:    natsserver.ClusterOpts{Name: "lsb-cluster", Host: "127.0.0.1", Port: clusterPorts[i]},
			Routes:     routes,
		}
		srv, err := natsserver.NewServer(opts)
		if err != nil {
			t.Fatalf("new node %d: %v", i, err)
		}
		if testing.Verbose() {
			srv.ConfigureLogger()
		}
		go srv.Start()
		if !srv.ReadyForConnections(20 * time.Second) {
			t.Fatalf("node %d not ready (cluster bootstrap)", i)
		}
		t.Cleanup(srv.Shutdown)
		servers = append(servers, srv)
		urls = append(urls, srv.ClientURL())
	}
	return servers, urls
}

// jsWithRetry opens JetStream on the cluster and builds a replicated NATSQueue, retrying
// until the meta leader is elected and the R3 stream is created.
func jsWithRetry(t *testing.T, urls []string, replicas int) (*nats.Conn, *queue.NATSQueue) {
	t.Helper()
	nc, err := nats.Connect(urls[0], nats.MaxReconnects(-1), func(o *nats.Options) error {
		o.Servers = urls
		return nil
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		q, err := queue.NewNATSQueue(context.Background(), js, "cell-a", queue.WithReplicas(replicas))
		if err == nil {
			return nc, q
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster never became ready for R%d stream: %v", replicas, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestNATSQueue_survives_node_failure proves per-cell HA: with a replicated (R3) stream on
// a 3-node cluster, killing the stream LEADER does not lose an enqueued job — a surviving
// node still delivers it (RAFT failover).
func TestNATSQueue_survives_node_failure(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster failover test skipped in -short")
	}
	servers, urls := embeddedCluster(t, 3)
	nc, q := jsWithRetry(t, urls, 3)
	ctx := context.Background()

	if err := q.Enqueue(ctx, queue.Job{ID: "ha-job", CellID: "cell-a", Kind: "destroy"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Find the stream leader and kill that node — the hard failover case.
	js, _ := jetstream.New(nc)
	leader := streamLeader(t, js)
	killed := false
	for _, s := range servers {
		if s.Name() == leader {
			s.Shutdown()
			s.WaitForShutdown()
			killed = true
			break
		}
	}
	if !killed {
		t.Fatalf("stream leader %q not found among nodes", leader)
	}

	// Give RAFT a moment to re-elect, then the client (multi-URL) reconnects to a survivor
	// and the job is still deliverable.
	time.Sleep(2 * time.Second)
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	got, ack, err := q.Dequeue(dctx)
	if err != nil {
		t.Fatalf("dequeue after leader kill: %v", err)
	}
	if got.ID != "ha-job" {
		t.Errorf("dequeued %q after failover, want ha-job (data lost on failover)", got.ID)
	}
	_ = ack(ctx)
}

func streamLeader(t *testing.T, js jetstream.JetStream) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s, err := js.Stream(context.Background(), "LSB_JOBS")
		if err == nil {
			if info, ierr := s.Info(context.Background()); ierr == nil && info.Cluster != nil && info.Cluster.Leader != "" {
				return info.Cluster.Leader
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stream leader elected")
		}
		time.Sleep(300 * time.Millisecond)
	}
}
