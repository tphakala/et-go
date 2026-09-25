package etservertest_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etservertest"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/seal"
	"github.com/tphakala/et-go/internal/wire"
)

const (
	testID  = "XXXtestclient001"
	testKey = "0123456789abcdef0123456789abcdef"
)

// rawClient speaks just enough of the protocol to exercise the server
// without etcp, so the two implementations stay independent.
type rawClient struct {
	conn net.Conn
	br   *bufio.Reader
	out  *seal.Stream
	in   *seal.Stream
}

func connect(t *testing.T, n *etservertest.Network, id string, version int32) (*rawClient, *protocol.ConnectResponse) {
	t.Helper()
	conn, err := n.DialContext(t.Context(), "tcp", "et:2022")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	req := &protocol.ConnectRequest{}
	req.SetClientId(id)
	req.SetVersion(version)
	if err := wire.WriteMessage(conn, req); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	resp := &protocol.ConnectResponse{}
	if err := wire.ReadMessage(conn, resp); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	var key [32]byte
	copy(key[:], testKey)
	return &rawClient{
		conn: conn,
		br:   bufio.NewReader(conn),
		out:  seal.New(&key, seal.ClientToServer),
		in:   seal.New(&key, seal.ServerToClient),
	}, resp
}

func (c *rawClient) send(t *testing.T, h protocol.Header, payload string) {
	t.Helper()
	frame := wire.AppendPacket(nil, true, h, c.out.Seal(nil, []byte(payload)))
	if err := wire.WriteFrame(c.conn, frame); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
}

func (c *rawClient) recv(t *testing.T) protocol.Packet {
	t.Helper()
	frame, err := wire.ReadFrame(c.br, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	_, h, payload, err := wire.ParsePacket(frame)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	plain, err := c.in.Open(nil, payload)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return protocol.Packet{Header: h, Payload: plain}
}

func TestServerNewClientExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		n := etservertest.NewNetwork(srv)
		defer n.Close()

		c, resp := connect(t, n, testID, protocol.Version)
		if got := resp.GetStatus(); got != protocol.ConnectStatus_NEW_CLIENT {
			t.Fatalf("status = %v, want NEW_CLIENT", got)
		}
		c.send(t, protocol.HeaderTerminalBuffer, "ls\n")
		p, err := srv.Recv(t.Context())
		if err != nil || p.Header != protocol.HeaderTerminalBuffer || string(p.Payload) != "ls\n" {
			t.Fatalf("Recv = %v %q, %v", p.Header, p.Payload, err)
		}
		if err := srv.Send(t.Context(), protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: []byte("out")}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got := c.recv(t); string(got.Payload) != "out" {
			t.Fatalf("client got %q, want %q", got.Payload, "out")
		}
	})
}

func TestServerEchoesKeepAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		n := etservertest.NewNetwork(srv)
		defer n.Close()

		c, _ := connect(t, n, testID, protocol.Version)
		c.send(t, protocol.HeaderKeepAlive, "")
		if got := c.recv(t); got.Header != protocol.HeaderKeepAlive {
			t.Fatalf("echo header = %v, want KEEP_ALIVE", got.Header)
		}
	})
}

func TestServerRejects(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		version int32
		end     bool
		want    protocol.ConnectStatus
	}{
		{name: "unknown id", id: "XXXsomeoneelse01", version: protocol.Version, want: protocol.ConnectStatus_INVALID_KEY},
		{name: "old protocol", id: testID, version: 5, want: protocol.ConnectStatus_MISMATCHED_PROTOCOL},
		{name: "ended session", id: testID, version: protocol.Version, end: true, want: protocol.ConnectStatus_INVALID_KEY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				srv := etservertest.NewServer(testID, testKey)
				n := etservertest.NewNetwork(srv)
				defer n.Close()
				if tt.end {
					srv.EndSession()
				}
				_, resp := connect(t, n, tt.id, tt.version)
				if got := resp.GetStatus(); got != tt.want {
					t.Fatalf("status = %v, want %v", got, tt.want)
				}
			})
		})
	}
}

