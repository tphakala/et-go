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
	defaultReplayLimit = 64 << 20 // upstream MAX_BACKUP_BYTES (src/base/BackedWriter.hpp:32)
	dialTimeout        = 10 * time.Second
)

// ErrSessionEnded reports that the server ended the session, normally because
// the remote shell exited (a redial got INVALID_KEY). It wraps io.EOF, the
// end-of-stream convention of net.Conn, so errors.Is(err, io.EOF) holds.
var ErrSessionEnded = fmt.Errorf("etcp: session ended by server: %w", io.EOF)

var (
	ErrVersion        = errors.New("etcp: protocol version mismatch")
	ErrIntegrity      = errors.New("etcp: stream integrity failure")
	ErrReplayExceeded = errors.New("etcp: peer needs data beyond the replay window")
	ErrRejected       = errors.New("etcp: server rejected the session")
)

// A Dialer contains options for connecting to an etserver. The zero value is usable.
type Dialer struct {
	// NetDialer dials each TCP link. Nil means a net.Dialer with a 10 s timeout
	// and TCP keepalive (KeepAliveConfig: idle 15 s, interval 5 s, count 3).
	NetDialer interface {
		DialContext(ctx context.Context, network, address string) (net.Conn, error)
	}
	// KeepAlive is the quiet period after which a probe is sent; after two quiet
	// periods the link is declared dead. Zero means 5 s (upstream's maximum).
	KeepAlive time.Duration
	// Probe is the packet sent as a liveness probe. The zero value sends header 0
	// with no payload, which is KEEP_ALIVE and is echoed by etserver.
	Probe protocol.Packet
	// ReplayLimit bounds the sealed bytes kept for replay. Zero means 64 MiB.
	// Packets count as sent once written to the socket, and written packets
	// are trimmed first, so a window smaller than what the kernel and the
	// network can hold in flight turns a reconnect into ErrReplayExceeded.
	// Small values are for tests only.
	ReplayLimit int
	// Logger receives connection events. Nil discards them.
	Logger *slog.Logger
}

// Dial connects and completes the first handshake. It expects NEW_CLIENT; a
// RETURNING_CLIENT answer (upstream allows it when a first attempt died after
// registering, src/base/ClientConnection.cpp:36-39) runs the recover exchange
// with empty state. INVALID_KEY yields ErrRejected and MISMATCHED_PROTOCOL
// yields ErrVersion.
func (d *Dialer) Dial(ctx context.Context, addr, id, passkey string) (*Conn, error) {
	if len(passkey) != 32 {
		return nil, fmt.Errorf("etcp: passkey must be 32 bytes, got %d", len(passkey))
	}
	c := d.newConn(addr, id, passkey)
	nc, catchup, err := c.connect(ctx, true)
	if err != nil {
		c.cancel(err)
		return nil, err
	}
	c.wg.Go(func() { c.supervise(nc, catchup) })
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
		netDialer: nd,
		addr:      addr,
		id:        id,
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		inbox:     make(chan protocol.Packet, inboxSize),
		wake:      make(chan struct{}, 1),
		space:     make(chan struct{}, 1),
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
// and the peer's catchup entries, which the new link delivers first.
func (c *Conn) connect(ctx context.Context, first bool) (net.Conn, [][]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	nc, err := c.netDialer.DialContext(dctx, "tcp", c.addr)
	if err != nil {
		return nil, nil, fmt.Errorf("etcp: dial: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	catchup, err := c.handshake(idleConn{Conn: nc, timeout: handshakeIdle}, first)
	if err == nil {
		err = nc.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = nc.Close()
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
		return nil, fmt.Errorf("%w: server answered NEW_CLIENT to a returning client", ErrRejected)
	case protocol.ConnectStatus_RETURNING_CLIENT:
		// Also possible on the first connect, when an earlier attempt died
		// after the server registered it. Upstream's client accepts that
		// answer but then skips the recover exchange
		// (src/base/ClientConnection.cpp:35-39), although the server always
		// runs it after RETURNING_CLIENT (src/base/ServerConnection.cpp:117-122).
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
// triggering a retry. Integrity failures never reach here: the reader ends the
// Conn directly.
func isFatal(err error) bool {
	for _, target := range []error{ErrSessionEnded, ErrVersion, ErrReplayExceeded, ErrRejected} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
