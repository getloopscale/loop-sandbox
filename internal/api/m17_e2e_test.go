package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// stubScanner is a fake vulnerability scanner: ok=true means every image passes the gate.
type stubScanner struct{ ok bool }

func (s stubScanner) Scan(context.Context, string) (bool, error) { return s.ok, nil }

const pinnedImage = "python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestE2E_template_register_and_provision: register a digest-pinned template, then provision a
// sandbox FROM it — the E2B template promise (M17).
func TestE2E_template_register_and_provision(t *testing.T) {
	srv := newServer()
	rec := req(srv, http.MethodPost, "/v1/templates", "key-a",
		[]byte(`{"name":"mydata","image":"`+pinnedImage+`","version":"1"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register template = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var tpl struct {
		ID     string `json:"id"`
		Digest string `json:"digest"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tpl)
	if tpl.Digest == "" {
		t.Errorf("registered template has no digest (digest-pinning not recorded)")
	}
	// Provision a sandbox from the template.
	rec = post(srv, "key-a", `{"template":"`+tpl.ID+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("provision from template = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var sb struct {
		Image string `json:"image"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sb)
	if sb.Image != pinnedImage {
		t.Errorf("sandbox image = %q, want the template's pinned image", sb.Image)
	}
}

// TestE2E_template_requires_digest_pin: an unpinned image is rejected (400).
func TestE2E_template_requires_digest_pin(t *testing.T) {
	srv := newServer()
	rec := req(srv, http.MethodPost, "/v1/templates", "key-a",
		[]byte(`{"name":"loose","image":"python:3.12-slim"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unpinned template = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestE2E_template_cross_tenant_is_404: tenant B cannot see/provision tenant A's PRIVATE template.
func TestE2E_template_cross_tenant_is_404(t *testing.T) {
	srv := newServer()
	rec := req(srv, http.MethodPost, "/v1/templates", "key-a",
		[]byte(`{"name":"secret","image":"`+pinnedImage+`"}`))
	var tpl struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tpl)
	if rec2 := req(srv, http.MethodGet, "/v1/templates/"+tpl.ID, "key-b", nil); rec2.Code != http.StatusNotFound {
		t.Errorf("cross-tenant GET template = %d, want 404", rec2.Code)
	}
	// tenant-b provisioning from tenant-a's private template → not-found.
	if rec3 := post(srv, "key-b", `{"template":"`+tpl.ID+`"}`); rec3.Code != http.StatusNotFound {
		t.Errorf("cross-tenant provision = %d, want 404; body=%s", rec3.Code, rec3.Body.String())
	}
}

// TestE2E_template_public_requires_scan_pass: publishing public runs the scan gate.
func TestE2E_template_public_scan_gate(t *testing.T) {
	// A server whose scanner FAILS everything: publishing public is rejected (422).
	srv := newServerScanner(stubScanner{ok: false})
	rec := req(srv, http.MethodPost, "/v1/templates", "key-a",
		[]byte(`{"name":"pub","image":"`+pinnedImage+`","public":true}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("public register with failing scan = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	// A private template with the same (failing-scan) server still registers (no gate).
	if rec2 := req(srv, http.MethodPost, "/v1/templates", "key-a",
		[]byte(`{"name":"priv","image":"`+pinnedImage+`"}`)); rec2.Code != http.StatusCreated {
		t.Errorf("private register = %d, want 201; body=%s", rec2.Code, rec2.Body.String())
	}
}
