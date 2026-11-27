# LoopSandbox

LoopSandbox runs untrusted code in isolated sandboxes. It is a control plane written in Go: an HTTP
API creates sandboxes, runs commands in them, moves files in and out, and destroys them when their
TTL expires. AI agents are the main consumer, so the same operations are also exposed as MCP tools
and through TypeScript and Python SDKs.

## What is in the repository

| Path | Contents |
| --- | --- |
| `cmd/api` | The HTTP API. It also serves the MCP endpoint and `/metrics`. |
| `cmd/worker` | Background process that enforces TTLs, reaps orphaned sandboxes and drains metering events. |
| `cmd/lsb` | User CLI: creates, lists, runs commands in, connects a terminal to, reads the logs of and destroys sandboxes, with only an API key. |
| `cmd/sandboxctl` | Admin CLI: creates tenants and API keys, renders per-tenant Kubernetes manifests, builds Firecracker templates. |
| `cmd/lsb-agent` | Agent that runs inside a Firecracker guest and serves exec and file requests over vsock. |
| `internal/` | Domain, application services, runtime drivers, persistence, queue, auth. |
| `api/openapi.yaml` | OpenAPI description of the core REST surface. |
| `migrations/` | SQL migrations, embedded in the API binary and applied at startup. |
| `sdk/javascript`, `sdk/python` | Client SDKs. |
| `deployments/` | Docker Compose files, a Helm chart and a Terraform reference for EKS on bare metal. |

## Runtime drivers

`LOOPSANDBOX_DRIVER` selects how a sandbox is isolated.

| Driver | Isolation | Requirements |
| --- | --- | --- |
| `fake` (default) | None. Sandboxes and their files live in the memory of the API process. For tests and local API development. | Nothing |
| `container` | A hardened Docker container: non-root user, all capabilities dropped, `no-new-privileges`, read-only root filesystem, no network, CPU, memory and PID limits. | A Docker daemon |
| `firecracker` | A Firecracker microVM, with exec and file operations served by `lsb-agent` over vsock. | `/dev/kvm` and the assets fetched by `make fc-assets` |

The standalone worker accepts `fake` and `container`. The `firecracker` driver keeps its microVMs in
the memory of the API process, so with that driver the API runs the worker loops itself and you
start no separate worker.

Drivers do not support the same operations. `GET /v1/capabilities` reports what the running
deployment supports, and a route outside that list answers 501. The table is the result of
`make conformance` on 2026-11-20:

| Capability | `container` | `firecracker` |
| --- | --- | --- |
| `exec`, `files` | yes | yes |
| `exec_stream` | yes | yes |
| `file_ops` (stat, mkdir, move) | yes | yes |
| `file_watch` (directory events) | yes, with `LOOPSANDBOX_AGENT_BIN` | yes |
| `processes` (background commands) | yes, with `LOOPSANDBOX_AGENT_BIN` | yes |
| `logs` (output of background processes) | yes, with `LOOPSANDBOX_AGENT_BIN` | yes |
| `metrics` (CPU and memory per sandbox) | yes | yes |
| `sessions` (code interpreter) | yes, with `LOOPSANDBOX_AGENT_BIN` | yes |
| `pty` (terminal) | yes, with `LOOPSANDBOX_AGENT_BIN` | yes |
| `network` (outbound internet with `allow_out` and `deny_out` rules) | yes, with `LOOPSANDBOX_CONTAINER_NETWORK` | no |
| `fork`, `pause` (also automatic, with `auto_pause_seconds`) | no | yes |
| `snapshots` (named, memory and disk) | no | yes |
| `volumes` | no | yes, one per sandbox, mounted at `/workspace/<name>` |
| `session_stream` | no | no |
| `browser` | no | no |

## Quick start

You need Go 1.26 or newer and Docker. The steps below start PostgreSQL in a container and run the
API on the host with the `container` driver.

```sh
docker run -d --name loopsandbox-pg \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=loopsandbox \
  -p 5432:5432 postgres:16-alpine

go build -o bin/api ./cmd/api
go build -o bin/sandboxctl ./cmd/sandboxctl

LOOPSANDBOX_DRIVER=container ./bin/api
```

The API applies the migrations on startup and listens on `:8080`. In another terminal, create a
tenant and an API key. The key is printed once.

```sh
./bin/sandboxctl tenants create --name local
./bin/sandboxctl api-keys create --tenant <tenant-id>
export LSB_API_KEY=<the key that was printed>
```

Create a sandbox, run a command, and destroy it:

