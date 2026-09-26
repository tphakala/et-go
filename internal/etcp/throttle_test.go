package etcp_test

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/etservertest"
)

// throttledDialer slows every client-side write to 1 KiB every perKiB of fake
// time (zero means 125 ms, 8 KiB/s), like a congested uplink, or with
// downlink set every client-side read instead, like a congested downlink.
type throttledDialer struct {
	inner interface {
		DialContext(ctx context.Context, network, address string) (net.Conn, error)
	}
	downlink bool
	perKiB   time.Duration
}

func (d throttledDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := d.inner.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if d.downlink {
		return slowReadConn{c}, nil
	}
	perKiB := d.perKiB
	if perKiB == 0 {
		perKiB = 125 * time.Millisecond
	}
	return throttledConn{Conn: c, perKiB: perKiB}, nil
}

// slowReadConn reads at most 1 KiB per 125 ms of fake time.
type slowReadConn struct{ net.Conn }

func (c slowReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p[:min(len(p), 1<<10)])
	if n > 0 {
		time.Sleep(125 * time.Millisecond)
	}
	return n, err
}

type throttledConn struct {
	net.Conn
	perKiB time.Duration
}

func (c throttledConn) Write(p []byte) (int, error) {
	var n int
	for len(p) > 0 {
		m, err := c.Conn.Write(p[:min(len(p), 1<<10)])
		n += m
		if err != nil {
			return n, err
		}
		p = p[m:]
		time.Sleep(c.perKiB)
	}
	return n, nil
}

// A catchup larger than one idle timeout's worth of bytes on
// a throttled link must not trip the 30 s handshake idle timeout while bytes
// keep flowing. About 1 MiB at 8 KiB/s takes over two minutes of fake time.
func TestThrottledCatchupDoesNotTimeOut(t *testing.T) {
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

		synctest.Wait()
		nw.SetRefuse(true)
		nw.CutAll()
		synctest.Wait()
		const n = 1024 // 1024 packets of 1 KiB: the catchup is about 1 MiB
		for i := range n {
			if err := conn.WritePacket(t.Context(), numbered(i, 1024)); err != nil {
				t.Fatalf("WritePacket %d: %v", i, err)
			}
		}
		before := nw.Dials()
		start := time.Now()
		nw.SetRefuse(false)

		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		if err := expectNumbered(ctx, n, srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		if elapsed := time.Since(start); elapsed <= 30*time.Second {
			t.Fatalf("recovery took %v; the catchup must outlast the 30s idle timeout for this test to mean anything", elapsed)
		}
		if got := nw.Dials() - before; got != 1 {
			t.Fatalf("recovery used %d dials, want 1: the idle timeout fired while bytes were flowing", got)
		}
	})
}

// Upstream writes its whole catchup before reading ours
// (src/base/Connection.cpp:125-135 at et-v7.0.0), so while a slow downlink
// carries the server's catchup, our own catchup write cannot progress. That
// write must not time out while the server's bytes are still arriving:
// progress in either direction keeps the handshake alive.
func TestRecoverSurvivesSlowServerCatchup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		nw := etservertest.NewNetwork(srv)
		defer nw.Close()
		d := etcp.Dialer{NetDialer: throttledDialer{inner: nw, downlink: true}}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()

		synctest.Wait()
		nw.SetRefuse(true)
		nw.CutAll()
		synctest.Wait()
		const serverPackets = 512 // 512 KiB at 8 KiB/s: over a minute, past the 30 s idle timeout
		for i := range serverPackets {
			if err := srv.Send(t.Context(), numbered(i, 1024)); err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
		}
		for i := range 4 { // a non-empty client catchup, which blocks until the server reads it
			if err := conn.WritePacket(t.Context(), numbered(i, 1024)); err != nil {
				t.Fatalf("WritePacket %d: %v", i, err)
			}
		}
		before := nw.Dials()
		start := time.Now()
		nw.SetRefuse(false)

		ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
		defer cancel()
		if err := expectNumbered(ctx, 4, srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		if err := expectNumbered(ctx, serverPackets, conn.ReadPacket); err != nil {
			t.Fatalf("client side: %v", err)
		}
		if elapsed := time.Since(start); elapsed <= 30*time.Second {
			t.Fatalf("recovery took %v; the server's catchup must outlast the 30s idle timeout for this test to mean anything", elapsed)
		}
		if got := nw.Dials() - before; got != 1 {
			t.Fatalf("recovery used %d dials, want 1: our blocked catchup write timed out while the server's catchup was arriving", got)
		}
	})
}
