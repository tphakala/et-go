package etcp_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/etservertest"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/seal"
	"github.com/tphakala/et-go/internal/wire"
)

const (
	testID   = "XXXtestclient001"
	testKey  = "0123456789abcdef0123456789abcdef"
	testAddr = "et.example:2022"
)

// harness is one session against the fake server over the in-memory network.
type harness struct {
	srv  *etservertest.Server
	net  *etservertest.Network
	conn *etcp.Conn
}

// newHarness dials a fresh session. Callers defer close, which closes the
// Conn and then the network, so every goroutine has exited before the
// synctest bubble ends.
func newHarness(t *testing.T, d etcp.Dialer) *harness {
	t.Helper()
	srv := etservertest.NewServer(testID, testKey)
	nw := etservertest.NewNetwork(srv)
	if d.NetDialer == nil {
		d.NetDialer = nw
	}
	conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
	if err != nil {
		nw.Close()
		t.Fatalf("Dial: %v", err)
	}
	return &harness{srv: srv, net: nw, conn: conn}
}

func (h *harness) close() {
	_ = h.conn.Close()
	h.net.Close()
}

// numbered builds a TERMINAL_BUFFER whose payload starts with i, so receivers
// can check order and completeness.
func numbered(i, size int) protocol.Packet {
	payload := fmt.Appendf(nil, "%08d", i)
	for len(payload) < size {
		payload = append(payload, byte('a'+i%26))
	}
	return protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: payload}
}

func number(p protocol.Packet) (int, error) {
	if len(p.Payload) < 8 {
		return 0, fmt.Errorf("payload %q too short", p.Payload)
	}
	return strconv.Atoi(string(p.Payload[:8]))
}

// expectNumbered reads packets, skipping keepalives, and checks that exactly
// 0..n-1 arrive in order.
func expectNumbered(ctx context.Context, n int, recv func(context.Context) (protocol.Packet, error)) error {
	for want := 0; want < n; {
		p, err := recv(ctx)
		if err != nil {
			return fmt.Errorf("after %d of %d packets: %w", want, n, err)
		}
		if p.Header == protocol.HeaderKeepAlive {
			continue
		}
		got, err := number(p)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("got packet %d, want %d (lost, duplicated or reordered)", got, want)
		}
		want++
	}
	return nil
}

// scripted is a NetDialer whose server side is a test function, for server
// behaviour the fake server does not produce on purpose.
type scripted struct {
	wg     sync.WaitGroup
	dials  atomic.Int32
	handle func(i int, c *rawServer)
}

func (s *scripted) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	i := int(s.dials.Add(1)) - 1
	client, server := net.Pipe()
	s.wg.Go(func() {
		defer func() { _ = server.Close() }()
		var key [32]byte
		copy(key[:], testKey)
		s.handle(i, &rawServer{
			conn: server,
			br:   bufio.NewReader(server),
			out:  seal.New(&key, seal.ServerToClient),
			in:   seal.New(&key, seal.ClientToServer),
		})
	})
	return client, nil
}

// rawServer is the server end of a scripted connection.
type rawServer struct {
	conn net.Conn
	br   *bufio.Reader
	out  *seal.Stream
	in   *seal.Stream
}

func (s *rawServer) respond(status protocol.ConnectStatus) error {
	var req protocol.ConnectRequest
	if err := wire.ReadMessage(s.br, &req); err != nil {
		return err
	}
	resp := &protocol.ConnectResponse{}
	resp.SetStatus(status)
	return wire.WriteMessage(s.conn, resp)
}

// drain reads and discards frames until the connection closes.
func (s *rawServer) drain() {
	var buf []byte
	for {
		frame, err := wire.ReadFrame(s.br, buf)
		if err != nil {
			return
		}
		buf = frame
	}
}

// acceptFrames answers NEW_CLIENT, reads n frames, then returns so the link
// drops.
func (s *rawServer) acceptFrames(n int) {
	if s.respond(protocol.ConnectStatus_NEW_CLIENT) != nil {
		return
	}
	var buf []byte
	for range n {
		frame, err := wire.ReadFrame(s.br, buf)
		if err != nil {
			return
		}
		buf = frame
	}
}

// claimSequence answers RETURNING_CLIENT and claims to have received peer
// packets, then drains.
func (s *rawServer) claimSequence(peer int32) {
	if s.respond(protocol.ConnectStatus_RETURNING_CLIENT) != nil {
		return
	}
	var mine protocol.SequenceHeader
	if wire.ReadMessage(s.br, &mine) != nil {
		return
	}
	sh := &protocol.SequenceHeader{}
	sh.SetSequenceNumber(peer)
	if wire.WriteMessage(s.conn, sh) != nil {
		return
	}
	s.drain()
}

// pausedRecover answers RETURNING_CLIENT, claims to have received nothing,
// reads the client's catchup (so its snapshot is taken), then closes snapped
// and waits for resume before finishing the exchange. It reports the number
// of the first data packet the new link carries on got.
func (s *rawServer) pausedRecover(snapped chan<- struct{}, resume <-chan struct{}, got chan<- int) {
	if s.respond(protocol.ConnectStatus_RETURNING_CLIENT) != nil {
		return
	}
	var mine protocol.SequenceHeader
	if wire.ReadMessage(s.br, &mine) != nil {
		return
	}
	if wire.WriteMessage(s.conn, &protocol.SequenceHeader{}) != nil {
		return
	}
	var theirs protocol.CatchupBuffer
	if wire.ReadMessage(s.br, &theirs) != nil {
		return
	}
	for _, b := range theirs.GetBuffer() {
		if _, err := s.open(b); err != nil { // keeps the nonce in step
			return
		}
	}
	close(snapped)
	<-resume
	if wire.WriteMessage(s.conn, &protocol.CatchupBuffer{}) != nil {
		return
	}
	for {
		frame, err := wire.ReadFrame(s.br, nil)
		if err != nil {
			return
		}
		p, err := s.open(frame)
		if err != nil {
			return
		}
		if p.Header == protocol.HeaderKeepAlive {
			continue
		}
		if n, err := number(p); err == nil {
			got <- n
		}
		s.drain()
		return
	}
}

// open parses and opens one sealed packet from the client.
func (s *rawServer) open(b []byte) (protocol.Packet, error) {
	_, h, payload, err := wire.ParsePacket(b)
	if err != nil {
		return protocol.Packet{}, err
	}
	plain, err := s.in.Open(nil, payload)
	if err != nil {
		return protocol.Packet{}, err
	}
	return protocol.Packet{Header: h, Payload: plain}, nil
}
