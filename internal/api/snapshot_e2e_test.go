package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

type snapshotBody struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SandboxID string `json:"sandbox_id"`
	SizeBytes int64  `json:"size_bytes"`
}

func snapshotOf(t *testing.T, srv http.Handler, sandboxID, name string) snapshotBody {
	t.Helper()
	rec := call(srv, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/snapshots", "key-a", fmt.Sprintf(`{"name":%q}`, name))
	return decode[snapshotBody](t, rec, http.StatusCreated)
}

func TestASandboxStartedFromASnapshotHasTheFilesOfTheSnapshotAndTheSourceKeepsRunning(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{"metadata":{"team":"search"},"env":{"MODE":"test"}}`)
	putFile(t, srv, source.ID, "/workspace/state.txt", "at-snapshot")
	snap := snapshotOf(t, srv, source.ID, "after-setup")
	putFile(t, srv, source.ID, "/workspace/state.txt", "after-snapshot")

	restored := createWith(t, srv, "key-a", fmt.Sprintf(`{"snapshot":%q}`, snap.ID))

	inRestored := call(srv, http.MethodGet, "/v1/sandboxes/"+restored.ID+"/files/workspace/state.txt", "key-a", "")
	inSource := call(srv, http.MethodGet, "/v1/sandboxes/"+source.ID+"/files/workspace/state.txt", "key-a", "")
	if inRestored.Body.String() != "at-snapshot" || inSource.Body.String() != "after-snapshot" {
		t.Errorf("restored reads %q and the source reads %q; want at-snapshot and after-snapshot", inRestored.Body.String(), inSource.Body.String())
	}
	if restored.ID == source.ID || restored.State != "running" || restored.Metadata["team"] != "search" || restored.Env["MODE"] != "test" {
		t.Errorf("restored = %+v, want a new running sandbox with the metadata and env of the source", restored)
	}
	if got := decode[sandboxBody](t, call(srv, http.MethodGet, "/v1/sandboxes/"+source.ID, "key-a", ""), http.StatusOK); got.State != "running" {
		t.Errorf("the source is %s after the snapshot, want running", got.State)
	}
}

func TestASnapshotOutlivesTheSandboxItWasTakenFrom(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, source.ID, "/workspace/a.txt", "kept")
	snap := snapshotOf(t, srv, source.ID, "kept")
	call(srv, http.MethodDelete, "/v1/sandboxes/"+source.ID, "key-a", "")

	first := createWith(t, srv, "key-a", fmt.Sprintf(`{"snapshot":%q}`, snap.ID))
	second := createWith(t, srv, "key-a", fmt.Sprintf(`{"snapshot":%q}`, snap.ID))

	for _, sb := range []sandboxBody{first, second} {
		if got := call(srv, http.MethodGet, "/v1/sandboxes/"+sb.ID+"/files/workspace/a.txt", "key-a", ""); got.Body.String() != "kept" {
			t.Errorf("sandbox %s reads %q, want kept", sb.ID, got.Body.String())
		}
	}
	putFile(t, srv, first.ID, "/workspace/only-first.txt", "x")
	if rec := call(srv, http.MethodGet, "/v1/sandboxes/"+second.ID+"/files/workspace/only-first.txt", "key-a", ""); rec.Code != http.StatusNotFound {
		t.Errorf("a write in one restored sandbox is visible in another: %d", rec.Code)
	}
}

func TestSnapshotsAreListedFetchedAndDeleted(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{}`)
	first, second := snapshotOf(t, srv, source.ID, "one"), snapshotOf(t, srv, source.ID, "two")

	listed := call(srv, http.MethodGet, "/v1/snapshots", "key-a", "").Body.String()
	fetched := decode[snapshotBody](t, call(srv, http.MethodGet, "/v1/snapshots/"+first.ID, "key-a", ""), http.StatusOK)
	deleted := call(srv, http.MethodDelete, "/v1/snapshots/"+first.ID, "key-a", "")

	if !strings.Contains(listed, first.ID) || !strings.Contains(listed, second.ID) || fetched.Name != "one" || fetched.SandboxID != source.ID {
		t.Errorf("listed = %s, fetched = %+v; want both snapshots and the first by id", listed, fetched)
	}
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", deleted.Code)
	}
	if rec := call(srv, http.MethodGet, "/v1/snapshots/"+first.ID, "key-a", ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", rec.Code)
	}
	if rec := call(srv, http.MethodPost, "/v1/sandboxes", "key-a", fmt.Sprintf(`{"snapshot":%q}`, first.ID)); rec.Code != http.StatusBadRequest {
		t.Errorf("create from a deleted snapshot = %d, want 400", rec.Code)
	}
}

