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

type snapshotRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SandboxID string `json:"sandbox_id"`
	SizeBytes int64  `json:"size_bytes"`
}

var snapshotSeq atomic.Int64

// snapshotName returns a name no other case of this run or of an earlier run uses.
func snapshotName(label string) string {
	return fmt.Sprintf("%s-%d-%d", label, time.Now().UnixNano(), snapshotSeq.Add(1))
}

func deleteSnapshot(t *testing.T, id string) {
	t.Helper()
	resp := call(t, apiKey, http.MethodDelete, "/v1/snapshots/"+id, nil)
	if resp.status != http.StatusNoContent && resp.status != http.StatusNotFound {
		t.Errorf("delete snapshot %s: status = %d: %s", id, resp.status, resp.body)
	}
}

// takeSnapshot snapshots a sandbox and deletes the snapshot when the test ends.
func takeSnapshot(t *testing.T, sandboxID, label string) snapshotRecord {
	t.Helper()
	resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/snapshots", map[string]any{"name": snapshotName(label)})
	resp.want(t, http.StatusCreated, "create snapshot")
	var snap snapshotRecord
	resp.json(t, &snap)
	if snap.ID == "" {
		t.Fatalf("create snapshot: empty id: %s", resp.body)
	}
	t.Cleanup(func() { deleteSnapshot(t, snap.ID) })
	return snap
}

// fromSnapshot starts a sandbox from a snapshot and destroys it when the test ends.
func fromSnapshot(t *testing.T, snapshotID string) sandbox {
	t.Helper()
	resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes", map[string]any{"snapshot": snapshotID, "ttl_seconds": 600})
	resp.want(t, http.StatusCreated, "create a sandbox from a snapshot")
	var sb sandbox
	resp.json(t, &sb)
	t.Cleanup(func() { destroy(t, sb.ID) })
	return sb
}

