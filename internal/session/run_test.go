package session

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/tphakala/et-go/internal/console"
	"github.com/tphakala/et-go/internal/protocol"
)

// runAsync runs Run in a goroutine and returns a channel with its error.
func runAsync(ctx context.Context, tr Transport, opts Options) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Run(ctx, tr, opts) }()
	return done
}

var size80x24 = console.Size{Rows: 24, Cols: 80}

func TestRunOutputAndExitStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})

		tr.in <- serverOutput(t, "hello ")
		tr.in <- serverOutput(t, "world")
		tr.in <- exitStatus(t, 3)
		close(tr.in)

		err := <-done
		if ee, ok := errors.AsType[*ExitError](err); !ok || ee.Code != 3 {
			t.Fatalf("Run = %v, want *ExitError{3}", err)
		}
		if got := term.output(); got != "hello world" {
			t.Fatalf("terminal got %q, want %q", got, "hello world")
		}
		if !term.isClosed() {
			t.Fatal("Run did not close the terminal")
		}
	})
}

func TestRunEndWithoutExitStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		close(tr.in) // etserver 7.0.0: shell exits, redial gets INVALID_KEY
		if err := <-done; err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}

func TestRunTerminalClose(t *testing.T) {
	tests := []struct {
		name     string
		withCode bool
	}{{"without exit status", false}, {"with exit status", true}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tr := newFakeTransport()
				done := runAsync(t.Context(), tr, Options{Terminal: newFakeTerminal(size80x24)})
				if tt.withCode {
					tr.in <- exitStatus(t, 0)
				}
				tr.in <- protocol.Packet{Header: protocol.HeaderTerminalClose}
				err := <-done
				ee, isExit := errors.AsType[*ExitError](err)
				switch {
				case tt.withCode && (!isExit || ee.Code != 0):
					t.Fatalf("Run = %v, want *ExitError{0}", err)
				case !tt.withCode && err != nil:
					t.Fatalf("Run = %v, want nil", err)
				}
			})
		})
	}
}

func TestRunSkipsUnknownAndKeepAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		tr.in <- protocol.Packet{Header: 99, Payload: []byte{1, 2, 3}}
		tr.in <- protocol.Packet{Header: protocol.HeaderKeepAlive}
		tr.in <- serverOutput(t, "still here")
		close(tr.in)
		if err := <-done; err != nil {
			t.Fatalf("Run = %v", err)
		}
		if got := term.output(); got != "still here" {
			t.Fatalf("terminal got %q", got)
		}
	})
}

func TestRunSendsInitialSizeAndResizes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		synctest.Wait()
		big := console.Size{Rows: 50, Cols: 200, Width: 1600, Height: 900}
		term.sizes <- big
		synctest.Wait()
		close(tr.in)
		<-done
		got := tr.sentSizes(t)
		if len(got) != 2 || got[0] != size80x24 || got[1] != big {
			t.Fatalf("sent sizes %v, want [%v %v]", got, size80x24, big)
		}
	})
}

// A pty whose size was never set reports 0x0, and a 0x0 TERMINAL_INFO breaks
// full-screen programs on the remote side, so zero cell dimensions are sent
// as 80x24 while the pixel dimensions pass through as reported.
func TestRunZeroSizeSentAs80x24(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(console.Size{})
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		synctest.Wait()
		term.sizes <- console.Size{Rows: 0, Cols: 132, Width: 640, Height: 480}
		synctest.Wait()
		term.sizes <- console.Size{Rows: 30, Cols: 0}
		synctest.Wait()
		close(tr.in)
		<-done
		got := tr.sentSizes(t)
		want := []console.Size{
			size80x24,
			{Rows: 24, Cols: 80, Width: 640, Height: 480},
			size80x24,
		}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("sent sizes %v, want %v", got, want)
		}
	})
}

// Review Focus 3: resizes during an outage; the server must end up with the
// latest size, and stale intermediate sizes are not sent after it.
func TestRunResizeDuringOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		release := tr.pause() // outage from the start: the first TERMINAL_INFO blocks
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		synctest.Wait() // the sender is now blocked writing the initial size

		sizes := []console.Size{{Rows: 30, Cols: 100}, {Rows: 40, Cols: 120}, {Rows: 45, Cols: 132}}
		for _, sz := range sizes {
			term.sizes <- sz
		}
		synctest.Wait()
		release()
		synctest.Wait()
		close(tr.in)
		<-done

		got := tr.sentSizes(t)
		want := []console.Size{size80x24, sizes[2]}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("sent sizes %v, want %v", got, want)
		}
	})
}

func TestRunForwardsKeyboardInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		term.typeKeys("echo hi\r")
		term.typeKeys("\x03") // Ctrl+C is a byte in raw mode, forwarded as is
		synctest.Wait()
		close(tr.in)
		<-done
		if got := string(tr.sentInput(t)); got != "echo hi\r\x03" {
			t.Fatalf("sent %q", got)
		}
	})
}

