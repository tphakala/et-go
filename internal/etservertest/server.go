// Package etservertest provides an in-process fake etserver and an in-memory
// network, so etcp can be tested without sockets and under testing/synctest.
//
// The server is written independently of etcp, from upstream EternalTerminal
// 7.0.0 semantics (~/src/et-build, tag et-v7.0.0), so the two implementations
// check each other. In particular it writes its whole catchup before reading
// the client's, exactly like upstream's Connection::recover
// (src/base/Connection.cpp:105-143).
package etservertest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/seal"
	"github.com/tphakala/et-go/internal/wire"
)

var errSessionEnded = errors.New("etservertest: session ended")

// Server is an in-process fake etserver for one session.
type Server struct {
	id  string
	key [32]byte

	mu         sync.Mutex
	registered bool // false after EndSession: connects get INVALID_KEY
	known      bool // a client connected before: the next connect is RETURNING_CLIENT
	echo       bool
	out        *seal.Stream // server to client
	sent       [][]byte     // every sealed packet sent; index is the sequence number
	in         *seal.Stream // client to server
	recvSeq    int64
	queue      []protocol.Packet // received, not yet returned by Recv
	cur        *serverLink

	notify chan struct{} // cap 1: queue grew
	wake   chan struct{} // cap 1: sent grew or the session ended
}

type serverLink struct {
	conn net.Conn
	done chan struct{} // closed when Serve for this link returns
}

// NewServer returns a server that accepts the session id with passkey. It
// panics if passkey is not 32 bytes, which is a bug in the test.
func NewServer(id, passkey string) *Server {
	if len(passkey) != 32 {
		panic("etservertest: passkey must be 32 bytes")
	}
	s := &Server{
		id:         id,
		registered: true,
		echo:       true,
		notify:     make(chan struct{}, 1),
		wake:       make(chan struct{}, 1),
	}
	copy(s.key[:], passkey)
	s.out = seal.New(&s.key, seal.ServerToClient)
	s.in = seal.New(&s.key, seal.ClientToServer)
	return s
}

// Serve runs one link on c: the connect handshake (and the recover exchange
// for a returning client), then the encrypted stream, until c fails, the
// session ends or ctx ends. A newer link replaces an older one, as upstream
// does.
func (s *Server) Serve(ctx context.Context, c net.Conn) error {
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	var req protocol.ConnectRequest
	if err := wire.ReadMessage(c, &req); err != nil {
		return fmt.Errorf("etservertest: connect request: %w", err)
	}
	status, text := s.admit(&req)
	resp := &protocol.ConnectResponse{}
	resp.SetStatus(status)
	if text != "" {
		resp.SetError(text)
	}
	if err := wire.WriteMessage(c, resp); err != nil {
		return fmt.Errorf("etservertest: connect response: %w", err)
	}
	if status != protocol.ConnectStatus_NEW_CLIENT && status != protocol.ConnectStatus_RETURNING_CLIENT {
		return fmt.Errorf("etservertest: rejected: %s", text)
	}

	done := s.takeOver(c)
	defer close(done)

	var flushed int64
	if status == protocol.ConnectStatus_RETURNING_CLIENT {
		f, err := s.recover(c)
		if err != nil {
			return err
		}
		flushed = f
	}

	lctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopClose := context.AfterFunc(lctx, func() { _ = c.Close() })
	defer stopClose()

	var wg sync.WaitGroup
	wg.Go(func() { cancel(s.readLoop(c)) })
	wg.Go(func() { cancel(s.writeLoop(lctx, c, flushed)) })
	wg.Wait()
	return context.Cause(lctx)
}

// admit decides the ConnectResponse status for req.
func (s *Server) admit(req *protocol.ConnectRequest) (status protocol.ConnectStatus, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case req.GetVersion() != protocol.Version:
		return protocol.ConnectStatus_MISMATCHED_PROTOCOL, "Mismatched protocol"
	case req.GetClientId() != s.id || !s.registered:
		// MEASURED against etserver 7.0.0: the error text for an unknown or
		// ended session is "Client is not registered".
		return protocol.ConnectStatus_INVALID_KEY, "Client is not registered"
	case !s.known:
		s.known = true
		return protocol.ConnectStatus_NEW_CLIENT, ""
	default:
		return protocol.ConnectStatus_RETURNING_CLIENT, ""
	}
}

// takeOver makes c the active link, closing the previous one and waiting for
// its Serve to finish so the two never touch the stream state at once.
func (s *Server) takeOver(c net.Conn) chan struct{} {
	done := make(chan struct{})
	s.mu.Lock()
	prev := s.cur
	s.cur = &serverLink{conn: c, done: done}
	s.mu.Unlock()
	if prev != nil {
		_ = prev.conn.Close()
		<-prev.done
	}
	return done
}

