package etcp

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/protocol"
)

// blockedConn returns a Conn with no link whose backlog is over its limit, so
// every WritePacket parks until the test makes room.
func blockedConn(t *testing.T) *Conn {
	t.Helper()
	var d Dialer
	c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
	c.mu.Lock()
	c.unsent = c.limit + 1
	c.mu.Unlock()
	return c
}

// makeRoom empties the backlog and hands out one wakeup, as a link writer
// does after a drain.
func makeRoom(c *Conn) {
	c.mu.Lock()
	c.unsent = 0
	c.mu.Unlock()
	signal(c.space)
}

func waitWrite(t *testing.T, name string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writer %s: %v", name, err)
		}
	case <-time.After(time.Minute):
		t.Fatalf("writer %s still blocked a minute after room was made", name)
	}
}

// One drain wakes one blocked writer, which passes the turn on, so every
// writer that fits proceeds.
func TestBlockedWritersAllProceed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := blockedConn(t)
		defer c.cancel(nil)

		doneA := make(chan error, 1)
		doneB := make(chan error, 1)
		go func() { doneA <- c.WritePacket(t.Context(), protocol.Packet{}) }()
		synctest.Wait()
		go func() { doneB <- c.WritePacket(t.Context(), protocol.Packet{}) }()
		synctest.Wait()

		makeRoom(c)
		waitWrite(t, "A", doneA)
		waitWrite(t, "B", doneB)
	})
}

// A writer that is handed the wakeup and whose context is cancelled before it
// runs must not drop the wakeup: the other blocked writer would then stay
// parked with room available. Channel receivers are served in order, so the
// wakeup goes to A, the first writer to park.
func TestWakeupNotLostOnCancel(t *testing.T) {
	for range 20 {
		synctest.Test(t, func(t *testing.T) {
			c := blockedConn(t)
			defer c.cancel(nil)

			ctxA, cancelA := context.WithCancel(t.Context())
			doneA := make(chan error, 1)
			doneB := make(chan error, 1)
			go func() { doneA <- c.WritePacket(ctxA, protocol.Packet{}) }()
			synctest.Wait()
			go func() { doneB <- c.WritePacket(t.Context(), protocol.Packet{}) }()
			synctest.Wait()

			makeRoom(c) // A is handed the wakeup
			cancelA()   // and is cancelled before it gets to run
			select {    // A may enqueue or return its cause; either is fine
			case <-doneA:
			case <-time.After(time.Minute):
				t.Fatal("writer A still blocked a minute after it was cancelled")
			}
			waitWrite(t, "B", doneB)
		})
	}
}
