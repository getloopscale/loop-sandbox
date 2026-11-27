package api_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

// secretValue is a value no other part of a test produces, so finding it
// anywhere means the secret leaked.
const secretValue = "s3cr3t-sentinel-7f3a9c"

func putSecret(t *testing.T, srv http.Handler, key, name, value string) *httptest.ResponseRecorder {
	t.Helper()
	return call(srv, http.MethodPut, "/v1/secrets/"+name, key, fmt.Sprintf(`{"value":%q}`, value))
}

// printenv runs printenv NAME in the sandbox and returns the response.
func printenv(srv http.Handler, sandboxID, name string) *httptest.ResponseRecorder {
	return call(srv, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/executions", "key-a", fmt.Sprintf(`{"cmd":["printenv",%q]}`, name))
}

type execBody struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
}

func TestASecretIsAnEnvironmentVariableOfTheCommandsOfASandboxThatNamesIt(t *testing.T) {
	srv := newServer()
	if rec := putSecret(t, srv, "key-secrets", "API_TOKEN", secretValue); rec.Code != http.StatusCreated {
		t.Fatalf("put secret = %d: %s", rec.Code, rec.Body.String())
	}
	with := createWith(t, srv, "key-a", `{"secrets":["API_TOKEN"]}`)
	without := createWith(t, srv, "key-a", `{}`)

	inWith := decode[execBody](t, printenv(srv, with.ID, "API_TOKEN"), http.StatusOK)
	inWithout := decode[execBody](t, printenv(srv, without.ID, "API_TOKEN"), http.StatusOK)

	if inWith.Stdout != secretValue+"\n" || inWithout.ExitCode == 0 {
		t.Errorf("with the secret: %q; without it: exit %d; want the value and a non-zero exit", inWith.Stdout, inWithout.ExitCode)
	}
}

func TestReplacingASecretReachesTheNextCommandOfAnExistingSandbox(t *testing.T) {
	srv := newServer()
	putSecret(t, srv, "key-secrets", "API_TOKEN", "first")
	sb := createWith(t, srv, "key-a", `{"secrets":["API_TOKEN"]}`)

	replaced := putSecret(t, srv, "key-secrets", "API_TOKEN", "second")

	got := decode[execBody](t, printenv(srv, sb.ID, "API_TOKEN"), http.StatusOK)
	if replaced.Code != http.StatusOK || got.Stdout != "second\n" {
		t.Errorf("replace = %d and the command reads %q; want 200 and second", replaced.Code, got.Stdout)
	}
}