func TestASandboxStartedFromASnapshotSurvivesTheDeletionOfTheSnapshot(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{}`)
	putFile(t, srv, source.ID, "/workspace/a.txt", "kept")
	snap := snapshotOf(t, srv, source.ID, "short-lived")
	restored := createWith(t, srv, "key-a", fmt.Sprintf(`{"snapshot":%q}`, snap.ID))

	call(srv, http.MethodDelete, "/v1/snapshots/"+snap.ID, "key-a", "")

	if got := call(srv, http.MethodGet, "/v1/sandboxes/"+restored.ID+"/files/workspace/a.txt", "key-a", ""); got.Body.String() != "kept" {
		t.Errorf("the restored sandbox reads %q after the snapshot was deleted, want kept", got.Body.String())
	}
}

func TestAnotherTenantCannotSeeUseOrDeleteASnapshot(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{}`)
	snap := snapshotOf(t, srv, source.ID, "private")
	cases := map[string]struct {
		method, path, body string
		want               int
	}{
		"get":                    {http.MethodGet, "/v1/snapshots/" + snap.ID, "", http.StatusNotFound},
		"delete":                 {http.MethodDelete, "/v1/snapshots/" + snap.ID, "", http.StatusNotFound},
		"create a sandbox":       {http.MethodPost, "/v1/sandboxes", fmt.Sprintf(`{"snapshot":%q}`, snap.ID), http.StatusBadRequest},
		"snapshot their sandbox": {http.MethodPost, "/v1/sandboxes/" + source.ID + "/snapshots", `{"name":"stolen"}`, http.StatusNotFound},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, c.method, c.path, "key-b", c.body)

			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
	if listed := call(srv, http.MethodGet, "/v1/snapshots", "key-b", "").Body.String(); strings.Contains(listed, snap.ID) {
		t.Errorf("the other tenant's list shows the snapshot: %s", listed)
	}
	if rec := call(srv, http.MethodGet, "/v1/snapshots/"+snap.ID, "key-a", ""); rec.Code != http.StatusOK {
		t.Errorf("the snapshot is gone for its owner: %d", rec.Code)
	}
}

func TestCreateSnapshotRejectsInvalidRequests(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{}`)
	snapshotOf(t, srv, source.ID, "taken")
	cases := map[string]struct {
		path, body string
		want       int
	}{
		"a name in use":      {"/v1/sandboxes/" + source.ID + "/snapshots", `{"name":"taken"}`, http.StatusConflict},
		"no name":            {"/v1/sandboxes/" + source.ID + "/snapshots", `{}`, http.StatusBadRequest},
		"upper case":         {"/v1/sandboxes/" + source.ID + "/snapshots", `{"name":"Taken"}`, http.StatusBadRequest},
		"a slash":            {"/v1/sandboxes/" + source.ID + "/snapshots", `{"name":"a/b"}`, http.StatusBadRequest},
		"dot-dot":            {"/v1/sandboxes/" + source.ID + "/snapshots", `{"name":".."}`, http.StatusBadRequest},
		"malformed JSON":     {"/v1/sandboxes/" + source.ID + "/snapshots", `{"name":`, http.StatusBadRequest},
		"an unknown sandbox": {"/v1/sandboxes/sbx_missing/snapshots", `{"name":"x"}`, http.StatusNotFound},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := call(srv, http.MethodPost, c.path, "key-a", c.body)

			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestSnapshotCannotBeCombinedWithAnImageOrATemplate(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{}`)
	snap := snapshotOf(t, srv, source.ID, "base")
	for _, body := range []string{
		fmt.Sprintf(`{"snapshot":%q,"image":"busybox:latest"}`, snap.ID),
		fmt.Sprintf(`{"snapshot":%q,"template":"tpl_1"}`, snap.ID),
		fmt.Sprintf(`{"snapshot":%q,"gpu":"a10"}`, snap.ID),
	} {
		rec := call(srv, http.MethodPost, "/v1/sandboxes", "key-a", body)

		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "snapshot") {
			t.Errorf("%s: status = %d body = %s, want 400 naming snapshot", body, rec.Code, rec.Body.String())
		}
	}
}

func TestATenantCannotKeepMoreSnapshotsThanTheLimit(t *testing.T) {
	srv := newServer()
	source := createWith(t, srv, "key-a", `{}`)
	for i := range domain.MaxSnapshotsPerTenant {
		snapshotOf(t, srv, source.ID, fmt.Sprintf("s%d", i))
	}

	over := call(srv, http.MethodPost, "/v1/sandboxes/"+source.ID+"/snapshots", "key-a", `{"name":"one-too-many"}`)

	if over.Code != http.StatusConflict || !strings.Contains(over.Body.String(), "too_many_snapshots") {
		t.Errorf("status = %d body = %s, want 409 too_many_snapshots", over.Code, over.Body.String())
	}
}

func TestSnapshotRoutesAnswer501WithoutASnapshotStore(t *testing.T) {
	srv := newSignedServer(t, "") // a server wired without a snapshot store
	source := createWith(t, srv, "key-a", `{}`)

	create := call(srv, http.MethodPost, "/v1/sandboxes/"+source.ID+"/snapshots", "key-a", `{"name":"x"}`)
	list := call(srv, http.MethodGet, "/v1/snapshots", "key-a", "")

	if create.Code != http.StatusNotImplemented || list.Code != http.StatusNotImplemented {
		t.Errorf("create = %d, list = %d; want 501 and 501", create.Code, list.Code)
	}
}
