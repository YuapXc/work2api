// Package ratelimit provides per-account rate limiting: a minimum request
// interval plus jitter, to avoid tripping upstream anti-abuse. Ported from
// workbuddy_one/ratelimit.py.
package ratelimit

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// Limiter enforces a minimum interval between requests with +/- jitter.
type Limiter struct {
	minInterval time.Duration
	jitter      time.Duration
	turn        chan struct{}
	pending     chan struct{}
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
	return &Limiter{minInterval: minInterval, jitter: jitter, turn: make(chan struct{}, 1), pending: make(chan struct{}, 32)}
}

var ErrQueueFull = errors.New("account rate-limit queue is full")

// Wait bounds queued callers and only records actual grants. Cancelled callers
// release admission without leaving reservations in the account's future.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.pending <- struct{}{}:
		defer func() { <-l.pending }()
	default:
		return ErrQueueFull
	}
	select {
	case l.turn <- struct{}{}:
		defer func() { <-l.turn }()
	case <-ctx.Done():
		return ctx.Err()
	}
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
	}
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.last = time.Now()
	return nil
}

// Try grants an immediately ready account without entering another queue.
func (l *Limiter) Try() bool {
	select {
	case l.turn <- struct{}{}:
		defer func() { <-l.turn }()
	default:
		return false
	}
	if len(l.pending) > 0 || time.Since(l.last) < l.minInterval {
		return false
	}
	l.last = time.Now()
	return true
}
