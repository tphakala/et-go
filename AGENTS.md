# AGENTS.md

Guidance for coding agents working in this repository. The README is the user-facing contract; this
file covers how the code is organised and the conventions a change is expected to follow.

## What this is

et-go is a native Eternal Terminal client in pure Go. It bootstraps a session by running the system
`ssh` to start `etterminal` on the server, then speaks Eternal Terminal protocol version 6 to
`etserver` over TCP, and reconnects transparently when the network drops.

Module path: `github.com/tphakala/et-go`. Go version: see `go.mod`.

## Layout

| Path                | Role                                                                                                                                                                                  |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/et`            | Entry point and flag parsing.                                                                                                                                                         |
| `internal/protocol` | Wire messages generated from upstream's `.proto` files (opaque API), plus `Header`, `Version` and `Packet`. Regenerate with `go generate ./internal/protocol` (needs protoc 3.21.12). |
| `internal/seal`     | One direction of the libsodium-compatible encrypted stream: secretbox with a counter nonce.                                                                                           |
| `internal/wire`     | Handshake message framing, stream frame framing, packet layout and size limits.                                                                                                       |
| `rules/`            | ruleguard matchers used by golangci-lint (build tag `ruleguard`).                                                                                                                     |

Update this table when a package is added.

## Build, test, lint

```bash
go build ./...
go vet ./...
go test ./... -race
golangci-lint run                  # uses .golangci.yaml; CI pins the version in .github/workflows/ci.yml
GOOS=windows golangci-lint run     # platform code is split by build tags; lint both sides
go build -tags ruleguard ./rules/  # a broken rule otherwise compiles clean and silently disables ruleguard
```

The client must stay pure Go: every target builds with `CGO_ENABLED=0`.

## Conventions

- **Wire compatibility is the contract.** The reference is upstream Eternal Terminal. A comment or
  doc sentence describing what the server does states how it is known:
  `MEASURED against etserver X.Y.Z`, or a citation of the upstream source file. If it was not
  measured or read, do not write it as fact.
- **Secrets never reach logs.** The session passkey and anything derived from it are never logged,
  printed or included in error text.
- **Platform splits** use file suffixes and build tags: `_windows.go`, and `_unix.go` carrying
  `//go:build unix`.
- **Tests must be able to fail.** For each new test, name the production line whose removal should
  turn it red, remove it, and watch the test fail on an assertion.
- **Counts go stale.** Prefer an invariant to a count in comments and docs.
- **Prose style:** no em or en dashes in code, comments, docs or commit messages.

## Commits and pull requests

- Conventional commit subjects: `fix:`, `feat:`, `refactor:`, `docs:`, `test(scope):`, `chore(deps):`.
- Describe what the change does and why, and how it was verified (tests, vet, lint).
- Local design notes and plans are never committed (`.gitignore` covers `/docs/`, `/specs/`,
  `/plans/`, `/notes/`, `*-design.md`, `*-plan.md`).
