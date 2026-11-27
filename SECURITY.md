# Security policy

LoopSandbox exists to run untrusted code, so a way out of a sandbox or across tenants is the most
serious kind of bug the project can have.

## Reporting a vulnerability

Do not open a public issue. Write to admin@getloopscale.com with:

- what is affected (API, worker, a runtime driver, an SDK, a deployment file);
- the version or commit you tested;
- steps to reproduce, or a proof of concept;
- the impact you expect, for example sandbox escape, access to another tenant's data or denial of
  service.

The maintainers confirm that they received the report, tell you whether they could reproduce it,
and agree a disclosure date with you. The fix and an advisory are published together, and you are
credited unless you ask not to be.

## Supported versions

The project has no tagged release yet. Security fixes are applied to the `main` branch.

## What counts as a vulnerability

[docs/threat-model.md](docs/threat-model.md) states what is defended, against whom, and what is
known to be weak.

In scope:

- code running in a sandbox that reaches the host, another sandbox or the control plane;
- one tenant reading or changing another tenant's sandboxes, files, volumes or templates;
- bypassing authentication, API key scopes or plan quotas;
- bypassing the egress policy from inside a sandbox.

Known limits that are not vulnerabilities:

- The `fake` driver provides no isolation. It keeps sandboxes in the memory of the API process and
  exists for tests and local development.
- `deployments/compose/self-host.yml` mounts the host Docker socket into the API. Whoever
  controls the API controls the host. For code from tenants you do not trust, use the
  `firecracker` driver on a host with KVM.
