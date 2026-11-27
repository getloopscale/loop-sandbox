package filestore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/filestore"
)

// TestPut_rejects_oversize_artifact proves the negative case (rules/testing.md §4.1):
// a source larger than MaxArtifactBytes fails-fast with a typed error instead of being
// silently truncated to a "successful" corrupt object.
func TestPut_rejects_oversize_artifact(t *testing.T) {
	client, bucket := fakeS3(t)
	store := filestore.NewS3(client, bucket)

	// A reader that yields MaxArtifactBytes+1 bytes without allocating them all.
	oversize := io.LimitReader(neverEnding{}, filestore.MaxArtifactBytes+1)
	err := store.Put(context.Background(), "tenants/t/sandboxes/s/big", oversize)
	if !errors.Is(err, filestore.ErrArtifactTooLarge) {
		t.Fatalf("Put(oversize) err = %v, want ErrArtifactTooLarge", err)
	}
}

// Boundary edge case: a source of EXACTLY the cap uploads successfully.
func TestPut_accepts_artifact_at_exact_cap(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates 1 GiB; skipped in -short")
	}
	client, bucket := fakeS3(t)
	store := filestore.NewS3(client, bucket)
	atCap := io.LimitReader(neverEnding{}, filestore.MaxArtifactBytes)
	if err := store.Put(context.Background(), "tenants/t/sandboxes/s/atcap", atCap); err != nil {
		t.Fatalf("Put(atCap) err = %v, want nil", err)
	}
}

// TestPresignGet_clamps_ttl: an over-long or non-positive TTL is clamped to
// MaxPresignTTL so a caller cannot mint a long-lived URL. We assert via the X-Amz-Expires
// query param embedded in the presigned URL.
func TestPresignGet_clamps_ttl(t *testing.T) {
	client, bucket := fakeS3(t)
	store := filestore.NewS3(client, bucket)
	if err := store.Put(context.Background(), "k", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	maxSecs := int(filestore.MaxPresignTTL.Seconds())

	for _, ttl := range []time.Duration{24 * time.Hour, 0, -time.Second} {
		url, err := store.PresignArtifact(context.Background(), "t", "s", "f", ttl)
		if err != nil {
			t.Fatalf("PresignArtifact(%v): %v", ttl, err)
		}
		if !strings.Contains(url, "X-Amz-Expires="+itoa(maxSecs)) {
			t.Errorf("ttl %v: presigned URL expires not clamped to %ds; url=%s", ttl, maxSecs, url)
		}
	}
}

// neverEnding yields an infinite stream of 'a' bytes (bounded by an io.LimitReader).
type neverEnding struct{}

func (neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
