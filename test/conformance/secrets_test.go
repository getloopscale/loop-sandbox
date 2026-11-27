//go:build conformance

package conformance

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var secretSeq atomic.Int64

// secretName returns a name no other case of this run or of an earlier run uses.
func secretName() string {
	return fmt.Sprintf("LSB_CONFORMANCE_%d_%d", time.Now().UnixNano(), secretSeq.Add(1))
}

// newSecret creates a secret and deletes it when the test ends.
func newSecret(t *testing.T, value string) string {
	t.Helper()
	name := secretName()
	call(t, apiKey, http.MethodPut, "/v1/secrets/"+name, map[string]any{"value": value}).want(t, http.StatusCreated, "put secret")
	t.Cleanup(func() {
		resp := call(t, apiKey, http.MethodDelete, "/v1/secrets/"+name, nil)
		if resp.status != http.StatusNoContent && resp.status != http.StatusNotFound {
			t.Errorf("delete secret %s: status = %d: %s", name, resp.status, resp.body)
		}
	})
	return name
}

func TestSecrets(t *testing.T) {
	probe := func(t *testing.T) *response { return call(t, apiKey, http.MethodGet, "/v1/secrets", nil) }
	supported["secrets"] = probe(t).status != http.StatusNotImplemented
	capability(t, "secrets", probe, map[string]func(t *testing.T){
		"a_secret_is_an_environment_variable_of_a_sandbox_that_names_it": func(t *testing.T) {
			name := newSecret(t, "value-of-the-secret")
			with := newSandboxWith(t, map[string]any{"secrets": []string{name}})
			without := newSandbox(t, shellImage)

			inWith := sh(t, with.ID, `printf %s "$`+name+`"`)
			inWithout := sh(t, without.ID, `printf %s "$`+name+`"`)

			if inWith.Stdout != "value-of-the-secret" || inWithout.Stdout != "" {
				t.Errorf("with the secret: %q; without it: %q; want the value and nothing", inWith.Stdout, inWithout.Stdout)
			}
		},
		"a_background_process_receives_the_secret": func(t *testing.T) {
			if !supported["processes"] {
				t.Skip("the driver has no background processes")
			}
			name := newSecret(t, "value-for-a-process")
			sb := newSandboxWith(t, map[string]any{"secrets": []string{name}})
			var started struct {
				PID int `json:"pid"`
			}
			call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/processes",
				map[string]any{"cmd": []string{"sh", "-c", `printf %s "$` + name + `"`}}).want(t, http.StatusCreated, "start process").json(t, &started)

			var out struct {
				Stdout string `json:"stdout"`
			}
			call(t, apiKey, http.MethodGet, fmt.Sprintf("/v1/sandboxes/%s/processes/%d/output?wait_ms=10000", sb.ID, started.PID), nil).
				want(t, http.StatusOK, "process output").json(t, &out)

			if out.Stdout != "value-for-a-process" {
				t.Errorf("the process printed %q, want the value of the secret", out.Stdout)
			}
		},
		"a_replaced_value_reaches_the_next_command": func(t *testing.T) {
			name := newSecret(t, "first")
			sb := newSandboxWith(t, map[string]any{"secrets": []string{name}})

			call(t, apiKey, http.MethodPut, "/v1/secrets/"+name, map[string]any{"value": "second"}).want(t, http.StatusOK, "replace secret")

			if out := sh(t, sb.ID, `printf %s "$`+name+`"`); out.Stdout != "second" {
				t.Errorf("the command reads %q after the replace, want second", out.Stdout)
			}
		},
		"no_response_returns_the_value": func(t *testing.T) {
			const value = "sentinel-that-must-not-come-back-91c2"
			name := secretName()
			put := call(t, apiKey, http.MethodPut, "/v1/secrets/"+name, map[string]any{"value": value})
			t.Cleanup(func() { call(t, apiKey, http.MethodDelete, "/v1/secrets/"+name, nil) })
			create := call(t, apiKey, http.MethodPost, "/v1/sandboxes", map[string]any{"image": shellImage, "ttl_seconds": 600, "secrets": []string{name}})
			var sb sandbox
			create.want(t, http.StatusCreated, "create").json(t, &sb)
			t.Cleanup(func() { destroy(t, sb.ID) })

			responses := map[string]*response{
				"put secret":     put,
				"create sandbox": create,
				"list secrets":   call(t, apiKey, http.MethodGet, "/v1/secrets", nil),
				"get sandbox":    call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+sb.ID, nil),
				"list sandboxes": call(t, apiKey, http.MethodGet, "/v1/sandboxes", nil),
				"exec":           call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions", map[string]any{"cmd": []string{"true"}}),
			}

			for what, resp := range responses {
				if strings.Contains(string(resp.body), value) || strings.Contains(fmt.Sprint(resp.header), value) {
					t.Errorf("%s returned the value of the secret: %s", what, resp.body)
				}
			}
		},
		"a_sandbox_cannot_be_created_with_a_secret_that_does_not_exist": func(t *testing.T) {
			resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes", map[string]any{"image": shellImage, "secrets": []string{secretName()}})

			resp.want(t, http.StatusBadRequest, "create with an unknown secret")
		},
		"commands_answer_409_after_the_secret_is_deleted": func(t *testing.T) {
			name := newSecret(t, "v")
			sb := newSandboxWith(t, map[string]any{"secrets": []string{name}})
			call(t, apiKey, http.MethodDelete, "/v1/secrets/"+name, nil).want(t, http.StatusNoContent, "delete secret")

			resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sb.ID+"/executions", map[string]any{"cmd": []string{"true"}})

			resp.want(t, http.StatusConflict, "exec after the secret was deleted")
		},
		"another_tenant_cannot_see_use_or_delete_a_secret": func(t *testing.T) {
			name := newSecret(t, "v")

			list := call(t, otherAPIKey, http.MethodGet, "/v1/secrets", nil)
			create := call(t, otherAPIKey, http.MethodPost, "/v1/sandboxes", map[string]any{"image": shellImage, "secrets": []string{name}})
			del := call(t, otherAPIKey, http.MethodDelete, "/v1/secrets/"+name, nil)

			create.want(t, http.StatusBadRequest, "create a sandbox as the other tenant")
			del.want(t, http.StatusNotFound, "delete as the other tenant")
			if strings.Contains(string(list.body), name) {
				t.Errorf("the other tenant's list shows the secret: %s", list.body)
			}
		},
	})
}
