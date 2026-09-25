package etcp_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
)

func TestLivenessKeepsHealthyLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		synctest.Sleep(time.Minute)
		if got := h.net.Dials(); got != 1 {
			t.Fatalf("Dials() = %d after a quiet minute with echoes, want 1", got)
		}
		p, err := h.srv.Recv(t.Context())
		if err != nil || p.Header != protocol.HeaderKeepAlive {
			t.Fatalf("server got %v, %v; want a KEEP_ALIVE probe", p.Header, err)
		}
	})
}

func TestLivenessDetectsDeadLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()
		h.srv.EchoKeepAlive(false)

		// Probe at 5 s, dead at 10 s, immediate redial.
		synctest.Sleep(9 * time.Second)
		if got := h.net.Dials(); got != 1 {
			t.Fatalf("Dials() = %d at 9 s, want 1", got)
		}
		synctest.Sleep(2 * time.Second)
		if got := h.net.Dials(); got != 2 {
			t.Fatalf("Dials() = %d at 11 s, want 2", got)
		}
	})
}

// While the reader is blocked on a caller that is not reading, the link is
// not declared dead: that silence is ours, not the network's.
func TestLivenessIgnoresSlowReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		for i := range 200 { // more than the Conn buffers, so its reader blocks
			if err := h.srv.Send(t.Context(), numbered(i, 10)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		synctest.Sleep(time.Minute)
		if err := expectNumbered(t.Context(), 200, h.conn.ReadPacket); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if got := h.net.Dials(); got != 1 {
			t.Fatalf("Dials() = %d, want 1: a slow reader is not a dead link", got)
		}
	})
}

// A probe that could never be sent is refused up front, not discovered when
// the first quiet period ends the Conn.
func TestDialRejectsOversizedProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{handle: func(_ int, c *rawServer) {
			if c.respond(protocol.ConnectStatus_NEW_CLIENT) == nil {
				c.drain()
			}
		}}
		d := etcp.Dialer{
			NetDialer: s,
			Probe:     protocol.Packet{Payload: make([]byte, wire.MaxFrameSize)},
		}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err == nil {
			_ = conn.Close()
		}
		s.wg.Wait()
		if !errors.Is(err, wire.ErrTooLarge) || s.dials.Load() != 0 {
			t.Fatalf("Dial = %v after %d dials; want wire.ErrTooLarge before dialing", err, s.dials.Load())
		}
	})
}
