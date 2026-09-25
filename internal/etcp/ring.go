package etcp

import "slices"

// ring holds sealed, serialized outbound packets by sequence number, for the
// link writer and for replay after a reconnect. Sequence numbers are
// contiguous: entries[i] has sequence number first+i.
type ring struct {
	first   int64
	entries [][]byte
	bytes   int
	limit   int
}

// next is the sequence number the next pushed packet gets, which is also the
// number of packets sealed so far.
func (r *ring) next() int64 { return r.first + int64(len(r.entries)) }

func (r *ring) push(data []byte) {
	r.entries = append(r.entries, data)
	r.bytes += len(data)
}

// trim drops the oldest entries while the ring holds more than limit bytes,
// never dropping an entry at or after keep (not yet written to any link).
func (r *ring) trim(keep int64) {
	n := 0
	for n < len(r.entries) && r.bytes > r.limit && r.first+int64(n) < keep {
		r.bytes -= len(r.entries[n])
		r.entries[n] = nil
		n++
	}
	r.entries = r.entries[n:]
	r.first += int64(n)
}

// appendRange appends entries [from, to) to dst. The caller guarantees
// first <= from <= to <= next().
func (r *ring) appendRange(dst [][]byte, from, to int64) [][]byte {
	return append(dst, r.entries[from-r.first:to-r.first]...)
}

// since returns a copy of entries [from, to), or false when from is outside
// the retained window [first, to].
func (r *ring) since(from, to int64) ([][]byte, bool) {
	if from < r.first || from > to || to > r.next() {
		return nil, false
	}
	return slices.Clone(r.entries[from-r.first : to-r.first]), true
}

// bytesBetween sums the sizes of entries [from, to).
func (r *ring) bytesBetween(from, to int64) int {
	var n int
	for _, e := range r.entries[from-r.first : to-r.first] {
		n += len(e)
	}
	return n
}
