package etcp

import (
	"errors"
	"math"
	"net"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
)

// A receive count past the int32 range cannot be stated in a SequenceHeader:
// writeRecover must end the Conn with ErrReplayExceeded rather than send a
// wrapped, negative sequence number.
func TestWriteRecoverRefusesSequenceBeyondInt32(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var d Dialer
		c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
		defer c.cancel(nil)
		c.recvSeq = math.MaxInt32 + 1

		client, server := net.Pipe()
		got := make(chan error, 1)
		go func() {
			var sh protocol.SequenceHeader
			err := wire.ReadMessage(server, &sh)
			if err == nil {
				err = errors.New("server received a SequenceHeader")
			} else {
				err = nil
			}
			_ = server.Close()
			got <- err
		}()

		// A reader failure is preloaded so a writeRecover that did send
		// returns instead of waiting for the peer's sequence.
		gotSeq := make(chan error, 1)
		gotSeq <- errors.New("no peer sequence")
		_, err := c.writeRecover(client, gotSeq, &protocol.SequenceHeader{})
		_ = client.Close()
		if !errors.Is(err, ErrReplayExceeded) {
			t.Fatalf("writeRecover = %v, want ErrReplayExceeded", err)
		}
		if serr := <-got; serr != nil {
			t.Fatal(serr)
		}
	})
}

// A receive count of exactly MaxInt32 still fits a SequenceHeader, so
// writeRecover sends it unchanged: the guard refuses only past the range.
func TestWriteRecoverSendsSequenceAtInt32Limit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var d Dialer
		c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
		defer c.cancel(nil)
		c.recvSeq = math.MaxInt32

		client, server := net.Pipe()
		sent := make(chan int32, 1)
		go func() {
			var sh protocol.SequenceHeader
			if err := wire.ReadMessage(server, &sh); err != nil {
				sent <- -1
			} else {
				sent <- sh.GetSequenceNumber()
			}
			_ = server.Close()
		}()

		gotSeq := make(chan error, 1)
		gotSeq <- errors.New("no peer sequence")
		_, err := c.writeRecover(client, gotSeq, &protocol.SequenceHeader{})
		_ = client.Close()
		if errors.Is(err, ErrReplayExceeded) {
			t.Fatalf("writeRecover = %v, want the header sent at the int32 limit", err)
		}
		if seq := <-sent; seq != math.MaxInt32 {
			t.Fatalf("server read sequence %d, want %d", seq, int32(math.MaxInt32))
		}
	})
}
