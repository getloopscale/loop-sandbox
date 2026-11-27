# Contributing to LoopSandbox

Bug reports, fixes, features and documentation are all welcome. By taking part you agree to follow
the [Code of Conduct](CODE_OF_CONDUCT.md).

## Before you start

Open an issue before a large change, so the design can be discussed before you write the code.
Small fixes can go straight to a pull request.

Do not report security vulnerabilities in public issues. Follow [SECURITY.md](SECURITY.md).

## Development setup

You need Go 1.26 or newer. Docker is needed for the `container` driver and for the integration
tests. The README has a [quick start](README.md#quick-start) that runs the API locally.

```sh
make test              # unit and end-to-end tests, no Docker needed
make test-integration  # integration tests against a real PostgreSQL (needs Docker)
make ci                # vet, golangci-lint, tests and helm lint
make sdk-test          # TypeScript SDK unit tests
```

`make ci` needs `golangci-lint` and `helm` installed. The Firecracker tests (`make fc-test`) need
`/dev/kvm` and the assets fetched by `make fc-assets`.

Dependencies are vendored. If you change `go.mod`, run `go mod vendor` and commit the result.

## Pull requests

1. Fork the repository and create a branch from `workspace`.
2. Make the change, with tests. A bug fix starts with a test that fails without the fix.
3. Run `make test` and `make vet`.
4. Open the pull request against `workspace`.

Maintainers promote `workspace` to `develop` and `develop` to `main`, so pull requests do not
target those two branches.

What reviewers look for:

- Tests cover the new behavior. Code without a test is treated as not working.
- Errors are returned with context and are never swallowed.
- Code, comments, log messages and commit messages are in English.
- A change that users of the API, the CLI or the SDKs can see has an entry under `## [Unreleased]`
  in [CHANGELOG.md](CHANGELOG.md). Documentation-only changes have none.
- A route added to `internal/api/router.go` is also described in `api/openapi.yaml`.

## Commit messages

Commits follow [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`,
`docs:`, `refactor:`, `test:`, `chore:`. Mark a breaking change with `!`, as in `refactor!:`. The
subject is in the imperative and has at most 72 characters. The body says why the change was made.

## Developer Certificate of Origin

Every commit must be signed off to certify the
[Developer Certificate of Origin](https://developercertificate.org/): you wrote the change or have
the right to submit it under the project license. Add the sign-off with `git commit -s`, which
appends a line like this to the message:

```
Signed-off-by: Your Name <your.email@example.com>
```

## License

Contributions are licensed under the [Apache License 2.0](LICENSE).
