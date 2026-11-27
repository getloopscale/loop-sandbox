package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	gomcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/clock"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

// toolBox calls data tools by name against one service.
type toolBox struct {
	t *testing.T
	h *handlers
}

func newToolBox(t *testing.T) toolBox {
	t.Helper()
	store := memory.New()
	svc := app.NewService(store, fake.NewWithSessions(), clock.Fixed{T: time.Unix(1_700_000_000, 0).UTC()},
		&seq{}, noAudit{}, nil, nil)
	svc.SetSnapshotStore(store)
	return toolBox{t: t, h: &handlers{svc: svc}}
}

// call runs the named data tool and returns its result.
func (b toolBox) call(ctx context.Context, name string, args map[string]any) *gomcp.CallToolResult {
	b.t.Helper()
	for _, tl := range dataTools() {
		if tl.spec.Name == name {
			res, err := b.h.handler(tl)(ctx, callArgs(name, args))
			if err != nil {
				b.t.Fatalf("%s returned a protocol error: %v", name, err)
			}
			return res
		}
	}
	b.t.Fatalf("no data tool named %s", name)
	return nil
}

// object decodes the JSON result of a successful call.
func (b toolBox) object(ctx context.Context, name string, args map[string]any) map[string]any {
	b.t.Helper()
	res := b.call(ctx, name, args)
	if res.IsError {
		b.t.Fatalf("%s failed: %s", name, resultText(res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(res)), &out); err != nil {
		b.t.Fatalf("%s result is not a JSON object: %q", name, resultText(res))
	}
	return out
}

func TestEveryDataToolRefusesACallerWithoutTheScope(t *testing.T) {
	b := newToolBox(t)
	noScope := ctxWith("tenant-a")

	for _, tl := range dataTools() {
		res := b.call(noScope, tl.spec.Name, map[string]any{"sandbox_id": "x"})

		if !res.IsError || !strings.Contains(resultText(res), "forbidden") {
			t.Errorf("%s without the scope = %q, want forbidden", tl.spec.Name, resultText(res))
		}
	}
}

func TestEveryDataToolRefusesAnUnauthenticatedCaller(t *testing.T) {
	b := newToolBox(t)

	for _, tl := range dataTools() {
		res := b.call(context.Background(), tl.spec.Name, map[string]any{"sandbox_id": "x"})

		if !res.IsError || !strings.Contains(resultText(res), "unauthorized") {
			t.Errorf("%s without a principal = %q, want unauthorized", tl.spec.Name, resultText(res))
		}
	}
}

func TestGetSandboxReturnsItsStateAndHidesAnotherTenants(t *testing.T) {
	b := newToolBox(t)
	owner, other := ctxWith("tenant-a", ScopeExecute), ctxWith("tenant-b", ScopeExecute)
	id := createOne(t, b.h, owner)

	mine := b.object(owner, "get_sandbox", map[string]any{"sandbox_id": id})
	theirs := b.call(other, "get_sandbox", map[string]any{"sandbox_id": id})

	if mine["id"] != id || mine["state"] != "running" {
		t.Errorf("owner got %v, want the running sandbox %s", mine, id)
	}
	if !theirs.IsError {
		t.Errorf("another tenant read the sandbox: %s", resultText(theirs))
	}
}

func TestListSandboxesFiltersByStateAndRejectsAnUnknownOne(t *testing.T) {
	b := newToolBox(t)
	ctx := ctxWith("tenant-a", ScopeExecute)
	createOne(t, b.h, ctx)

	running := b.object(ctx, "list_sandboxes", map[string]any{"state": "running"})
	paused := b.object(ctx, "list_sandboxes", map[string]any{"state": "paused"})
	unknown := b.call(ctx, "list_sandboxes", map[string]any{"state": "sleeping"})

	if len(running["sandboxes"].([]any)) != 1 || len(paused["sandboxes"].([]any)) != 0 {
		t.Errorf("running = %v, paused = %v; want one and none", running["sandboxes"], paused["sandboxes"])
	}
	if !unknown.IsError || !strings.Contains(resultText(unknown), "sleeping") {
		t.Errorf("unknown state = %q, want an error naming it", resultText(unknown))
	}
}

func TestForkSandboxWithCountReturnsThatManySandboxes(t *testing.T) {
	b := newToolBox(t)
	ctx := ctxWith("tenant-a", ScopeExecute)
	id := createOne(t, b.h, ctx)

	out := b.object(ctx, "fork_sandbox", map[string]any{"sandbox_id": id, "count": float64(2)})

	if forks := out["sandboxes"].([]any); len(forks) != 2 {
		t.Errorf("got %d forks, want 2", len(forks))
	}
}

func TestSetSandboxTimeoutMovesTheExpiry(t *testing.T) {
	b := newToolBox(t)
	ctx := ctxWith("tenant-a", ScopeExecute)
	id := createOne(t, b.h, ctx)

	out := b.object(ctx, "set_sandbox_timeout", map[string]any{"sandbox_id": id, "ttl_seconds": float64(120)})

	if want := time.Unix(1_700_000_120, 0).UTC().Format(time.RFC3339); out["expires_at"] != want {
		t.Errorf("expires_at = %v, want %s", out["expires_at"], want)
	}
}

func TestADataToolReportsAMissingRequiredArgument(t *testing.T) {
	b := newToolBox(t)
	ctx := ctxWith("tenant-a", ScopeExecute)

	res := b.call(ctx, "move_file", map[string]any{"sandbox_id": "x", "from": "/workspace/a"})

	if !res.IsError || !strings.Contains(resultText(res), "to") {
		t.Errorf("result = %q, want an error naming the missing argument", resultText(res))
	}
}

func TestStartProcessRejectsAnEnvThatIsNotStrings(t *testing.T) {
	b := newToolBox(t)
	ctx := ctxWith("tenant-a", ScopeExecute)
	id := createOne(t, b.h, ctx)

	res := b.call(ctx, "start_process", map[string]any{"sandbox_id": id, "cmd": []any{"env"}, "env": map[string]any{"N": float64(1)}})

	if !res.IsError || !strings.Contains(resultText(res), "env.N") {
		t.Errorf("result = %q, want an error naming env.N", resultText(res))
	}
}

func TestCapabilitiesNamesTheDriver(t *testing.T) {
	b := newToolBox(t)

	out := b.object(ctxWith("tenant-a", ScopeExecute), "capabilities", nil)

	if out["driver"] != "fake" || len(out["capabilities"].([]any)) == 0 {
		t.Errorf("capabilities = %v, want the fake driver and a list", out)
	}
}

func TestCreateSandboxStartsFromASnapshotTakenWithCreateSnapshot(t *testing.T) {
	b := newToolBox(t)
	ctx := ctxWith("tenant-a", ScopeExecute)
	source := createOne(t, b.h, ctx)
	if r, _ := b.h.writeFile(ctx, callArgs(ToolWriteFile, map[string]any{"sandbox_id": source, "path": "/workspace/a.txt", "content": "kept"})); r.IsError {
		t.Fatalf("write_file: %s", resultText(r))
	}
	snap := b.object(ctx, "create_snapshot", map[string]any{"sandbox_id": source, "name": "base"})

	res, err := b.h.createSandbox(ctx, callArgs(ToolCreateSandbox, map[string]any{"snapshot": snap["id"]}))

	if err != nil || res.IsError {
		t.Fatalf("create_sandbox from a snapshot: err=%v result=%q", err, resultText(res))
	}
	restored, _ := res.StructuredContent.(map[string]any)["id"].(string)
	if r, _ := b.h.readFile(ctx, callArgs(ToolReadFile, map[string]any{"sandbox_id": restored, "path": "/workspace/a.txt"})); restored == source || resultText(r) != "kept" {
		t.Errorf("restored = %q reads %q; want a new sandbox that reads kept", restored, resultText(r))
	}
}

func TestSnapshotToolsListGetAndDeleteWithinTheTenant(t *testing.T) {
	b := newToolBox(t)
	ctx := ctxWith("tenant-a", ScopeExecute)
	snap := b.object(ctx, "create_snapshot", map[string]any{"sandbox_id": createOne(t, b.h, ctx), "name": "base"})
	id := snap["id"]

	listed := b.object(ctx, "list_snapshots", nil)["snapshots"].([]any)
	fetched := b.object(ctx, "get_snapshot", map[string]any{"snapshot_id": id})
	other := b.call(ctxWith("tenant-b", ScopeExecute), "get_snapshot", map[string]any{"snapshot_id": id})
	deleted := b.call(ctx, "delete_snapshot", map[string]any{"snapshot_id": id})

	if len(listed) != 1 || fetched["name"] != "base" || !other.IsError || deleted.IsError {
		t.Errorf("listed %d, fetched %v, other tenant error %v, delete error %v", len(listed), fetched, other.IsError, deleted.IsError)
	}
	if after := b.call(ctx, "get_snapshot", map[string]any{"snapshot_id": id}); !after.IsError {
		t.Errorf("get after delete succeeded: %s", resultText(after))
	}
}
