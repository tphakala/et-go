// Package etcp is a reliable, ordered, encrypted packet connection to an
// etserver that survives reconnects. Upstream calls this layer EternalTCP
// (src/base/BackedReader.cpp, BackedWriter.cpp, Connection.cpp).
package etcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/seal"
	"github.com/tphakala/et-go/internal/wire"
)

const (
	// defaultKeepAlive is upstream's maximum (src/base/Headers.hpp:180 at
	// et-v7.0.0).
	defaultKeepAlive = 5 * time.Second
	// defaultReplayLimit is upstream's MAX_BACKUP_BYTES
	// (src/base/BackedWriter.hpp:32 at et-v7.0.0).
	defaultReplayLimit = 64 << 20
	dialTimeout        = 10 * time.Second

	// minKeepAlive is the smallest non-zero KeepAlive Dial accepts, a policy
	// floor: much below it the watcher would probe and drop links faster
	// than a typical round trip, and a nanosecond value would spin.
	minKeepAlive = 100 * time.Millisecond
	// maxReplayLimit is the largest ReplayLimit Dial accepts: upstream's own
	// MAX_BACKUP_BYTES. It does not by itself keep a catchup under the
	// message limit (a disconnected ring can hold about twice the limit);
	// writeRecover checks the size and fails with ErrReplayExceeded.
	maxReplayLimit = defaultReplayLimit
)

// ErrSessionEnded reports that the server ended the session, normally because
// the remote shell exited (a redial got INVALID_KEY). It wraps io.EOF, the
// end-of-stream convention of net.Conn, so errors.Is(err, io.EOF) holds.
var ErrSessionEnded = fmt.Errorf("etcp: session ended by server: %w", io.EOF)

var (
	// ErrVersion reports that the server speaks another protocol version
	// (MISMATCHED_PROTOCOL), on the first connect or a redial.
	ErrVersion = errors.New("etcp: protocol version mismatch")
	// ErrIntegrity reports a packet that failed authentication or could not
	// be parsed, a frame above wire.MaxFrameSize, or a handshake message
	// that is oversized or does not decode: the stream is out of step or
	// tampered with and cannot be resumed.
	ErrIntegrity = errors.New("etcp: stream integrity failure")
	// ErrReplayExceeded reports that the peer needs packets no longer
	// retained, is ahead of what was sent, or needs a catchup too large to
	// send in one message.
	ErrReplayExceeded = errors.New("etcp: peer needs data beyond the replay window")
	// ErrRejected reports that the server refused the session: INVALID_KEY
	// on the first connect, NEW_CLIENT on a redial, or an unknown status.
	ErrRejected = errors.New("etcp: server rejected the session")
)

// A Dialer contains options for connecting to an etserver. The zero value is usable.
type Dialer struct {
	// NetDialer dials each TCP link. Nil means a net.Dialer with a 10 s timeout
	// and TCP keepalive (KeepAliveConfig: idle 15 s, interval 5 s, count 3).
	NetDialer interface {
		DialContext(ctx context.Context, network, address string) (net.Conn, error)
	}
	// KeepAlive is the quiet period after which a probe is sent; after two
	// quiet periods the link is declared dead. Only inbound frames count as
	// proof of life, and the probe queues behind any unsent backlog, so an
	// upload that takes longer than two periods to drain while the server
	// sends nothing costs a reconnect (the data survives it). Zero means 5 s
	// (upstream's maximum, src/base/Headers.hpp:180 at et-v7.0.0); Dial
	// refuses a negative value or one below 100 ms. Probing starts only
	// after the first WritePacket, because etserver aborts when a session's
	// first packet is not INITIAL_PAYLOAD
	// (src/terminal/TerminalServer.cpp:429-439 at et-v7.0.0); before that, a
	// dead link is detected only by a read error or by TCP keepalive when
	// the NetDialer enables it (the default one does).
	KeepAlive time.Duration
	// Probe is the packet sent as a liveness probe. The zero value sends header 0
	// with no payload, which is KEEP_ALIVE, echoed by etserver once the session
	// runs (src/terminal/TerminalServer.cpp:389-393 at et-v7.0.0).
	Probe protocol.Packet
	// ReplayLimit bounds the sealed bytes kept for replay; Dial refuses a
	// negative value or one above 64 MiB, and zero means 64 MiB. It is a soft
	// bound applied separately to the two kinds of retained packets: packets
	// already written to a socket are trimmed down to it, and WritePacket
	// blocks while the not-yet-written backlog exceeds it (a single packet
	// may take the backlog over, and probes are never blocked). While
	// disconnected the ring can therefore hold about twice ReplayLimit plus
	// one packet. Packets count as sent once written to the socket, and
	// written packets are trimmed first, so a window smaller than what the
	// kernel and the network can hold in flight turns a reconnect into
	// ErrReplayExceeded; so does a catchup too large for one handshake
	// message. Small values are for tests only.
	ReplayLimit int
	// Logger receives connection events. Nil discards them.
	Logger *slog.Logger
}

