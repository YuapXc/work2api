package trae

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"work2api/internal/trae/auth"
)

type settledRecord struct{ credits, cache, input, output *float64 }
type settlementEntry struct {
	record  settledRecord
	version uint64
}
type settlementFlight struct {
	active  int
	overlap bool
}
type settlementLease struct {
	r      *Runtime
	key    string
	state  *settlementFlight
	before settlementEntry
}

func (r *Runtime) settledUsageEnabled() bool {
	if r.db == nil {
		return false
	}
	settings, err := r.db.GetSettings()
	return err == nil && settings["trae_settled_usage"] == "1"
}
func conversationID(session string) string {
	sum := sha256.Sum256([]byte("trae-conversation\x00" + session))
	if session == "" {
		_, _ = rand.Read(sum[:])
	}
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func (r *Runtime) beginSettlement(key string) *settlementLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settlementFlights == nil {
		r.settlementFlights = map[string]*settlementFlight{}
	}
	f := r.settlementFlights[key]
	if f == nil {
		f = &settlementFlight{}
		r.settlementFlights[key] = f
	} else {
		f.overlap = true
	}
	f.active++
	return &settlementLease{r: r, key: key, state: f, before: r.settlements[key]}
}
func (l *settlementLease) close() {
	l.r.mu.Lock()
	defer l.r.mu.Unlock()
	l.state.active--
	if l.state.active == 0 && l.r.settlementFlights[l.key] == l.state {
		delete(l.r.settlementFlights, l.key)
	}
}
func billingNumber(v any) *float64 {
	var n float64
	switch value := v.(type) {
	case float64:
		n = value
	case string:
		var err error
		n, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil
		}
	default:
		return nil
	}
	if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func billingField(m map[string]any, keys ...string) *float64 {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if n := billingNumber(v); n != nil {
				return n
			}
		}
	}
	return nil
}
func parseSettlement(raw []byte) (*settledRecord, bool) {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return nil, true
	}
	data, _ := root["data"].(map[string]any)
	if data == nil {
		data = root
	}
	for _, m := range []map[string]any{root, data} {
		if code, ok := m["code"]; ok && code != nil && code != float64(0) && code != "0" {
			return nil, false
		}
	}
	bill, _ := data["user_usage_group_by_session"].(map[string]any)
	if bill == nil {
		bill, _ = data["userUsageGroupBySession"].(map[string]any)
	}
	if bill == nil {
		bill = data
	}
	extra, _ := bill["extra_info"].(map[string]any)
	if extra == nil {
		extra, _ = bill["extraInfo"].(map[string]any)
	}
	rec := &settledRecord{cache: billingField(extra, "cache_read_token", "cacheReadToken"), input: billingField(extra, "input_token", "inputToken"), output: billingField(extra, "output_token", "outputToken")}
	sources, _ := bill["credits_by_source"].(map[string]any)
	if sources == nil {
		sources, _ = bill["creditsBySource"].(map[string]any)
	}
	var total float64
	valid := len(sources) > 0
	for _, v := range sources {
		n := billingNumber(v)
		if n == nil {
			valid = false
			break
		}
		total += *n
	}
	if valid && !math.IsInf(total, 0) {
		rec.credits = &total
	} else {
		rec.credits = billingField(bill, "credits_float", "creditsFloat", "credits_consumed", "consumed_credits", "credit_cost", "credits_cost", "amount_float", "amountFloat")
	}
	for _, v := range []*float64{rec.credits, rec.cache, rec.input, rec.output} {
		if v != nil && *v > 0 {
			return rec, true
		}
	}
	return nil, true
}
func (r *Runtime) readSettlement(ctx context.Context, a *auth.Auth, session string) (*settledRecord, bool) {
	endpoint := r.billingURL
	if endpoint == "" {
		endpoint = "https://api5-normal.mchost.guru/api/v1/commercial/get_session_usage"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"session_id": session})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT())
	resp, err := r.client(ctx).HTTP.Do(req)
	if err != nil {
		return nil, true
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode == 429 || resp.StatusCode >= 500
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, true
	}
	return parseSettlement(raw)
}
func settlementTotal(r settledRecord) float64 {
	var n float64
	for _, v := range []*float64{r.credits, r.cache, r.input, r.output} {
		if v != nil {
			n += *v
		}
	}
	return n
}
func settlementDifference(now, before *float64) *float64 {
	if now == nil || before == nil {
		return nil
	}
	n := *now - *before
	if n < 0 {
		return nil
	}
	return &n
}
func (l *settlementLease) settle(ctx context.Context, a *auth.Auth, session string, usage map[string]any, stream bool) *float64 {
	wait := 2500 * time.Millisecond
	if stream {
		wait = 400 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var record *settledRecord
	for attempt := 0; ; attempt++ {
		next, retry := l.r.readSettlement(ctx, a, session)
		if !retry {
			return nil
		}
		record = next
		if record != nil && (l.before.version == 0 || settlementTotal(*record) != settlementTotal(l.before.record)) {
			break
		}
		delays := []time.Duration{150 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond}
		if attempt >= len(delays) {
			return nil
		}
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) <= delays[attempt] {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delays[attempt]):
		}
	}
	if record == nil || ctx.Err() != nil {
		return nil
	}
	l.r.mu.Lock()
	defer l.r.mu.Unlock()
	current := l.r.settlements[l.key]
	if current.version != l.before.version {
		return nil
	}
	if l.r.settlements == nil {
		l.r.settlements = map[string]settlementEntry{}
	}
	if len(l.r.settlements) >= 512 && current.version == 0 {
		for k := range l.r.settlements {
			delete(l.r.settlements, k)
			break
		}
	}
	l.r.settlementClock++
	l.r.settlements[l.key] = settlementEntry{*record, l.r.settlementClock}
	share := *record
	if l.before.version > 0 {
		for _, pair := range [][2]*float64{{record.credits, l.before.record.credits}, {record.cache, l.before.record.cache}, {record.input, l.before.record.input}, {record.output, l.before.record.output}} {
			if pair[0] != nil && pair[1] != nil && *pair[0] < *pair[1] {
				return nil
			}
		}
		share = settledRecord{settlementDifference(record.credits, l.before.record.credits), settlementDifference(record.cache, l.before.record.cache), settlementDifference(record.input, l.before.record.input), settlementDifference(record.output, l.before.record.output)}
	}
	asked := billingField(usage, "prompt_tokens")
	wrote := billingField(usage, "completion_tokens")
	if l.state.overlap || asked == nil || *asked <= 0 || share.input == nil || math.Abs(*share.input-*asked) > math.Max(64, *asked*.02) || share.output != nil && (wrote == nil || *share.output > *wrote+64) {
		return nil
	}
	streamCache := billingField(asBillingMap(usage["prompt_tokens_details"]), "cached_tokens")
	if share.cache != nil && math.Trunc(*share.cache) == *share.cache && *share.cache <= *asked && (streamCache == nil || *streamCache == 0) {
		details := map[string]any{}
		for k, v := range asBillingMap(usage["prompt_tokens_details"]) {
			details[k] = v
		}
		details["cached_tokens"] = *share.cache
		usage["prompt_tokens_details"] = details
	}
	if share.credits != nil {
		usage["credit"], usage["credits"] = *share.credits, *share.credits
	}
	return share.credits
}
func asBillingMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
