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
