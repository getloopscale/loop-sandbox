package ratelimit_test

import (
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/ratelimit"
)

var start = time.Unix(1_700_000_000, 0)

func TestAllowsTheBurstThenRefusesWithTheWaitUntilTheNextToken(t *testing.T) {
	l := ratelimit.New(2, 3)
	for i := range 3 {
		if ok, _ := l.Allow("k", start); !ok {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}

	ok, retry := l.Allow("k", start)

	if ok || retry != 500*time.Millisecond {
		t.Errorf("fourth request: ok=%v retry=%s, want refused with 500ms", ok, retry)
	}
}

func TestARefusedRequestDoesNotPostponeTheNextAllowedOne(t *testing.T) {
	l := ratelimit.New(1, 1)
	l.Allow("k", start)
	for range 50 {
		l.Allow("k", start.Add(100*time.Millisecond))
	}

	ok, _ := l.Allow("k", start.Add(time.Second))

	if !ok {
		t.Error("the request one second later was refused: refused requests consumed tokens")
	}
}

func TestKeysDoNotShareABucket(t *testing.T) {
	l := ratelimit.New(1, 1)
	l.Allow("a", start)

	okA, _ := l.Allow("a", start)
	okB, _ := l.Allow("b", start)

	if okA || !okB {
		t.Errorf("a=%v b=%v, want a refused and b allowed", okA, okB)
	}
}

func TestANilLimiterAllowsEverything(t *testing.T) {
	l := ratelimit.New(0, 10)

	for range 1000 {
		if ok, _ := l.Allow("k", start); !ok {
			t.Fatal("a disabled limiter refused a request")
		}
	}
}

func TestRetryAfterSecondsRoundsUpAndIsAtLeastOne(t *testing.T) {
	cases := map[time.Duration]int{10 * time.Millisecond: 1, time.Second: 1, 1100 * time.Millisecond: 2, 0: 1}
	for d, want := range cases {
		if got := ratelimit.RetryAfterSeconds(d); got != want {
			t.Errorf("RetryAfterSeconds(%s) = %d, want %d", d, got, want)
		}
	}
}
