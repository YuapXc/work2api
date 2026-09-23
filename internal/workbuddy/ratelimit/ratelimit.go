// Package ratelimit provides per-account rate limiting: a minimum request
// interval plus jitter, to avoid tripping upstream anti-abuse. Ported from
// workbuddy_one/ratelimit.py.
package ratelimit

import (
	"math/rand"
	"sync"
	"time"
)

// Limiter enforces a minimum interval between requests with +/- jitter.
type Limiter struct {
	minInterval time.Duration
	jitter      time.Duration
	mu          sync.Mutex
	last        time.Time
}

// New returns a limiter. Defaults mirror the Python impl: 1.5s interval,
// 0.3s jitter.
func New(minInterval, jitter time.Duration) *Limiter {
	if minInterval <= 0 {
		minInterval = 1500 * time.Millisecond
	}
	if jitter < 0 {
		jitter = 300 * time.Millisecond
	}
	return &Limiter{minInterval: minInterval, jitter: jitter}
}

// Wait blocks until the account is allowed to make its next request.
func (l *Limiter) Wait() {
	l.mu.Lock()
	now := time.Now()
	wait := l.minInterval - now.Sub(l.last)
	if wait > 0 {
		if l.jitter > 0 {
			// add jitter in [-jitter, +jitter]
			delta := time.Duration(rand.Int63n(int64(2*l.jitter))) - l.jitter
			wait += delta
		}
		if wait < 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		l.last = time.Now().Add(wait)
	} else {
		wait = 0
		l.last = now
	}
	l.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}
