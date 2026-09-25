package console

import (
	"unicode/utf16"
	"unicode/utf8"
)

// utf16Decoder converts UTF-16 code units to UTF-8. A high surrogate at the
// end of one call is carried into the next, so a character split across two
// console reads is decoded once, correctly. Unpaired surrogates become U+FFFD.
// The zero value is ready to use. Not safe for concurrent use.
type utf16Decoder struct {
	high uint16 // pending high surrogate, 0 if none
}

// append decodes units and appends the UTF-8 result to dst.
func (d *utf16Decoder) append(dst []byte, units []uint16) []byte {
	for _, u := range units {
		if d.high != 0 {
			high := d.high
			d.high = 0
			if isLowSurrogate(u) {
				dst = utf8.AppendRune(dst, utf16.DecodeRune(rune(high), rune(u)))
				continue
			}
			dst = utf8.AppendRune(dst, utf8.RuneError)
		}
		switch {
		case isHighSurrogate(u):
			d.high = u
		case isLowSurrogate(u):
			dst = utf8.AppendRune(dst, utf8.RuneError)
		default:
			dst = utf8.AppendRune(dst, rune(u))
		}
	}
	return dst
}

// utf8Encoder converts UTF-8 to UTF-16. An incomplete UTF-8 sequence at the
// end of one call is carried into the next, so a character split across two
// network packets is encoded once, correctly. Invalid bytes become U+FFFD,
// one per byte, as in a Go []rune conversion. The zero value is ready to use.
// Not safe for concurrent use.
type utf8Encoder struct {
	carry [utf8.UTFMax]byte
	n     int // bytes held in carry, always < utf8.UTFMax
}

// append encodes p and appends the UTF-16 result to dst.
func (e *utf8Encoder) append(dst []uint16, p []byte) []uint16 {
	if e.n > 0 {
		// Join the carried bytes with at most UTFMax bytes of p; that is
		// always enough to finish or reject the carried sequence.
		k := min(len(p), utf8.UTFMax)
		joined := append(e.carry[:e.n:e.n], p[:k]...)
		used := 0
		for used < e.n {
			if !utf8.FullRune(joined[used:]) {
				// Only reachable when all of p fit into joined: keep waiting.
				e.n = copy(e.carry[:], joined[used:])
				return dst
			}
			r, size := utf8.DecodeRune(joined[used:])
			dst = utf16.AppendRune(dst, r)
			used += size
		}
		p = p[used-e.n:]
		e.n = 0
	}
	for len(p) > 0 {
		if !utf8.FullRune(p) {
			e.n = copy(e.carry[:], p)
			break
		}
		r, size := utf8.DecodeRune(p)
		dst = utf16.AppendRune(dst, r)
		p = p[size:]
	}
	return dst
}

// chunkLen returns how many of units to pass to one console write of at most
// limit units. It never ends a chunk between a high and a low surrogate, so
// each write carries whole characters. limit must be at least 2.
func chunkLen(units []uint16, limit int) int {
	if len(units) <= limit {
		return len(units)
	}
	if isHighSurrogate(units[limit-1]) && isLowSurrogate(units[limit]) {
		return limit - 1
	}
	return limit
}

func isHighSurrogate(u uint16) bool { return u >= 0xD800 && u < 0xDC00 }

func isLowSurrogate(u uint16) bool { return u >= 0xDC00 && u < 0xE000 }