```sh
curl -s -X POST http://localhost:8080/v1/sandboxes \
  -H "Authorization: Bearer $LSB_API_KEY" -H 'Content-Type: application/json' \
  -d '{"image":"python:3.12-slim","ttl_seconds":300}'
# {"id":"sbx_...","state":"running",...}

curl -s -X POST http://localhost:8080/v1/sandboxes/<sandbox-id>/executions \
  -H "Authorization: Bearer $LSB_API_KEY" -H 'Content-Type: application/json' \
  -d '{"cmd":["python","-c","print(6*7)"]}'
# {"exit_code":0,"stdout":"42\n","stderr":""}

curl -s -X DELETE http://localhost:8080/v1/sandboxes/<sandbox-id> \
  -H "Authorization: Bearer $LSB_API_KEY"
```

Commands are passed as an argv array and are not run through a shell.

The same steps with the `lsb` CLI, which reads the key from `LOOPSANDBOX_API_KEY` and the address
from `LOOPSANDBOX_URL` (default `http://localhost:8080`):

```sh
go build -o bin/lsb ./cmd/lsb
export LOOPSANDBOX_API_KEY=$LSB_API_KEY

id=$(./bin/lsb sandbox create --image python:3.12-slim --ttl 5m)
./bin/lsb sandbox exec "$id" -- python -c 'print(6*7)'   # prints 42, exits with the command's code
./bin/lsb sandbox connect "$id"                           # interactive terminal
./bin/lsb sandbox logs --follow "$id"                     # output of background processes
./bin/lsb sandbox kill "$id"
```

With the `container` driver a sandbox can only use an image from the built-in template catalog:
`busybox:latest`, `python:3.12-slim`, `node:20-slim`, `jupyter/scipy-notebook:latest` and
`browserless/chrome:latest`. `GET /v1/templates` returns the list.

## API

Every `/v1` route except the health checks requires `Authorization: Bearer <api key>`.

| Area | Routes |
| --- | --- |
| Sandboxes | `POST /v1/sandboxes`, `GET` and `DELETE /v1/sandboxes/{id}`, `GET /v1/sandboxes/{id}/metrics` |
| Commands | `POST /v1/sandboxes/{id}/executions`, `GET /v1/sandboxes/{id}/pty` (WebSocket) |
| Files | `GET /v1/sandboxes/{id}/files`, `PUT`, `GET` and `DELETE /v1/sandboxes/{id}/files/{path}`, `GET /v1/sandboxes/{id}/artifact-url/{path}`, `GET /v1/sandboxes/{id}/file-info/{path}`, `PUT /v1/sandboxes/{id}/directories/{path}`, `POST /v1/sandboxes/{id}/file-moves`, `POST /v1/sandboxes/{id}/watches`, `GET /v1/sandboxes/{id}/watches/{wid}/events`, `DELETE /v1/sandboxes/{id}/watches/{wid}`, `POST /v1/sandboxes/{id}/file-urls`, `GET` and `PUT /v1/signed-files/{token}` |
| Background processes | `POST` and `GET /v1/sandboxes/{id}/processes`, `GET /v1/sandboxes/{id}/processes/{pid}` and `/output`, `POST` and `DELETE /v1/sandboxes/{id}/processes/{pid}/stdin`, `POST /v1/sandboxes/{id}/processes/{pid}/signal`, `GET /v1/sandboxes/{id}/logs` |
| Code sessions | `POST /v1/sandboxes/{id}/sessions`, `POST /v1/sandboxes/{id}/sessions/{sid}/execute`, `DELETE /v1/sandboxes/{id}/sessions/{sid}` |
| Lifecycle | `POST /v1/sandboxes/{id}/fork`, `/pause`, `/resume` |
| Browser | `POST /v1/sandboxes/{id}/browser/navigate` and `/input`, `GET /v1/sandboxes/{id}/browser/screenshot` and `/dom` |
| Snapshots | `POST /v1/sandboxes/{id}/snapshots`, `GET /v1/snapshots`, `GET` and `DELETE /v1/snapshots/{snapid}`; start from one with `"snapshot"` in `POST /v1/sandboxes` |
| Secrets | `GET /v1/secrets`, `PUT` and `DELETE /v1/secrets/{name}` (scope `secrets:manage`); give them to a sandbox with `"secrets"` in `POST /v1/sandboxes` |
| Volumes | `POST` and `GET /v1/volumes`, `DELETE /v1/volumes/{vid}` |
| Templates | `GET` and `POST /v1/templates`, `GET /v1/templates/{id}` |
| Operations | `GET /v1/healthz`, `GET /v1/readyz`, `GET /metrics` (Prometheus) |

`api/openapi.yaml` describes every route. A test compares it with the route table in
`internal/api/router.go`, so the two cannot drift.

`POST /v1/sandboxes` accepts an `Idempotency-Key` header. `artifact-url` returns 501 unless an
artifact bucket is configured.

### MCP

The API serves an MCP endpoint at `/mcp/sandbox`, authenticated with the same API key. It exposes
49 tools that cover the REST API: sandboxes, commands, files, background processes, directory
watches, logs, metrics, code sessions, fork, pause, snapshots, volumes, templates, capabilities and
the browser. A tool is listed and callable only when the key has the scope that grants it.

