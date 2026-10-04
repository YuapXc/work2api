// Package tokenusage normalizes upstream usage without inventing cache observations.
package tokenusage

import "math"

// number accepts integral token counts. Missing, null and malformed values remain unknown.
func number(v any) *int {
	var n int
	switch x := v.(type) {
	case int:
		n = x
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Trunc(x) != x || x >= float64(math.MaxInt) || x < float64(math.MinInt) {
			return nil
		}
		n = int(x)
	default:
		return nil
	}
	return &n
}

func first(u map[string]any, keys ...string) *int {
	for _, key := range keys {
		if n := number(u[key]); n != nil {
			return n
		}
	}
	return nil
}

// cacheCount supports redundant aliases; a positive count takes precedence over
// a zero placeholder. Invalid values are retained for diagnostics, never invented as zero.
func cacheCount(values ...any) *int {
	var zero, invalid *int
	for _, v := range values {
		if n := number(v); n != nil {
			if *n > 0 {
				return n
			}
			if *n == 0 {
				zero = n
			} else {
				invalid = n
			}
		}
	}
	if zero != nil {
		return zero
	}
	return invalid
}

func detail(u map[string]any, key, field string) any {
	m, _ := u[key].(map[string]any)
	return m[field]
}

func Cached(u map[string]any) *int {
	return cacheCount(u["prompt_cache_hit_tokens"], u["cache_read_input_tokens"],
		detail(u, "prompt_tokens_details", "cached_tokens"), detail(u, "input_tokens_details", "cached_tokens"))
}

func Created(u map[string]any) *int {
	return cacheCount(u["cache_creation_input_tokens"], u["prompt_cache_write_tokens"])
}

// Reasoning preserves a valid observed output count; absent observations stay unknown.
func Reasoning(u map[string]any) *int {
	for _, v := range []any{detail(u, "completion_tokens_details", "reasoning_tokens"), detail(u, "output_tokens_details", "reasoning_tokens"), u["reasoning_tokens"]} {
		if n := number(v); n != nil && *n >= 0 {
			output := first(u, "completion_tokens", "output_tokens")
			if output == nil || *n <= *output {
				return n
			}
		}
	}
	return nil
}

// Totals returns inclusive input and output counts. Native Anthropic input is
// exclusive of cache read/write; OpenAI prompt/input tokens are inclusive.
func Totals(u map[string]any) (int, int) {
	input := first(u, "prompt_tokens", "input_tokens")
	output := first(u, "completion_tokens", "output_tokens")
	i, o := 0, 0
	if input != nil {
		i = *input
	}
	if output != nil {
		o = *output
	}
	if nativeAnthropic(u) {
		for _, n := range []*int{Cached(u), Created(u)} {
			if n != nil && *n >= 0 && i >= 0 && *n <= math.MaxInt-i {
				i += *n
			}
		}
	}
	return i, o
}

// KnownTotals distinguishes observed zero from malformed or absent counts.
func KnownTotals(u map[string]any) (bool, bool) {
	i, o := first(u, "prompt_tokens", "input_tokens"), first(u, "completion_tokens", "output_tokens")
	return i != nil && *i >= 0, o != nil && *o >= 0
}

func nativeAnthropic(u map[string]any) bool {
	if number(u["prompt_tokens"]) != nil || number(u["input_tokens"]) == nil {
		return false
	}
	// Responses also uses input_tokens, but exposes its cache detail structure.
	if _, ok := u["input_tokens_details"]; ok {
		return false
	}
	_, read := u["cache_read_input_tokens"]
	_, write := u["cache_creation_input_tokens"]
	return read || write
}

// ValidCache excludes impossible counts from client-facing cache calculations.
func ValidCache(u map[string]any) (*int, *int) {
	i, _ := Totals(u)
	r, w := Cached(u), Created(u)
	if r != nil && (*r < 0 || *r > i) {
		r = nil
	}
	if w != nil && (*w < 0 || *w > i) {
		w = nil
	}
	if r != nil && w != nil && *w > i-*r {
		return nil, nil
	}
	return r, w
}
