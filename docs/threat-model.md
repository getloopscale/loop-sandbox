# Threat model

LoopSandbox runs code that nobody reviewed, for many tenants, on shared hosts. This page states
what it defends, against whom, where each boundary is, and what is known to be weak. Report a way
around any of it as described in [SECURITY.md](../SECURITY.md).

## What is protected

- The host: its kernel, its files, the container runtime and the credentials on it.
- The control plane: the API process, the database, the URL signing key and the cloud credentials
  of the machine.
- Each tenant's sandboxes, files, volumes, usage records and API keys, from every other tenant.
- One sandbox from another sandbox of the same tenant.

## Who attacks

| Adversary | Starts with | Wants |
| --- | --- | --- |
| Code inside a sandbox | Arbitrary code execution as the sandbox user | The host, another sandbox, the network, the control plane |
| Another tenant | A valid API key of their own | Data or sandboxes of a tenant they are not |
| A holder of a leaked credential | An API key or a signed file URL | Whatever that credential allows, for as long as it works |
| A client without credentials | Network access to the API | Any answer other than 401 |

The operator and the host are trusted. A person with root on the host, access to the Docker socket
or access to the database controls everything, and nothing here tries to stop them.

## Boundaries

### Between a sandbox and the host

The boundary depends on the runtime driver. `GET /v1/capabilities` names the driver in use.

| Driver | Boundary | Use it for |
| --- | --- | --- |
| `firecracker` | A KVM virtual machine with its own kernel | Code from parties you do not trust |
| `container` | Linux namespaces, cgroups, seccomp and dropped capabilities, on the host kernel | Code from your own users or agents, where a kernel bug is an accepted risk |
| `container` with `runsc` | The gVisor user-space kernel in front of the host kernel | The same as `container`, with a smaller host kernel surface |
| `fake` | None. Commands run in the API process | Tests only |

A `container` sandbox runs as uid 65534 with every capability dropped, `no-new-privileges`, the
default seccomp profile of the runtime, a read-only root filesystem, no network interface other
than loopback, a private IPC namespace, and limits on memory, CPU and number of processes. The
driver refuses to start when the daemon would run containers without seccomp.

A `firecracker` sandbox has no network device. The host reaches the agent inside it over a
vsock-backed Unix socket.

### Between tenants

Every request carries one API key, which belongs to one tenant. Every read and write in the
service layer is scoped to that tenant, and an object of another tenant answers 404, the same as an
object that does not exist. API keys are stored as hashes. A key has scopes, and a route outside
them answers 403.

### Between the network and the API

Every route under `/v1` needs an API key, except the two health checks and the two signed file
routes. A signed file URL carries an HMAC-SHA256 token that names one tenant, one sandbox, one
path, one operation and an expiry of at most one hour. Each API key has a request rate limit.

## Attacks considered and what stops them

| Attack | What stops it | Checked by |
| --- | --- | --- |
| Reach the container runtime socket from a sandbox | The socket is never mounted in a sandbox | Conformance, `escape` |
| Read cloud credentials from the metadata service | A sandbox has no route to any address | Conformance, `escape` and `network_default_deny` |
| See or signal host processes | PID namespace, or a separate kernel | Conformance, `escape` |
| Read the files of another sandbox | Mount namespace per sandbox, or a separate disk image | Conformance, `escape` |
| Read `/etc/passwd` of the sandbox image, or any path outside `/workspace`, through the file API | Paths are validated in the API and resolved again inside the sandbox, where a symlink that leaves `/workspace` is refused | Conformance, `escape`, `files` and `file_ops` |
| Gain root or a capability in a `container` sandbox | Non-root user, empty capability sets, `no-new-privileges` | Conformance, `escape` |
| Mount a filesystem or change a kernel setting from a `container` sandbox | No `CAP_SYS_ADMIN`, read-only `/proc/sys` and `/sys` | Conformance, `escape` |
| Use up host memory or the process table | cgroup limits; the workspace and `/tmp` are size-bounded tmpfs | Conformance, `escape` |
| Use another tenant's sandbox, volume, API key or watch | Tenant scoping in the service layer | Conformance, cross-tenant cases in every group |
| Forge or alter a signed file URL, or use it for the other operation | HMAC check before any field of the token is read | Conformance, `signed_urls` |
| Flood the API with one key | Token bucket per key, 429 with `Retry-After` | API tests |
| Make the lifecycle webhook call an internal address | The webhook dials through the egress policy, which checks every resolved address at connect time | Unit tests of `internal/egress` |

Run the checks against your own deployment with `scripts/conformance.sh container` or
`scripts/conformance.sh firecracker`. The suite needs only the address of the API and two API
keys, so it also runs against a deployment that is already up.

## Known weaknesses

- The `container` driver shares the host kernel. A kernel vulnerability reachable through the
  default seccomp profile is a way out. Use `firecracker` for code from parties you do not trust.
- The process that runs the `container` driver holds the Docker socket, which is root on the host.
  Today that process is the API itself, so a remote code execution bug in the API is a host
  compromise. Moving the driver out of the process that serves HTTP is planned and not done.
- gVisor is accepted as a runtime name and has not been run under the conformance suite.
- A sandbox has no network at all. There is no allow list of domains yet, so a workload that needs
  the network cannot be given a narrow slice of it.
- A secret is encrypted at rest and no API response, log line or span carries its value, and that
  is where the protection ends. Inside a sandbox that names it the value is an environment
  variable, so the code that runs there can read it and send it wherever that sandbox can reach.
  Whoever is root on the host can read it from the environment of the process. A
  snapshot or a pause taken while a process holds the value writes it to the host disk with the
  rest of the memory. There is no key rotation yet: changing `LOOPSANDBOX_SECRETS_KEY` makes the
  stored secrets unreadable until they are written again.
- Revoking an API key does not revoke the signed file URLs that key requested. They stop working
  when they expire, within one hour, or when the sandbox is destroyed.
- The request rate limit is kept per API replica. With N replicas a key can make N times the
  configured rate.
- A fork bomb in a `container` sandbox is contained by the process limit, and it leaves that
  sandbox unable to start another process. A command sent to it then answers with exit code 128
  and the runtime's error text. Destroying the sandbox still works.
- The usage webhook and the artifact store are configured by the operator and are trusted. The
  usage webhook is not dialed through the egress policy, so that it can point at an internal
  collector.
- Side channels between sandboxes on one host, such as cache timing, are not addressed.
- The escape checks are a list of known techniques. Passing them shows those techniques fail, and
  says nothing about the ones not listed.
