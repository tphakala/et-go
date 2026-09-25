package etcp

import (
	"math/rand/v2"
	"time"
)

const (
	backoffBase  = 250 * time.Millisecond
	backoffMax   = 5 * time.Second
	backoffReset = 30 * time.Second // a link that lived this long resets the schedule
)

// backoff yields reconnect delays: none before the first attempt, then 250 ms
// doubling to a 5 s cap, each with +/-20% jitter, never above the cap.
type backoff struct {
	attempt int
	jitter  func() float64 // returns [0, 1); nil means math/rand/v2
}

func (b *backoff) next() time.Duration {
	n := b.attempt
	b.attempt++
	if n == 0 {
		return 0
	}
	d := min(backoffBase<<min(n-1, 5), backoffMax)
	j := rand.Float64
	if b.jitter != nil {
		j = b.jitter
	}
	d = time.Duration(float64(d) * (0.8 + 0.4*j()))
	return min(d, backoffMax)
}

func (b *backoff) reset() { b.attempt = 0 }
