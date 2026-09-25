package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/tphakala/et-go/internal/console"
	"github.com/tphakala/et-go/internal/protocol"
)

// fakeTransport scripts the server side. Packets pushed into in are returned
// by ReadPacket in order; closing in makes ReadPacket return io.EOF, like
// etcp after the server ended the session. pause makes WritePacket block
// until the returned release func is called, like etcp during an outage.
type fakeTransport struct {
	in      chan protocol.Packet
	readErr chan error

	mu   sync.Mutex
	sent []protocol.Packet
	gate chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{in: make(chan protocol.Packet, 128), readErr: make(chan error, 1)}
}

func (f *fakeTransport) ReadPacket(ctx context.Context) (protocol.Packet, error) {
	select {
	case p, ok := <-f.in:
		if !ok {
			return protocol.Packet{}, fmt.Errorf("fake: %w", io.EOF)
		}
		return p, nil
	case err := <-f.readErr:
		return protocol.Packet{}, err
	case <-ctx.Done():
		return protocol.Packet{}, ctx.Err()
	}
}

func (f *fakeTransport) WritePacket(ctx context.Context, p protocol.Packet) error {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, protocol.Packet{Header: p.Header, Payload: slices.Clone(p.Payload)})
	return nil
}

// pause blocks WritePacket until release is called.
//
//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTransport) pause() (release func()) {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.gate = nil
		f.mu.Unlock()
		close(gate)
	}
}

// sentWith returns the packets written with header h, in order.
func (f *fakeTransport) sentWith(h protocol.Header) []protocol.Packet {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []protocol.Packet
	for _, p := range f.sent {
		if p.Header == h {
			out = append(out, p)
		}
	}
	return out
}

// sentInput concatenates the keyboard bytes written in TERMINAL_BUFFER packets.
//
//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTransport) sentInput(t *testing.T) []byte {
	t.Helper()
	var all bytes.Buffer
	for _, p := range f.sentWith(protocol.HeaderTerminalBuffer) {
		all.Write(decodeBuffer(t, p))
	}
	return all.Bytes()
}

// sentSizes decodes the TERMINAL_INFO packets written so far.
//
//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTransport) sentSizes(t *testing.T) []console.Size {
	t.Helper()
	infos := f.sentWith(protocol.HeaderTerminalInfo)
	out := make([]console.Size, 0, len(infos))
	for _, p := range infos {
		ti := &protocol.TerminalInfo{}
		if err := proto.Unmarshal(p.Payload, ti); err != nil {
			t.Fatalf("decode TERMINAL_INFO: %v", err)
		}
		out = append(out, console.Size{
			Rows: int(ti.GetRow()), Cols: int(ti.GetColumn()),
			Width: int(ti.GetWidth()), Height: int(ti.GetHeight()),
		})
	}
	return out
}

//nolint:unused // consumed by task 4's terminal service tests
func decodeBuffer(t *testing.T, p protocol.Packet) []byte {
	t.Helper()
	tb := &protocol.TerminalBuffer{}
	if err := proto.Unmarshal(p.Payload, tb); err != nil {
		t.Fatalf("decode TERMINAL_BUFFER: %v", err)
	}
	return tb.GetBuffer()
}

// serverOutput builds a TERMINAL_BUFFER packet as the server would send it.
func serverOutput(t *testing.T, s string) protocol.Packet {
	t.Helper()
	tb := &protocol.TerminalBuffer{}
	tb.SetBuffer([]byte(s))
	b, err := proto.Marshal(tb)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: b}
}

// exitStatus builds a TERMINAL_EXIT_STATUS packet.
//
//nolint:unused // consumed by task 4's terminal service tests
func exitStatus(t *testing.T, code int32) protocol.Packet {
	t.Helper()
	st := &protocol.TerminalExitStatus{}
	st.SetExitcode(code)
	b, err := proto.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Packet{Header: protocol.HeaderTerminalExitStatus, Payload: b}
}

//nolint:unused // consumed by task 4's terminal service tests
var errFakeClosed = errors.New("fake terminal: closed")

// fakeTerminal is a scripted local terminal. Keyboard input is fed through
// type, size changes through resize; everything written is kept in out.
//
//nolint:unused // consumed by task 4's terminal service tests
type fakeTerminal struct {
	keys    chan []byte
	sizes   chan console.Size
	closed  chan struct{}
	closeMu sync.Once
	size    console.Size
	rest    []byte // unread tail of the current key chunk (reader goroutine only)

	mu  sync.Mutex
	out bytes.Buffer
}

//nolint:unused // consumed by task 4's terminal service tests
func newFakeTerminal(size console.Size) *fakeTerminal {
	return &fakeTerminal{
		keys:   make(chan []byte),
		sizes:  make(chan console.Size),
		closed: make(chan struct{}),
		size:   size,
	}
}

//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) Read(p []byte) (int, error) {
	if len(f.rest) == 0 {
		select {
		case b, ok := <-f.keys:
			if !ok {
				return 0, io.EOF
			}
			f.rest = b
		case <-f.closed:
			return 0, errFakeClosed
		}
	}
	n := copy(p, f.rest)
	f.rest = f.rest[n:]
	return n, nil
}

//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) Write(p []byte) (int, error) {
	select {
	case <-f.closed:
		return 0, errFakeClosed
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.Write(p)
}

//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) Close() error {
	f.closeMu.Do(func() { close(f.closed) })
	return nil
}

//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) Size() (console.Size, error) { return f.size, nil }

//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) Resizes(ctx context.Context) iter.Seq[console.Size] {
	return func(yield func(console.Size) bool) {
		for {
			select {
			case sz := <-f.sizes:
				if !yield(sz) {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

// typeKeys feeds keyboard input; it blocks until the reader takes it.
//
//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) typeKeys(s string) { f.keys <- []byte(s) }

//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.String()
}

//nolint:unused // consumed by task 4's terminal service tests
func (f *fakeTerminal) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}