// Dial connects and completes the first handshake. It expects NEW_CLIENT; a
// RETURNING_CLIENT answer (upstream allows it when a first attempt died after
// registering, src/base/ClientConnection.cpp:35-39 at et-v7.0.0) runs the recover exchange
// with empty state. INVALID_KEY yields ErrRejected and MISMATCHED_PROTOCOL
// yields ErrVersion. A passkey that is not 32 bytes, a Probe payload too
// large for one sealed frame, or a KeepAlive or ReplayLimit outside the range
// its field documents is refused before dialing.
func (d *Dialer) Dial(ctx context.Context, addr, id, passkey string) (*Conn, error) {
	if len(passkey) != 32 {
		return nil, fmt.Errorf("etcp: passkey must be 32 bytes, got %d", len(passkey))
	}
	if len(d.Probe.Payload) > maxPayload {
		return nil, fmt.Errorf("etcp: probe payload of %d bytes: %w", len(d.Probe.Payload), wire.ErrTooLarge)
	}
	if d.KeepAlive < 0 || (d.KeepAlive > 0 && d.KeepAlive < minKeepAlive) {
		return nil, fmt.Errorf("etcp: KeepAlive %v: must be 0 (the default) or at least %v", d.KeepAlive, minKeepAlive)
	}
	if d.ReplayLimit < 0 || d.ReplayLimit > maxReplayLimit {
		return nil, fmt.Errorf("etcp: ReplayLimit %d: must be between 0 (the default) and %d", d.ReplayLimit, maxReplayLimit)
	}
	c := d.newConn(addr, id, passkey)
	nc, catchup, err := c.connect(ctx, true)
	if err != nil {
		c.cancel(err)
		return nil, err
	}
	c.wg.Go(func() {
		defer close(c.readerDone)
		c.supervise(nc, catchup)
	})
	return c, nil
}

