package etcp_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
)

func TestDialAndExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		for i := range 3 {
			if err := h.conn.WritePacket(t.Context(), numbered(i, 100)); err != nil {
				t.Fatalf("WritePacket: %v", err)
			}
			if err := h.srv.Send(t.Context(), numbered(i, 100)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		if err := expectNumbered(t.Context(), 3, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		if err := expectNumbered(t.Context(), 3, h.conn.ReadPacket); err != nil {
			t.Fatalf("client side: %v", err)
		}
	})
}

func TestCloseEndsReadsAndWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		if err := h.conn.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := readPacket(t, h.conn); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("ReadPacket after Close = %v, want net.ErrClosed", err)
		}
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("WritePacket after Close = %v, want net.ErrClosed", err)
		}
	})
}

func TestDialErrors(t *testing.T) {
	tests := []struct {
		name   string
		status protocol.ConnectStatus
		want   error
	}{
		{name: "unregistered id", status: protocol.ConnectStatus_INVALID_KEY, want: etcp.ErrRejected},
		{name: "protocol mismatch", status: protocol.ConnectStatus_MISMATCHED_PROTOCOL, want: etcp.ErrVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &scripted{handle: func(_ int, c *rawServer) { _ = c.respond(tt.status) }}
				d := etcp.Dialer{NetDialer: s}
				_, err := d.Dial(t.Context(), testAddr, testID, testKey)
				if !errors.Is(err, tt.want) {
					t.Fatalf("Dial error = %v, want %v", err, tt.want)
				}
				s.wg.Wait()
			})
		})
	}
}

// Options that cannot work are refused before dialing: a negative KeepAlive
// fires the watcher at once and redials forever, a negative ReplayLimit
// blocks every write, and a limit above upstream's 64 MiB lets a catchup
// outgrow what etserver accepts.
func TestDialRejectsBadOptions(t *testing.T) {
	tests := []struct {
		name string
		d    etcp.Dialer
	}{
		{name: "negative KeepAlive", d: etcp.Dialer{KeepAlive: -time.Second}},
		{name: "tiny KeepAlive", d: etcp.Dialer{KeepAlive: time.Nanosecond}},
		{name: "negative ReplayLimit", d: etcp.Dialer{ReplayLimit: -1}},
		{name: "ReplayLimit above 64 MiB", d: etcp.Dialer{ReplayLimit: 64<<20 + 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &scripted{handle: func(_ int, c *rawServer) {
					if c.respond(protocol.ConnectStatus_NEW_CLIENT) == nil {
						c.drain()
					}
				}}
				d := tt.d
				d.NetDialer = s
				conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
				if err == nil {
					_ = conn.Close()
				}
				s.wg.Wait()
				if err == nil || s.dials.Load() != 0 {
					t.Fatalf("Dial = %v after %d dials; want an error before dialing", err, s.dials.Load())
				}
			})
		})
	}
}

// When the caller's context ends during the handshake, Dial reports the
// context's cause, not the closed-connection error that ending it produced.
func TestDialContextCauseMidHandshake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{handle: func(_ int, c *rawServer) {
			var req protocol.ConnectRequest
			if wire.ReadMessage(c.br, &req) == nil {
				c.drain() // never answer
			}
		}}
		d := etcp.Dialer{NetDialer: s}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		conn, err := d.Dial(ctx, testAddr, testID, testKey)
		if err == nil {
			_ = conn.Close()
		}
		s.wg.Wait()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dial = %v, want an error wrapping context.DeadlineExceeded", err)
		}
	})
}

func TestDialRejectsShortPasskey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{handle: func(_ int, c *rawServer) {
			if c.respond(protocol.ConnectStatus_NEW_CLIENT) == nil {
				c.drain()
			}
		}}
		d := etcp.Dialer{NetDialer: s}
		conn, err := d.Dial(t.Context(), testAddr, testID, "short")
		if err == nil {
			_ = conn.Close()
		}
		s.wg.Wait()
		if err == nil || s.dials.Load() != 0 {
			t.Fatalf("Dial with a 5-byte passkey = %v after %d dials; want an error before dialing", err, s.dials.Load())
		}
	})
}

// On etserver 7.0.0 a shell exit closes the link and the redial gets
// INVALID_KEY. The shell's last output must still reach the reader first.
func TestSessionEndDeliversLastOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		for i := range 20 {
			if err := h.srv.Send(t.Context(), numbered(i, 50)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		h.srv.EndSession()
		synctest.Wait() // the Conn has already failed when reading starts
		if err := expectNumbered(t.Context(), 20, h.conn.ReadPacket); err != nil {
			t.Fatalf("last output: %v", err)
		}
		if _, err := readPacket(t, h.conn); !errors.Is(err, etcp.ErrSessionEnded) {
			t.Fatalf("after last output: %v, want ErrSessionEnded", err)
		}
	})
}

func TestReconnectAfterCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		for i := range 10 {
			if i == 5 {
				synctest.Wait()
				h.net.CutAll()
			}
			if err := h.conn.WritePacket(t.Context(), numbered(i, 200)); err != nil {
				t.Fatalf("WritePacket: %v", err)
			}
			if err := h.srv.Send(t.Context(), numbered(i, 200)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		if err := expectNumbered(t.Context(), 10, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		if err := expectNumbered(t.Context(), 10, h.conn.ReadPacket); err != nil {
			t.Fatalf("client side: %v", err)
		}
		if got := h.net.Dials(); got != 2 {
			t.Fatalf("Dials() = %d, want 2", got)
		}
	})
}

// Cutting the replacement connection after n bytes lands the cut inside the
// connect handshake, the sequence exchange, a catchup message, or a frame.
func TestReconnectSurvivesCutsAnywhere(t *testing.T) {
	for _, n := range []int64{1, 9, 20, 40, 60, 100, 300, 1000, 5000} {
		t.Run(fmt.Sprintf("bytes=%d", n), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, etcp.Dialer{})
				defer h.close()

				for i := range 20 {
					if i == 10 {
						synctest.Wait()
						h.net.CutAfter(n)
						h.net.CutAll()
					}
					if err := h.conn.WritePacket(t.Context(), numbered(i, 300)); err != nil {
						t.Fatalf("WritePacket: %v", err)
					}
					if err := h.srv.Send(t.Context(), numbered(i, 300)); err != nil {
						t.Fatalf("Send: %v", err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
				defer cancel()
				if err := expectNumbered(ctx, 20, h.srv.Recv); err != nil {
					t.Fatalf("server side: %v", err)
				}
				if err := expectNumbered(ctx, 20, h.conn.ReadPacket); err != nil {
					t.Fatalf("client side: %v", err)
				}
				// The initial link, the one CutAll ends, and the one after
				// the budgeted cut: fewer means the cut never landed.
				if got := h.net.Dials(); got < 3 {
					t.Fatalf("Dials() = %d, want at least 3: the cut after %d bytes never fired", got, n)
				}
			})
		})
	}
}

// A backlog larger than one link write batch is sent across several batches
// with nothing skipped or repeated.
//
// The server holds its first read until every packet is queued, so the
// writer's first Write blocks while the rest pile up behind it and at least
// one later batch must stop short of the backlog (100 frames of 2070 bytes
// do not fit one 64 KiB batch), whatever the scheduling.
func TestBacklogSpansWriteBatches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n = 100
		release := make(chan struct{})
		got := make(chan []int, 1)
		s := &scripted{handle: func(i int, c *rawServer) {
			if i != 0 || c.respond(protocol.ConnectStatus_NEW_CLIENT) != nil {
				return
			}
			<-release
			var nums []int
			for len(nums) < n {
				frame, err := wire.ReadFrame(c.br, nil)
				if err != nil {
					break
				}
				p, err := c.open(frame)
				if err != nil {
					break
				}
				k, err := number(p)
				if err != nil {
					break
				}
				nums = append(nums, k)
			}
			got <- nums
			c.drain()
		}}
		d := etcp.Dialer{NetDialer: s}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() {
			_ = conn.Close()
			s.wg.Wait()
		}()
		var releaseOnce sync.Once
		releaseServer := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseServer() // runs first, so an early failure cannot strand s.wg.Wait
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		for i := range n {
			if err := conn.WritePacket(ctx, numbered(i, 2048)); err != nil {
				t.Fatalf("WritePacket: %v", err)
			}
		}
		synctest.Wait()
		releaseServer()

		select {
		case nums := <-got:
			for i, k := range nums {
				if k != i {
					t.Fatalf("server got packet %d at position %d (lost, duplicated or reordered)", k, i)
				}
			}
			if len(nums) != n {
				t.Fatalf("server got %d packets, want %d", len(nums), n)
			}
		case <-time.After(time.Minute):
			t.Fatal("server never received the backlog")
		}
		if got := s.dials.Load(); got != 1 {
			t.Fatalf("dials = %d, want 1", got)
		}
	})
}

// A payload whose sealed frame cannot fit wire.MaxFrameSize is refused at
// once instead of being queued where no link could ever send it.
func TestWritePacketTooLarge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		big := protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: make([]byte, wire.MaxFrameSize)}
		if err := h.conn.WritePacket(t.Context(), big); !errors.Is(err, wire.ErrTooLarge) {
			t.Fatalf("WritePacket(%d bytes) = %v, want wire.ErrTooLarge", len(big.Payload), err)
		}
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket after the refusal: %v", err)
		}
		if err := expectNumbered(t.Context(), 1, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
	})
}

// The session passkey never appears in Dial's errors.
func TestDialErrorsHidePasskey(t *testing.T) {
	short := testKey[:31]
	var d etcp.Dialer
	if _, err := d.Dial(t.Context(), testAddr, testID, short); err == nil || strings.Contains(err.Error(), short) {
		t.Fatalf("Dial with a 31-byte passkey = %v; want an error that does not quote it", err)
	}
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{handle: func(_ int, c *rawServer) { _ = c.respond(protocol.ConnectStatus_INVALID_KEY) }}
		d := etcp.Dialer{NetDialer: s}
		_, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err == nil || strings.Contains(err.Error(), testKey) {
			t.Fatalf("rejected Dial = %v; want an error that does not quote the passkey", err)
		}
		s.wg.Wait()
	})
}