func TestRunDetach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		term.typeKeys("ls\r")
		synctest.Wait() // "ls\r" has been sent
		term.typeKeys("~.")
		if err := <-done; !errors.Is(err, ErrDetached) {
			t.Fatalf("Run = %v, want ErrDetached", err)
		}
		if got := string(tr.sentInput(t)); got != "ls\r" {
			t.Fatalf("sent %q, want %q", got, "ls\r")
		}
		if !term.isClosed() {
			t.Fatal("terminal not closed")
		}
	})
}

// The escape must work while the connection is stalled and input piles up.
func TestRunDetachWhileTransportBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		release := tr.pause()
		defer release()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})

		chunk := bytes.Repeat([]byte("a"), readBufSize)
		for range (2 * maxPending) / readBufSize {
			term.keys <- chunk
		}
		term.typeKeys("\r~.")
		if err := <-done; !errors.Is(err, ErrDetached) {
			t.Fatalf("Run = %v, want ErrDetached", err)
		}
	})
}

// Review Focus 5: a 5 MiB paste is sent byte-exact, in order, in packets of
// at most MaxInputPacket bytes. The first MiB arrives while the transport is
// paused, so input piles up in the pending buffer and the sender must split
// the backlog; without the pause every read would fit one packet anyway.
func TestRunLargePasteIsChunked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})

		paste := make([]byte, 5<<20)
		for i := range paste {
			paste[i] = "abcdefghijklmnopqrstuvwxyz0123456789\n"[i%37]
		}
		feed := func(from, to int) {
			for off := from; off < to; off += readBufSize {
				term.keys <- paste[off:min(off+readBufSize, to)]
			}
		}
		release := tr.pause()
		feed(0, maxPending) // fits the pending buffer: nothing is dropped
		synctest.Wait()
		release()
		feed(maxPending, len(paste))
		synctest.Wait()
		close(tr.in)
		if err := <-done; err != nil {
			t.Fatalf("Run = %v", err)
		}
		for i, p := range tr.sentWith(protocol.HeaderTerminalBuffer) {
			if n := len(decodeBuffer(t, p)); n == 0 || n > MaxInputPacket {
				t.Fatalf("packet %d carries %d bytes, want 1..%d", i, n, MaxInputPacket)
			}
		}
		if got := tr.sentInput(t); !bytes.Equal(got, paste) {
			t.Fatalf("sent %d bytes, want the %d-byte paste unchanged", len(got), len(paste))
		}
	})
}

func TestRunTransportError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		boom := errors.New("integrity failure")
		tr.readErr <- boom
		if err := <-done; !errors.Is(err, boom) {
			t.Fatalf("Run = %v, want %v", err, boom)
		}
		if !term.isClosed() {
			t.Fatal("terminal not closed")
		}
	})
}

func TestRunTerminalInputClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(t.Context(), tr, Options{Terminal: term})
		close(term.keys)
		if err := <-done; err == nil {
			t.Fatal("Run = nil, want an error when terminal input closes")
		}
	})
}

func TestRunParentCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		tr := newFakeTransport()
		term := newFakeTerminal(size80x24)
		done := runAsync(ctx, tr, Options{Terminal: term})
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
		if !term.isClosed() {
			t.Fatal("terminal not closed")
		}
	})
}

func TestRunWithoutTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		done := runAsync(t.Context(), tr, Options{})
		tr.in <- serverOutput(t, "nobody listens")
		close(tr.in)
		if err := <-done; err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	})
}

// strandingTerminal models a Windows console whose Close failed to wake the
// reader: Close reports an error and Read ignores it, staying blocked until
// the test calls wake.
type strandingTerminal struct {
	*fakeTerminal
	woken chan struct{}
}

func (s *strandingTerminal) Read([]byte) (int, error) {
	<-s.woken
	return 0, errFakeClosed
}

func (s *strandingTerminal) Close() error {
	_ = s.fakeTerminal.Close()
	return errors.New("fake terminal: reader may still be blocked")
}

func (s *strandingTerminal) wake() { close(s.woken) }

// When Close fails where that can strand the reader (Windows), Run must stop
// waiting for the reader instead of hanging; elsewhere it must still join it.
func TestRunCloseErrorReaderJoin(t *testing.T) {
	tests := []struct {
		name          string
		mayStrand     bool
		wantJoinsRead bool
	}{
		{"close can strand the reader", true, false},
		{"close cannot strand the reader", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := closeMayStrandReader
			closeMayStrandReader = tt.mayStrand
			t.Cleanup(func() { closeMayStrandReader = saved })

			synctest.Test(t, func(t *testing.T) {
				tr := newFakeTransport()
				term := &strandingTerminal{fakeTerminal: newFakeTerminal(size80x24), woken: make(chan struct{})}
				done := runAsync(t.Context(), tr, Options{Terminal: term})
				close(tr.in)
				synctest.Wait()

				var returned bool
				select {
				case err := <-done:
					returned = true
					if err != nil {
						t.Errorf("Run = %v, want nil", err)
					}
				default:
				}
				term.wake()
				if returned == tt.wantJoinsRead {
					t.Fatalf("Run returned before the reader woke = %v, want %v", returned, !tt.wantJoinsRead)
				}
				if !returned {
					if err := <-done; err != nil {
						t.Fatalf("Run = %v, want nil", err)
					}
				}
			})
		})
	}
}
