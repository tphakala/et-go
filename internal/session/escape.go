package session

// escapeFilter implements the ssh-style local escape on the keyboard stream.
// At the start of input or right after CR or LF, "~." detaches and "~~" sends
// one "~". A "~" followed by anything else is sent unchanged with that byte.
// State carries across calls, so an escape split over two reads still works.
type escapeFilter struct {
	midLine bool // false at the start of input and right after CR or LF
	tilde   bool // a "~" at line start is held until the next byte decides
}

// filter appends the bytes of src that should reach the server to dst. It
// reports true when src completed "~.", in which case the rest of src is
// discarded.
func (f *escapeFilter) filter(dst, src []byte) ([]byte, bool) {
	for _, b := range src {
		if f.tilde {
			f.tilde = false
			switch b {
			case '.':
				return dst, true
			case '~':
				dst = append(dst, '~')
				f.midLine = true
				continue
			default:
				dst = append(dst, '~')
			}
		} else if !f.midLine && b == '~' {
			f.tilde = true
			continue
		}
		dst = append(dst, b)
		f.midLine = b != '\r' && b != '\n'
	}
	return dst, false
}
