package etcp_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/etservertest"
)

// dialClock records when each dial happens.
type dialClock struct {
	inner interface {
		DialContext(ctx context.Context, network, address string) (net.Conn, error)
	}
	mu    sync.Mutex
	times []time.Time
}

func (d *dialClock) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.times = append(d.times, time.Now())
	d.mu.Unlock()
	return d.inner.DialContext(ctx, network, address)
}

// Review Focus 2: a long outage (laptop asleep, network down). Dials keep
// failing, the delay between them never exceeds 5 s, and once the network
// returns the session resumes with nothing lost.
func TestLongOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		nw := etservertest.NewNetwork(srv)
		defer nw.Close()
		clock := &dialClock{inner: nw}
		d := etcp.Dialer{NetDialer: clock}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()

		synctest.Wait()
		nw.SetRefuse(true)
		nw.CutAll()
		synctest.Wait()
		if err := conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		if err := srv.Send(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		// 500 refused dials take about 40 minutes of fake time at the 5 s
		// backoff cap: a long outage by any measure. The deadline turns a
		// backoff that stopped dialing into a failure instead of a hang.
		deadline := time.Now().Add(2 * time.Hour)
		for nw.Dials() < 501 {
			if time.Now().After(deadline) {
				t.Fatalf("only %d dials in two hours of outage", nw.Dials())
			}
			synctest.Sleep(5 * time.Second)
		}
		nw.SetRefuse(false)
		if err := expectNumbered(t.Context(), 1, srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		if err := expectNumbered(t.Context(), 1, conn.ReadPacket); err != nil {
			t.Fatalf("client side: %v", err)
		}

		clock.mu.Lock()
		defer clock.mu.Unlock()
		var longest time.Duration
		for i := 1; i < len(clock.times); i++ {
			longest = max(longest, clock.times[i].Sub(clock.times[i-1]))
		}
		if longest > 5*time.Second {
			t.Fatalf("longest gap between dials = %v, want at most 5s", longest)
		}
		if longest < 4*time.Second {
			t.Fatalf("longest gap between dials = %v; backoff never reached its cap", longest)
		}
	})
}
