package metering_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/metering"
)

// receiver is a usage webhook endpoint that records what it gets and answers
// with a programmable status.
type receiver struct {
	mu        sync.Mutex
	status    int
	bodies    [][]byte
	signature string
	srv       *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusNoContent}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.signature = req.Header.Get(metering.SignatureHeader)
		status := r.status
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func sink(t *testing.T, url, secret string) *metering.HTTPSink {
	t.Helper()
	s, err := metering.NewHTTPSink(url, secret)
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	return s
}

var batch = []metering.MeterInput{
	{EventID: 7, TenantID: "tnt_a", Metric: "sandbox_creations", Amount: 1, Period: "2026-11"},
	{EventID: 8, TenantID: "tnt_b", Metric: "executions", Amount: 3, Period: "2026-11"},
}

func TestHTTPSinkPostsTheBatchAsJSON(t *testing.T) {
	r := newReceiver(t)

	err := sink(t, r.srv.URL, "").Emit(context.Background(), batch)

	if err != nil || len(r.bodies) != 1 {
		t.Fatalf("err = %v, requests = %d; want one delivery", err, len(r.bodies))
	}
	want := `{"events":[{"event_id":7,"tenant_id":"tnt_a","metric":"sandbox_creations","amount":1,"period":"2026-11"},` +
		`{"event_id":8,"tenant_id":"tnt_b","metric":"executions","amount":3,"period":"2026-11"}]}`
	if string(r.bodies[0]) != want {
		t.Errorf("body = %s\nwant   %s", r.bodies[0], want)
	}
	if r.signature != "" {
		t.Errorf("signature = %q without a secret, want none", r.signature)
	}
}

func TestHTTPSinkSignsTheBodyWithTheSecret(t *testing.T) {
	r := newReceiver(t)

	if err := sink(t, r.srv.URL, "s3cret").Emit(context.Background(), batch); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(r.bodies[0])
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); r.signature != want {
		t.Errorf("signature = %q, want %q", r.signature, want)
	}
}

func TestHTTPSinkFailsWhenTheReceiverDoesNotAnswer2xx(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusBadRequest, http.StatusInternalServerError} {
		r := newReceiver(t)
		r.status = status

		err := sink(t, r.srv.URL, "").Emit(context.Background(), batch)

		if err == nil || !strings.Contains(err.Error(), "first id 7") {
			t.Errorf("status %d: err = %v, want an error naming the first event", status, err)
		}
	}
}

func TestHTTPSinkFailsWhenTheReceiverIsUnreachable(t *testing.T) {
	r := newReceiver(t)
	url := r.srv.URL
	r.srv.Close()

	if err := sink(t, url, "").Emit(context.Background(), batch); err == nil {
		t.Error("delivery to a closed endpoint returned no error")
	}
}

func TestHTTPSinkSendsNothingForAnEmptyBatch(t *testing.T) {
	r := newReceiver(t)

	err := sink(t, r.srv.URL, "").Emit(context.Background(), nil)

	if err != nil || len(r.bodies) != 0 {
		t.Errorf("err = %v, requests = %d; want no request", err, len(r.bodies))
	}
}

func TestAFailedDeliveryIsSentAgainAndNothingIsLost(t *testing.T) {
	r := newReceiver(t)
	r.status = http.StatusServiceUnavailable
	box := &memOutbox{}
	for i := int64(1); i <= 5; i++ {
		box.events = append(box.events, metering.MeterEvent{ID: i, TenantID: "tnt_a", Metric: "executions", Amount: i, Period: "2026-11"})
	}
	emitter := metering.NewEmitter(box, sink(t, r.srv.URL, ""), 2)
	if _, err := emitter.Drain(context.Background()); err == nil {
		t.Fatal("draining to a failing receiver returned no error")
	}

	r.mu.Lock()
	r.status, r.bodies = http.StatusOK, nil
	r.mu.Unlock()
	n, err := emitter.Drain(context.Background())

	var total int64
	seen := map[int64]bool{}
	for _, body := range r.bodies {
		var got struct {
			Events []struct {
				EventID int64 `json:"event_id"`
				Amount  int64 `json:"amount"`
			} `json:"events"`
		}
		if jerr := json.Unmarshal(body, &got); jerr != nil {
			t.Fatalf("body %s: %v", body, jerr)
		}
		for _, e := range got.Events {
			seen[e.EventID] = true
			total += e.Amount
		}
	}
	if err != nil || n != 5 || len(seen) != 5 || total != 15 {
		t.Errorf("err = %v, drained = %d, distinct events = %d, amount = %d; want 5 events adding up to 15", err, n, len(seen), total)
	}
}

func TestNewHTTPSinkRefusesAURLThatIsNotHTTP(t *testing.T) {
	for _, raw := range []string{"collector:9000", "ftp://collector/usage", "http://", ""} {
		if _, err := metering.NewHTTPSink(raw, ""); err == nil {
			t.Errorf("NewHTTPSink(%q) returned no error", raw)
		}
	}
}

func TestSinkFromEnvChoosesTheSinkTheEnvironmentAsksFor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	logSink, logErr := metering.SinkFromEnv(env(nil), logger)
	httpSink, httpErr := metering.SinkFromEnv(env(map[string]string{"LOOPSANDBOX_USAGE_WEBHOOK_URL": "https://billing.example.org/usage"}), logger)
	_, orphanErr := metering.SinkFromEnv(env(map[string]string{"LOOPSANDBOX_USAGE_WEBHOOK_SECRET": "x"}), logger)
	_, badErr := metering.SinkFromEnv(env(map[string]string{"LOOPSANDBOX_USAGE_WEBHOOK_URL": "nope"}), logger)

	if _, ok := logSink.(*metering.LogSink); !ok || logErr != nil {
		t.Errorf("without the variable: %T, %v; want the log sink", logSink, logErr)
	}
	if _, ok := httpSink.(*metering.HTTPSink); !ok || httpErr != nil {
		t.Errorf("with the variable: %T, %v; want the HTTP sink", httpSink, httpErr)
	}
	if orphanErr == nil || badErr == nil {
		t.Errorf("a secret without a URL = %v, a bad URL = %v; want both refused", orphanErr, badErr)
	}
}
