package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/getloopscale/loop-sandbox/internal/app"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// tool is one MCP tool defined as data: its schema, the REST routes it stands
// for, and the service call it makes. The result of run becomes the tool
// result: nil is "ok", a string is text, anything else is JSON.
type tool struct {
	spec   mcp.Tool
	routes []string // "METHOD /path", as in api.Routes
	run    func(ctx context.Context, svc *app.Service, actor app.Actor, req mcp.CallToolRequest) (any, error)
}

// artifactURLTTL is how long a presigned artifact URL lasts, as on the REST route.
const artifactURLTTL = 10 * time.Minute

// maxListLimit bounds list_sandboxes.
const maxListLimit = 100

// legacyRoutes are the REST routes covered by the tools registered by hand in
// NewServer.
var legacyRoutes = map[string][]string{
	ToolCreateSandbox:     {"POST /v1/sandboxes"},
	ToolExec:              {"POST /v1/sandboxes/{id}/executions"},
	ToolWriteFile:         {"PUT /v1/sandboxes/{id}/files/{path...}"},
	ToolReadFile:          {"GET /v1/sandboxes/{id}/files/{path...}"},
	ToolListFiles:         {"GET /v1/sandboxes/{id}/files"},
	ToolDestroySandbox:    {"DELETE /v1/sandboxes/{id}"},
	ToolBrowserNavigate:   {"POST /v1/sandboxes/{id}/browser/navigate"},
	ToolBrowserScreenshot: {"GET /v1/sandboxes/{id}/browser/screenshot"},
	ToolBrowserDOM:        {"GET /v1/sandboxes/{id}/browser/dom"},
	ToolBrowserClick:      {"POST /v1/sandboxes/{id}/browser/input"},
	ToolBrowserType:       {"POST /v1/sandboxes/{id}/browser/input"},
}

// RoutesWithoutTool are the REST routes that have no MCP tool on purpose, each
// with the reason. A route that is neither here nor covered by a tool fails the
// coverage test.
var RoutesWithoutTool = map[string]string{
	"GET /metrics":                 "operational endpoint for a scraper, not an action on a sandbox",
	"GET /v1/healthz":              "operational endpoint for a load balancer",
	"GET /v1/readyz":               "operational endpoint for a load balancer",
	"GET /v1/sandboxes/{id}/pty":   "an interactive WebSocket stream; a tool call is one request and one response. start_process with write_process_stdin covers scripted use",
	"GET /v1/signed-files/{token}": "made for a holder without credentials; an MCP caller has read_file",
	"PUT /v1/signed-files/{token}": "made for a holder without credentials; an MCP caller has write_file",
	"POST /v1/sandboxes/{id}/file-urls": "the signer belongs to the HTTP surface, which knows the public address of the API; " +
		"an MCP caller moves files with read_file and write_file",
	"GET /v1/api-keys":               "credential management is kept off the agent surface: a tool that mints keys would let a model widen its own access",
	"POST /v1/api-keys":              "credential management is kept off the agent surface: a tool that mints keys would let a model widen its own access",
	"DELETE /v1/api-keys/{kid}":      "credential management is kept off the agent surface: a model could revoke the keys of its operator",
	"PUT /v1/sandboxes/{id}/network": "the outbound rules are set by the operator of the agent: a tool that replaces them would let a model open its own network",
	"POST /v1/templates":             "registering or publishing an image is an administrative action, done by an operator",
	"GET /v1/secrets":                "secret management is kept off the agent surface; create_sandbox takes the names of the secrets a sandbox receives",
	"PUT /v1/secrets/{name}":         "secret management is kept off the agent surface: the value would pass through the context of the model",
	"DELETE /v1/secrets/{name}":      "secret management is kept off the agent surface: a model could delete the secrets of its operator",
}

// Coverage returns, for every tool, the REST routes it stands for.
func Coverage() map[string][]string {
	out := map[string][]string{}
	for name, routes := range legacyRoutes {
		out[name] = routes
	}
	for _, t := range dataTools() {
		out[t.spec.Name] = t.routes
	}
	return out
}