Fourteen routes have no tool on purpose, among them the terminal WebSocket, the management of API
keys and of secrets, and signed file URLs. `internal/mcp/tools.go` lists each with its reason, and a test fails when a route
has neither a tool nor a reason.

## SDKs

Both SDKs are built from this repository.

TypeScript (`sdk/javascript`, Node 18 or newer). Build it with `make sdk`.

```ts
import { LoopSandbox } from 'loopsandbox';

const client = new LoopSandbox({ baseUrl: 'http://localhost:8080', apiKey: process.env.LSB_API_KEY });
const sandbox = await client.sandboxes.create({ image: 'busybox:latest', ttlSeconds: 300 });
await sandbox.files.write('/workspace/hello.txt', 'hello\n');
const result = await sandbox.commands.run(['cat', '/workspace/hello.txt']);
console.log(result.stdout);
await sandbox.destroy();
```

Python (`sdk/python`, Python 3.9 or newer, no runtime dependencies). Install it with
`pip install ./sdk/python`.

```python
import os
from loopsandbox import LoopSandbox

client = LoopSandbox(api_key=os.environ["LSB_API_KEY"], base_url="http://localhost:8080")
sandbox = client.create_sandbox(image="python:3.12-slim", ttl_seconds=300)
sandbox.write_file("/workspace/hello.txt", b"hello\n")
result = sandbox.run(["cat", "/workspace/hello.txt"])
print(result.stdout)
sandbox.destroy()
```

The Python SDK also has `run_code` for stateful code sessions, `fork`, and adapters in
`loopsandbox.adapters` that expose a sandbox as OpenAI or LangChain tools.

## Configuration

