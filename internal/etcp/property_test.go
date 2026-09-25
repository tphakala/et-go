package etcp_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
)

// Random traffic in both directions while the network is cut at random
// moments, including in the middle of frames, handshakes and catchups. Every
// packet must arrive exactly once and in order on both sides.
func TestPropertyExactlyOnceUnderRandomCuts(t *testing.T) {
	for seed := range uint64(20) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runProperty(t, seed)
			})
		})
	}
}

func runProperty(t *testing.T, seed uint64) {
	t.Helper()
	h := newHarness(t, etcp.Dialer{})
	defer h.close()

	const n = 300
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() { // client to server
		rng := rand.New(rand.NewPCG(seed, 1))
		for i := range n {
			if err := h.conn.WritePacket(ctx, numbered(i, rng.IntN(2048))); err != nil {
				t.Errorf("WritePacket %d: %v", i, err)
				return
			}
			time.Sleep(time.Duration(rng.IntN(5)) * time.Millisecond)
		}
	})
	wg.Go(func() { // server to client
		rng := rand.New(rand.NewPCG(seed, 2))
		for i := range n {
			if err := h.srv.Send(ctx, numbered(i, rng.IntN(2048))); err != nil {
				t.Errorf("Send %d: %v", i, err)
				return
			}
			time.Sleep(time.Duration(rng.IntN(5)) * time.Millisecond)
		}
	})
	wg.Go(func() { // the network
		rng := rand.New(rand.NewPCG(seed, 3))
		for range 15 {
			time.Sleep(time.Duration(rng.IntN(100)) * time.Millisecond)
			if rng.IntN(2) == 0 {
				h.net.CutAll()
			} else {
				h.net.CutAfter(rng.Int64N(20_000))
				h.net.CutAll()
			}
		}
	})

	errs := make(chan error, 2)
	wg.Go(func() { errs <- expectNumbered(ctx, n, h.srv.Recv) })
	wg.Go(func() { errs <- expectNumbered(ctx, n, h.conn.ReadPacket) })
	for range 2 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	cancel()
	wg.Wait()

	if got := h.net.Dials(); got < 2 {
		t.Errorf("Dials() = %d: no cut forced a reconnect, so the run proved nothing", got)
	}

	// expectNumbered stops at packet n-1, so a recover after the last packet
	// is otherwise never exercised. Force one now that both sides hold
	// everything: it must replay nothing (a resent packet would surface as a
	// duplicate or, with counter nonces, as an integrity error) and must
	// settle, with no further redial across the quiet minute.
	beforeCut := h.net.Dials()
	h.net.CutAll()
	synctest.Sleep(time.Minute)
	settled := h.net.Dials()
	if settled == beforeCut {
		t.Errorf("the late cut forced no reconnect (Dials() stayed %d)", settled)
	}
	if err := expectNothingMore(t.Context(), h.srv.Recv); err != nil {
		t.Errorf("server side after the late recover: %v", err)
	}
	if err := expectNothingMore(t.Context(), h.conn.ReadPacket); err != nil {
		t.Errorf("client side after the late recover: %v", err)
	}
	if got := h.net.Dials(); got != settled {
		t.Errorf("Dials() went from %d to %d with no traffic and no cuts: a redial loop", settled, got)
	}
}
