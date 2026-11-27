package metering

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// SignatureHeader carries "sha256=" and the hex HMAC-SHA256 of the request body,
// keyed with the configured secret. It is sent only when a secret is set.
const SignatureHeader = "X-LoopSandbox-Signature"

// httpSinkTimeout bounds one delivery. The outbox keeps the batch when it expires.
const httpSinkTimeout = 10 * time.Second

// usageEvent is one usage record on the wire.
type usageEvent struct {
	EventID  int64  `json:"event_id"`
	TenantID string `json:"tenant_id"`
	Metric   string `json:"metric"`
	Amount   int64  `json:"amount"`
	Period   string `json:"period"`
}

// HTTPSink posts each batch of usage events as JSON to one URL. Any answer that
// is not 2xx is an error, so the outbox keeps the batch and sends it again: the
// receiver gets every event at least once and must ignore an event_id it has
// already seen.
type HTTPSink struct {
	url    string
	secret []byte
	client *http.Client
}

// NewHTTPSink returns a sink that posts to rawURL. A non-empty secret signs each
// body; see SignatureHeader.
func NewHTTPSink(rawURL, secret string) (*HTTPSink, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("usage webhook: %q is not an http or https URL", rawURL)
	}
	return &HTTPSink{url: rawURL, secret: []byte(secret), client: &http.Client{Timeout: httpSinkTimeout}}, nil
}

// Emit posts the batch. An empty batch sends nothing.
func (s *HTTPSink) Emit(ctx context.Context, batch []MeterInput) error {
	if len(batch) == 0 {
		return nil
	}
	events := make([]usageEvent, len(batch))
	for i, m := range batch {
		events[i] = usageEvent(m) // same fields, with the wire names
	}
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return fmt.Errorf("usage webhook: encode %d events: %w", len(batch), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("usage webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if len(s.secret) > 0 {
		mac := hmac.New(sha256.New, s.secret)
		mac.Write(body)
		req.Header.Set(SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("usage webhook: deliver %d events (first id %d): %w", len(batch), batch[0].EventID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("usage webhook: the receiver answered %d for %d events (first id %d)", resp.StatusCode, len(batch), batch[0].EventID)
	}
	return nil
}

// SinkFromEnv returns the usage sink the environment asks for: an HTTPSink when
// LOOPSANDBOX_USAGE_WEBHOOK_URL is set, signed with
// LOOPSANDBOX_USAGE_WEBHOOK_SECRET when that is set too, and otherwise a LogSink.
func SinkFromEnv(getenv func(string) string, logger *slog.Logger) (Sink, error) {
	rawURL, secret := getenv("LOOPSANDBOX_USAGE_WEBHOOK_URL"), getenv("LOOPSANDBOX_USAGE_WEBHOOK_SECRET")
	if rawURL == "" {
		if secret != "" {
			return nil, errors.New("LOOPSANDBOX_USAGE_WEBHOOK_SECRET is set without LOOPSANDBOX_USAGE_WEBHOOK_URL")
		}
		return NewLogSink(logger), nil
	}
	return NewHTTPSink(rawURL, secret)
}
