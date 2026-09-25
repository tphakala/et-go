package protocol

// The generated code is committed, so a normal build needs neither protoc nor
// the generators. Regenerate with `go generate ./internal/protocol` after
// changing a .proto file or bumping protoc-gen-go. The generated headers record
// the protoc version, so use protoc 3.21.12 (the version CI pins) or the CI
// freshness check reports a diff. The protoc step needs a POSIX shell.

//go:generate sh -c "protoc -I proto --plugin=protoc-gen-go=\"$(go tool -n protoc-gen-go)\" --go_out=. --go_opt=paths=source_relative --go_opt=default_api_level=API_OPAQUE --go_opt=MET.proto=github.com/tphakala/et-go/internal/protocol --go_opt=METerminal.proto=github.com/tphakala/et-go/internal/protocol ET.proto ETerminal.proto"
//go:generate go tool stringer -type=Header -trimprefix=Header