func TestNoResponseLogOrSpanContainsTheValueOfASecret(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	previousTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(previousTP); _ = tp.Shutdown(context.Background()) })
	srv := newServer()
	var responses []string
	do := func(method, path, key, body string) *httptest.ResponseRecorder {
		rec := call(srv, method, path, key, body)
		responses = append(responses, fmt.Sprintf("%s %s -> %d %v %s", method, path, rec.Code, rec.Header(), rec.Body.String()))
		return rec
	}

	do(http.MethodPut, "/v1/secrets/API_TOKEN", "key-secrets", fmt.Sprintf(`{"value":%q}`, secretValue))
	do(http.MethodPut, "/v1/secrets/API_TOKEN", "key-secrets", fmt.Sprintf(`{"value":%q}`, secretValue)) // a replace
	do(http.MethodGet, "/v1/secrets", "key-secrets", "")
	sb := decode[sandboxBody](t, do(http.MethodPost, "/v1/sandboxes", "key-a", `{"secrets":["API_TOKEN"]}`), http.StatusCreated)
	do(http.MethodGet, "/v1/sandboxes/"+sb.ID, "key-a", "")
	do(http.MethodGet, "/v1/sandboxes", "key-a", "")
	do(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions", "key-a", `{"cmd":["echo","no secret here"]}`)
	do(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/fork", "key-a", "")
	do(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/snapshots", "key-a", `{"name":"with-secret"}`)
	do(http.MethodPut, "/v1/secrets/TOO_LONG", "key-secrets", fmt.Sprintf(`{"value":%q}`, secretValue+strings.Repeat("x", domain.MaxEnvValueLen)))
	do(http.MethodPut, "/v1/secrets/bad-name", "key-secrets", fmt.Sprintf(`{"value":%q}`, secretValue))
	do(http.MethodPut, "/v1/secrets/API_TOKEN", "key-a", fmt.Sprintf(`{"value":%q}`, secretValue)) // refused: no scope
	do(http.MethodDelete, "/v1/secrets/API_TOKEN", "key-secrets", "")
	do(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions", "key-a", `{"cmd":["echo","after delete"]}`)

	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush spans: %v", err)
	}
	for _, r := range responses {
		if strings.Contains(r, secretValue) {
			t.Errorf("a response contains the value: %s", r)
		}
	}
	if strings.Contains(logs.String(), secretValue) {
		t.Errorf("the log contains the value: %s", logs.String())
	}
	for _, span := range spans.Ended() {
		if dump := fmt.Sprintf("%s %v %v %v", span.Name(), span.Attributes(), span.Events(), span.Status()); strings.Contains(dump, secretValue) {
			t.Errorf("a span contains the value: %s", dump)
		}
	}
	if len(spans.Ended()) == 0 {
		t.Error("no span was recorded, so the span check proved nothing")
	}
}

func TestListSecretsReturnsNamesAndDatesOrderedByName(t *testing.T) {
	srv := newServer()
	putSecret(t, srv, "key-secrets", "ZETA", "z")
	putSecret(t, srv, "key-secrets", "ALPHA", "a")

	rec := call(srv, http.MethodGet, "/v1/secrets", "key-secrets", "")

	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Index(body, "ALPHA") > strings.Index(body, "ZETA") || !strings.Contains(body, "created_at") || strings.Contains(body, `"value"`) {
		t.Errorf("status = %d body = %s; want ALPHA before ZETA, with dates and no value", rec.Code, body)
	}
}

func TestSecretRoutesNeedTheSecretsScope(t *testing.T) {
	srv := newServer()
	putSecret(t, srv, "key-secrets", "API_TOKEN", "v")
	cases := map[string]struct{ method, path, body string }{
		"list":   {http.MethodGet, "/v1/secrets", ""},
		"put":    {http.MethodPut, "/v1/secrets/OTHER", `{"value":"v"}`},
		"delete": {http.MethodDelete, "/v1/secrets/API_TOKEN", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, c.method, c.path, "key-a", c.body)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 for a key without secrets:manage (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAnotherTenantCannotSeeUseOrDeleteASecret(t *testing.T) {
	srv := newServer()
	putSecret(t, srv, "key-secrets", "API_TOKEN", secretValue)

	list := call(srv, http.MethodGet, "/v1/secrets", "key-secrets-b", "")
	create := call(srv, http.MethodPost, "/v1/sandboxes", "key-b", `{"secrets":["API_TOKEN"]}`)
	del := call(srv, http.MethodDelete, "/v1/secrets/API_TOKEN", "key-secrets-b", "")

	if strings.Contains(list.Body.String(), "API_TOKEN") || create.Code != http.StatusBadRequest || del.Code != http.StatusNotFound {
		t.Errorf("list = %s, create = %d, delete = %d; want an empty list, 400 and 404", list.Body.String(), create.Code, del.Code)
	}
	if rec := call(srv, http.MethodGet, "/v1/secrets", "key-secrets", ""); !strings.Contains(rec.Body.String(), "API_TOKEN") {
		t.Errorf("the secret is gone for its owner: %s", rec.Body.String())
	}
}

func TestCommandsOfASandboxAnswer409AfterItsSecretIsDeleted(t *testing.T) {
	srv := newServer()
	putSecret(t, srv, "key-secrets", "API_TOKEN", "v")
	sb := createWith(t, srv, "key-a", `{"secrets":["API_TOKEN"]}`)
	call(srv, http.MethodDelete, "/v1/secrets/API_TOKEN", "key-secrets", "")

	rec := printenv(srv, sb.ID, "API_TOKEN")

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "secret_unavailable") || !strings.Contains(rec.Body.String(), "API_TOKEN") {
		t.Errorf("status = %d body = %s; want 409 secret_unavailable naming API_TOKEN", rec.Code, rec.Body.String())
	}
}

func TestPutSecretRejectsInvalidRequests(t *testing.T) {
	srv := newServer()
	cases := map[string]struct{ name, body string }{
		"a name with a hyphen":    {"bad-name", `{"value":"v"}`},
		"a name starting a digit": {"1TOKEN", `{"value":"v"}`},
		"an empty value":          {"TOKEN", `{"value":""}`},
		"no value":                {"TOKEN", `{}`},
		"a value too long":        {"TOKEN", fmt.Sprintf(`{"value":%q}`, strings.Repeat("x", domain.MaxEnvValueLen+1))},
		"a NUL byte":              {"TOKEN", `{"value":"a\u0000b"}`},
		"malformed JSON":          {"TOKEN", `{"value":`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, http.MethodPut, "/v1/secrets/"+c.name, "key-secrets", c.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateRejectsSecretsItCannotGive(t *testing.T) {
	srv := newServer()
	putSecret(t, srv, "key-secrets", "API_TOKEN", "v")
	many := `"S0"`
	for i := 1; i <= domain.MaxSecretsPerSandbox; i++ {
		many += fmt.Sprintf(`,"S%d"`, i)
	}
	for name, body := range map[string]string{
		"a secret that does not exist": `{"secrets":["MISSING"]}`,
		"a repeated name":              `{"secrets":["API_TOKEN","API_TOKEN"]}`,
		"an invalid name":              `{"secrets":["bad-name"]}`,
		"more than the limit":          `{"secrets":[` + many + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, http.MethodPost, "/v1/sandboxes", "key-a", body)

			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "secrets") {
				t.Errorf("status = %d body = %s, want 400 naming secrets", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestATenantCannotKeepMoreSecretsThanTheLimit(t *testing.T) {
	srv := newServer()
	for i := range domain.MaxSecretsPerTenant {
		if rec := putSecret(t, srv, "key-secrets", fmt.Sprintf("S%d", i), "v"); rec.Code != http.StatusCreated {
			t.Fatalf("put %d = %d", i, rec.Code)
		}
	}

	over := putSecret(t, srv, "key-secrets", "ONE_TOO_MANY", "v")
	replace := putSecret(t, srv, "key-secrets", "S0", "new")

	if over.Code != http.StatusConflict || !strings.Contains(over.Body.String(), "too_many_secrets") || replace.Code != http.StatusOK {
		t.Errorf("over = %d %s, replace = %d; want 409 too_many_secrets and 200", over.Code, over.Body.String(), replace.Code)
	}
}

func TestSecretRoutesAnswer501WithoutAKey(t *testing.T) {
	srv := newServerScanner(stubScanner{ok: true}) // a server wired without secrets

	put := putSecret(t, srv, "key-secrets", "API_TOKEN", "v")
	create := call(srv, http.MethodPost, "/v1/sandboxes", "key-a", `{"secrets":["API_TOKEN"]}`)

	if put.Code != http.StatusNotImplemented || create.Code != http.StatusNotImplemented {
		t.Errorf("put = %d, create = %d; want 501 and 501", put.Code, create.Code)
	}
}
