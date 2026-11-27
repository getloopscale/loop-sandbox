# Changelog

All notable changes to this project are recorded in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Sandbox network on the `container` driver: with `LOOPSANDBOX_CONTAINER_NETWORK=on`, or a comma-separated list of IPv4 DNS resolvers in its place, every sandbox gets a network interface and can reach the public internet. Private, loopback, link-local and cloud metadata addresses are always refused. Each sandbox gains a second container that holds `CAP_NET_ADMIN` and enforces the policy, and the sandbox itself still has no capability. The setting needs `LOOPSANDBOX_AGENT_BIN`. Sandboxes on the gVisor runtime get no network. The API does not accept outbound rules yet. (#18)
- Tenant secrets: `PUT /v1/secrets/{name}` stores a value encrypted with AES-256-GCM, `GET /v1/secrets` lists names and dates, and `DELETE /v1/secrets/{name}` removes one. No route returns a value. A sandbox created with `secrets` receives the named secrets as environment variables of its commands and background processes, and a replaced value reaches the next command. The routes need the new scope `secrets:manage` and are on when `LOOPSANDBOX_SECRETS_KEY` is set. The SDKs have the same operations, and the MCP tool `create_sandbox` takes `secrets`.
- Automatic pause: `POST /v1/sandboxes` accepts `auto_pause_seconds`, between 10 and 86400. A sandbox that goes that long without a request that uses it is paused, and the next such request resumes it and is served. A request in flight keeps the sandbox running however long it takes. Reading the sandbox record, its logs or its metrics does not count as use. Needs the capability `pause`, so today the `firecracker` driver; the SDKs and the MCP tool `create_sandbox` take the same option.
- Named snapshots, under the new capability `snapshots`: `POST /v1/sandboxes/{id}/snapshots` saves the memory and disk state of a running sandbox, which keeps running, and `POST /v1/sandboxes` with `snapshot` starts a new sandbox from it with its files and running processes. `GET /v1/snapshots`, `GET` and `DELETE /v1/snapshots/{snapid}` list, fetch and delete. A snapshot outlives its sandbox, a tenant keeps at most 20, and a sandbox with a volume attached cannot be snapshotted. Supported by the `firecracker` driver; the TypeScript and Python SDKs and the MCP server have the same operations.
- Usage events can be delivered to an HTTP endpoint: set `LOOPSANDBOX_USAGE_WEBHOOK_URL`, and optionally `LOOPSANDBOX_USAGE_WEBHOOK_SECRET` to sign each request. Delivery is at least once; without the variable the events go to the log as before.
- TypeScript SDK: a method for every API operation, including background processes, directory watches, file info, directory creation and move, signed file URLs, logs, metrics, volumes, pause and resume, fork and batch fork, API keys, capabilities, browser control, the terminal and template registration.
- TypeScript SDK: streamed output of commands and code cells through `commands.stream` and `session.executeStream`; `RateLimitError.retryAfterSeconds`, `QuotaExceededError` and `NotImplementedError`.
- Python SDK: a method for every API operation except the terminal WebSocket, plus `run_stream` and `Session.execute_stream` for streamed output and one typed error per status, including `RateLimitError` with `retry_after_seconds`.
- A test in each SDK compares its methods with the operations of `api/openapi.yaml` and fails when one is missing.
- `lsb`, a user CLI that needs only `LOOPSANDBOX_API_KEY` and `LOOPSANDBOX_URL`: `lsb sandbox create`, `list`, `exec`, `connect`, `logs` and `kill`. `exec` exits with the exit code of the command, and `connect` opens a terminal in the sandbox.
- MCP tools for the rest of the API: sandbox lookup, listing and timeout, fork, pause and resume, file operations, background processes, directory watches, logs, metrics, code sessions, volumes, templates, capabilities and browser scroll. The server now lists 48 tools.
- Batch fork: `POST /v1/sandboxes/{id}/fork` with `{"count": N}`, up to 16, creates N independent forks and answers with the list `sandboxes`. The batch is all or nothing. Without a body the route still answers with a single sandbox.
- Signed file URLs: `POST /v1/sandboxes/{id}/file-urls` returns a URL that uploads or downloads one file of a sandbox without an API key until it expires, by default after 5 minutes and at most after 1 hour. The feature is on when `LOOPSANDBOX_URL_SIGNING_KEY` is set; `LOOPSANDBOX_PUBLIC_URL` sets the address used in the URL.
- `GET /v1/sandboxes/{id}/logs` returns the output of every background process of a sandbox, read from a cursor (capability `logs`), and `GET /v1/sandboxes/{id}/metrics` returns its CPU time and memory use (capability `metrics`). Both drivers support them; on the `container` driver, `logs` needs `LOOPSANDBOX_AGENT_BIN`.
- Directory watches, under the new capability `file_watch`: `POST /v1/sandboxes/{id}/watches` starts recording the changes to a directory, optionally with its subdirectories, and `GET /v1/sandboxes/{id}/watches/{wid}/events` reads them from a cursor. Supported by the `firecracker` driver, and by the `container` driver with `LOOPSANDBOX_AGENT_BIN`.
- Background processes, under the new capability `processes`: `POST /v1/sandboxes/{id}/processes` starts a command and answers at once, and the routes under `/v1/sandboxes/{id}/processes/{pid}` read its output from a cursor, write to and close its standard input, and send it a signal. A client that lost its connection resumes from the last cursor. Supported by the `firecracker` driver, and by the `container` driver with `LOOPSANDBOX_AGENT_BIN`.
- Code sessions and the interactive terminal (`GET /v1/sandboxes/{id}/pty`) on the `container` driver. When `LOOPSANDBOX_AGENT_BIN` names a static `lsb-agent` binary, each new sandbox runs the agent as its first process; the published image sets the variable. A sandbox image needs `python3` to run a session. Sandboxes created before the change keep running without either.
- File operations on the `container` and `firecracker` drivers, under the new capability `file_ops`: `GET /v1/sandboxes/{id}/file-info/{path}` describes a path, `PUT /v1/sandboxes/{id}/directories/{path}` creates a directory with its parents, and `POST /v1/sandboxes/{id}/file-moves` moves or renames a path. The Firecracker rootfs has to be rebuilt with `make fc-rootfs`.
- `GET /v1/api-keys`, `POST /v1/api-keys` and `DELETE /v1/api-keys/{kid}` list, create and revoke the caller's API keys. They need the new scope `api_keys:manage`, and a key can only grant scopes it holds.
- The `container` and `firecracker` drivers stream command output: `POST /v1/sandboxes/{id}/executions?stream=1` sends each piece of output as the command produces it. The Firecracker rootfs has to be rebuilt with `make fc-rootfs`.
- `GET /v1/sandboxes` lists the caller's sandboxes, newest first, with `state` and `metadata` filters and cursor pagination.
- `POST /v1/sandboxes` accepts `metadata` (labels returned as given) and `env` (default environment of every execution in the sandbox).
- `POST /v1/sandboxes/{id}/timeout` replaces the expiry of a live sandbox.
- `sandboxctl tenants create` accepts `--plan starter|pro|team`.
- `GET /v1/capabilities` lists what the active runtime driver supports, so a client can check before calling an optional route.
- Conformance suite (`make conformance DRIVER=container|firecracker`) that runs the same black-box cases against any driver through the HTTP API.
- The API runs the TTL, orphan and metering loops itself when the driver keeps sandboxes in process memory, so sandboxes on the `firecracker` driver now expire; `LOOPSANDBOX_EMBEDDED_WORKER=true` turns the loops on for other drivers.
- Schema migrations are recorded in `schema_migrations` and applied once, each in its own transaction; an applied migration that was edited stops the API at startup.
- Durable volumes attach to a running Firecracker sandbox and are mounted at `/workspace/<name>`; one volume per sandbox.
- Terminal resize works on the `firecracker` driver.
- Traces are exported over OTLP/HTTP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, and a request that carries a `traceparent` header joins the caller's trace.
- `api/openapi.yaml` describes every route, and a test fails when the document and the router differ.

### Changed

- The server, the TypeScript SDK and the Python SDK declare one version, `0.1.0`, and a test fails when they differ. The SDKs carried unpublished numbers of their own, `0.6.0` and `0.21.0`.
- **Breaking:** the TypeScript SDK package is named `loopsandbox`, the same name as the Python package. It was `@loopsandbox/sdk`, which was never published.
- **Breaking:** the integrated authentication mode is named `loopscale`: set `LOOPSANDBOX_AUTH=loopscale`, the variables are `LOOPSANDBOX_LOOPSCALE_*`, and the signed forward headers are `X-Loop-Runtime-User-ID`, `X-Loop-Runtime-Tenant`, `X-Loop-Runtime-Timestamp`, `X-Loop-Runtime-Nonce` and `X-Loop-Runtime-Signature`. The former `theo` mode, `LOOPSANDBOX_THEO_*` variables and `X-Theo-*` headers are gone.
- **Breaking:** the Go module path is `github.com/getloopscale/loop-sandbox`, after the repository was renamed.
- TypeScript SDK: a rate-limited idempotent request waits for the `Retry-After` time before it is retried, and a 501 answer is no longer retried.
- TypeScript SDK: `templates.list` returns registered templates with camelCase fields, so `scan_status` became `scanStatus`.
- Python SDK: `read_file` and `write_file` raise the error that matches the HTTP status. `read_file` used to raise `NotFoundError` for every failure.
- Each API key is limited to 100 requests per second, with bursts of 200. A request beyond that answers 429 with the code `rate_limited` and a `Retry-After` header. `LOOPSANDBOX_RATE_LIMIT_RPS` and `LOOPSANDBOX_RATE_LIMIT_BURST` change the limit, and a rate of `0` turns it off.
- **Breaking:** scopes are enforced. Every route except the API key routes needs the scope `sandbox:execute`, and a key without it gets 403 `forbidden`. Keys created by `sandboxctl api-keys create` have that scope by default.
- `sandboxctl api-keys create` rejects an unknown scope and an empty name.
- A 400 caused by an invalid field now names the field and the rule it broke, instead of the fixed text `invalid argument`.
- A fork carries the metadata and the environment of its parent.
- The API shuts down on SIGINT and SIGTERM after letting in-flight requests finish, instead of being killed mid-request.
- A request for a capability the deployment does not support answers 501 `not_implemented` on every route; some answered 500 or a 200 event stream before.
- The message of a 404 is now `not found`; it read `sandbox not found` even for a missing file, volume or template.
- The Firecracker guest image must be rebuilt with `make fc-rootfs`: the terminal channel moved to vsock port 1026 and volumes are mounted by the agent on request.
- Forking a Firecracker sandbox that has a volume attached answers 409; detach the volume first.
- `deployments/compose/self-host.yml` now runs the API with the `container` driver and no separate worker or NATS, builds the image itself, and binds the API to 127.0.0.1.
- The container image includes `/sandboxctl`.
- The module no longer depends on a private contracts repository; the build works from a clean clone without `vendor/`.
- The worker refuses `LOOPSANDBOX_DRIVER=firecracker` with a message that points to the API, and refuses an unknown driver name instead of falling back to `fake`.

### Removed

- The SBOM files kept in the repository (`sbom/` and `docs/supply-chain/sbom.cdx.json`). They listed a dependency the module no longer has. `make sbom` generates them, and the `supply-chain` workflow attaches one to each published image.

### Fixed

- Two deployments that shared a Docker host destroyed each other's sandboxes: the orphan reaper of the `container` driver listed every managed container on the host and removed those without a record in its own database. Each database now has a random deployment id, new containers carry it as the label `loopsandbox.instance`, and the reaper only looks at containers of its own deployment. Containers created before the upgrade have no label and are no longer reaped as orphans. (#1)
- Python SDK: errors carry the code and message the API sent. They were always `error` and empty, because the SDK read them from the wrong level of the answer.
- TypeScript and Python SDKs: a file path with a space, `?`, `#` or `%` is percent-encoded and no longer produces a broken URL.
- `GET /v1/templates` answered 200 with only the built-in templates when the template registry could not be read; it now answers 500.
- The OpenAPI document listed 11 of the 16 capability names that `GET /v1/capabilities` returns; it now lists all of them, and a test keeps the two in step.
- Starting a code session in a sandbox whose image has no `python3` answered 500. It now answers 400 and names the missing interpreter.
- On the `container` driver, a process orphaned inside a sandbox stayed a zombie until the sandbox was destroyed. With `LOOPSANDBOX_AGENT_BIN` set, the agent reaps it.
- On the `container` driver, writing a file into a directory that did not exist answered 500. The parent directories are now created, as on the `firecracker` driver.
- A process orphaned inside a Firecracker sandbox stayed a zombie for the life of the microVM. The guest agent now runs a minimal init as PID 1 that reaps it.
- `make fc-test` ran only the tests named `TestFirecracker_*` and silently left out the others.
- A fork computed its lifetime from the wall clock instead of the service clock.
- `GET /v1/sandboxes/{id}/files` returned size 0 for every file on the `container` driver.
- `POST /v1/volumes` on the `firecracker` driver reported success without mounting anything in a running sandbox.
- The API never wired the volume store, so volume routes answered 501 on every driver.
- An internal error returned a 500 with no log line; the cause is now logged with the request id.
- The Firecracker driver now rejects a state directory too long for its unix sockets at startup; before, every sandbox creation timed out.
- `api/openapi.yaml` was not valid YAML.

### Security

- The image is built with Go 1.26.11, and `golang.org/x/net`, `golang.org/x/crypto`, `golang.org/x/text`, `google.golang.org/grpc` and `github.com/klauspost/compress` are updated. The image scan had reported 23 high-severity advisories in the `api` binary, among them CVE-2026-111943 in `crypto/x509` and CVE-2026-84304 in gRPC.
- On the `container` driver, a file route followed a symbolic link out of `/workspace`. Every file route now answers 400 for a path that resolves outside the workspace, as the `firecracker` driver already did.
- A forked, resumed or warm-pool Firecracker sandbox gets a block device of its own for the volume slot, so no two sandboxes share a writable device.
- GitHub Actions in the supply-chain workflow are pinned to commit SHAs, and its write permissions are scoped to the one job that publishes.
