module github.com/tphakala/et-go

go 1.27.0

require (
	github.com/creack/pty v1.1.24 // test-only: imported by test/e2e under the e2e build tag
	github.com/quasilyte/go-ruleguard/dsl v0.3.23
	golang.org/x/crypto v0.58.0
	golang.org/x/sys v0.49.0
	golang.org/x/term v0.47.0
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
)

tool (
	golang.org/x/tools/cmd/stringer
	google.golang.org/protobuf/cmd/protoc-gen-go
)
