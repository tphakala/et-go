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
type Stream struct {
	key   [32]byte
	nonce [24]byte
}

// New returns a Stream for direction d with the nonce at its initial value.
func New(key *[32]byte, d Direction) *Stream {
	s := &Stream{key: *key}
	s.nonce[len(s.nonce)-1] = byte(d)
	return s
}

// Seal encrypts and authenticates plaintext with the next nonce and appends
// the box (16-byte MAC, then ciphertext) to dst.
func (s *Stream) Seal(dst, plaintext []byte) []byte {
	s.increment()
	return secretbox.Seal(dst, plaintext, &s.nonce, &s.key)
}

// Open authenticates and decrypts box with the next nonce and appends the
// plaintext to dst. It returns ErrOpen if authentication fails; the nonce
// still advances, as it does upstream.
func (s *Stream) Open(dst, box []byte) ([]byte, error) {
	s.increment()
	out, ok := secretbox.Open(dst, box, &s.nonce, &s.key)
	if !ok {
		return nil, ErrOpen
	}
	return out, nil
}

// increment adds one to the nonce as a little-endian number across all 24
// bytes, matching upstream CryptoHandler::incrementNonce.
func (s *Stream) increment() {
	for i := range s.nonce {
		s.nonce[i]++
		if s.nonce[i] != 0 {
			return
		}
	}
}
