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
	// While held, trim keeps entries at or after hold, the sequence the
	// peer last acknowledged, until written entries exceed twice limit.
	held bool
	hold int64
}

// next is the sequence number the next pushed packet gets, which is also the
// number of packets sealed so far.
func (r *ring) next() int64 { return r.first + int64(len(r.entries)) }

func (r *ring) push(data []byte) {
	r.entries = append(r.entries, data)
	r.bytes += len(data)
}

// trim drops the oldest written entries while they hold more than limit
// bytes, never dropping an entry at or after keep (not yet written to any
// link). unsent is the size of the entries from keep on; the limit applies
// to written entries alone, so a full backlog cannot squeeze out the replay
// copies of packets still in flight. While held, entries at or after hold
// are dropped only once written entries exceed twice limit.
func (r *ring) trim(keep int64, unsent int) {
	n := 0
	for n < len(r.entries) && r.bytes-unsent > r.limit && r.first+int64(n) < keep {
		if r.held && r.first+int64(n) >= r.hold && r.bytes-unsent <= 2*r.limit {
			break
		}
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
// the retained window [first, to] or to is past next().
func (r *ring) since(from, to int64) ([][]byte, bool) {
	if from < r.first || from > to || to > r.next() {
		return nil, false
	}
	return slices.Clone(r.entries[from-r.first : to-r.first]), true
}

// bytesBetween sums the sizes of entries [from, to). The caller guarantees
// first <= from <= to <= next().
func (r *ring) bytesBetween(from, to int64) int {
	var n int
	for _, e := range r.entries[from-r.first : to-r.first] {
		n += len(e)
	}
	return n
}
