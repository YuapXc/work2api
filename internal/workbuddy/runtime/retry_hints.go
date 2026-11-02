package runtime

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"
	"work2api/internal/workbuddy/upstream"
)

// Expose only an actual upstream reset hint. Never manufacture a new limit or
// advertise a reset for non-429 responses. Payloads remain credential-free.
func upstreamReset(err error, now time.Time) (time.Time, bool) {
	var e *upstream.UpstreamError
	if !errors.As(err, &e) || e.StatusCode != 429 {
		return time.Time{}, false
	}
	if reset, ok := ParseRateReset(string(e.Raw)); ok && reset > float64(now.Unix()) && reset < float64(now.Add(7*24*time.Hour).Unix()) {
		return time.Unix(int64(reset), 0), true
	}
	if seconds, ok := ParseRetryAfter(e.Header, float64(now.Unix())); ok && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
		return now.Add(time.Duration(math.Ceil(seconds)) * time.Second), true
	}
	if deadline, er := http.ParseTime(e.Header.Get("Retry-After")); er == nil && deadline.After(now) && deadline.Before(now.Add(7*24*time.Hour)) {
		return deadline, true
	}
	return time.Time{}, false
}
func setRetryHints(w http.ResponseWriter, err error) {
	now := time.Now().UTC()
	if reset, ok := upstreamReset(err, now); ok {
		seconds := int64(math.Ceil(reset.Sub(now).Seconds()))
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.Header().Set("X-RateLimit-Reset-At", reset.UTC().Format(time.RFC3339))
	}
}
