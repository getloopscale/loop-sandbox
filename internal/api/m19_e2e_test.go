package api_test

import (
	"net/http"
	"testing"
)

// M19: template and gpu are mutually exclusive — a GPU template would silently drop the GPU
// (bypassing the premium quota gate), so the API rejects the combination (review H2).
func TestE2E_template_and_gpu_mutually_exclusive(t *testing.T) {
	srv := newServer()
	rec := post(srv, "key-a", `{"template":"tpl_x","gpu":"a100"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("template+gpu = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
