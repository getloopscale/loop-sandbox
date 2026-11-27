// Package filestore is the object-storage FileTransport for large sandbox artifacts
// (M11, B6): rather than streaming big files through the control plane, they live in
// S3-compatible object storage and are fetched by the client via a short-lived
// pre-signed URL. Keys are tenant-prefixed so one tenant's objects are namespaced
// away from another's.
package filestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// MaxArtifactBytes caps a single artifact upload. A source larger than this is
// REJECTED (typed error) — never silently truncated (Rule 8: fail-fast, no silent
// corruption).
const MaxArtifactBytes = 1 << 30 // 1 GiB

// MaxPresignTTL clamps how long a pre-signed URL may live, so a caller can never mint
// a long-lived credential-in-a-URL.
const MaxPresignTTL = 15 * time.Minute

// ErrArtifactTooLarge is returned when an upload exceeds MaxArtifactBytes.
var ErrArtifactTooLarge = errors.New("filestore: artifact exceeds max size")

// FileStore stores an artifact and issues a time-limited download URL for it.
type FileStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// TenantKey builds the tenant-namespaced object key for a sandbox artifact, so one
// tenant can never guess or collide with another's key. (Parallel to loop-runtime's
// storage.KeyForTenant convention — kept as a separate impl by design: LoopSandbox
// is its own module sharing only LoopScale contracts, and its keys are sandbox-scoped,
// not just tenant-scoped.)
func TenantKey(tenantID, sandboxID, path string) string {
	return fmt.Sprintf("tenants/%s/sandboxes/%s/%s", tenantID, sandboxID, path)
}

// S3 implements FileStore against any S3-compatible endpoint (AWS S3 or LocalStack).
type S3 struct {
	client   *s3.Client
	uploader *manager.Uploader
	presign  *s3.PresignClient
	bucket   string
}

// NewS3 wraps an s3.Client for the given bucket.
func NewS3(client *s3.Client, bucket string) *S3 {
	return &S3{
		client:   client,
		uploader: manager.NewUploader(client), // streams in parts — no full-object buffer
		presign:  s3.NewPresignClient(client),
		bucket:   bucket,
	}
}

// Put streams the object under key. The upload is size-capped: a source larger than
// MaxArtifactBytes fails with ErrArtifactTooLarge instead of being silently truncated.
// The multipart uploader streams in parts, so a large artifact is never buffered whole
// in memory.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	capped := &cappedReader{r: r, remaining: MaxArtifactBytes}
	_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   capped,
	})
	if capped.tripped {
		return ErrArtifactTooLarge
	}
	return err
}

// PresignArtifact builds the tenant-namespaced key and returns a pre-signed GET URL.
// This is the high-level entry the application layer calls (it owns key construction so
// callers never assemble raw storage keys).
func (s *S3) PresignArtifact(ctx context.Context, tenantID, sandboxID, path string, ttl time.Duration) (string, error) {
	return s.PresignGet(ctx, TenantKey(tenantID, sandboxID, path), ttl)
}

// PresignGet returns a pre-signed GET URL valid for ttl (clamped to MaxPresignTTL).
func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > MaxPresignTTL {
		ttl = MaxPresignTTL
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// cappedReader lets at most `remaining` bytes through; on the first read that would
// exceed the cap it stops and marks `tripped` so Put can return a typed error rather
// than silently truncating (io.LimitReader would swallow the overflow as EOF).
type cappedReader struct {
	r         io.Reader
	remaining int64
	tripped   bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	// Allow one extra byte so a source of exactly the cap succeeds but cap+1 trips.
	if int64(len(p)) > c.remaining+1 {
		p = p[:c.remaining+1]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining < 0 {
		c.tripped = true
		return 0, ErrArtifactTooLarge
	}
	return n, err
}