func (d *Dialer) newConn(addr, id, passkey string) *Conn {
	var nd netDialer = d.NetDialer
	if nd == nil {
		nd = &net.Dialer{
			Timeout: dialTimeout,
			KeepAliveConfig: net.KeepAliveConfig{
				Enable:   true,
				Idle:     15 * time.Second,
				Interval: 5 * time.Second,
				Count:    3,
			},
		}
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	c := &Conn{
		netDialer:  nd,
		addr:       addr,
		id:         id,
		keepAlive:  cmp.Or(d.KeepAlive, defaultKeepAlive),
		probe:      d.Probe,
		logger:     logger,
		ctx:        ctx,
		cancel:     cancel,
		inbox:      make(chan protocol.Packet, inboxSize),
		readerDone: make(chan struct{}),
		wake:       make(chan struct{}, 1),
		space:      make(chan struct{}, 1),
	}
	c.limit = cmp.Or(d.ReplayLimit, defaultReplayLimit)
	c.ring.limit = c.limit
	var key [32]byte
	copy(key[:], passkey)
	c.out = seal.New(&key, seal.ClientToServer)
	c.in = seal.New(&key, seal.ServerToClient)
	return c
}

type netDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// connect dials one TCP link and runs the connect handshake on it (plus the
// recover exchange for a returning client). It returns the ready connection
// and the peer's catchup entries, which the new link delivers first. If ctx
// ends during the dial or the handshake, the error wraps context.Cause(ctx),
// unless a definitive error (one isFatal accepts, such as the server's
// rejection) came first. A handshake message that is oversized or does not
// decode yields ErrIntegrity.
func (c *Conn) connect(ctx context.Context, first bool) (net.Conn, [][]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	nc, err := c.netDialer.DialContext(dctx, "tcp", c.addr)
	if err != nil {
		if ctx.Err() != nil {
			// As for the handshake below: the caller needs its cause, not
			// the dialer's generic context error.
			return nil, nil, fmt.Errorf("etcp: dial: %w", context.Cause(ctx))
		}
		return nil, nil, fmt.Errorf("etcp: dial: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	catchup, err := c.handshake(idleConn{Conn: nc, timeout: handshakeIdle}, first)
	if err == nil && !stop() {
		// ctx ended as the handshake finished and the AfterFunc has
		// closed nc: the link is unusable.
		err = context.Cause(ctx)
	}
	if err == nil {
		err = nc.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = nc.Close()
		if errors.Is(err, wire.ErrTooLarge) || errors.Is(err, wire.ErrMalformed) {
			// A handshake message from the server that is oversized or
			// does not decode means a broken or hostile peer (a cut stream
			// yields an EOF error instead); as on the stream, the session
			// cannot continue, and redialing would meet it again. Our own
			// catchup cannot reach here: writeRecover checks its size
			// first and fails with ErrReplayExceeded.
			err = fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		if ctx.Err() != nil && !isFatal(err) {
			// Ending ctx closes nc, so the handshake reports a closed
			// connection; the cause is what the caller needs. A definitive
			// answer that arrived just before (a rejection, a version
			// mismatch) is kept: it says more than the deadline does.
			return nil, nil, fmt.Errorf("etcp: connect: %w", context.Cause(ctx))
		}
		return nil, nil, err
	}
	return nc, catchup, nil
}

func (c *Conn) handshake(conn net.Conn, first bool) ([][]byte, error) {
	req := &protocol.ConnectRequest{}
	req.SetClientId(c.id)
	req.SetVersion(protocol.Version)
	if err := wire.WriteMessage(conn, req); err != nil {
		return nil, fmt.Errorf("etcp: connect request: %w", err)
	}
	var resp protocol.ConnectResponse
	if err := wire.ReadMessage(conn, &resp); err != nil {
		return nil, fmt.Errorf("etcp: connect response: %w", err)
	}
	switch resp.GetStatus() {
	case protocol.ConnectStatus_NEW_CLIENT:
		if first {
			return nil, nil
		}
		// Deliberately stricter than upstream, whose client closes the
		// socket and keeps redialing on any status other than INVALID_KEY
		// and RETURNING_CLIENT (src/base/ClientConnection.cpp:113-127 at
		// et-v7.0.0). This applies to NEW_CLIENT here and equally to
		// MISMATCHED_PROTOCOL and unknown statuses below: a server that
		// has lost the session's state or changed protocol will not answer
		// differently on the next redial.
		return nil, fmt.Errorf("%w: server answered NEW_CLIENT to a returning client", ErrRejected)
	case protocol.ConnectStatus_RETURNING_CLIENT:
		// Also possible on the first connect, when an earlier attempt died
		// after the server registered it. Upstream's client accepts that
		// answer but then skips the recover exchange
		// (src/base/ClientConnection.cpp:35-62 at et-v7.0.0), although the
		// server always runs it after RETURNING_CLIENT
		// (src/base/ServerConnection.cpp:117-123).
		// Running it here with empty state (nothing sent, nothing received)
		// matches what the server expects.
		return c.recover(conn)
	case protocol.ConnectStatus_INVALID_KEY:
		if first {
			return nil, fmt.Errorf("%w: %s", ErrRejected, resp.GetError())
		}
		return nil, ErrSessionEnded
	case protocol.ConnectStatus_MISMATCHED_PROTOCOL:
		return nil, fmt.Errorf("%w: %s", ErrVersion, resp.GetError())
	default:
		return nil, fmt.Errorf("%w: unexpected status %v", ErrRejected, resp.GetStatus())
	}
}

// isFatal reports whether a reconnect error ends the Conn instead of
// triggering a retry. Integrity failures on the stream never reach here (the
// reader ends the Conn directly); ones in a handshake message do.
func isFatal(err error) bool {
	for _, target := range []error{ErrSessionEnded, ErrVersion, ErrReplayExceeded, ErrRejected, ErrIntegrity} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