func sandboxView(sb *domain.Sandbox) map[string]any {
	return map[string]any{
		"id": sb.ID, "state": string(sb.State), "image": sb.Image,
		"expires_at": sb.ExpiresAt.UTC().Format(time.RFC3339), "metadata": sb.Metadata,
	}
}

func processView(p runtime.ProcessInfo) map[string]any {
	out := map[string]any{
		"pid": p.PID, "cmd": p.Cmd, "running": p.Running,
		"started_at": p.StartedAt.UTC().Format(time.RFC3339Nano),
	}
	if !p.Running {
		out["exit_code"] = p.ExitCode
	}
	return out
}

func volumeView(v *domain.Volume) map[string]any {
	return map[string]any{"id": v.ID, "name": v.Name, "size_gb": v.SizeGB, "sandbox_id": v.SandboxID}
}

func snapshotView(s *domain.Snapshot) map[string]any {
	return map[string]any{
		"id": s.ID, "name": s.Name, "sandbox_id": s.SandboxID, "image": s.Image,
		"size_bytes": s.SizeBytes, "created_at": s.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func templateView(t *domain.Template) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "image": t.ImageRef, "version": t.Version,
		"public": t.Public, "scan_status": t.ScanStatus,
	}
}

// waitOf reads the long-poll argument. The service caps it.
func waitOf(req mcp.CallToolRequest) time.Duration {
	return time.Duration(req.GetInt("wait_ms", 0)) * time.Millisecond
}

// stringMap reads an object argument whose values must all be strings.
func stringMap(req mcp.CallToolRequest, key string) (map[string]string, error) {
	raw, ok := req.GetArguments()[key]
	if !ok || raw == nil {
		return nil, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object of strings", key)
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s must be a string", key, k)
		}
		out[k] = s
	}
	return out, nil
}

// Schema fragments shared by many tools.
var (
	sandboxArg = mcp.WithString("sandbox_id", mcp.Required())
	afterArg   = mcp.WithNumber("after", mcp.Description("cursor: the next value of the previous call; 0 reads from the start"))
	waitArg    = mcp.WithNumber("wait_ms", mcp.Description("wait up to this long for something new, at most 30000"))
)

