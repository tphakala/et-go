// Package session runs one Eternal Terminal session over a transport: the
// start handshake, a router that splits the incoming packet stream by header,
// and the services that handle those packets. The interactive terminal is the
// first service; port forwarding is meant to become the second (spec 5.9).
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tphakala/et-go/internal/console"
	"github.com/tphakala/et-go/internal/protocol"
)

// Transport is what session needs from the connection. ReadPacket must return
// an error wrapping io.EOF when the server has ended the session; that is how
// Run tells a normal end (the remote shell exited) from a failure. *etcp.Conn
// satisfies this: its etcp.ErrSessionEnded wraps io.EOF.
type Transport interface {
	ReadPacket(ctx context.Context) (protocol.Packet, error)
	WritePacket(ctx context.Context, p protocol.Packet) error
}

// Terminal is what session needs from the local console; *console.Console
// implements it. Close must unblock a pending Read. Session takes this
// interface rather than *console.Console, and uses only console.Size from the
// console package (declared in the portable console.go), so it keeps building
// for js/wasm and wasip1/wasm, where *console.Console does not exist.
type Terminal interface {
	io.ReadWriteCloser
	Size() (console.Size, error)
	Resizes(ctx context.Context) iter.Seq[console.Size]
}

// Options configures a session. Each field maps onto upstream's InitialPayload
// or a client feature. A zero field always means "feature off", so adding a
// field breaks no caller.
type Options struct {
	// Terminal is the local terminal. Nil runs the session without one
	// (upstream -N), which becomes useful once port forwarding exists.
	Terminal Terminal
	// Logger receives session events. Nil discards them.
	Logger *slog.Logger
}

// ExitError reports the remote shell's exit status, when the server sent one.
// Upstream master forwards TERMINAL_EXIT_STATUS only to a client that set
// supports_exit_status (src/terminal/TerminalServer.cpp:594-595 at upstream
// master b834d6ebb; lines 278-279 do the same on the jumphost path). The
// et-v7.0.0 proto/ETerminal.proto has no such field and no
// TERMINAL_EXIT_STATUS header, so a 7.0.0 server never sends one.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("session: remote shell exited with status %d", e.Code)
}

var (
	// ErrDetached is returned by Run when the user typed the local escape.
	ErrDetached = errors.New("session: detached by local escape")
	// ErrStartTimeout is returned by Start when INITIAL_RESPONSE does not arrive in time.
	ErrStartTimeout = errors.New("session: no INITIAL_RESPONSE from server")
	// ErrStartRejected is returned by Start when INITIAL_RESPONSE carries an error.
	ErrStartRejected = errors.New("session: server rejected the session start")
)

// MaxInputPacket bounds the payload of one TERMINAL_BUFFER packet sent to the
// server, so a large paste becomes several modest packets.
const MaxInputPacket = 64 << 10

// startTimeout is how long Start waits for INITIAL_RESPONSE.
const startTimeout = 5 * time.Second

// errEnded is the internal cause for a normal end without an exit status.
var errEnded = errors.New("session: ended by server")

// Start sends INITIAL_PAYLOAD built from opts and waits for INITIAL_RESPONSE.
// Call it before switching the console to raw mode, so an error prints normally.
func Start(ctx context.Context, t Transport, opts Options) error {
	payload := &protocol.InitialPayload{}
	// Upstream master sends TERMINAL_EXIT_STATUS only when this is set
	// (src/terminal/TerminalServer.cpp:594-595 at b834d6ebb). et-v7.0.0's
	// proto/ETerminal.proto has no supports_exit_status field, so a 7.0.0
	// server skips it as an unknown field and never sends the status (see
	// internal/protocol/protocol.go on commit 6f53869).
	payload.SetSupportsExitStatus(true)
	b, err := proto.Marshal(payload)
	if err != nil {
		return fmt.Errorf("session: encode initial payload: %w", err)
	}
	if err := t.WritePacket(ctx, protocol.Packet{Header: protocol.HeaderInitialPayload, Payload: b}); err != nil {
		return fmt.Errorf("session: send initial payload: %w", err)
	}

	rctx, cancel := context.WithTimeoutCause(ctx, startTimeout, ErrStartTimeout)
	defer cancel()
	p, err := t.ReadPacket(rctx)
	if err != nil {
		if errors.Is(context.Cause(rctx), ErrStartTimeout) {
			return ErrStartTimeout
		}
		return fmt.Errorf("session: wait for initial response: %w", err)
	}
	if p.Header != protocol.HeaderInitialResponse {
		return fmt.Errorf("session: expected INITIAL_RESPONSE, got %v", p.Header)
	}
	resp := &protocol.InitialResponse{}
	if err := proto.Unmarshal(p.Payload, resp); err != nil {
		return fmt.Errorf("session: decode initial response: %w", err)
	}
	if msg := resp.GetError(); msg != "" {
		return fmt.Errorf("%w: %s", ErrStartRejected, msg)
	}
	return nil
}
