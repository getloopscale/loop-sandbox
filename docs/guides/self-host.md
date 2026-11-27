# Self-host on one machine

This guide brings up LoopSandbox with Docker Compose and runs a command in a first
sandbox. Each sandbox is a hardened container on the machine's own Docker daemon.

You need Docker with the Compose plugin, and `curl`.

## What you get, and its limit

The compose file starts PostgreSQL and the API. The API uses the `container` driver
and runs the background loops that enforce sandbox TTLs.

The API reaches Docker through the host's Docker socket, and access to that socket
is root-equivalent control of the host. Use this setup on a machine you own, for
code you would run on it anyway. To run code you do not trust, use the `firecracker`
driver on a host with `/dev/kvm`.

The `container` driver supports sandboxes, commands and files. `GET /v1/capabilities`
lists what a deployment supports.

## 1. Start the stack

From the repository root:

```sh
docker compose -f deployments/compose/self-host.yml up --build -d
```

The first run builds the image. The API listens on `127.0.0.1:8080`; set
`LOOPSANDBOX_PORT` before the command to use another port. Wait until it is ready:

```sh
curl -fsS http://127.0.0.1:8080/v1/readyz
```

```json
{"status":"ready"}
```

## 2. Create a tenant and an API key

The admin CLI is in the API image:

```sh
docker compose -f deployments/compose/self-host.yml exec api /sandboxctl tenants create --name local
```

The output has the tenant id, which starts with `tnt_`. Create a key for it:

```sh
docker compose -f deployments/compose/self-host.yml exec api /sandboxctl api-keys create --tenant <tenant id>
```

The last line of the output is the key. It is shown once; LoopSandbox stores only
its hash. This key can use sandboxes. To also create and revoke keys through
`/v1/api-keys`, add `--scopes sandbox:execute,api_keys:manage` to the command.
Keep the key in a shell variable for the next steps:

```sh
export LSB_API_KEY=<the key>
```

## 3. Create a sandbox and run a command

```sh
curl -fsS -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H "Authorization: Bearer $LSB_API_KEY" \
  -d '{"image":"python:3.12-slim","ttl_seconds":300}'
```

The response has the sandbox `id`. The first sandbox of an image takes longer,
because Docker pulls the image. Run a command in it:

```sh
curl -fsS -X POST http://127.0.0.1:8080/v1/sandboxes/<sandbox id>/executions \
  -H "Authorization: Bearer $LSB_API_KEY" \
  -d '{"cmd":["python","-c","print(6*7)"]}'
```

```json
{"exit_code":0,"stdout":"42\n","stderr":""}
```

The sandbox is destroyed when its TTL ends, or when you delete it:

```sh
curl -fsS -X DELETE http://127.0.0.1:8080/v1/sandboxes/<sandbox id> \
  -H "Authorization: Bearer $LSB_API_KEY"
```

## Stop

```sh
docker compose -f deployments/compose/self-host.yml down
```

The database lives in the `postgres-data` volume and survives `down`. Add `-v` to
delete it too.

## Before exposing it to a network

- Set `POSTGRES_PASSWORD` to a value of your own before the first `up`.
- The API speaks plain HTTP. Put a TLS-terminating reverse proxy in front of it.
- Signed file URLs are off by default. To turn them on, set
  `LOOPSANDBOX_URL_SIGNING_KEY` to a secret of 32 characters or more, for example
  the output of `openssl rand -hex 32`, and `LOOPSANDBOX_PUBLIC_URL` to the
  address of that proxy. A signed URL is a credential for one file until it
  expires, so share it like one.
- Tenant secrets are off by default. To turn them on, set `LOOPSANDBOX_SECRETS_KEY`
  to the output of `openssl rand -hex 32`. The key encrypts every secret in the
  database, so back it up apart from the database: without it the secrets cannot
  be read, and with it and a database dump they can. A key that writes secrets
  needs the scope `secrets:manage`, given with `--scopes`.
- Read [SECURITY.md](../../SECURITY.md) for the trust model of each driver.