The API and the worker read their configuration from environment variables.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/loopsandbox?sslmode=disable` | PostgreSQL connection string. |
| `LOOPSANDBOX_ADDR` | `:8080` | API listen address. |
| `LOOPSANDBOX_DRIVER` | `fake` | Runtime driver: `fake`, `container` or `firecracker`. |
| `LOOPSANDBOX_AUTH` | `standalone` | `standalone` authenticates API keys. `loopscale` verifies a signed edge forward plus a JWT and needs the `LOOPSANDBOX_LOOPSCALE_*` variables. `gateway` trusts a gateway that authenticated the caller: see the two rows below. |
| `LOOPSANDBOX_GATEWAY_PUBKEYS` | none | With `LOOPSANDBOX_AUTH=gateway`: comma-separated base64 Ed25519 public keys of the gateway. The sandbox accepts the tenant and the user of a request one of them signed, with the headers `X-Loop-Runtime-{User-ID,Tenant,Timestamp,Nonce,Signature}`. Whoever holds the private key acts as any tenant, so expose the API only to the gateway. |
| `LOOPSANDBOX_GATEWAY_TENANT_PLAN` | `starter` | With `LOOPSANDBOX_AUTH=gateway`: plan of a tenant created on its first request. A tenant that exists keeps its plan. |
| `LOOPSANDBOX_QUEUE` | `pg` | Worker job queue: `pg` (PostgreSQL) or `nats` (NATS JetStream, with `LOOPSANDBOX_NATS_URL`). |
| `LOOPSANDBOX_RECONCILE_INTERVAL` | `10s` | How often the worker looks for expired sandboxes. |
| `LOOPSANDBOX_EMBEDDED_WORKER` | unset | `true` runs the worker loops inside the API process. Always on with the `firecracker` driver. |
| `LOOPSANDBOX_SANDBOX_IMAGE` | `busybox:latest` | Image used by the `container` driver when a request names none. |
| `LOOPSANDBOX_USAGE_WEBHOOK_URL` | unset | Where the worker posts usage events, in batches, as `{"events":[{"event_id","tenant_id","metric","amount","period"}]}`. Unset, they are written to the log. A batch is sent again until the receiver answers 2xx, so the receiver must ignore an `event_id` it has seen. |
| `LOOPSANDBOX_USAGE_WEBHOOK_SECRET` | unset | When set, each usage request carries `X-LoopSandbox-Signature: sha256=<hex HMAC-SHA256 of the body>`. |
| `LOOPSANDBOX_RATE_LIMIT_RPS` | `100` | Requests per second each API key may make, on average. Beyond it the API answers 429 with `Retry-After`. `0` turns the limit off. Each API replica counts on its own. |
| `LOOPSANDBOX_RATE_LIMIT_BURST` | twice the rate | Requests an API key may make at once. |
| `LOOPSANDBOX_SECRETS_KEY` | unset | 64 hexadecimal characters (`openssl rand -hex 32`): the key that encrypts tenant secrets at rest. Unset, the `/v1/secrets` routes answer 501. Every replica needs the same value, and losing it loses the secrets. |
| `LOOPSANDBOX_URL_SIGNING_KEY` | unset | Secret of at least 32 bytes that signs file URLs. Unset, `POST /v1/sandboxes/{id}/file-urls` answers 501. Every replica needs the same value. |
| `LOOPSANDBOX_PUBLIC_URL` | unset | Address clients reach the API at, used in signed file URLs. Unset, the URL is built from the request. |
| `LOOPSANDBOX_AGENT_BIN` | unset; `/lsb-agent` in the published image | Path of a static `lsb-agent` binary. With it, the `container` driver runs the agent as the first process of every sandbox, which enables background processes, directory watches, code sessions and the interactive terminal, and reaps orphaned processes. |
| `LOOPSANDBOX_CONTAINER_NETWORK` | unset | `on`, or a comma-separated list of IPv4 DNS resolvers (the default is `8.8.8.8`). With it, the `container` driver gives every sandbox a network and a second container that enforces its outbound rules. Needs `LOOPSANDBOX_AGENT_BIN`. Unset, a sandbox has no network at all. |
| `LOOPSANDBOX_FC_BIN`, `LOOPSANDBOX_FC_KERNEL`, `LOOPSANDBOX_FC_ROOTFS`, `LOOPSANDBOX_FC_STATEDIR` | none | Firecracker binary, guest kernel, root filesystem and state directory. |
| `LOOPSANDBOX_FC_WARMPOOL` | `0` | Number of pre-booted microVMs kept ready for snapshot restore. |
| `LOOPSANDBOX_ARTIFACT_BUCKET` | none | S3 bucket for artifact URLs. Related: `LOOPSANDBOX_ARTIFACT_REGION`, `_ENDPOINT`, `_ACCESS_KEY`, `_SECRET_KEY`. |
| `LOOPSANDBOX_WEBHOOK_URL` | none | When set, sandbox created, exec and destroyed events are POSTed to this URL. |
| `LOOPSANDBOX_TRIVY_BIN` | none | Path to the `trivy` binary that scans public templates before they are published. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | none | When set, the API and the worker export traces to this OTLP/HTTP collector. The other standard `OTEL_*` variables apply. |
| `LOOPSANDBOX_CELLS` | `cell-0` | Comma-separated list of cells that tenants are routed to. |

## Development

```sh
make test              # unit and end-to-end tests, no Docker needed
make test-integration  # integration tests against a real PostgreSQL (needs Docker)
make conformance       # black-box conformance suite against a driver (DRIVER=container|firecracker)
make load-test         # latency checks under concurrency; run on a quiet machine
make ci                # vet, golangci-lint, tests and helm lint
make sdk-test          # TypeScript SDK unit tests
make sdk-e2e           # boots PostgreSQL and the API, then drives them through the TypeScript SDK
make fc-assets         # fetch Firecracker, a guest kernel and build the lsb-agent rootfs
make fc-test           # Firecracker driver tests against real microVMs (needs /dev/kvm)
make dev               # k3d cluster with PostgreSQL and the API installed through the Helm chart
```

Dependencies are vendored in `vendor/`. The build also works without that directory:
`go build -mod=mod ./...` fetches the modules listed in `go.mod`, all of them public.

## Deployment

- `Dockerfile` builds one image that contains the `api`, `worker`, `sandboxctl` and `lsb` binaries.
- `deployments/compose/self-host.yml` brings up the API and PostgreSQL on one machine, with the
  `container` driver. [docs/guides/self-host.md](docs/guides/self-host.md) goes from an empty
  machine to a command running in a sandbox. `control-plane-ha.yml` and `pg-ha.yml` are short-lived topologies used by
  `scripts/validate-cp-ha.sh` and `scripts/validate-pg-ha.sh` to test API and PostgreSQL failover.
  They are not production deployments.
- `deployments/kubernetes/loopsandbox` is a Helm chart for the API, the worker, PostgreSQL and NATS.
- `deployments/terraform/eks-metal` is a Terraform reference for an EKS node group on bare metal.
  It has been checked with `terraform validate` and has not been applied to a real account.

The `supply-chain` workflow builds the image on pushes to `develop` and on `v*` tags, pushes it to
GHCR, scans it with trivy, generates an SBOM and signs the image with cosign.

## Community

- [CONTRIBUTING.md](CONTRIBUTING.md) explains how to send a change.
- [ROADMAP.md](ROADMAP.md) lists what is planned.
- [GOVERNANCE.md](GOVERNANCE.md) and [MAINTAINERS.md](MAINTAINERS.md) describe who decides and how.
- [SECURITY.md](SECURITY.md) explains how to report a vulnerability.
- [ADOPTERS.md](ADOPTERS.md) lists who uses the project.

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

LoopSandbox is licensed under the [Apache License 2.0](LICENSE).