// dataTools returns the tools defined as data. It builds a fresh slice so no
// caller can change the definitions of another.
func dataTools() []tool {
	type (
		ctxT = context.Context
		svcT = *app.Service
		reqT = mcp.CallToolRequest
	)
	// sandbox reads the required sandbox_id.
	sandbox := func(req reqT) (string, error) { return req.RequireString("sandbox_id") }
	// pid reads the required process id.
	pid := func(req reqT) (int, error) { return req.RequireInt("pid") }

	return []tool{
		{
			spec:   mcp.NewTool("get_sandbox", mcp.WithDescription("Get the state and expiry of a sandbox."), sandboxArg),
			routes: []string{"GET /v1/sandboxes/{id}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				sb, err := svc.GetSandbox(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				return sandboxView(sb), nil
			},
		},
		{
			spec: mcp.NewTool("list_sandboxes", mcp.WithDescription("List the caller's sandboxes."),
				mcp.WithString("state", mcp.Description("keep only this state: pending, running, paused or destroyed")),
				mcp.WithNumber("limit", mcp.Description("at most this many, 1 to 100; default 50"))),
			routes: []string{"GET /v1/sandboxes"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				q := domain.SandboxQuery{Limit: req.GetInt("limit", 50)}
				if q.Limit < 1 || q.Limit > maxListLimit {
					return nil, fmt.Errorf("limit must be between 1 and %d", maxListLimit)
				}
				switch state := domain.State(req.GetString("state", "")); state {
				case "":
				case domain.StatePending, domain.StateRunning, domain.StatePaused, domain.StateDestroyed:
					q.States = []domain.State{state}
				default:
					return nil, fmt.Errorf("unknown state %q", state)
				}
				page, err := svc.ListSandboxes(ctx, actor, q)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(page.Sandboxes))
				for _, sb := range page.Sandboxes {
					out = append(out, sandboxView(sb))
				}
				return map[string]any{"sandboxes": out, "has_more": page.Next != nil}, nil
			},
		},
		{
			spec: mcp.NewTool("list_network_denials", mcp.WithDescription("List the outbound connections the sandbox's network rules refused, oldest first. Use it to find out why a download or a request from the sandbox failed."),
				sandboxArg, mcp.WithNumber("limit", mcp.Description("at most this many, 1 to 1000; default 100"))),
			routes: []string{"GET /v1/sandboxes/{id}/network/denials"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				denials, err := svc.SandboxNetworkDenials(ctx, actor, id, req.GetInt("limit", 0))
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(denials))
				for _, d := range denials {
					out = append(out, map[string]any{
						"time": d.Time.UTC().Format(time.RFC3339Nano), "destination": d.Destination, "host": d.Host, "reason": d.Reason,
					})
				}
				return map[string]any{"denials": out}, nil
			},
		},
		{
			spec: mcp.NewTool("set_sandbox_timeout", mcp.WithDescription("Set how long from now the sandbox lives."),
				sandboxArg, mcp.WithNumber("ttl_seconds", mcp.Required())),
			routes: []string{"POST /v1/sandboxes/{id}/timeout"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				ttl, e2 := req.RequireInt("ttl_seconds")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				sb, err := svc.SetSandboxTTL(ctx, actor, id, time.Duration(ttl)*time.Second)
				if err != nil {
					return nil, err
				}
				return sandboxView(sb), nil
			},
		},
		{
			spec: mcp.NewTool("fork_sandbox", mcp.WithDescription("Create independent copies of a running sandbox, with its memory and disk. All or nothing."),
				sandboxArg, mcp.WithNumber("count", mcp.Description("how many forks, 1 to 16; default 1"))),
			routes: []string{"POST /v1/sandboxes/{id}/fork"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				forks, err := svc.ForkSandboxes(ctx, actor, id, req.GetInt("count", 1))
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(forks))
				for _, sb := range forks {
					out = append(out, sandboxView(sb))
				}
				return map[string]any{"sandboxes": out}, nil
			},
		},
		{
			spec:   mcp.NewTool("pause_sandbox", mcp.WithDescription("Snapshot a sandbox and release its compute."), sandboxArg),
			routes: []string{"POST /v1/sandboxes/{id}/pause"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				return nil, svc.PauseSandbox(ctx, actor, id)
			},
		},
		{
			spec:   mcp.NewTool("resume_sandbox", mcp.WithDescription("Resume a paused sandbox."), sandboxArg),
			routes: []string{"POST /v1/sandboxes/{id}/resume"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				sb, err := svc.ResumeSandbox(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				return sandboxView(sb), nil
			},
		},
		{
			spec: mcp.NewTool("delete_file", mcp.WithDescription("Delete a file under /workspace."),
				sandboxArg, mcp.WithString("path", mcp.Required())),
			routes: []string{"DELETE /v1/sandboxes/{id}/files/{path...}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				path, e2 := req.RequireString("path")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				return nil, svc.DeleteFile(ctx, actor, id, path)
			},
		},
		{
			spec: mcp.NewTool("file_info", mcp.WithDescription("Get the size, kind, mode and modification time of a path under /workspace."),
				sandboxArg, mcp.WithString("path", mcp.Required())),
			routes: []string{"GET /v1/sandboxes/{id}/file-info/{path...}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				path, e2 := req.RequireString("path")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				info, err := svc.StatFile(ctx, actor, id, path)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"name": info.Name, "size": info.Size, "is_dir": info.IsDir,
					"mode": fmt.Sprintf("%04o", info.Mode), "modified_at": info.ModTime.UTC().Format(time.RFC3339),
				}, nil
			},
		},
		{
			spec: mcp.NewTool("make_directory", mcp.WithDescription("Create a directory under /workspace, with its parents."),
				sandboxArg, mcp.WithString("path", mcp.Required())),
			routes: []string{"PUT /v1/sandboxes/{id}/directories/{path...}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				path, e2 := req.RequireString("path")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				return nil, svc.MakeDir(ctx, actor, id, path)
			},
		},
		{
			spec: mcp.NewTool("move_file", mcp.WithDescription("Move or rename a file or directory under /workspace."),
				sandboxArg, mcp.WithString("from", mcp.Required()), mcp.WithString("to", mcp.Required()),
				mcp.WithBoolean("overwrite", mcp.Description("replace the destination when it exists; default false"))),
			routes: []string{"POST /v1/sandboxes/{id}/file-moves"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				from, e2 := req.RequireString("from")
				to, e3 := req.RequireString("to")
				if err := firstErr(e1, e2, e3); err != nil {
					return nil, err
				}
				return nil, svc.MoveFile(ctx, actor, id, from, to, req.GetBool("overwrite", false))
			},
		},
		{
			spec: mcp.NewTool("artifact_url", mcp.WithDescription("Get a download URL, valid for 10 minutes, for a file of the sandbox stored in object storage."),
				sandboxArg, mcp.WithString("path", mcp.Required())),
			routes: []string{"GET /v1/sandboxes/{id}/artifact-url/{path...}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				path, e2 := req.RequireString("path")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				url, err := svc.PresignArtifactURL(ctx, actor, id, path, artifactURLTTL)
				if err != nil {
					return nil, err
				}
				return map[string]any{"url": url}, nil
			},
		},
		{
			spec: mcp.NewTool("start_process", mcp.WithDescription("Start a command in the background and return its pid. Read it with process_output."),
				sandboxArg, mcp.WithArray("cmd", mcp.Required(), mcp.WithStringItems(), mcp.Description("argv")),
				mcp.WithObject("env", mcp.Description("extra environment variables")),
				mcp.WithString("working_dir", mcp.Description("directory under /workspace"))),
			routes: []string{"POST /v1/sandboxes/{id}/processes"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				cmd, e2 := req.RequireStringSlice("cmd")
				env, e3 := stringMap(req, "env")
				if err := firstErr(e1, e2, e3); err != nil {
					return nil, err
				}
				p, err := svc.StartProcess(ctx, actor, id, runtime.ProcessSpec{Cmd: cmd, Env: env, WorkingDir: req.GetString("working_dir", "")})
				if err != nil {
					return nil, err
				}
				return processView(*p), nil
			},
		},
		{
			spec:   mcp.NewTool("list_processes", mcp.WithDescription("List the background processes of a sandbox."), sandboxArg),
			routes: []string{"GET /v1/sandboxes/{id}/processes"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				procs, err := svc.ListProcesses(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(procs))
				for _, p := range procs {
					out = append(out, processView(p))
				}
				return map[string]any{"processes": out}, nil
			},
		},
		{
			spec: mcp.NewTool("get_process", mcp.WithDescription("Get whether a background process is running and its exit code."),
				sandboxArg, mcp.WithNumber("pid", mcp.Required())),
			routes: []string{"GET /v1/sandboxes/{id}/processes/{pid}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				p, e2 := pid(req)
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				info, err := svc.GetProcess(ctx, actor, id, p)
				if err != nil {
					return nil, err
				}
				return processView(*info), nil
			},
		},
		{
			spec: mcp.NewTool("process_output", mcp.WithDescription("Read the output of a background process after a cursor. Pass the returned next as after to continue."),
				sandboxArg, mcp.WithNumber("pid", mcp.Required()), afterArg, waitArg),
			routes: []string{"GET /v1/sandboxes/{id}/processes/{pid}/output"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				p, e2 := pid(req)
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				out, err := svc.ProcessOutput(ctx, actor, id, p, int64(req.GetInt("after", 0)), waitOf(req))
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"stdout": string(out.Stdout), "stderr": string(out.Stderr), "next": out.Next,
					"truncated": out.Truncated, "process": processView(out.Process),
				}, nil
			},
		},
		{
			spec: mcp.NewTool("write_process_stdin", mcp.WithDescription("Write text to the standard input of a background process."),
				sandboxArg, mcp.WithNumber("pid", mcp.Required()), mcp.WithString("data", mcp.Required())),
			routes: []string{"POST /v1/sandboxes/{id}/processes/{pid}/stdin"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				p, e2 := pid(req)
				data, e3 := req.RequireString("data")
				if err := firstErr(e1, e2, e3); err != nil {
					return nil, err
				}
				return nil, svc.WriteProcessStdin(ctx, actor, id, p, []byte(data))
			},
		},
		{
			spec: mcp.NewTool("close_process_stdin", mcp.WithDescription("Close the standard input of a background process, which it sees as end of file."),
				sandboxArg, mcp.WithNumber("pid", mcp.Required())),
			routes: []string{"DELETE /v1/sandboxes/{id}/processes/{pid}/stdin"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				p, e2 := pid(req)
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				return nil, svc.CloseProcessStdin(ctx, actor, id, p)
			},
		},
		{
			spec: mcp.NewTool("signal_process", mcp.WithDescription("Send a signal to a background process and its children."),
				sandboxArg, mcp.WithNumber("pid", mcp.Required()),
				mcp.WithString("signal", mcp.Required(), mcp.Description("HUP, INT, QUIT, KILL, TERM, USR1, USR2, STOP or CONT"))),
			routes: []string{"POST /v1/sandboxes/{id}/processes/{pid}/signal"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				p, e2 := pid(req)
				sig, e3 := req.RequireString("signal")
				if err := firstErr(e1, e2, e3); err != nil {
					return nil, err
				}
				return nil, svc.SignalProcess(ctx, actor, id, p, sig)
			},
		},
		{
			spec: mcp.NewTool("start_watch", mcp.WithDescription("Watch a directory under /workspace for changes. Read them with watch_events."),
				sandboxArg, mcp.WithString("path", mcp.Required()),
				mcp.WithBoolean("recursive", mcp.Description("include subdirectories; default false"))),
			routes: []string{"POST /v1/sandboxes/{id}/watches"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				path, e2 := req.RequireString("path")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				watchID, clean, err := svc.StartWatch(ctx, actor, id, path, req.GetBool("recursive", false))
				if err != nil {
					return nil, err
				}
				return map[string]any{"watch_id": watchID, "path": clean}, nil
			},
		},
		{
			spec: mcp.NewTool("watch_events", mcp.WithDescription("Read the file events of a watch after a cursor."),
				sandboxArg, mcp.WithString("watch_id", mcp.Required()), afterArg, waitArg),
			routes: []string{"GET /v1/sandboxes/{id}/watches/{wid}/events"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				watchID, e2 := req.RequireString("watch_id")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				ev, err := svc.WatchEvents(ctx, actor, id, watchID, int64(req.GetInt("after", 0)), waitOf(req))
				if err != nil {
					return nil, err
				}
				events := make([]map[string]any, 0, len(ev.Events))
				for _, e := range ev.Events {
					events = append(events, map[string]any{"type": e.Type, "path": e.Path, "is_dir": e.IsDir})
				}
				return map[string]any{"events": events, "next": ev.Next, "truncated": ev.Truncated, "closed": ev.Closed}, nil
			},
		},
		{
			spec: mcp.NewTool("stop_watch", mcp.WithDescription("Stop a directory watch."),
				sandboxArg, mcp.WithString("watch_id", mcp.Required())),
			routes: []string{"DELETE /v1/sandboxes/{id}/watches/{wid}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				watchID, e2 := req.RequireString("watch_id")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				return nil, svc.StopWatch(ctx, actor, id, watchID)
			},
		},
		{
			spec: mcp.NewTool("sandbox_logs", mcp.WithDescription("Read the combined output of the background processes of a sandbox after a cursor."),
				sandboxArg, afterArg, waitArg),
			routes: []string{"GET /v1/sandboxes/{id}/logs"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				logs, err := svc.SandboxLogs(ctx, actor, id, int64(req.GetInt("after", 0)), waitOf(req))
				if err != nil {
					return nil, err
				}
				entries := make([]map[string]any, 0, len(logs.Entries))
				for _, e := range logs.Entries {
					entries = append(entries, map[string]any{
						"pid": e.PID, "stream": e.Stream, "data": string(e.Data), "at": e.At.UTC().Format(time.RFC3339Nano),
					})
				}
				return map[string]any{"entries": entries, "next": logs.Next, "truncated": logs.Truncated}, nil
			},
		},
		{
			spec:   mcp.NewTool("sandbox_metrics", mcp.WithDescription("Get the CPU time and memory use of a sandbox."), sandboxArg),
			routes: []string{"GET /v1/sandboxes/{id}/metrics"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				m, err := svc.SandboxMetrics(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"cpu_usage_ns": m.CPUUsageNanos, "memory_used_bytes": m.MemoryUsedBytes,
					"memory_limit_bytes": m.MemoryLimitBytes, "at": m.At.UTC().Format(time.RFC3339Nano),
				}, nil
			},
		},
		{
			spec:   mcp.NewTool("start_session", mcp.WithDescription("Start a Python session whose variables persist between cells."), sandboxArg),
			routes: []string{"POST /v1/sandboxes/{id}/sessions"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				sessionID, err := svc.StartSession(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				return map[string]any{"session_id": sessionID}, nil
			},
		},
		{
			spec: mcp.NewTool("run_cell", mcp.WithDescription("Run Python code in a session."),
				sandboxArg, mcp.WithString("session_id", mcp.Required()), mcp.WithString("code", mcp.Required())),
			routes: []string{"POST /v1/sandboxes/{id}/sessions/{sid}/execute"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				sessionID, e2 := req.RequireString("session_id")
				code, e3 := req.RequireString("code")
				if err := firstErr(e1, e2, e3); err != nil {
					return nil, err
				}
				res, err := svc.RunCell(ctx, actor, id, sessionID, code)
				if err != nil {
					return nil, err
				}
				return map[string]any{"exit_code": res.ExitCode, "stdout": string(res.Stdout), "stderr": string(res.Stderr)}, nil
			},
		},
		{
			spec: mcp.NewTool("close_session", mcp.WithDescription("Close a Python session."),
				sandboxArg, mcp.WithString("session_id", mcp.Required())),
			routes: []string{"DELETE /v1/sandboxes/{id}/sessions/{sid}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				sessionID, e2 := req.RequireString("session_id")
				if err := firstErr(e1, e2); err != nil {
					return nil, err
				}
				return nil, svc.CloseSession(ctx, actor, id, sessionID)
			},
		},
		{
			spec: mcp.NewTool("create_volume", mcp.WithDescription("Attach a durable volume to a sandbox at /workspace/<name>. A volume with that name is reattached with its data."),
				sandboxArg, mcp.WithString("name", mcp.Required()), mcp.WithNumber("size_gb", mcp.Required())),
			routes: []string{"POST /v1/volumes"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, e1 := sandbox(req)
				name, e2 := req.RequireString("name")
				size, e3 := req.RequireInt("size_gb")
				if err := firstErr(e1, e2, e3); err != nil {
					return nil, err
				}
				vol, err := svc.CreateVolume(ctx, actor, id, name, int64(size))
				if err != nil {
					return nil, err
				}
				return volumeView(vol), nil
			},
		},
		{
			spec:   mcp.NewTool("list_volumes", mcp.WithDescription("List the volumes attached to a sandbox."), sandboxArg),
			routes: []string{"GET /v1/volumes"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				vols, err := svc.ListVolumes(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(vols))
				for _, v := range vols {
					out = append(out, volumeView(v))
				}
				return map[string]any{"volumes": out}, nil
			},
		},
		{
			spec: mcp.NewTool("delete_volume", mcp.WithDescription("Delete a volume and its data. This cannot be undone."),
				mcp.WithString("volume_id", mcp.Required())),
			routes: []string{"DELETE /v1/volumes/{vid}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := req.RequireString("volume_id")
				if err != nil {
					return nil, err
				}
				return nil, svc.DeleteVolume(ctx, actor, id)
			},
		},
		{
			spec: mcp.NewTool("create_snapshot", mcp.WithDescription("Save the memory and disk state of a running sandbox under a name. The sandbox keeps running. Start sandboxes from it with the snapshot argument of create_sandbox."),
				sandboxArg, mcp.WithString("name", mcp.Required(), mcp.Description("unique within the tenant: lower-case letters, digits, dot, underscore, hyphen"))),
			routes: []string{"POST /v1/sandboxes/{id}/snapshots"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				name, err := req.RequireString("name")
				if err != nil {
					return nil, err
				}
				snap, err := svc.CreateSnapshot(ctx, actor, id, name)
				if err != nil {
					return nil, err
				}
				return snapshotView(snap), nil
			},
		},
		{
			spec:   mcp.NewTool("list_snapshots", mcp.WithDescription("List the snapshots of the tenant, oldest first.")),
			routes: []string{"GET /v1/snapshots"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, _ reqT) (any, error) {
				snaps, err := svc.ListSnapshots(ctx, actor)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(snaps))
				for _, s := range snaps {
					out = append(out, snapshotView(s))
				}
				return map[string]any{"snapshots": out}, nil
			},
		},
		{
			spec:   mcp.NewTool("get_snapshot", mcp.WithDescription("Get one snapshot."), mcp.WithString("snapshot_id", mcp.Required())),
			routes: []string{"GET /v1/snapshots/{snapid}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := req.RequireString("snapshot_id")
				if err != nil {
					return nil, err
				}
				snap, err := svc.GetSnapshot(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				return snapshotView(snap), nil
			},
		},
		{
			spec: mcp.NewTool("delete_snapshot", mcp.WithDescription("Delete a snapshot. Sandboxes already started from it keep running. This cannot be undone."),
				mcp.WithString("snapshot_id", mcp.Required())),
			routes: []string{"DELETE /v1/snapshots/{snapid}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := req.RequireString("snapshot_id")
				if err != nil {
					return nil, err
				}
				return nil, svc.DeleteSnapshot(ctx, actor, id)
			},
		},
		{
			spec:   mcp.NewTool("capabilities", mcp.WithDescription("List what the runtime of this deployment supports. Check it before using an optional tool.")),
			routes: []string{"GET /v1/capabilities"},
			run: func(_ ctxT, svc svcT, _ app.Actor, _ reqT) (any, error) {
				driver, caps := svc.Capabilities()
				out := make([]map[string]any, 0, len(caps))
				for _, c := range caps {
					out = append(out, map[string]any{"name": string(c.Name), "supported": c.Supported})
				}
				return map[string]any{"driver": driver, "capabilities": out}, nil
			},
		},
		{
			spec:   mcp.NewTool("list_templates", mcp.WithDescription("List the sandbox templates the caller can use.")),
			routes: []string{"GET /v1/templates"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, _ reqT) (any, error) {
				templates, err := svc.ListTemplates(ctx, actor)
				if err != nil {
					return nil, err
				}
				out := make([]map[string]any, 0, len(templates))
				for _, t := range templates {
					out = append(out, templateView(t))
				}
				return map[string]any{"templates": out}, nil
			},
		},
		{
			spec: mcp.NewTool("get_template", mcp.WithDescription("Get one sandbox template."),
				mcp.WithString("template_id", mcp.Required())),
			routes: []string{"GET /v1/templates/{id}"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := req.RequireString("template_id")
				if err != nil {
					return nil, err
				}
				t, err := svc.GetTemplate(ctx, actor, id)
				if err != nil {
					return nil, err
				}
				return templateView(t), nil
			},
		},
		{
			spec: mcp.NewTool("browser_scroll", mcp.WithDescription("Scroll the sandbox browser by (dx, dy) with the pointer at (x, y)."),
				sandboxArg, mcp.WithNumber("x"), mcp.WithNumber("y"), mcp.WithNumber("dx"), mcp.WithNumber("dy")),
			routes: []string{"POST /v1/sandboxes/{id}/browser/input"},
			run: func(ctx ctxT, svc svcT, actor app.Actor, req reqT) (any, error) {
				id, err := sandbox(req)
				if err != nil {
					return nil, err
				}
				return nil, svc.BrowserScroll(ctx, actor, id, req.GetFloat("x", 0), req.GetFloat("y", 0), req.GetFloat("dx", 0), req.GetFloat("dy", 0))
			},
		},
	}
}

// handler adapts a data tool to the MCP server: it applies the same scope gate
// as every other tool, runs it, and turns the outcome into a tool result. An
// error of the service is a tool error the model can read, not a protocol error.
func (h *handlers) handler(t tool) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		actor, deny := actorFor(ctx, t.spec.Name)
		if deny != nil {
			return deny, nil
		}
		out, err := t.run(ctx, h.svc, actor, req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		switch v := out.(type) {
		case nil:
			return mcp.NewToolResultText("ok"), nil
		case string:
			return mcp.NewToolResultText(v), nil
		default:
			return mcp.NewToolResultJSON(v)
		}
	}
}
