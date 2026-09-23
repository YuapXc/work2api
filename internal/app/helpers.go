// Package app wires the shared HTTP surface: the three OpenAI/Anthropic
// inference endpoints and the /admin/* management API. Ported from
// workbuddy_one/app.py (single-user all-in-one; multi-user portal deferred).
package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Cooldown durations (seconds) per error class.
const (
	cooldownNone = 0.0
	cooldownSoft = 60.0
	cooldownHard = 1800.0
)

// cooldownFor picks a cooldown by upstream HTTP status.
func cooldownFor(status int) float64 {
	switch {
	case status == 429:
		return 300.0
	case status == 401 || status == 403:
		return cooldownHard
	case status >= 500:
		return 120.0
	default:
		return cooldownSoft
	}
}

// failoverStatus reports whether an upstream status warrants rotating to a
// healthy account and retrying once. Beyond the upstream Python's 429/502/503,
// we also fail over on 401/403 (dead credential): the offending account gets a
// hard cooldown while a healthy account serves the request, so a single bad
// credential no longer leaks a spurious auth error to the client when another
// account could have served it. (High-value deviation from upstream, approved.)
func failoverStatus(status int) bool {
	switch status {
	case 401, 403, 429, 502, 503:
		return true
	default:
		return false
	}
}

var limitResetRe = regexp.MustCompile(`(?i)(20\d{2}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})\s*UTC\+8`)

// dailyModelLimit detects the upstream per-day model limit (code=6004) and
// returns its explicit UTC+8 reset time.
func dailyModelLimit(raw []byte, now float64) (float64, string, bool) {
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return 0, "", false
	}
	if toStr(data["code"]) != "6004" {
		return 0, "", false
	}
	message := toStr(data["msg"])
	m := limitResetRe.FindStringSubmatch(message)
	if m == nil {
		return 0, "", false
	}
	loc := time.FixedZone("UTC+8", 8*3600)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], loc)
	if err != nil {
		return 0, "", false
	}
	reset := float64(t.Unix())
	if now == 0 {
		now = float64(time.Now().UnixNano()) / 1e9
	}
	if reset <= now || reset > now+48*3600 {
		return 0, "", false
	}
	return reset, message, true
}

// upstreamErrorText extracts a searchable error summary for usage logs.
func upstreamErrorText(status int, raw []byte) string {
	var data map[string]any
	if json.Unmarshal(raw, &data) == nil {
		code := data["code"]
		msg := data["msg"]
		if msg == nil {
			msg = data["message"]
		}
		if code != nil || msg != nil {
			return strings.TrimSpace("HTTP " + itoa(status) + " code=" + orDash(code) + " " + toStr(msg))
		}
	}
	return "HTTP " + itoa(status)
}

// parseModelAliases parses "alias=real" lines.
func parseModelAliases(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "=") {
			continue
		}
		alias, real, _ := strings.Cut(line, "=")
		alias, real = strings.TrimSpace(alias), strings.TrimSpace(real)
		if alias != "" && real != "" {
			out[alias] = real
		}
	}
	return out
}

func jsonError(status int, message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message, "type": "upstream_error", "code": status}})
	return string(b)
}

// errAnthropic formats an Anthropic-style SSE error event.
func errAnthropic(status int, message string) string {
	b, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "upstream_error", "message": message},
	})
	return "event: error\ndata: " + string(b) + "\n\n"
}

// safeErr builds an error detail body from a raw upstream error.
func safeErr(raw []byte, status int) map[string]any {
	var data any
	if json.Unmarshal(raw, &data) == nil {
		if m, ok := data.(map[string]any); ok {
			if _, has := m["error"]; has {
				return m
			}
		}
	}
	msg := string(raw)
	if msg == "" {
		msg = "upstream error"
	}
	return map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": status}}
}

// convUsage maps converter usage (prompt/completion or input/output) to a
// usage map for logging.
func convUsage(u map[string]any) map[string]any { return u }

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return itoa(int(x))
		}
	}
	return ""
}

func orDash(v any) string {
	s := toStr(v)
	if s == "" {
		return "-"
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
