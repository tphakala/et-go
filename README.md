<p align="center">
  <a href="https://github.com/tphakala/et-go/actions/workflows/ci.yml"><img src="https://github.com/tphakala/et-go/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/tphakala/et-go/actions/workflows/codeql.yml"><img src="https://github.com/tphakala/et-go/actions/workflows/codeql.yml/badge.svg" alt="CodeQL"></a>
  <a href="https://github.com/tphakala/et-go/tags"><img src="https://img.shields.io/github/v/tag/tphakala/et-go?label=version&sort=semver" alt="Version"></a>
  <a href="https://pkg.go.dev/github.com/tphakala/et-go"><img src="https://pkg.go.dev/badge/github.com/tphakala/et-go.svg" alt="Go Reference"></a>
  <a href="https://codecov.io/gh/tphakala/et-go"><img src="https://codecov.io/gh/tphakala/et-go/branch/main/graph/badge.svg" alt="Coverage"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/tphakala/et-go" alt="License: Apache-2.0"></a>
</p>

A native [Eternal Terminal](https://eternalterminal.dev/) client written in Go. It runs on Windows without WSL, Cygwin or a C++ toolchain, and also builds for Linux and macOS. It is a single static binary with no runtime dependencies beyond an OpenSSH client.

> Status: early development. The repository is scaffolded; the client is not functional yet.

## Why

Eternal Terminal (ET) is a remote shell that survives network changes and sleep: when the connection drops, the client reconnects and replays anything that was lost in transit, and the remote session never notices. The official client is C++ and needs a CMake and vcpkg build on Windows. `et-go` speaks the same wire protocol (version 6) from pure Go, so it cross-compiles to every platform, Windows on ARM included, from one command.

## Requirements

- An OpenSSH client (`ssh`) on `PATH`. It is used once per session to start `etterminal` on the server, exactly like the official client. Windows 10 and 11 ship one.
- A server running the official `etserver` (default TCP port 2022) with `etterminal` on the remote `PATH`.

## Install

### Go install

```bash
go install github.com/tphakala/et-go/cmd/et@latest
```

### Prebuilt binaries

Tagged releases publish archives for Windows, Linux and macOS (amd64 and arm64) on the [releases page](https://github.com/tphakala/et-go/releases).

## Usage

```text
et [user@]host[:port]
```

## Development

```bash
go build ./...
go vet ./...
go test ./... -race
golangci-lint run
go build -tags ruleguard ./rules/
```

CI builds and vets on Linux, macOS and Windows, cross-compiles every release target with `CGO_ENABLED=0`, runs the race-enabled test suite on Linux and macOS, and runs the test suite without the race detector on Windows.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
