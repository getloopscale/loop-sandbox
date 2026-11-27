# Roadmap

The roadmap lists what the maintainers plan to work on. It carries no dates. Items are tracked as
issues, and anyone can propose a change to this file in a pull request.

## Next

- Give sandboxes a network that is denied by default, with a per-sandbox list of allowed domains.
  Sandboxes have no network at all today.
- Bring code sessions, the terminal and volumes to the `container` driver, so both drivers pass the
  same conformance cases.
- Split the control plane from the node, so the process that serves HTTP holds no Docker socket
  and no microVMs.
- Cut the first tagged release, with a published container image.

## Later

- Apply to the CNCF Sandbox. The application needs a repository with at least six months of
  history, and the review looks at adopters and at maintainers from more than one organization.
- A documentation website.

## Not planned

- An agent framework. LoopSandbox runs the code an agent produces. It does not decide what the
  agent does.
