// Package seal implements one direction of Eternal Terminal's encrypted
// stream: libsodium crypto_secretbox (XSalsa20-Poly1305) with a counter nonce.
//
// Wire compatibility with upstream (src/base/CryptoHandler.cpp at et-v7.0.0):
// the key is the 32-byte session passkey; the nonce is 24 bytes whose last
// byte is the stream direction, and it is incremented little-endian from
// byte 0 before every seal or open. golang.org/x/crypto/nacl/secretbox
// produces the same MAC-then-ciphertext layout as crypto_secretbox_easy
// (MEASURED 2026-09-24, C against Go).
package seal

import (
	"errors"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/nacl/secretbox"
)

// Direction selects the nonce stream. Its value is the nonce's last byte.
type Direction uint8

// The two streams of a connection (upstream CLIENT_SERVER_NONCE_MSB and
// SERVER_CLIENT_NONCE_MSB, src/base/Headers.hpp:172-173 at et-v7.0.0).
const (
	ClientToServer Direction = 0
	ServerToClient Direction = 1
)

// ErrOpen reports a box that failed authentication: the wrong key, the wrong
// position in the stream, or tampered data.
var ErrOpen = errors.New("seal: message authentication failed")

// Stream seals or opens one direction of a connection. Each call advances the
// nonce, so boxes must be opened in the order they were sealed. Not safe for
// concurrent use.
//
// The nonce lives behind a pointer, so a copy of a Stream shares its nonce
// counter with the original and sealing through both never reuses a nonce.
// The key sits behind a second pointer: fmt prints a Stream held in an
// unexported field by reflection, without calling Format, and for a verb
// that is invalid on a pointer (%s, %q, %t and others) it dereferences the
// state once, which shows the key only as an address and the nonce, which is
// public, as bytes. The zero value is not usable; get a Stream from New.
type Stream struct {
	st *state
}

// state is the mutable part of a Stream.
type state struct {
	key   *[32]byte
	nonce [24]byte
}

// New returns a Stream for direction d with the nonce at its initial value.
// It copies *key, so later changes to the caller's array do not affect it.
// key must not be nil, and d must be ClientToServer or ServerToClient: any
// other direction produces a nonce stream the peer never uses, so every Open
// fails as if the key were wrong.
func New(key *[32]byte, d Direction) *Stream {
	k := *key
	st := &state{key: &k}
	st.nonce[len(st.nonce)-1] = byte(d)
	return &Stream{st: st}
}

// Seal encrypts and authenticates plaintext with the next nonce and appends
// the box (16-byte MAC, then ciphertext) to dst. dst must not share memory
// with plaintext: golang.org/x/crypto/nacl/secretbox panics when the output
// it writes overlaps the input, as it does for Seal(buf[:0], buf) whenever
// buf has capacity for the box.
func (s *Stream) Seal(dst, plaintext []byte) []byte {
	s.st.increment()
	return secretbox.Seal(dst, plaintext, &s.st.nonce, s.st.key)
}

// Open authenticates and decrypts box with the next nonce and appends the
// plaintext to dst. dst must not share memory with box, for the same reason
// as in Seal; a payload from wire.ParsePacket aliases the frame buffer, so
// that buffer cannot be reused as dst.
//
// If authentication fails, Open returns ErrOpen. The nonce has still
// advanced, so the Stream is now out of step with the peer and must be
// discarded: upstream treats a failed decrypt as fatal
// (src/base/CryptoHandler.cpp:37-42 at et-v7.0.0), so a session cannot be
// resumed after one.
func (s *Stream) Open(dst, box []byte) ([]byte, error) {
	s.st.increment()
	out, ok := secretbox.Open(dst, box, &s.st.nonce, s.st.key)
	if !ok {
		return nil, ErrOpen
	}
	return out, nil
}

// increment adds one to the nonce as a little-endian number across all 24
// bytes, matching upstream CryptoHandler::incrementNonce.
func (st *state) increment() {
	for i := range st.nonce {
		st.nonce[i]++
		if st.nonce[i] != 0 {
			return
		}
	}
}

// Format implements fmt.Formatter so every verb prints a fixed, redacted
// form instead of the state, because the binding rule forbids the session
// passkey ever appearing in fmt output. The direction byte is shown; the
// nonce counter is not, and the key never is. The receiver is a value so that
// both fmt.Sprint(s) and fmt.Sprint(*s) reach this method; the zero value
// prints seal.Stream{}.
func (s Stream) Format(f fmt.State, _ rune) {
	if s.st == nil {
		_, _ = fmt.Fprint(f, "seal.Stream{}")
		return
	}
	dir := s.st.nonce[len(s.st.nonce)-1]
	_, _ = fmt.Fprintf(f, "seal.Stream{dir:%d key:REDACTED}", dir)
}

// LogValue implements slog.LogValuer for the same reason Format exists: a
// handler that does not go through fmt (the JSON handler, for one) must not
// see the key either. The value receiver keeps both slog.Any("s", s) and
// slog.Any("s", *s) redacted, matching Format. A value receiver cannot
// guard a nil *Stream: slog of one logs the recovered nil-dereference panic,
// never key bytes.
func (s Stream) LogValue() slog.Value {
	if s.st == nil {
		return slog.GroupValue()
	}
	dir := s.st.nonce[len(s.st.nonce)-1]
	return slog.GroupValue(
		slog.Int("dir", int(dir)),
		slog.String("key", "REDACTED"),
	)
}