// Upstream writes each recover message before reading the peer's
// (src/base/Connection.cpp:105-143 at et-v7.0.0); etcp's deadlock test
// (TestCatchupBothWays) only means something if the fake does the same. A
// client that reads each server message before writing its own must get
// both; a fake that read first would leave these reads to time out.
func TestServerRecoverWritesCatchupFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		n := etservertest.NewNetwork(srv)
		defer n.Close()

		connect(t, n, testID, protocol.Version) // registers the client
		// Let the first link's Serve take over before the second connects:
		// otherwise its late takeOver can close the second link.
		synctest.Wait()
		if err := srv.Send(t.Context(), protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: []byte("owed")}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		n.CutAll()

		c, resp := connect(t, n, testID, protocol.Version)
		if got := resp.GetStatus(); got != protocol.ConnectStatus_RETURNING_CLIENT {
			t.Fatalf("status = %v, want RETURNING_CLIENT", got)
		}
		if err := c.conn.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		var seq protocol.SequenceHeader
		if err := wire.ReadMessage(c.br, &seq); err != nil {
			t.Fatalf("server SequenceHeader before ours: %v", err)
		}
		if err := wire.WriteMessage(c.conn, &protocol.SequenceHeader{}); err != nil {
			t.Fatalf("write SequenceHeader: %v", err)
		}
		var cb protocol.CatchupBuffer
		if err := wire.ReadMessage(c.br, &cb); err != nil {
			t.Fatalf("server CatchupBuffer before ours: %v", err)
		}
		if got := len(cb.GetBuffer()); got != 1 {
			t.Fatalf("server catchup holds %d packets, want the 1 it owes", got)
		}
		if err := wire.WriteMessage(c.conn, &protocol.CatchupBuffer{}); err != nil {
			t.Fatalf("write CatchupBuffer: %v", err)
		}
	})
}

func TestServerEndSessionFlushesThenCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		n := etservertest.NewNetwork(srv)
		defer n.Close()

		c, _ := connect(t, n, testID, protocol.Version)
		if err := srv.Send(t.Context(), protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: []byte("bye")}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		srv.EndSession()
		if got := c.recv(t); string(got.Payload) != "bye" {
			t.Fatalf("last output = %q, want %q", got.Payload, "bye")
		}
		if _, err := wire.ReadFrame(c.br, nil); err == nil {
			t.Fatal("link still open after EndSession")
		}
	})
}

// A dial whose context has already ended fails the way net.Dialer does: a
// *net.OpError wrapping the context's error.
func TestNetworkDialEndedContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := etservertest.NewNetwork(etservertest.NewServer(testID, testKey))
		defer n.Close()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := n.DialContext(ctx, "tcp", "et:2022")
		if _, ok := errors.AsType[*net.OpError](err); !ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("dial with an ended context = %v, want a *net.OpError wrapping context.Canceled", err)
		}
	})
}

func TestNetworkRefuseAndCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := etservertest.NewNetwork(etservertest.NewServer(testID, testKey))
		defer n.Close()

		n.SetRefuse(true)
		_, err := n.DialContext(t.Context(), "tcp", "et:2022")
		if _, ok := errors.AsType[*net.OpError](err); !ok {
			t.Fatalf("refused dial error = %v, want *net.OpError", err)
		}
		n.SetRefuse(false)
		conn, err := n.DialContext(t.Context(), "tcp", "et:2022")
		if err != nil {
			t.Fatalf("DialContext: %v", err)
		}
		_ = conn.Close()
		if got := n.Dials(); got != 2 {
			t.Fatalf("Dials() = %d, want 2", got)
		}
	})
}

func TestNetworkCutAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := etservertest.NewNetwork(etservertest.NewServer(testID, testKey))
		defer n.Close()

		n.CutAfter(5)
		conn, err := n.DialContext(t.Context(), "tcp", "et:2022")
		if err != nil {
			t.Fatalf("DialContext: %v", err)
		}
		// An 8-byte write (the size of a handshake length prefix) exceeds
		// the 5-byte budget, so the cut lands inside it.
		wrote, err := conn.Write(make([]byte, 8))
		if wrote != 5 || err == nil {
			t.Fatalf("Write = %d, %v; want 5 bytes and an error", wrote, err)
		}
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Fatal("Read after cut succeeded")
		}
	})
}