// recover runs the server side of the recover exchange in upstream order and
// returns the sequence number the new link's writer starts from.
func (s *Server) recover(c io.ReadWriter) (int64, error) {
	s.mu.Lock()
	mine := s.recvSeq
	s.mu.Unlock()

	sh := &protocol.SequenceHeader{}
	sh.SetSequenceNumber(int32(mine))
	if err := wire.WriteMessage(c, sh); err != nil {
		return 0, fmt.Errorf("etservertest: write sequence: %w", err)
	}
	var peer protocol.SequenceHeader
	if err := wire.ReadMessage(c, &peer); err != nil {
		return 0, fmt.Errorf("etservertest: read sequence: %w", err)
	}

	s.mu.Lock()
	from := int64(peer.GetSequenceNumber())
	if from < 0 || from > int64(len(s.sent)) {
		s.mu.Unlock()
		return 0, fmt.Errorf("etservertest: client sequence %d outside [0, %d]", from, len(s.sent))
	}
	catchup := slices.Clone(s.sent[from:])
	flushed := int64(len(s.sent))
	s.mu.Unlock()

	// Upstream writes its whole catchup before reading ours
	// (src/base/Connection.cpp:125-135). Over net.Pipe, which has no buffer,
	// this deadlocks unless the client reads concurrently.
	cb := &protocol.CatchupBuffer{}
	cb.SetBuffer(catchup)
	if err := wire.WriteMessage(c, cb); err != nil {
		return 0, fmt.Errorf("etservertest: write catchup: %w", err)
	}
	var theirs protocol.CatchupBuffer
	if err := wire.ReadMessage(c, &theirs); err != nil {
		return 0, fmt.Errorf("etservertest: read catchup: %w", err)
	}
	for _, b := range theirs.GetBuffer() {
		if err := s.accept(b); err != nil {
			return 0, err
		}
	}
	return flushed, nil
}

func (s *Server) readLoop(r io.Reader) error {
	br := bufio.NewReader(r)
	var buf []byte
	for {
		frame, err := wire.ReadFrame(br, buf)
		if err != nil {
			return fmt.Errorf("etservertest: read: %w", err)
		}
		buf = frame
		if err := s.accept(frame); err != nil {
			return err
		}
	}
}

// accept opens one sealed packet from the client and queues it for Recv.
func (s *Server) accept(b []byte) error {
	encrypted, h, payload, err := wire.ParsePacket(b)
	if err != nil {
		return fmt.Errorf("etservertest: %w", err)
	}
	if !encrypted {
		return errors.New("etservertest: unencrypted packet")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plain, err := s.in.Open(nil, payload)
	if err != nil {
		return fmt.Errorf("etservertest: %w", err)
	}
	s.recvSeq++
	s.queue = append(s.queue, protocol.Packet{Header: h, Payload: plain})
	signal(s.notify)
	// Upstream echoes KEEP_ALIVE (src/terminal/TerminalServer.cpp:389-393).
	if h == protocol.HeaderKeepAlive && s.echo {
		s.enqueueLocked(protocol.Packet{Header: protocol.HeaderKeepAlive})
		signal(s.wake)
	}
	return nil
}

func (s *Server) writeLoop(ctx context.Context, w io.Writer, flushed int64) error {
	for {
		s.mu.Lock()
		pending := s.sent[flushed:]
		ended := !s.registered
		s.mu.Unlock()
		if len(pending) == 0 {
			if ended {
				return errSessionEnded
			}
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		for _, f := range pending {
			if err := wire.WriteFrame(w, f); err != nil {
				return fmt.Errorf("etservertest: write: %w", err)
			}
		}
		flushed += int64(len(pending))
	}
}

func (s *Server) enqueueLocked(p protocol.Packet) {
	sealed := s.out.Seal(nil, p.Payload)
	s.sent = append(s.sent, wire.AppendPacket(nil, true, p.Header, sealed))
}

// Recv returns the next packet the client sent, exactly once and in order,
// including KEEP_ALIVE probes.
func (s *Server) Recv(ctx context.Context) (protocol.Packet, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			p := s.queue[0]
			s.queue[0] = protocol.Packet{}
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return p, nil
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-ctx.Done():
			return protocol.Packet{}, context.Cause(ctx)
		}
	}
}

// Send queues a packet to the client. It is sealed at once and replayed
// across reconnects, like upstream's BackedWriter.
func (s *Server) Send(ctx context.Context, p protocol.Packet) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	s.mu.Lock()
	s.enqueueLocked(p)
	s.mu.Unlock()
	signal(s.wake)
	return nil
}

// EndSession drops the session like a shell exit on etserver 7.0.0: the live
// link flushes what was already sent and closes, and later connects get
// INVALID_KEY (src/terminal/TerminalServer.cpp:329-332,422-426).
func (s *Server) EndSession() {
	s.mu.Lock()
	s.registered = false
	s.mu.Unlock()
	signal(s.wake)
}

// EchoKeepAlive controls whether KEEP_ALIVE packets are echoed (default true).
func (s *Server) EchoKeepAlive(on bool) {
	s.mu.Lock()
	s.echo = on
	s.mu.Unlock()
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
