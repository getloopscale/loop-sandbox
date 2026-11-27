package filestore_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/getloopscale/loop-sandbox/internal/filestore"
)

// fakeS3 stands up an in-process server speaking the REAL S3 wire protocol, so the
// test exercises the actual aws-sdk-go-v2 client + presigner end to end — hermetic,
// no docker/LocalStack needed.
func fakeS3(t *testing.T) (*s3.Client, string) {
	t.Helper()
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewServer(faker.Server())
	t.Cleanup(srv.Close)

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true, // path-style for the local endpoint
	})
	const bucket = "lsb-artifacts"
	if _, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	return client, bucket
}

// TestS3_put_then_presigned_download proves the FileTransport (B6): an artifact is
// uploaded to object storage and fetched back through a short-lived pre-signed GET
// URL — identical bytes, without streaming through the control plane.
func TestS3_put_then_presigned_download(t *testing.T) {
	client, bucket := fakeS3(t)
	store := filestore.NewS3(client, bucket)
	ctx := context.Background()

	key := filestore.TenantKey("tenant-a", "sbx-1", "big-output.bin")
	payload := bytes.Repeat([]byte("loopsandbox-artifact-"), 4096) // ~90 KiB
	if err := store.Put(ctx, key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	url, err := store.PresignGet(ctx, key, 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}

	// Download via the pre-signed URL over plain HTTP (no auth headers → the signature
	// in the URL is what authorises the GET).
	resp, err := http.Get(url) //nolint:gosec // URL is the presigned, test-local endpoint
	if err != nil {
		t.Fatalf("GET presigned: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, payload) {
		t.Errorf("downloaded %d bytes, want %d identical", len(got), len(payload))
	}
}

// Tenant isolation: two tenants' artifacts live under distinct keys; a presigned URL
// for tenant-a's key never yields tenant-b's object.
func TestS3_tenant_key_isolation(t *testing.T) {
	client, bucket := fakeS3(t)
	store := filestore.NewS3(client, bucket)
	ctx := context.Background()

	if err := store.Put(ctx, filestore.TenantKey("tenant-a", "s", "f"), bytes.NewReader([]byte("A"))); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := store.Put(ctx, filestore.TenantKey("tenant-b", "s", "f"), bytes.NewReader([]byte("B"))); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	url, _ := store.PresignGet(ctx, filestore.TenantKey("tenant-a", "s", "f"), time.Minute)
	resp, err := http.Get(url) //nolint:gosec // presigned test URL
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "A" {
		t.Errorf("tenant-a presigned URL returned %q, want 'A' (isolation)", got)
	}
}
