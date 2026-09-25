package etcp_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/etservertest"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
	"golang.org/x/crypto/nacl/secretbox"
)

// A probe still waiting to be written is not followed by another: probes
// bypass ReplayLimit, so while the writer is stuck on a server that has
// stopped reading, a new probe every quiet keepAlive period (the server's
// occasional output keeps the link from being declared dead) would grow the
// replay ring without bound.
func TestProbesDoNotPileUpBehindStuckWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{handle: func(i int, c *rawServer) {
			if i > 0 || c.respond(protocol.ConnectStatus_NEW_CLIENT) != nil {
				return
			}
			if _, err := wire.ReadFrame(c.br, nil); err != nil { // packet 0
				return
			}
			// Stop reading, but send a little output just slower than
			// the keepalive period.
			for {
				time.Sleep(6 * time.Second)
				sealed := c.out.Seal(nil, []byte("x"))
				if wire.WriteFrame(c.conn, wire.AppendPacket(nil, true, protocol.HeaderTerminalBuffer, sealed)) != nil {
					return
				}
			}
		}}
		d := etcp.Dialer{NetDialer: s}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		done := make(chan struct{})
		defer func() {
			_ = conn.Close()
			<-done
			s.wg.Wait()
		}()
		go func() { // the caller keeps reading
			defer close(done)
			for {
				if _, err := conn.ReadPacket(t.Context()); err != nil {
					return
				}
			}
		}()
		if err := conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		time.Sleep(20 * time.Minute)
		// One probe, sealed with an empty payload, may wait unwritten.
		if got, want := etcp.Unsent(conn), 2+secretbox.Overhead; got > want {
			t.Fatalf("unsent = %d bytes after 20 minutes behind a stuck writer, want at most %d (one probe)", got, want)
		}
	})
}

// etserver 7.0.0 aborts the whole server when a session's first packet is not
// INITIAL_PAYLOAD (src/terminal/TerminalServer.cpp:429-439 at et-v7.0.0), so
// no probe may go out before the caller's first packet, however long the link
// stays quiet, and that quiet is not taken for a dead link.
func TestNoProbeBeforeFirstPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		synctest.Sleep(time.Minute)
		if got := h.net.Dials(); got != 1 {
			t.Fatalf("Dials() = %d after a quiet minute before any write, want 1", got)
		}
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		p, err := h.srv.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if n, err := number(p); p.Header != protocol.HeaderTerminalBuffer || err != nil || n != 0 {
			t.Fatalf("first packet the server got = header %v %q, want the caller's packet 0", p.Header, p.Payload)
		}
	})
}

func TestLivenessKeepsHealthyLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}

		synctest.Sleep(time.Minute)
		if got := h.net.Dials(); got != 1 {
			t.Fatalf("Dials() = %d after a quiet minute with echoes, want 1", got)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if err := expectNumbered(ctx, 1, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		p, err := h.srv.Recv(ctx)
		if err != nil || p.Header != protocol.HeaderKeepAlive {
			t.Fatalf("server got %v, %v; want a KEEP_ALIVE probe after the packet", p.Header, err)
		}
	})
}

func TestLivenessDetectsDeadLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()
		h.srv.EchoKeepAlive(false)
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}

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

// A long upload over a slow uplink queues the probe behind the backlog, so
// its echo comes late while the server itself sends nothing, and the
// watcher may drop the link (the KeepAlive doc states this limit). Whatever
// reconnects that costs, every packet must still arrive exactly once and in
// order.
func TestLivenessSlowUploadDeliversEverything(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		nw := etservertest.NewNetwork(srv)
		defer nw.Close()
		d := etcp.Dialer{NetDialer: throttledDialer{inner: nw}}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()

		const n = 400 // 400 KiB at 8 KiB/s: 50 s, ten keepalive periods
		for i := range n {
			if err := conn.WritePacket(t.Context(), numbered(i, 1024)); err != nil {
				t.Fatalf("WritePacket %d: %v", i, err)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		if err := expectNumbered(ctx, n, srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
	})
}