func TestSnapshots(t *testing.T) {
	probe := func(t *testing.T) *response {
		resp := call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+newSandbox(t, shellImage).ID+"/snapshots",
			map[string]any{"name": snapshotName("probe")})
		if resp.status == http.StatusCreated {
			var snap snapshotRecord
			resp.json(t, &snap)
			t.Cleanup(func() { deleteSnapshot(t, snap.ID) })
		}
		return resp
	}
	capability(t, "snapshots", probe, map[string]func(t *testing.T){
		"a_sandbox_from_a_snapshot_has_the_files_of_the_snapshot": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			writeFile(t, source.ID, "/workspace/state.txt", "at-snapshot")
			snap := takeSnapshot(t, source.ID, "files")
			writeFile(t, source.ID, "/workspace/state.txt", "after-snapshot")

			restored := fromSnapshot(t, snap.ID)

			got := readFile(t, restored.ID, "/workspace/state.txt").want(t, http.StatusOK, "read in the restored sandbox")
			if string(got.body) != "at-snapshot" || restored.ID == source.ID || restored.State != "running" {
				t.Errorf("restored %s (%s) reads %q, want a new running sandbox that reads at-snapshot", restored.ID, restored.State, got.body)
			}
		},
		"a_sandbox_from_a_snapshot_has_the_processes_that_were_running": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			started := sh(t, source.ID, "nohup sleep 600 >/dev/null 2>&1 & echo $!")
			pid := strings.TrimSpace(started.Stdout)
			snap := takeSnapshot(t, source.ID, "memory")

			restored := fromSnapshot(t, snap.ID)

			if alive := sh(t, restored.ID, "kill -0 "+pid); pid == "" || alive.ExitCode != 0 {
				t.Errorf("process %q is not running in the restored sandbox: exit %d %s", pid, alive.ExitCode, alive.Stderr)
			}
		},
		"the_source_keeps_running_after_the_snapshot": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			writeFile(t, source.ID, "/workspace/a.txt", "before")

			takeSnapshot(t, source.ID, "source")

			out := sh(t, source.ID, "cat /workspace/a.txt && echo after > /workspace/b.txt && cat /workspace/b.txt")
			var got sandbox
			call(t, apiKey, http.MethodGet, "/v1/sandboxes/"+source.ID, nil).want(t, http.StatusOK, "get source").json(t, &got)
			if out.ExitCode != 0 || out.Stdout != "beforeafter\n" || got.State != "running" {
				t.Errorf("source is %s and exec gave exit %d stdout %q; want running and beforeafter", got.State, out.ExitCode, out.Stdout)
			}
		},
		"two_sandboxes_from_one_snapshot_do_not_share_writes": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			snap := takeSnapshot(t, source.ID, "isolation")
			first, second := fromSnapshot(t, snap.ID), fromSnapshot(t, snap.ID)

			writeFile(t, first.ID, "/workspace/only-first.txt", "x")

			readFile(t, second.ID, "/workspace/only-first.txt").want(t, http.StatusNotFound, "read in the second sandbox")
			readFile(t, source.ID, "/workspace/only-first.txt").want(t, http.StatusNotFound, "read in the source")
		},
		"a_snapshot_outlives_the_sandbox_it_was_taken_from": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			writeFile(t, source.ID, "/workspace/a.txt", "kept")
			snap := takeSnapshot(t, source.ID, "outlive")
			destroy(t, source.ID)

			restored := fromSnapshot(t, snap.ID)

			got := readFile(t, restored.ID, "/workspace/a.txt").want(t, http.StatusOK, "read in the restored sandbox")
			if string(got.body) != "kept" {
				t.Errorf("restored reads %q, want kept", got.body)
			}
		},
		"snapshots_are_listed_and_fetched_by_id": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			snap := takeSnapshot(t, source.ID, "listed")
			var listed struct {
				Snapshots []snapshotRecord `json:"snapshots"`
			}
			var fetched snapshotRecord

			call(t, apiKey, http.MethodGet, "/v1/snapshots", nil).want(t, http.StatusOK, "list snapshots").json(t, &listed)
			call(t, apiKey, http.MethodGet, "/v1/snapshots/"+snap.ID, nil).want(t, http.StatusOK, "get snapshot").json(t, &fetched)

			found := false
			for _, s := range listed.Snapshots {
				found = found || s.ID == snap.ID
			}
			if !found || fetched.Name != snap.Name || fetched.SandboxID != source.ID || fetched.SizeBytes <= 0 {
				t.Errorf("listed has it: %v; fetched = %+v; want the snapshot of %s with a size", found, fetched, source.ID)
			}
		},
		"a_deleted_snapshot_cannot_start_a_sandbox_and_running_ones_survive": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			writeFile(t, source.ID, "/workspace/a.txt", "kept")
			snap := takeSnapshot(t, source.ID, "deleted")
			restored := fromSnapshot(t, snap.ID)

			call(t, apiKey, http.MethodDelete, "/v1/snapshots/"+snap.ID, nil).want(t, http.StatusNoContent, "delete snapshot")

			call(t, apiKey, http.MethodGet, "/v1/snapshots/"+snap.ID, nil).want(t, http.StatusNotFound, "get after delete")
			call(t, apiKey, http.MethodPost, "/v1/sandboxes", map[string]any{"snapshot": snap.ID}).
				want(t, http.StatusBadRequest, "create from a deleted snapshot")
			if out := sh(t, restored.ID, "cat /workspace/a.txt"); out.Stdout != "kept" {
				t.Errorf("the restored sandbox reads %q after the snapshot was deleted, want kept", out.Stdout)
			}
		},
		"a_snapshot_name_in_use_is_refused": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			snap := takeSnapshot(t, source.ID, "taken")

			again := call(t, apiKey, http.MethodPost, "/v1/sandboxes/"+source.ID+"/snapshots", map[string]any{"name": snap.Name})

			again.want(t, http.StatusConflict, "second snapshot with the same name")
		},
		"another_tenant_cannot_see_use_or_delete_a_snapshot": func(t *testing.T) {
			source := newSandbox(t, shellImage)
			snap := takeSnapshot(t, source.ID, "private")

			get := call(t, otherAPIKey, http.MethodGet, "/v1/snapshots/"+snap.ID, nil)
			list := call(t, otherAPIKey, http.MethodGet, "/v1/snapshots", nil)
			create := call(t, otherAPIKey, http.MethodPost, "/v1/sandboxes", map[string]any{"snapshot": snap.ID})
			del := call(t, otherAPIKey, http.MethodDelete, "/v1/snapshots/"+snap.ID, nil)

			get.want(t, http.StatusNotFound, "get as the other tenant")
			create.want(t, http.StatusBadRequest, "create a sandbox as the other tenant")
			del.want(t, http.StatusNotFound, "delete as the other tenant")
			if strings.Contains(string(list.body), snap.ID) {
				t.Errorf("the other tenant's list shows the snapshot: %s", list.body)
			}
			call(t, apiKey, http.MethodGet, "/v1/snapshots/"+snap.ID, nil).want(t, http.StatusOK, "get as the owner afterwards")
		},
	})
}
