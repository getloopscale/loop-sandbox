package api_test

import (
	"net/http"
	"strings"
	"testing"
)

type autoPauseBody struct {
	ID               string `json:"id"`
	AutoPauseSeconds int64  `json:"auto_pause_seconds"`
}

func TestCreateReturnsTheIdleLimitItWasGiven(t *testing.T) {
	srv := newServer()

	created := decode[autoPauseBody](t, post(srv, "key-a", `{"auto_pause_seconds":60}`), http.StatusCreated)
	fetched := decode[autoPauseBody](t, doGet(srv, created.ID, "key-a"), http.StatusOK)

	if created.AutoPauseSeconds != 60 || fetched.AutoPauseSeconds != 60 {
		t.Errorf("created with %d and fetched with %d, want 60 and 60", created.AutoPauseSeconds, fetched.AutoPauseSeconds)
	}
}

func TestASandboxWithoutAnIdleLimitOmitsTheField(t *testing.T) {
	srv := newServer()

	rec := post(srv, "key-a", `{}`)

	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "auto_pause_seconds") {
		t.Errorf("status = %d body = %s, want 201 without auto_pause_seconds", rec.Code, rec.Body.String())
	}
}

func TestCreateRejectsAnIdleLimitOutOfBoundsWith400(t *testing.T) {
	srv := newServer()
	for _, body := range []string{`{"auto_pause_seconds":9}`, `{"auto_pause_seconds":86401}`, `{"auto_pause_seconds":-1}`} {
		rec := post(srv, "key-a", body)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auto_pause_seconds") {
			t.Errorf("%s: status = %d body = %s, want 400 naming auto_pause_seconds", body, rec.Code, rec.Body.String())
		}
	}
}
