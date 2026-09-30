// Package backoffutil holds small reconnect/backoff helpers shared across
// this repo's bridge clients and the bridge facade.
package backoffutil

import (
	"math/rand"
	"time"
)

// Jitter returns d randomized within +/-20%, so many devices reconnecting at
// once after a shared outage don't all retry in lockstep.
func Jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) * 2 / 5 // 40% of d, i.e. the full +/-20% range
	return d - time.Duration(spread/2) + time.Duration(rand.Int63n(spread+1))
}
