package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/config"
	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

func TestRoutePayloadClonePreservesHistoryAndAttemptIsolation(t *testing.T) {
	var input map[string]any
	if err := json.Unmarshal([]byte(`{"model":"merged","messages":[{"content":[{"type":"text","text":"history"}]}],"tools":[{"function":{"parameters":{"enum":["a","b"]}}}],"n":1,"empty":null}`), &input); err != nil {
		t.Fatal(err)
	}
	first, second := cloneRoutePayload(input), cloneRoutePayload(input)
	if !reflect.DeepEqual(first, input) {
		t.Fatal("clone changed decoded JSON values")
	}
	first["model"] = "member"
	first["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "changed"
	first["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["enum"].([]any)[0] = "changed"
	if !reflect.DeepEqual(second, input) {
		t.Fatal("attempt mutation leaked into original or sibling attempt")
	}
}

func BenchmarkRoutePayloadClone(b *testing.B) {
	history := make([]any, 100)
	for i := range history {
		history[i] = map[string]any{"role": "user", "content": strings.Repeat("x", 10<<10)}
	}
	input := map[string]any{"model": "merged", "messages": history}
	b.Run("json_roundtrip", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			raw, _ := json.Marshal(input)
			var output map[string]any
			_ = json.Unmarshal(raw, &output)
			if len(output) != 2 {
				b.Fatal("invalid clone")
			}
		}
	})
	b.Run("container_copy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if len(cloneRoutePayload(input)) != 2 {
				b.Fatal("invalid clone")
			}
		}
	})
}

func TestBufferedRequestBodyReleasesAfterParsingAcrossClones(t *testing.T) {
	for _, input := range []string{`{"value":"ok"}`, `{"value":"ok"} {}`, `{"value":`} {
		budget := &bodyBudget{}
		data, reserved, err := budget.read(strings.NewReader(input), int64(len(input)))
		if err != nil {
			t.Fatal(err)
		}
		body := &bufferedRequestBody{reader: bytes.NewReader(data), budget: budget, reserved: reserved}
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Body = body
		clone := r.WithContext(context.Background())
		_, err = readJSON(clone)
		if (err == nil) != (input == `{"value":"ok"}`) {
			t.Fatalf("parse %q: %v", input, err)
		}
		if budget.usage() != 0 || body.reader != nil {
			t.Fatal("raw body retained after parsing")
		}
		if n, err := r.Body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Fatalf("original clone still readable: %d %v", n, err)
		}
		_ = body.Close()
		_ = clone.Body.Close()
		if budget.usage() != 0 {
			t.Fatal("reservation released twice")
		}
	}
}

var benchmarkPayloadRequest *http.Request

func TestRequestGuardBodyCleanupBeforeAndAfterHandler(t *testing.T) {
	for _, mode := range []string{"parse", "skip", "panic"} {
		t.Run(mode, func(t *testing.T) {
			s := NewServer(&Orchestrator{cfg: &config.Config{}})
			var retained io.ReadCloser
			h := s.requestGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				retained = r.Body
				if s.bodies.usage() == 0 {
					t.Fatal("body not charged before parsing")
				}
				if mode == "parse" {
					if _, err := readJSON(r.WithContext(context.Background())); err != nil {
						t.Fatal(err)
					}
					if s.bodies.usage() != 0 {
						t.Fatal("raw body charged until handler completion")
					}
				}
				if mode == "panic" {
					panic("handler failure")
				}
			}))
			func() {
				defer func() {
					if p := recover(); p != nil && mode != "panic" {
						panic(p)
					}
				}()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/test", strings.NewReader(`{"value":"ok"}`)))
			}()
			if s.bodies.usage() != 0 {
				t.Fatal("guard fallback leaked reservation")
			}
			if retained == nil {
				t.Fatal("handler not reached")
			}
			if n, err := retained.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatal("guard retained raw buffer")
			}
		})
	}
}

func TestWorkBuddyPayloadReaderRetainsRedirectReplay(t *testing.T) {
	payload := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "test"}}}
	want, _ := json.Marshal(payload)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		data, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(data, want) || r.ContentLength != int64(len(want)) || r.Method != "POST" {
			t.Errorf("payload metadata/replay changed: %v length=%d body=%q", err, r.ContentLength, data)
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/done", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	ctx, timing := streamwatch.WithTiming(context.Background())
	if err := upstream.Shared().StreamUpstream(ctx, nil, payload, server.URL+"/start", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("redirect was not replayed: %d", requests)
	}
	if timing.Snapshot().GenerationEvents != 1 {
		t.Fatal("WorkBuddy terminal frame double-counted generation")
	}
}

func BenchmarkRequestPayloadReader(b *testing.B) {
	payload := []byte(`{"content":"` + strings.Repeat("x", 1<<20) + `"}`)
	for _, legacy := range []bool{true, false} {
		b.Run(fmt.Sprintf("string_copy=%t", legacy), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var reader io.Reader = bytes.NewReader(payload)
				if legacy {
					reader = strings.NewReader(string(payload))
				}
				request, err := http.NewRequest("POST", "http://localhost/test", reader)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkPayloadRequest = request
			}
		})
	}
}

func TestBudgetPredictionSeparatesDomainsAndFreeModels(t *testing.T) {
	balance := func(n float64) *float64 { return &n }
	data := map[string]provider.AdminData{
		"workbuddy": {DisplayName: "WorkBuddy", Budgets: []provider.BudgetPool{
			{AccountID: "cn", Domain: "cn-cli", Remaining: balance(100), Models: []string{"cheap", "expensive"}},
			{AccountID: "intl", Domain: "intl-work", Remaining: balance(10), Models: []string{"deepseek", "glm"}, FreeModels: []string{"deepseek"}},
		}},
		"qoder": {DisplayName: "Qoder", Budgets: []provider.BudgetPool{{AccountID: "cn", Domain: "cn", Remaining: balance(20), Models: []string{"qoder/qfmodel"}}}},
		"trae":  {DisplayName: "Trae", Budgets: []provider.BudgetPool{{AccountID: "trae", Remaining: balance(999), Reason: "消费未知"}}},
	}
	samples := []store.BudgetSample{
		{AccountUID: "cn", Model: "cheap", Recent: true, Credits: 5, Tokens: 5000, Calls: 5, LegacyCalls: 5},
		{AccountUID: "cn", Model: "expensive", Recent: true, Credits: 50, Tokens: 5000, Calls: 5},
		{AccountUID: "intl", Model: "glm", Recent: true, Credits: 20, Tokens: 5000, Calls: 5},
		{AccountUID: "intl", Model: "deepseek", Recent: true, Credits: 500, Tokens: 1e9, Calls: 50, ZeroTokens: 1e8},
		{AccountUID: "cn", Model: "qoder/qfmodel", Recent: true, Credits: 5, Tokens: 10000, Calls: 5},
		{AccountUID: "deleted", Model: "cheap", Recent: true, Credits: 1, Tokens: 1e12, Calls: 99},
	}
	out := estimateBudgets(data, samples)
	if out["predicted_tokens"] != int64(60681) || out["estimated_pools"] != 3 {
		t.Fatalf("prediction=%#v", out)
	}
	for _, channel := range out["providers"].([]map[string]any) {
		if channel["provider"] == "workbuddy" && !reflect.DeepEqual(channel["free_models"], []string{"deepseek"}) {
			t.Fatal("free capability lost")
		}
		if channel["provider"] == "workbuddy" && channel["pools"].([]map[string]any)[0]["legacy_samples"] != 5 {
			t.Fatal("legacy reference marker lost")
		}
	}
}

func TestBudgetPredictionFallbackAndUnknownBalances(t *testing.T) {
	remaining := 10.0
	pool := provider.BudgetPool{AccountID: "old", Domain: "same", Unit: "credits", Remaining: &remaining, Models: []string{"m"}}
	newPool := pool
	newPool.AccountID = "new"
	d := map[string]provider.AdminData{"workbuddy": {Budgets: []provider.BudgetPool{pool, newPool}}}
	samples := []store.BudgetSample{{AccountUID: "old", Model: "m", Recent: true, Credits: 1, Tokens: 1000, Calls: 1}, {AccountUID: "old", Model: "m", Credits: 4, Tokens: 4000, Calls: 4}}
	out := estimateBudgets(d, samples)
	pools := out["providers"].([]map[string]any)[0]["pools"].([]map[string]any)
	if out["predicted_tokens"] != int64(20000) || len(pools) != 1 || pools[0]["window_days"] != 30 || pools[0]["account_count"] != 2 {
		t.Fatalf("fallback=%#v", out)
	}
	newPool.Domain = "different"
	d["workbuddy"] = provider.AdminData{Budgets: []provider.BudgetPool{pool, newPool}}
	if estimateBudgets(d, samples)["estimated_pools"] != 1 {
		t.Fatal("cross-domain calibration")
	}
	pool.Remaining = nil
	d["workbuddy"] = provider.AdminData{Budgets: []provider.BudgetPool{pool}}
	if estimateBudgets(d, samples)["predicted_tokens"] != nil {
		t.Fatal("unknown balance became zero")
	}
	pool.Remaining = &remaining
	pool.FreeModels = []string{"m"}
	d["workbuddy"] = provider.AdminData{Budgets: []provider.BudgetPool{pool}}
	if estimateBudgets(d, samples)["predicted_tokens"] != nil {
		t.Fatal("free traffic amplified balance")
	}
	row := map[string]any{"uid": "a", "credits_remaining": 100.0, "credits_expire_at": 99.0, "healthy": false, "cooldown_until": 200.0, "credit_packages": []map[string]any{{"remain": 20.0, "expire_at": 99.0}, {"remain": 30.0, "budget_restricted": true}}}
	budget := provider.CreditBudget(row, "domain", "label", "credits", 100)
	if !budget.Deferred || budget.Reason != "" || budget.Remaining == nil || *budget.Remaining != 50 || budget.Excluded != 50 {
		t.Fatalf("budget=%#v", budget)
	}
	row["enabled"] = false
	if provider.CreditBudget(row, "domain", "label", "credits", 100).Reason == "" {
		t.Fatal("disabled account budget included")
	}
}

func TestBudgetPredictionPoolsSameTariffsBeforeApplyingModelMix(t *testing.T) {
	balance := 100.0
	accounts := []provider.BudgetPool{}
	for _, uid := range []string{"a", "b", "new"} {
		accounts = append(accounts, provider.BudgetPool{AccountID: uid, Domain: "domestic", DomainLabel: "国内", Unit: "积分", Remaining: &balance, Models: []string{"cheap", "expensive"}, ModelCosts: map[string]float64{"cheap": .1, "expensive": 1}})
	}
	data := map[string]provider.AdminData{"workbuddy": {Budgets: accounts}}
	samples := []store.BudgetSample{{AccountUID: "a", Model: "cheap", Recent: true, Credits: 9, Tokens: 9000, Calls: 9}, {AccountUID: "b", Model: "expensive", Recent: true, Credits: 10, Tokens: 1000, Calls: 1}}
	out := estimateBudgets(data, samples)
	pools := out["providers"].([]map[string]any)[0]["pools"].([]map[string]any)
	if len(pools) != 1 || pools[0]["account_count"] != 3 || pools[0]["remaining"] != 300.0 || out["predicted_tokens"] != int64(157894) {
		t.Fatalf("group=%#v", out)
	}
	details := pools[0]["models"].([]map[string]any)
	if len(details) != 2 || details[0]["token_share"] != .9 || details[1]["token_share"] != .1 {
		t.Fatal(details)
	}
	// The model distribution belongs to the billing group, not the account.
	samples[0].AccountUID, samples[1].AccountUID = samples[1].AccountUID, samples[0].AccountUID
	if estimateBudgets(data, samples)["predicted_tokens"] != out["predicted_tokens"] {
		t.Fatal("account assignment changed estimate")
	}
	// Adding the first versioned record must not blank an otherwise usable
	// historical estimate while the new versioned sample set warms up.
	price := .1
	samples[0].Tokens = 8000
	samples[0].Credits = 8
	samples[0].Calls = 8
	samples = append(samples, store.BudgetSample{AccountUID: "a", Model: "cheap", Domain: "domestic", Coefficient: &price, Recent: true, Credits: 1, Tokens: 1000, Calls: 1})
	if estimateBudgets(data, samples)["predicted_tokens"] != out["predicted_tokens"] {
		t.Fatal("first stamped request erased historical estimate")
	}
	// Model-restricted balances cannot finance another account's model mix.
	accounts[2].Models = []string{"cheap"}
	if len(groupedBudgets("workbuddy", accounts)) != 2 {
		t.Fatal("different applicability merged")
	}
}

func TestBudgetPredictionExtraFreeCatalogDoesNotSplitPaidBalances(t *testing.T) {
	balance := 100.0
	standard := provider.BudgetPool{AccountID: "active", Domain: "domestic", Unit: "积分", Remaining: &balance, Models: []string{"deepseek", "glm", "shared-free"}, FreeModels: []string{"shared-free"}, ModelCosts: map[string]float64{"deepseek": .11, "glm": .1, "shared-free": 0}}
	extra := standard
	extra.AccountID = "new"
	extra.Models = []string{"deepseek", "glm", "shared-free", "extra-free"}
	extra.FreeModels = []string{"shared-free", "extra-free"}
	extra.ModelCosts = map[string]float64{"deepseek": .11, "glm": .1, "shared-free": 0, "extra-free": 0}
	data := map[string]provider.AdminData{"workbuddy": {Budgets: []provider.BudgetPool{extra, standard}}}
	samples := []store.BudgetSample{
		{AccountUID: "new", Model: "deepseek", Recent: true, Credits: .2, Tokens: 12000, Calls: 1},
		{AccountUID: "active", Model: "deepseek", Credits: 10, Tokens: 1e6, Calls: 100},
		{AccountUID: "active", Model: "glm", Recent: true, Credits: 10, Tokens: 1e6, Calls: 100},
		{AccountUID: "new", Model: "extra-free", Recent: true, ZeroTokens: 1e12, ZeroCalls: 100},
	}
	out := estimateBudgets(data, samples)
	channel := out["providers"].([]map[string]any)[0]
	groups := channel["pools"].([]map[string]any)
	if len(groups) != 1 || groups[0]["remaining"] != 200.0 || groups[0]["status"] != "estimated" {
		t.Fatalf("extra free model split balance/calibration: %#v", out)
	}
	if !reflect.DeepEqual(channel["free_models"], []string{"extra-free", "shared-free"}) || len(groups[0]["models"].([]map[string]any)) != 2 {
		t.Fatal("free capability lost or free tokens inflated mix", out)
	}
	// Expanding paid applicability is different: restricted balances stay separate.
	extra.FreeModels = []string{"shared-free"}
	extra.ModelCosts["extra-free"] = 1
	if len(groupedBudgets("workbuddy", []provider.BudgetPool{standard, extra})) != 2 {
		t.Fatal("paid applicability merged")
	}
	// An unavailable account cannot advertise its exclusive free capability.
	extra.FreeModels = []string{"shared-free", "extra-free"}
	extra.ModelCosts["extra-free"] = 0
	extra.Reason = "disabled"
	out = estimateBudgets(map[string]provider.AdminData{"workbuddy": {Budgets: []provider.BudgetPool{extra, standard}}}, samples)
	if got := out["providers"].([]map[string]any)[0]["free_models"]; !reflect.DeepEqual(got, []string{"shared-free"}) {
		t.Fatal("disabled free capability exposed", got)
	}
}

func TestBudgetPredictionUsesRecentMixWithPerModelFallback(t *testing.T) {
	balance := 100.0
	data := map[string]provider.AdminData{"workbuddy": {Budgets: []provider.BudgetPool{{AccountID: "a", Domain: "domestic", Remaining: &balance, Models: []string{"cheap", "expensive", "unknown"}}}}}
	samples := []store.BudgetSample{
		{AccountUID: "a", Model: "cheap", Recent: true, Credits: 9, Tokens: 9000, Calls: 5},
		{AccountUID: "a", Model: "expensive", Recent: false, Credits: 10, Tokens: 1000, Calls: 5},
		{AccountUID: "a", Model: "expensive", Recent: true, UnknownTokens: 1000, UnknownCalls: 1},
	}
	out := estimateBudgets(data, samples)
	if out["predicted_tokens"] != int64(52631) {
		t.Fatalf("model fallback=%#v", out)
	}
	samples = append(samples, store.BudgetSample{AccountUID: "a", Model: "unknown", Recent: true, UnknownTokens: 1000, UnknownCalls: 1})
	out = estimateBudgets(data, samples)
	if out["predicted_tokens"] != nil {
		t.Fatal("missing model cost was silently excluded", out)
	}
	row := out["providers"].([]map[string]any)[0]["pools"].([]map[string]any)[0]
	if !reflect.DeepEqual(row["missing_models"], []string{"unknown"}) {
		t.Fatal(row)
	}
}

func TestBudgetPredictionSeparatesOffersAndHistoricalPriceSnapshots(t *testing.T) {
	balance := 10.0
	price, discount := 1.0, .5
	standard := provider.BudgetPool{AccountID: "regular", Domain: "domestic", Remaining: &balance, Models: []string{"m"}, ModelCosts: map[string]float64{"m": price}}
	special := standard
	special.AccountID = "offer"
	special.ModelCosts = map[string]float64{"m": discount}
	data := map[string]provider.AdminData{"workbuddy": {Budgets: []provider.BudgetPool{standard, special}}}
	samples := []store.BudgetSample{
		{AccountUID: "regular", Model: "m", Recent: true, Domain: "domestic", Coefficient: &price, Credits: 10, Tokens: 10000, Calls: 5},
		{AccountUID: "offer", Model: "m", Recent: true, Domain: "domestic", Coefficient: &discount, Credits: 5, Tokens: 10000, Calls: 5},
		{AccountUID: "offer", Model: "m", Recent: true, Domain: "domestic", Coefficient: &price, Credits: 1000, Tokens: 1e6, Calls: 99},
		{AccountUID: "offer", Model: "m", Recent: false, Credits: 1000, Tokens: 1e6, Calls: 99},
		{AccountUID: "regular", Model: "m", Recent: true, Domain: "international", Coefficient: &price, Credits: 1, Tokens: 1e9, Calls: 99},
	}
	out := estimateBudgets(data, samples)
	if out["total_pools"] != 2 || out["predicted_tokens"] != int64(30000) {
		t.Fatalf("tariff isolation=%#v", out)
	}
	// No current-price evidence means old-price and unversioned data must not
	// be treated as proof that this offer still has a known consumption rate.
	samples = samples[2:]
	if estimateBudgets(data, samples)["predicted_tokens"] != nil {
		t.Fatal("stale tariff evidence reused")
	}
}

func awaitQueue(t *testing.T, a *modelAdmission, n int) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if a.snapshot()["queued"].(int) == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue never reached %d: %v", n, a.snapshot())
}

func TestModelAdmissionBoundedAndCancelable(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, ModelQueueSize: 8, ModelKeyQueueSize: 8})
	var active []*modelLease
	for i := 0; i < 4; i++ {
		l, err := a.acquire(context.Background(), "1")
		if err != nil {
			t.Fatal(err)
		}
		active = append(active, l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			l, err := a.acquire(ctx, "1")
			if l != nil {
				l.release()
			}
			results <- err
		}()
	}
	awaitQueue(t, a, 8)
	if _, err := a.acquire(context.Background(), "2"); err == nil {
		t.Fatal("13th request admitted")
	}
	cancel()
	for i := 0; i < 8; i++ {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel lost: %v", err)
		}
	}
	for _, l := range active {
		l.release()
	}
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 {
		t.Fatal(s)
	}
}

func TestAdmissionFairKeysAndLimits(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 1, ModelKeyLimits: `{"1":1}`})
	first, _ := a.acquire(context.Background(), "1")
	got := make(chan *modelLease, 2)
	go func() { l, _ := a.acquire(context.Background(), "1"); got <- l }()
	awaitQueue(t, a, 1)
	go func() { l, _ := a.acquire(context.Background(), "2"); got <- l }()
	awaitQueue(t, a, 2)
	first.release()
	second := <-got
	if second.t.key != "2" {
		t.Fatal("busy key starved a different key")
	}
	second.release()
	(<-got).release()
	a = newModelAdmission(&config.Config{MaxConcurrentRequests: 4, ModelKeyLimits: `{"1":1}`})
	one, _ := a.acquire(context.Background(), "1")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := a.acquire(ctx, "1"); result <- err }()
	awaitQueue(t, a, 1)
	other, _ := a.acquire(context.Background(), "2")
	if a.snapshot()["running"] != 2 {
		t.Fatal("per-key limit ignored or blocked other key")
	}
	cancel()
	<-result
	one.release()
	other.release()
}

func TestThrottleReturnsSlotAndSharesWaitBudget(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 1})
	a.wait = 100 * time.Millisecond
	l, _ := a.acquire(context.Background(), "1")
	parked := make(chan struct{})
	finish := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- l.throttle(context.Background(), func(ctx context.Context) error {
			close(parked)
			select {
			case <-finish:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-parked
	other, err := a.acquire(context.Background(), "2")
	if err != nil {
		t.Fatal(err)
	}
	close(finish)
	if err := <-result; err == nil {
		t.Fatal("account wait + second admission escaped shared deadline")
	}
	l.release()
	other.release()
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 {
		t.Fatal(s)
	}
}

func TestManagementIndependentOfFullModelPool(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{MaxConcurrentRequests: 4}})
	var leases []*modelLease
	for i := 0; i < 4; i++ {
		l, _ := s.modelsAdmission.acquire(context.Background(), "1")
		leases = append(leases, l)
		defer l.release()
	}
	h := s.requestGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for _, path := range []string{"/admin/overview", "/health", "/assets/app.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("management blocked by model traffic: %s: %d", path, w.Code)
		}
	}
}

func TestBodyBudgetChargesCapacityAndRecovers(t *testing.T) {
	b := &bodyBudget{limit: 2 << 20}
	data, reserved, err := b.read(strings.NewReader(strings.Repeat("x", 1<<20)), 1<<20)
	if err != nil || len(data) != 1<<20 || b.usage() != reserved {
		t.Fatal("body budget mismatch", err)
	}
	_, second, err := b.read(strings.NewReader(strings.Repeat("x", 2<<20)), 2<<20)
	if err == nil {
		t.Fatal("body budget overrun")
	}
	b.release(second)
	b.release(reserved)
	if b.usage() != 0 {
		t.Fatal("body budget leaked")
	}
	_, unknown, err := b.read(bytes.NewBuffer(make([]byte, 1<<20)), -1)
	if err != nil {
		t.Fatal(err)
	}
	b.release(unknown)
}

func TestLargeAgentBodyPreservesAuthLimits(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	o.cfg.MaxRequestBytes = config.DefaultMaxRequestBytes
	o.cfg.RequestBodyBudget = config.DefaultRequestBodyBudget
	if _, err := o.db.CreateApp("large-agent", o.hashKey("large-agent-key"), "", "", "", "", 0); err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	payload := `{"model":"test","input":"` + strings.Repeat("x", 20<<20) + `"}`
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != len(payload) {
			t.Fatal("large body was damaged", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, lengthKnown := range []bool{true, false} {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
		r.Header.Set("Authorization", "Bearer large-agent-key")
		if !lengthKnown {
			r.ContentLength = -1
		}
		w := httptest.NewRecorder()
		s.requestGuard(next).ServeHTTP(w, r)
		if w.Code != http.StatusNoContent || s.bodies.usage() != 0 {
			t.Fatal("large Agent request rejected or leaked its body budget", w.Code)
		}
	}
	for _, path := range []string{"/admin/login", "/portal/api/auth/login"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", 9<<10)))
		w := httptest.NewRecorder()
		s.requestGuard(next).ServeHTTP(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatal("large authentication request was permitted", path, w.Code)
		}
	}
}

func TestBodyReaderQueueCancellationAndRecovery(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{BodyReadConcurrency: 1, ModelQueueSize: 1}})
	s.bodySlots <- struct{}{}
	h := s.requestGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	request := func(ctx context.Context) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/test", strings.NewReader(`{}`)).WithContext(ctx))
		done <- w.Code
	}
	waitQueued := func() {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for len(s.bodyWaiting) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(s.bodyWaiting) != 1 {
			t.Fatal("body reader did not queue")
		}
	}
	go request(ctx)
	waitQueued()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/test", strings.NewReader(`{}`)))
	if w.Code != 429 || !strings.Contains(w.Body.String(), "body_read_capacity") {
		t.Fatal("body waiting queue was not bounded", w.Code, w.Body.String())
	}
	cancel()
	<-done
	if len(s.bodyWaiting) != 0 || len(s.bodySlots) != 1 || s.bodies.usage() != 0 {
		t.Fatal("cancelled body waiter leaked capacity")
	}
	go request(context.Background())
	waitQueued()
	<-s.bodySlots
	if code := <-done; code != 204 || len(s.bodyWaiting) != 0 || len(s.bodySlots) != 0 || s.bodies.usage() != 0 {
		t.Fatal("body queue did not recover", code)
	}
}

func TestRefreshSingleFlightAndPanicCleanup(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{MaxConcurrentRequests: 4}})
	var calls atomic.Int32
	started, finish := make(chan struct{}), make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-finish
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	result := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		w := httptest.NewRecorder()
		s.serveRefresh(w, httptest.NewRequest("POST", "/admin/models", nil), next)
		result <- w
	}()
	<-started
	go func() {
		w := httptest.NewRecorder()
		s.serveRefresh(w, httptest.NewRequest("POST", "/admin/models/refresh", nil), next)
		result <- w
	}()
	// Wait for the follower to take a snapshot while the leader remains blocked.
	until := time.Now().Add(time.Second)
	for {
		s.refreshMu.Lock()
		n := s.refreshes["/admin/models/refresh"].waiters
		s.refreshMu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("follower did not join refresh")
		}
		time.Sleep(time.Millisecond)
	}
	close(finish)
	for i := 0; i < 2; i++ {
		w := <-result
		body, _ := io.ReadAll(w.Result().Body)
		if w.Code != 200 || !bytes.Contains(body, []byte(`"ok":true`)) {
			t.Fatal(w.Code, string(body))
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate refresh")
	}
	func() {
		defer func() { _ = recover() }()
		s.serveRefresh(httptest.NewRecorder(), httptest.NewRequest("POST", "/admin/models/refresh", nil), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test") }))
	}()
	if len(s.heavySlots) != 0 || len(s.refreshes) != 0 {
		t.Fatal("panic leaked admission")
	}
}

func TestModelPolicyAndInternalTestCannotBypassAdmission(t *testing.T) {
	client := &failingUpstream{err: errors.New("must not dispatch")}
	o, _ := newDNSFailoverOrch(t, client)
	s := NewServer(o)
	var leases []*modelLease
	for i := 0; i < 4; i++ {
		l, _ := s.modelsAdmission.acquire(context.Background(), "1")
		leases = append(leases, l)
		defer l.release()
	}
	s.modelsAdmission.wait = 5 * time.Millisecond
	// Unauthorized models must be rejected without spending any queue budget.
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"blocked","messages":[]}`))
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &Principal{AppID: 1, AllowedModels: []string{"allowed"}}))
	w := httptest.NewRecorder()
	s.handleChat(w, r)
	if w.Code != 403 || s.modelsAdmission.snapshot()["queued"] != 0 {
		t.Fatal("policy checked after admission", w.Code)
	}
	// Public authentication runs before reading bodies/admission.
	w = httptest.NewRecorder()
	s.requestGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unauthenticated request reached handler") })).ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`)))
	if w.Code != 401 || s.bodies.usage() != 0 {
		t.Fatal("unauthenticated request used resources")
	}
	testMu.Lock()
	testRunning = false
	testLastDone = time.Time{}
	testMu.Unlock()
	t.Cleanup(func() { testMu.Lock(); testRunning = false; testLastDone = time.Time{}; testMu.Unlock() })
	w = httptest.NewRecorder()
	s.adminModelTest(w, httptest.NewRequest("POST", "/admin/models/test", strings.NewReader(`{"model":"test-model"}`)))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || client.calls != 0 {
		t.Fatal("internal test bypassed model pool", w.Code, client.calls)
	}
}

type hangingUpstream struct{ started chan struct{} }

func (h *hangingUpstream) StreamUpstream(ctx context.Context, _ map[string]string, _ map[string]any, _ string, yield upstream.LineFunc) error {
	if err := yield(`data: {"choices":[{"delta":{"content":"hello"},"index":0}]}`); err != nil {
		return err
	}
	h.started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func TestRealSSEHandlersRespectAccountAndGlobalLimits(t *testing.T) {
	for _, accountLimit := range []int{2, 4} {
		t.Run(fmt.Sprint(accountLimit), func(t *testing.T) {
			o, _ := newDNSFailoverOrch(t, &failingUpstream{})
			o.cfg.WorkBuddyAccountConcurrency = accountLimit
			acc := o.wb.Pool.Accounts()[0]
			o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
			o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.wb.Catalog.Refresh() // Catalog transport returns fixture JSON without network access.

			h := &hangingUpstream{started: make(chan struct{}, 4)}
			o.wb.UpstreamClient = h
			s := NewServer(o)
			done := make(chan struct{}, 4)
			if _, err := o.db.CreateApp("test", o.hashKey("test-private-key"), "", "", "", "", 0); err != nil {
				t.Fatal(err)
			}
			var cancels []context.CancelFunc
			for i := 0; i < 4; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				cancels = append(cancels, cancel)
				defer cancel()
				r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
				r.Header.Set("Authorization", "Bearer test-private-key")
				go func() {
					s.requestGuard(http.HandlerFunc(s.handleChat)).ServeHTTP(httptest.NewRecorder(), r)
					done <- struct{}{}
				}()
			}
			for i := 0; i < accountLimit; i++ {
				select {
				case <-h.started:
				case <-time.After(2 * time.Second):
					t.Fatal("stream never reached upstream")
				}
			}
			awaitQueue(t, s.modelsAdmission, 4-accountLimit)
			w := httptest.NewRecorder()
			s.requestGuard(http.HandlerFunc(s.adminOverview)).ServeHTTP(w, httptest.NewRequest("GET", "/admin/overview", nil))
			if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(fmt.Sprintf(`"running":%d`, accountLimit))) {
				t.Fatal("management unavailable", w.Code, w.Body.String())
			}
			for _, cancel := range cancels {
				cancel()
			}
			for i := 0; i < 4; i++ {
				<-done
			}
			if s.modelsAdmission.snapshot()["running"] != 0 || s.bodies.usage() != 0 {
				t.Fatal("disconnected streams leaked admission/body budget")
			}

		})
	}
}

func TestThirdKeyCannotStarve(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 1})
	first, _ := a.acquire(context.Background(), "1")
	got := make(chan *modelLease, 9)
	for _, key := range []string{"1", "1", "2", "2", "3", "3"} {
		go func(key string) {
			l, err := a.acquire(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			got <- l
		}(key)
	}
	awaitQueue(t, a, 6)
	first.release()
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		l := <-got
		seen[l.t.key] = true
		if i == 2 && len(seen) != 3 {
			t.Error("three-key round robin starved a key", seen)
		}
		l.release()
	}
}

type catalogOffline struct{ pool.Credential }

func (catalogOffline) CatalogHeaders() (map[string]string, error) {
	return map[string]string{}, nil
}

type catalogTransport struct{}

func (catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"models":[{"id":"test-model","name":"Test"}],"agents":[{"name":"cli","models":["test-model"]}]}}`)), Request: r}, nil
}

func TestLocalResponseLimitDoesNotPunishAccount(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{emit: true})
	ctx := streamwatch.WithResponseLimit(context.Background(), 1)
	_, err := o.openUpstream(ctx, acc, map[string]any{}, "test-model", "", func(string) error { return nil }, nil)
	if !errors.Is(err, streamwatch.ErrResponseTooLarge) || o.wb.Pool.Get(acc.UID).FailureCount != 0 {
		t.Fatal("local output limit punished upstream account", err)
	}
}

func TestDownstreamWriteFailureCancelsWithoutAccountPenalty(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{emit: true})
	send, ok := sseWriter(brokenWriter{header: make(http.Header)})
	if !ok {
		t.Fatal("flusher lost")
	}
	_, err := o.openUpstream(context.Background(), acc, map[string]any{}, "test-model", "", send, nil)
	if !errors.Is(err, context.Canceled) || o.wb.Pool.Get(acc.UID).FailureCount != 0 {
		t.Fatal("downstream failure punished account", err)
	}
}

type brokenWriter struct{ header http.Header }

func (w brokenWriter) Header() http.Header     { return w.header }
func (brokenWriter) WriteHeader(int)           {}
func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (brokenWriter) Flush()                    {}

func TestJSONAllocationAmplificationBounded(t *testing.T) {
	if jsonWithinComplexity([]byte(`{"messages":[{},{},{},{},{}]}`), 8, 128) {
		t.Fatal("object amplification allowed")
	}
	if jsonWithinComplexity([]byte(`[[[[[]]]]]`), 100, 4) {
		t.Fatal("deep JSON allowed")
	}
	if !jsonWithinComplexity([]byte(`{"text":"{[\\\"braces\\\"]}:,,,","image":"long base64"}`), 10, 4) {
		t.Fatal("string punctuation counted as structure")
	}
}

func TestEveryModelProtocolUsesAdmission(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	s := NewServer(o)
	s.modelsAdmission.wait = 5 * time.Millisecond
	for i := 0; i < 4; i++ {
		l, _ := s.modelsAdmission.acquire(context.Background(), "1")
		defer l.release()
	}
	for _, tc := range []struct {
		path    string
		handler http.HandlerFunc
	}{{"/v1/chat/completions", s.handleChat}, {"/v1/messages", s.handleMessages}, {"/v1/responses", s.handleResponses}} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"test-model","messages":[],"input":"hi"}`))
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &Principal{AppID: 2}))
		w := httptest.NewRecorder()
		s.requestGuard(tc.handler).ServeHTTP(w, r)
		if w.Code != 429 || !bytes.Contains(w.Body.Bytes(), []byte(`"model_queue_timeout"`)) || w.Header().Get("Content-Type") != "application/json" {
			t.Fatal(tc.path, "bypassed admission or sent premature SSE", w.Code, w.Body.String())
		}
	}
}

func TestRuntimeDownstreamFailurePropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelWriter{ResponseWriter: brokenWriter{header: make(http.Header)}, cancel: cancel}
	_, err := w.Write([]byte("test"))
	if err == nil || ctx.Err() != context.Canceled {
		t.Fatal("upstream context not canceled after write failure")
	}
}

// One UID is a single upstream resource even across different users, models,
// and private/shared pool membership. Busy resources must not hold server slots.
func TestAccountAdmissionUIDIsolationAlternativesAndCancellation(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, WorkBuddyAccountConcurrency: 2})
	first := &pool.Account{UID: "one"}
	second := &pool.Account{UID: "two"}
	choose := func(accounts ...*pool.Account) func(map[string]bool) (*pool.Account, *apiError) {
		return func(busy map[string]bool) (*pool.Account, *apiError) {
			for _, acc := range accounts {
				if !busy[acc.UID] {
					return acc, nil
				}
			}
			return nil, nil
		}
	}
	bind := func(key, model string, selector func(map[string]bool) (*pool.Account, *apiError)) *modelLease {
		l, err := a.acquireWithLimit(context.Background(), key, 2, model)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.release)
		if _, err = l.bindAccount(context.Background(), selector); err != nil {
			t.Fatal(err)
		}
		return l
	}
	one := bind("portal-user-1", "model-a", choose(first))
	private := bind("private", "model-b", choose(first))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked, _ := a.acquireWithLimit(ctx, "portal-user-2", 2, "model-c")
	result := make(chan error, 1)
	go func() { _, err := blocked.bindAccount(ctx, choose(first)); result <- err }()
	awaitQueue(t, a, 1)
	if a.snapshot()["running"] != 2 {
		t.Fatal("busy UID consumed execution slot", a.snapshot())
	}
	alternative := bind("portal-user-3", "model-a", choose(first, second))
	if alternative.t.account.LocalID != "two" {
		t.Fatal("idle alternative not used")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	blocked.release()
	one.release()
	private.release()
	alternative.release()
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 || len(s["account_running"].(map[string]int)) != 0 {
		t.Fatal("permit leak", s)
	}
}

func TestAccountBindingDoesNotRejectRunnableWhenQueueFull(t *testing.T) {
	a := newModelAdmission(&config.Config{ModelQueueSize: 1})
	l, _ := a.acquire(context.Background(), "private")
	defer l.release()
	a.mu.Lock()
	a.enqueueLocked(&modelTicket{key: "parked", ready: false})
	a.mu.Unlock()
	if _, err := l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: "free"}, nil }); err != nil {
		t.Fatal("free capacity rejected", err)
	}
}

func TestFairAdmissionPrefersUserWithoutExecution(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, PortalSharedConcurrency: 3})
	var leases []*modelLease
	for _, key := range []string{"portal-user-a", "portal-user-a", "portal-user-b", "private"} {
		l, err := a.acquireWithLimit(context.Background(), key, 3, "m")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
		defer l.release()
	}
	got := make(chan *modelLease, 2)
	go func() { l, _ := a.acquireWithLimit(context.Background(), "portal-user-a", 3, "m"); got <- l }()
	awaitQueue(t, a, 1)
	go func() { l, _ := a.acquireWithLimit(context.Background(), "portal-user-c", 3, "m"); got <- l }()
	awaitQueue(t, a, 2)
	leases[2].release()
	next := <-got
	if next.t.key != "portal-user-c" {
		t.Error("new user lost to another child agent", next.t.key)
	}
	next.release()
	(<-got).release()
}

func TestAccountSelectorUsesSameSiteAccountsAndLiveAuthorizedReadiness(t *testing.T) {
	o, original := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.Pool = pool.New(map[string]pool.Credential{"one": catalogOffline{original.Mgr}, "two": catalogOffline{original.Mgr}}, "")
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	zero := 0.0
	o.wb.Pool.SetCredits("one", &zero, nil, nil, nil)
	exhausted, e := o.accountSelector("test-model", "one", nil, map[string]bool{"one": true, "two": true})
	if e != nil {
		t.Fatal(e)
	}
	if acc, e := exhausted(nil); e != nil || acc == nil || acc.UID != "two" {
		t.Fatal("preferred cooldown grace revived exhausted account", acc, e)
	}
	o.wb.Pool.SetCredits("one", nil, nil, nil, nil)
	selector, err := o.accountSelector("test-model", "one", nil, map[string]bool{"one": true, "two": true})
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := selector(map[string]bool{"one": true}); err != nil || acc != nil {
		t.Fatal("busy preferred account lost affinity", acc, err)
	}
	other, err := o.accountSelector("test-model", "two", nil, map[string]bool{"one": true, "two": true})
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := other(map[string]bool{"one": true}); err != nil || acc == nil || acc.UID != "two" {
		t.Fatal("same-site account wrongly coalesced", acc, err)
	}
	restricted, err := o.accountSelector("test-model", "one", nil, map[string]bool{"one": true})
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := restricted(map[string]bool{"one": true}); acc != nil || err != nil {
		t.Fatal("scope escaped instead of waiting", acc, err)
	}
	o.wb.Cooldowns["two|test-model"] = cdEntry{Until: nowSec() + 60}
	o.wb.Pool.OnFailure("one", 60)
	if acc, err := selector(nil); acc != nil || err == nil {
		t.Fatal("cooling accounts admitted", acc, err)
	}
}

func TestAccountPermitRetryAndThrottleFailureRecover(t *testing.T) {
	a := newModelAdmission(&config.Config{})
	l, _ := a.acquire(context.Background(), "private")
	defer l.release()
	for _, uid := range []string{"failed", "replacement"} {
		if _, err := l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: uid}, nil }); err != nil {
			t.Fatal(err)
		}
		counts := a.snapshot()["account_running"].(map[string]int)
		if len(counts) != 1 || counts[uid] != 1 {
			t.Fatal("retry retained old permit", counts)
		}
	}
	if err := l.throttle(context.Background(), func(context.Context) error { return context.DeadlineExceeded }); err == nil {
		t.Fatal("lost wait failure")
	}
	l.release()
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 || len(s["account_running"].(map[string]int)) != 0 {
		t.Fatal("failed throttle leaked permit", s)
	}
}

func TestFiveSlotProfileSupportsFourUserAgentsAndPrivateReserve(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 5, PortalSharedConcurrency: 4})
	var leases []*modelLease
	defer func() {
		for _, l := range leases {
			l.release()
		}
	}()
	for i := 0; i < 4; i++ {
		l, err := a.acquireWithLimit(context.Background(), "portal-user-1", 4, "m")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
		if _, err = l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: "contributed"}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		l, err := a.acquireWithLimit(ctx, "portal-user-2", 4, "m")
		if l != nil {
			l.release()
		}
		result <- err
	}()
	awaitQueue(t, a, 1)
	private, err := a.acquire(context.Background(), "private")
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, private)
	if a.snapshot()["running"] != 5 {
		t.Fatal("private reserve unavailable", a.snapshot())
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type capacityUpstream struct {
	started chan struct{}
	finish  chan struct{}
}

func (c *capacityUpstream) StreamUpstream(ctx context.Context, _ map[string]string, _ map[string]any, _ string, yield upstream.LineFunc) error {
	// Almost 8 MiB of wire response: retain nonstream aggregates concurrently.
	line := `data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", 128<<10) + `"}}]}`
	for i := 0; i < 63; i++ {
		if err := yield(line); err != nil {
			return err
		}
	}
	c.started <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.finish:
		return yield(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	}
}
func TestFiveSlotCapacityNonstreamNearResponseLimit(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{})
	o.cfg.MaxConcurrentRequests = 5
	o.cfg.PortalSharedConcurrency = 4
	// Single fixture UID deliberately permits 5 to exercise the global memory
	// ceiling. Production 4+1 spans at least two distinct accounts.
	o.cfg.WorkBuddyAccountConcurrency = 5
	o.cfg.UsageContentMaxBytes = 32768
	o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	upstream := &capacityUpstream{started: make(chan struct{}, 5), finish: make(chan struct{})}
	o.wb.UpstreamClient = upstream
	s := NewServer(o)
	if _, err := o.db.CreateApp("capacity", o.hashKey("capacity-key"), "", "", "", "", 0); err != nil {
		t.Fatal(err)
	}
	var finish sync.Once
	defer finish.Do(func() { close(upstream.finish) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := make(chan int, 5)
	input := `{"model":"auto","messages":[{"role":"user","content":"` + strings.Repeat("a", 2<<20) + `"}],"stream":false}`
	for i := 0; i < 5; i++ {
		go func() {
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(input)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer capacity-key")
			w := httptest.NewRecorder()
			s.requestGuard(http.HandlerFunc(s.handleChat)).ServeHTTP(w, r)
			result <- w.Code
		}()
		// Stagger body reads so five large uploads do not intentionally exceed
		// the shared 32 MiB transient-allocation budget. All aggregates overlap.
		select {
		case <-upstream.started:
		case code := <-result:
			t.Fatalf("request exited before five aggregates: %d", code)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if s.modelsAdmission.snapshot()["running"] != 5 {
		t.Fatal("five requests did not execute")
	}
	finish.Do(func() { close(upstream.finish) })
	for i := 0; i < 5; i++ {
		if code := <-result; code != 200 {
			t.Fatal(code)
		}
	}
	if s.modelsAdmission.snapshot()["running"] != 0 || s.bodies.usage() != 0 {
		t.Fatal("capacity not recovered")
	}
}

func TestAdaptiveBudgetAdmitsThreeUsersWithoutFixedFourSlotBottleneck(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 13, PortalSharedConcurrency: 12})
	var leases []*modelLease
	defer func() {
		for _, l := range leases {
			l.release()
		}
	}()
	for u := 0; u < 3; u++ {
		for child := 0; child < 4; child++ {
			l, err := a.acquireWithBudget(context.Background(), fmt.Sprintf("portal-user-%d", u), 4, "m", 16<<20)
			if err != nil {
				t.Fatal(err)
			}
			leases = append(leases, l)
			uid := fmt.Sprint(u)
			if _, err = l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: uid}, nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	private, err := a.acquireWithBudget(context.Background(), "private", 0, "m", 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, private)
	if a.snapshot()["running"] != 13 || a.buffers != 224<<20 {
		t.Fatal("resource budget failed to expand", a.snapshot())
	}
	for _, l := range leases {
		l.release()
	}
	if a.buffers != 0 || a.sharedBuffers != 0 {
		t.Fatal("buffer reservation leak")
	}
}
func TestAdaptiveMemoryPressureRecoversWithoutCompletion(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 13, PortalSharedConcurrency: 12, ModelMemoryGuard: true})
	var pressure atomic.Uint64
	pressure.Store(a.memoryHigh)
	a.memoryUsage = func() uint64 { return pressure.Load() }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := make(chan *modelLease, 1)
	go func() {
		l, err := a.acquireWithBudget(ctx, "private", 0, "m", 16<<20)
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	awaitQueue(t, a, 1)
	if a.snapshot()["running"] != 0 {
		t.Fatal("pressure ignored")
	}
	deadline := time.Now().Add(time.Second)
	var collected time.Time
	for time.Now().Before(deadline) {
		a.mu.Lock()
		collected = a.lastPressureGC
		a.mu.Unlock()
		if !collected.IsZero() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if collected.IsZero() {
		t.Fatal("pressure never requested dead-object collection")
	}
	time.Sleep(300 * time.Millisecond)
	a.mu.Lock()
	repeated := !a.lastPressureGC.Equal(collected)
	a.mu.Unlock()
	if repeated {
		t.Fatal("pressure waiters triggered repeated collection")
	}
	pressure.Store(0)
	l := <-got
	if l == nil {
		t.Fatal("pressure queue never recovered")
	}
	l.release()
	if a.buffers != 0 || len(a.queue) != 0 {
		t.Fatal("pressure recovery leaked reservations")
	}
}

type discardResponse struct {
	header http.Header
	code   int
}

func (d *discardResponse) Header() http.Header  { return d.header }
func (d *discardResponse) WriteHeader(code int) { d.code = code }
func (d *discardResponse) Write(b []byte) (int, error) {
	if d.code == 0 {
		d.code = 200
	}
	return len(b), nil
}
func (d *discardResponse) Flush() {}
func TestAdaptiveProtocolCapacity(t *testing.T) {
	for _, route := range []string{"chat", "responses", "anthropic"} {
		for _, n := range []int{9, 13} {
			t.Run(fmt.Sprintf("%s/%d", route, n), func(t *testing.T) {
				o, acc := newDNSFailoverOrch(t, &failingUpstream{})
				o.cfg.MaxConcurrentRequests = 13
				o.cfg.PortalSharedConcurrency = 12
				o.cfg.ModelMemoryGuard = true
				// Single fake credential removes account routing as a variable in the
				// server-capacity test; real multitenant UID limits are tested separately.
				o.cfg.WorkBuddyAccountConcurrency = 13
				o.cfg.UsageContentMaxBytes = 32768
				o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
				o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
				o.wb.Catalog.Refresh()
				c := &capacityUpstream{started: make(chan struct{}, n), finish: make(chan struct{})}
				o.wb.UpstreamClient = c
				s := NewServer(o)
				if _, err := o.db.CreateApp("capacity", o.hashKey("capacity-key"), "", "", "", "", 0); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				var finish sync.Once
				defer finish.Do(func() { close(c.finish) })
				result := make(chan int, n)
				payload := `{"model":"auto","messages":[{"role":"user","content":"` + strings.Repeat("a", 128<<10) + `"}],"input":"hello","stream":true,"max_tokens":16384}`
				handler := s.handleChat
				path := "/v1/chat/completions"
				if route == "responses" {
					handler = s.handleResponses
					path = "/v1/responses"
				}
				if route == "anthropic" {
					handler = s.handleMessages
					path = "/v1/messages"
				}
				for i := 0; i < n; i++ {
					go func() {
						r := httptest.NewRequest("POST", path, strings.NewReader(payload)).WithContext(ctx)
						r.Header.Set("Authorization", "Bearer capacity-key")
						w := &discardResponse{header: make(http.Header)}
						s.requestGuard(http.HandlerFunc(handler)).ServeHTTP(w, r)
						result <- w.code
					}()
					select {
					case <-c.started:
					case code := <-result:
						t.Fatalf("early exit %d", code)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				if s.modelsAdmission.snapshot()["running"] != n {
					t.Fatal("capacity stalled", s.modelsAdmission.snapshot())
				}
				finish.Do(func() { close(c.finish) })
				for i := 0; i < n; i++ {
					if code := <-result; code != 200 {
						t.Fatal(code)
					}
				}
				if s.modelsAdmission.buffers != 0 || s.bodies.usage() != 0 {
					t.Fatal("capacity leak")
				}
			})
		}
	}
}

func TestPublicBodySaturationPreservesProtectedIntake(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{BodyReadConcurrency: 1}})
	for i := 0; i < cap(s.publicBodySlots); i++ {
		s.publicBodySlots <- struct{}{}
	}
	for i := 0; i < cap(s.publicBodyWaiting); i++ {
		s.publicBodyWaiting <- struct{}{}
	}
	if !s.publicBodies.reserve(s.publicBodies.limit) {
		t.Fatal("failed to saturate public budget")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /portal/api/auth/login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("POST /protected", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := s.requestGuard(mux)
	public := httptest.NewRecorder()
	h.ServeHTTP(public, httptest.NewRequest("POST", "/portal/api/auth/login", strings.NewReader(`{}`)))
	if public.Code != 429 {
		t.Fatal("public intake was not bounded", public.Code)
	}
	protected := httptest.NewRecorder()
	h.ServeHTTP(protected, httptest.NewRequest("POST", "/protected", strings.NewReader(`{}`)))
	if protected.Code != 204 || s.bodies.usage() != 0 || len(s.bodySlots) != 0 {
		t.Fatal("public traffic exhausted protected intake", protected.Code)
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"POST", "/missing", 404}, {"POST", "//protected", 307}, {"POST", "/./protected", 307}, {"POST", "//portal/api/auth/login", 307}, {"POST", "/health", 405}, {"GET", "/health", 204}, {"HEAD", "/health", 204}} {
		body := &unreadAdmissionBody{t: t}
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Body = body
		r.ContentLength = -1
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if tc.code == 405 && w.Header().Get("Allow") == "" {
			t.Fatal("method allowance lost")
		}
	}
}

type unreadAdmissionBody struct{ t *testing.T }

func (b *unreadAdmissionBody) Read([]byte) (int, error) {
	b.t.Error("unused body was read")
	return 0, io.EOF
}
func (*unreadAdmissionBody) Close() error { return nil }

type refreshCatalogTransport struct{ stage, calls int }

func (tr *refreshCatalogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.calls++
	// 双路目录（/v3/config + 插件目录）共享同一传输：stage 1 只让先拉的
	// /v3/config 失败一次，插件目录仍成功——这才构成「单路失败」语义；
	// 否则两路同失败会触发整路失败退避，不再保留旧映射。
	if tr.stage == 1 && tr.calls == 1 {
		return nil, errors.New("catalog temporarily unavailable")
	}
	if tr.stage == 1 && r.URL.Path != "/v3/config" {
		// 插件目录成功且不含 test-model：v3 失败回落旧快照 → Fresh 保留
		return &http.Response{StatusCode: 200, Header: make(http.Header),
			Body:    io.NopCloser(strings.NewReader(`{"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`)),
			Request: r}, nil
	}
	body := `{"data":{"models":[{"id":"test-model","name":"Fresh"}],"agents":[{"name":"cli","models":["test-model"]}]}}`
	if tr.stage == 0 {
		body = strings.ReplaceAll(body, "Fresh", "Old")
	}
	if tr.stage == 2 {
		body = `{"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}
func TestCatalogPartialFailurePreservesOnlyConfirmedMappings(t *testing.T) {
	o, original := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.Pool = pool.New(map[string]pool.Credential{"one": catalogOffline{original.Mgr}, "two": catalogOffline{original.Mgr}}, "")
	tr := &refreshCatalogTransport{}
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: tr})
	o.wb.Catalog.Refresh()
	tr.stage, tr.calls = 1, 0
	o.wb.Catalog.Refresh()
	var found map[string]any
	for _, model := range o.wb.Catalog.ListCached() {
		if model["id"] == "test-model" {
			found = model
		}
	}
	if found == nil || found["name"] != "Fresh" || len(found["account_uids"].([]string)) != 2 {
		t.Fatal("partial refresh lost verified account mapping or fresh metadata", found)
	}
	tr.stage, tr.calls = 2, 0
	o.wb.Catalog.Refresh()
	// 双路语义下的「模型被撤销」判定：空目录是合法响应、覆盖成功快照，但单路
	// 失败会让另一来源的快照继续供应该模型——只有两路同时给空目录才算撤销。
	// 逐个账号清快照再刷新，等价于「两路从此都返回空」。
	o.wb.Catalog.ClearCatalogSnapshots()
	o.wb.Catalog.Refresh()
	for _, model := range o.wb.Catalog.ListCached() {
		if model["id"] == "test-model" {
			t.Fatal("valid empty catalog retained revoked model", model)
		}
	}
}

func TestAdmissionSnapshotExplainsWaitWithoutSelectingAccounts(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4})
	a.queue = []*modelTicket{{key: "paced", limit: 4}, {key: "limited", limit: 1, ready: true}, {key: "buffer", limit: 4, ready: true, bufferBytes: a.bufferBudget + 1}, {key: "account", limit: 4, ready: true, chooseAccount: func(map[string]bool) (*provider.AccountRef, *apiError) {
		t.Fatal("snapshot invoked account selection")
		return nil, nil
	}}}
	a.byKey["limited"] = 1
	reasons := a.snapshot()["waiting_reasons"].(map[string]int)
	if reasons["upstream_pacing"] != 1 || reasons["key_or_user_limit"] != 1 || reasons["response_buffer_budget"] != 1 || reasons["account_busy"] != 1 {
		t.Fatal(reasons)
	}
	a.active = a.capacity
	reasons = a.snapshot()["waiting_reasons"].(map[string]int)
	if reasons["execution_limit"] != 1 || reasons["account_busy"] != 0 {
		t.Fatal(reasons)
	}
}

// Registry probes have no credentials or network work.
type registryProbe struct {
	name, prefix string
	closed       int
}

type customProtocolProbe struct {
	registryProbe
	t *testing.T
}

func (*customProtocolProbe) Ready() bool { return true }
func (p *customProtocolProbe) Serve(_ context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
	tools := req.Payload["tools"].([]any)
	if req.Protocol != provider.ProtocolResponses || tools[0].(map[string]any)["type"] != "function" {
		p.t.Fatal("custom tool not adapted at dispatch", req)
	}
	if _, err := protocol.ConvertRequest(protocol.Responses, protocol.Chat, req.Payload); err != nil {
		p.t.Fatal("prepared tool request cannot reach downstream chat bridge", err)
	}
	name := tools[0].(map[string]any)["name"].(string)
	args := `{"input":"*** Begin Patch\n*** End Patch"}`
	conv := adapters.NewResponsesStreamConverter("model")
	stream, _ := req.Payload["stream"].(bool)
	if !stream {
		conv.SetNonstream()
	}
	chunk := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":` + strconv.Quote(name) + `,"arguments":` + strconv.Quote(args) + `}}]}}]}`
	events := conv.FeedLine(chunk) + conv.FeedLine(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	if stream {
		req.Writer.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(req.Writer, events+conv.Finish()); err != nil {
			return provider.UsageReport{}, err
		}
		req.Writer.(http.Flusher).Flush()
	} else {
		writeJSON(req.Writer, 200, conv.GetNonstreamResponse())
	}
	return provider.UsageReport{Status: "ok"}, nil
}

func TestResponsesCustomToolsAtGatewayBoundary(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	probe := &customProtocolProbe{registryProbe: registryProbe{name: "custom", prefix: "custom/"}, t: t}
	if err := o.runtimes.Register(probe); err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	appID, err := o.db.CreateApp("custom-caller", o.hashKey("custom-fixture-key"), "", "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tools := range []string{
		`"tools":[{"type":"custom","name":"apply_patch"}]`,
		`"tools":[{"type":"namespace","name":"apps","tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"apply_patch"}]}]}],"tool_choice":{"type":"function","function":{"name":"apps::editor::apply_patch"}}`,
	} {
		for _, stream := range []bool{false, true} {
			body := fmt.Sprintf(`{"model":"custom/model","stream":%t,"input":"make a patch",%s}`, stream, tools)
			r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer custom-fixture-key")
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &Principal{AppID: appID, AppName: "custom-caller"}))
			w := httptest.NewRecorder()
			s.observeCalls(http.HandlerFunc(s.handleResponses)).ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"custom_tool_call"`) || !strings.Contains(w.Body.String(), `"call_id":"call_1"`) {
				t.Fatal("custom output lost at gateway", w.Code, w.Body.String())
			}
			if strings.Contains(tools, `"namespace"`) && !strings.Contains(w.Body.String(), `"namespace":"apps__editor"`) {
				t.Fatal("nested namespace lost at gateway", w.Body.String())
			}
			if !stream && w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Fatal("custom result completion requires handler EOF")
			}
			if stream && (!strings.Contains(w.Body.String(), `"type":"response.completed"`) || strings.Contains(w.Body.String(), `"type":"response.function_call_arguments.delta"`)) {
				t.Fatal("custom stream not restored", w.Body.String())
			}
			if state := s.modelsAdmission.snapshot(); state["running"] != 0 || state["queued"] != 0 || state["buffer_reserved_bytes"] != int64(0) {
				t.Fatal("custom request leaked resources", state)
			}
		}
	}
}

func (r *registryProbe) Name() string                                 { return r.name }
func (r *registryProbe) Prefix() string                               { return r.prefix }
func (*registryProbe) Ready() bool                                    { return false }
func (*registryProbe) Models(context.Context) []provider.CatalogModel { return nil }
func (*registryProbe) Serve(context.Context, provider.ServeRequest) (provider.UsageReport, error) {
	return provider.UsageReport{}, nil
}
func (r *registryProbe) Close() error { r.closed++; return nil }

func TestRuntimeRegistryIsolationAndReplacement(t *testing.T) {
	a, b := provider.NewRegistry(), provider.NewRegistry()
	old := &registryProbe{name: "channel", prefix: "channel/"}
	other := &registryProbe{name: "channel", prefix: "channel/"}
	if err := a.Register(old); err != nil {
		t.Fatal(err)
	}
	if err := b.Register(other); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(&registryProbe{name: "nested", prefix: "channel/sub/"}); err == nil {
		t.Fatal("ambiguous namespace accepted")
	}
	held, ok := a.ForModel("channel/model")
	if !ok || held != old {
		t.Fatal("route mismatch")
	}
	next := &registryProbe{name: "channel", prefix: "channel/"}
	if err := a.Replace(old, next); err != nil {
		t.Fatal(err)
	}
	if err := a.Replace(old, old); err == nil {
		t.Fatal("stale runtime replaced current")
	}
	if a.IsCurrent(old) || !a.IsCurrent(next) {
		t.Fatal("replacement ownership mismatch")
	}
	if got, _ := b.ByName("channel"); got != other {
		t.Fatal("replacement leaked across instances")
	}
	if held != old {
		t.Fatal("in-flight runtime changed")
	}
	if _, ok := a.ForModel("plain-model"); ok {
		t.Fatal("namespace claimed bare model")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if next.closed != 1 || old.closed != 0 || other.closed != 0 {
		t.Fatal("shutdown crossed ownership boundary")
	}
}

func TestProviderInvocationCancellationAndDispatchChecks(t *testing.T) {
	checks := 0
	denied := errors.New("account authorization changed")
	call := provider.Invocation{Caller: provider.Caller{AppID: 17, UserID: 9}, Check: func(ctx context.Context, ref provider.AccountRef, model string) error {
		checks++
		if ref.Provider != "workbuddy" || ref.LocalID != "one" || model != "model" {
			t.Fatal("dispatch identity changed")
		}
		if checks == 2 {
			return denied
		}
		return nil
	}}
	ctx := provider.WithInvocation(context.Background(), call)
	ref := provider.AccountRef{Provider: "workbuddy", LocalID: "one"}
	if err := provider.CheckDispatch(ctx, ref, "model"); err != nil {
		t.Fatal(err)
	}
	if err := provider.CheckDispatch(ctx, ref, "model"); !errors.Is(err, denied) {
		t.Fatal("earlier permission success bypassed recheck")
	}
	if provider.CurrentInvocation(ctx).Caller.AppID != 17 {
		t.Fatal("caller identity missing")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := provider.CheckDispatch(cancelled, ref, "model"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if checks != 2 {
		t.Fatal("cancelled dispatch consulted provider")
	}
}

func TestRegistryDefaultAndNamespaceCoexist(t *testing.T) {
	r := provider.NewRegistry()
	def := &registryProbe{name: "default"}
	other := &registryProbe{name: "named", prefix: "named/"}
	if err := r.Register(def); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(other); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&registryProbe{name: "another-default"}); err == nil {
		t.Fatal("second default accepted")
	}
	if got, _ := r.Resolve("bare-model"); got != def {
		t.Fatal("default missing")
	}
	if got, _ := r.Resolve("named/model"); got != other {
		t.Fatal("default shadowed namespace")
	}
	if _, ok := r.ForModel("bare-model"); ok {
		t.Fatal("namespace-only lookup changed")
	}
}

type completedProviderStream struct{}

func (*completedProviderStream) StreamUpstream(ctx context.Context, _ map[string]string, _ map[string]any, _ string, yield upstream.LineFunc) error {
	if err := yield(`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`); err != nil {
		return err
	}
	return yield("data: [DONE]")
}
func TestDefaultRuntimeNativeProtocolsLogOnce(t *testing.T) {
	for _, proto := range []provider.Protocol{provider.ProtocolChat, provider.ProtocolAnthropic, provider.ProtocolResponses} {
		t.Run(string(proto), func(t *testing.T) {
			o, acc := newDNSFailoverOrch(t, &failingUpstream{})
			o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
			o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.wb.Catalog.Refresh()
			o.wb.UpstreamClient = &completedProviderStream{}
			s := NewServer(o)
			payload := map[string]any{"model": "test-model", "stream": false}
			if proto == provider.ProtocolResponses {
				payload["input"] = "hello"
			} else {
				payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
			}
			w := httptest.NewRecorder()
			if !s.dispatchRuntimeTo(w, httptest.NewRequest("POST", "/", nil), proto, payload, &Principal{AppID: 7, AppName: "fixture"}) {
				t.Fatal("default runtime not dispatched")
			}
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var doc map[string]any
			if json.Unmarshal(w.Body.Bytes(), &doc) != nil {
				t.Fatal(w.Body.String())
			}
			if proto == provider.ProtocolResponses && doc["object"] != "response" {
				t.Fatal(doc)
			}
			if proto == provider.ProtocolAnthropic && doc["type"] != "message" {
				t.Fatal(doc)
			}
			rows, err := o.db.UsageRecent(10, "", "", nil, "", false, 0, "")
			if err != nil || len(rows) != 1 || rows[0]["protocol"] != string(proto) {
				t.Fatal("usage duplicated or protocol lost", rows, err)
			}
			health := httptest.NewRecorder()
			s.adminHealth(health, httptest.NewRequest("GET", "/", nil))
			var snapshot map[string]any
			_ = json.Unmarshal(health.Body.Bytes(), &snapshot)
			if snapshot["total_providers"] != float64(1) {
				t.Fatal("default health duplicated", snapshot)
			}
		})
	}
}

func TestAccountLeasesDoNotMixProviderLocalIDs(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 2, WorkBuddyAccountConcurrency: 1})
	first, err := a.acquire(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	ref1 := provider.AccountRef{Provider: "workbuddy", LocalID: "same"}
	if _, err := first.bindProviderAccount(context.Background(), func(map[string]bool) (*provider.AccountRef, *apiError) { return &ref1, nil }); err != nil {
		t.Fatal(err)
	}
	second, err := a.acquire(context.Background(), "second")
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	ref2 := provider.AccountRef{Provider: "other", LocalID: "same"}
	if _, err := second.bindProviderAccount(context.Background(), func(busy map[string]bool) (*provider.AccountRef, *apiError) {
		if provider.LocalBusy(busy, "other")["same"] {
			t.Fatal("other channel inherited WB occupancy")
		}
		return &ref2, nil
	}); err != nil {
		t.Fatal(err)
	}
	counts := a.snapshot()["account_running"].(map[string]int)
	if counts["same"] != 1 || counts["other/same"] != 1 {
		t.Fatal(counts)
	}
}

type nativeProtocolProbe struct {
	registryProbe
	seen provider.ServeRequest
}

type renameOnlyProbe struct {
	registryProbe
	name string
}

func (p *renameOnlyProbe) RenameAccount(id, name string) error {
	p.name = name
	return nil
}

func TestIndependentManagementCapabilitiesAndStrictSettings(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{})
	s := NewServer(o)
	probe := &renameOnlyProbe{registryProbe: registryProbe{name: "rename-only", prefix: "rename-only/"}}
	if err := o.runtimes.Register(probe); err != nil {
		t.Fatal(err)
	}
	request := func(name, id, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/admin/providers/fixture/accounts/fixture", strings.NewReader(body))
		r.SetPathValue("name", name)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	if w := request("rename-only", "fixture", `{"name":"new-name"}`, s.adminProviderRename); w.Code != 200 || probe.name != "new-name" {
		t.Fatal("independent rename rejected", w.Code, w.Body.String())
	}
	for _, body := range []string{`{}`, `{"name":null}`, `{"name":5}`, `{`} {
		if w := request("rename-only", "fixture", body, s.adminProviderRename); w.Code != 400 || probe.name != "new-name" {
			t.Fatal("bad body changed name", w.Code, body)
		}
	}
	if w := request("rename-only", "fixture", `{}`, s.adminProviderActivate); w.Code != 400 {
		t.Fatal("unsupported activation allowed", w.Code)
	}
	for _, body := range []string{`{}`, `{"enabled":true,"priority":1}`, `{"enabled":"yes"}`, `{"priority":1.5}`, `{"priority":1} {}`, `{"unknown":true}`} {
		if w := request("workbuddy", acc.UID, body, s.adminProviderAccountSettings); w.Code != 400 {
			t.Fatal("bad settings accepted", body, w.Code)
		}
	}
	if w := request("rename-only", "fixture", `{"enabled":false}`, s.adminProviderAccountSettings); w.Code != 400 {
		t.Fatal("unsupported enable allowed", w.Code)
	}
	if w := request("workbuddy", "missing", `{"enabled":false}`, s.adminProviderAccountSettings); w.Code != 400 {
		t.Fatal("missing account accepted", w.Code)
	}
	if _, err := o.db.UpsertProviderAccount(store.ProviderAccount{UID: acc.UID, Provider: "workbuddy", Profile: acc.Profile}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"enabled":false}`, `{"priority":7}`} {
		if w := request("workbuddy", acc.UID, body, s.adminProviderAccountSettings); w.Code != 200 {
			t.Fatal("WB setting failed", w.Code, w.Body.String())
		}
	}
	if w := request("workbuddy", acc.UID, `{"name":"  alias  "}`, s.adminProviderRename); w.Code != 200 {
		t.Fatal("WB alias failed", w.Code, w.Body.String())
	}
	persisted, err := o.db.GetAccount(acc.UID)
	current := o.wb.Pool.Get(acc.UID)
	if err != nil || current.Enabled || current.Priority != 7 || current.Alias != "alias" || persisted["alias"] != "alias" {
		t.Fatal("pool and persistent state diverged", persisted, current, err)
	}
	resource := provider.ResourceMetadata(map[string]any{"id": "resource", "label": "safe", "access_token": "secret-fixture", "password": "secret-fixture"}, "account", map[string]provider.ResourceAction{"rename": {Label: "重命名", Enabled: true}})
	encoded, _ := json.Marshal(resource)
	if strings.Contains(string(encoded), "secret-fixture") {
		t.Fatal("management summary exposed credentials")
	}
	if resource.Quota.Remaining != nil || resource.Status != "unknown" {
		t.Fatal("unknown quota reported as available", resource)
	}
	zero := provider.ResourceMetadata(map[string]any{"credits_remaining": float64(0), "healthy": true}, "account", nil)
	if zero.Quota.Remaining == nil || *zero.Quota.Remaining != 0 || zero.Status != "healthy" {
		t.Fatal("zero balance confused with unknown", zero)
	}
	data := o.wb.AdminData(context.Background())
	if len(data.Resources) != 1 || data.Resources[0].ID != acc.UID || !data.Resources[0].Actions["priority"].Enabled || data.Resources[0].Actions["activate"].Enabled {
		t.Fatal("WB action semantics changed", data.Resources)
	}
}

func (*nativeProtocolProbe) Ready() bool { return true }
func (p *nativeProtocolProbe) Serve(ctx context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
	p.seen = req
	writeJSON(req.Writer, 200, map[string]any{"ok": true})
	return provider.UsageReport{Status: "ok"}, nil
}
func TestNamespacedNativeRequestWorksWithoutWorkBuddyAccounts(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.Pool = pool.New(map[string]pool.Credential{}, "")
	o.wb.Catalog = models.New(o.wb.Pool, o.db)
	rt := &nativeProtocolProbe{registryProbe: registryProbe{name: "native", prefix: "native/"}}
	if err := o.runtimes.Register(rt); err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	appID, err := o.db.CreateApp("native-caller", o.hashKey("native-fixture-key"), "", "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"native/model","system":"original system","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"native_tool","input_schema":{"type":"object"}}],"n":2,"opaque":{"keep":true}}`
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-Claude-Code-Session-Id", "original-id")
	r.Header.Set("Authorization", "Bearer native-fixture-key")
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &Principal{AppID: appID, AppName: "native-caller"}))
	w := httptest.NewRecorder()
	s.handleMessages(w, r)
	if w.Code != 200 || rt.seen.Protocol != provider.ProtocolAnthropic || rt.seen.Caller.AppID != appID || rt.seen.Headers.Get("X-Claude-Code-Session-Id") != "original-id" || rt.seen.Payload["system"] != "original system" || rt.seen.Payload["n"] != float64(2) || rt.seen.Payload["opaque"] == nil {
		t.Fatal("native protocol changed or WB credentials required", w.Code, w.Body.String(), rt.seen)
	}
	if state := s.modelsAdmission.snapshot(); state["running"] != 0 || state["queued"] != 0 || state["buffer_reserved_bytes"] != int64(0) {
		t.Fatal("native request leaked resources", state)
	}
	if observed := o.observations.Views(); len(observed) != 1 || observed[0].Provider != "native" || observed[0].Model != "native/model" || observed[0].Running != 0 || observed[0].Requests != 1 {
		t.Fatal("common observation missing", observed)
	}
}

// Three slow providers must start together; one failure cannot erase another
// provider's success or expose the upstream's sensitive diagnostic.
type catalogRefreshProbe struct {
	registryProbe
	mu      sync.Mutex
	ids     []string
	started chan string
	finish  <-chan struct{}
	err     error
}

func (*catalogRefreshProbe) Ready() bool { return true }
func (p *catalogRefreshProbe) Models(context.Context) []provider.CatalogModel {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []provider.CatalogModel{}
	for _, id := range p.ids {
		out = append(out, provider.CatalogModel{ID: id})
	}
	return out
}
func (p *catalogRefreshProbe) RefreshModels(ctx context.Context) error {
	p.started <- p.name
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.finish:
	}
	if p.err != nil {
		return p.err
	}
	p.mu.Lock()
	p.ids = []string{p.name + "/new"}
	p.mu.Unlock()
	return nil
}
func TestParallelCatalogRefreshAndSafePartialFeedback(t *testing.T) {
	started := make(chan string, 3)
	finish := make(chan struct{})
	registry := provider.NewRegistry()
	for _, name := range []string{"one", "two", "three"} {
		p := &catalogRefreshProbe{registryProbe: registryProbe{name: name, prefix: name + "/"}, ids: []string{name + "/old"}, started: started, finish: finish}
		if name == "two" {
			p.err = fmt.Errorf("upstream SECRET_CREDENTIAL")
		}
		if name == "three" {
			p.err = provider.ErrPartialRefresh
		}
		if err := registry.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	o := &Orchestrator{runtimes: registry}
	done := make(chan []modelRefreshResult, 1)
	go func() { done <- o.refreshModelCatalogs(context.Background(), "") }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(finish)
			t.Fatal("refreshes serialized")
		}
	}
	close(finish)
	results := <-done
	statuses := map[string]string{}
	for _, r := range results {
		statuses[r.Provider] = r.Status
		if strings.Contains(r.Message, "SECRET") {
			t.Fatal("raw error leaked")
		}
		if r.Provider == "one" && (r.Added != 1 || r.Removed != 1 || r.Count != 1) {
			t.Fatal("change counts incorrect", r)
		}
		if r.Provider != "one" && !r.Stale {
			t.Fatal("failed snapshot not marked stale")
		}
	}
	if statuses["one"] != "ok" || statuses["two"] != "error" || statuses["three"] != "partial" {
		t.Fatal(statuses)
	}
	if len(modelRefreshWarnings(results)) != 2 {
		t.Fatal("failure hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	selected := o.refreshModelCatalogs(ctx, "one")
	if len(selected) != 1 || selected[0].Status != "error" {
		t.Fatal("cancellation ignored", selected)
	}
}
func TestRefreshSingleFlightSeparatesSelectedProviders(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{HeavyAdminConcurrency: 2}})
	started := make(chan string, 2)
	finish := make(chan struct{})
	done := make(chan *httptest.ResponseRecorder, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("provider")
		started <- name
		<-finish
		writeJSON(w, 200, map[string]any{"provider": name})
	})
	for _, name := range []string{"workbuddy", "qoder"} {
		go func(name string) {
			w := httptest.NewRecorder()
			s.serveRefresh(w, httptest.NewRequest("POST", "/admin/models/refresh?provider="+name, nil), handler)
			done <- w
		}(name)
	}
	names := map[string]bool{}
	for range 2 {
		select {
		case name := <-started:
			names[name] = true
		case <-time.After(time.Second):
			close(finish)
			t.Fatal("provider refresh merged incorrectly")
		}
	}
	close(finish)
	for range 2 {
		w := <-done
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if !names["workbuddy"] || !names["qoder"] {
		t.Fatal(names)
	}
}

type observedRefreshContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *observedRefreshContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestProviderRefreshGateCanceledWaiterAndIdentityChange(t *testing.T) {
	var gate provider.RefreshGate
	started := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.Do(context.Background(), "old", func(context.Context) error { close(started); <-finish; return provider.ErrPartialRefresh })
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.Do(ctx, "old", func(context.Context) error { t.Error("canceled waiter ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var calls atomic.Int32
	same := make(chan error, 1)
	observer := &observedRefreshContext{Context: context.Background(), joined: make(chan struct{})}
	go func() { same <- gate.Do(observer, "old", func(context.Context) error { calls.Add(1); return nil }) }()
	<-observer.joined
	next := make(chan error, 1)
	go func() {
		next <- gate.Do(context.Background(), "new", func(context.Context) error { calls.Add(1); return nil })
	}()
	close(finish)
	if err := <-done; !errors.Is(err, provider.ErrPartialRefresh) {
		t.Fatal(err)
	}
	if err := <-same; !errors.Is(err, provider.ErrPartialRefresh) {
		t.Fatal("same identity did not share refresh outcome", err)
	}
	if err := <-next; err != nil || calls.Load() != 1 {
		t.Fatal("changed identity shared old result", err)
	}
}

func TestCommonSessionObservationIsolationAndCompletion(t *testing.T) {
	var observations provider.SessionObservations
	request := provider.ServeRequest{Caller: provider.Caller{AppID: 1}, AppName: "client", Payload: map[string]any{"model": "qoder/model", "session_id": "PRIVATE_SESSION_SIGNAL"}}
	first := observations.Begin("qoder", request)
	second := observations.Begin("qoder", request)
	request.Caller.AppID = 2
	other := observations.Begin("qoder", request)
	request.Caller.AppID = 1
	channel := observations.Begin("opencode", request)
	rows := observations.Views()
	if len(rows) != 3 {
		t.Fatal("auth/channel isolation", rows)
	}
	for _, row := range rows {
		if row.Requests == 2 && row.Running != 2 {
			t.Fatal("concurrent requests lost", row)
		}
	}
	encoded, _ := json.Marshal(rows)
	if bytes.Contains(encoded, []byte("PRIVATE_SESSION_SIGNAL")) {
		t.Fatal("raw session signal retained")
	}
	credits := 1.5
	first(provider.UsageReport{AccountUID: "account", Credits: &credits})
	first(provider.UsageReport{Credits: &credits}) // completion is idempotent
	second(provider.UsageReport{AccountUID: "account"})
	other(provider.UsageReport{})
	channel(provider.UsageReport{})
	for _, row := range observations.Views() {
		if row.Running != 0 {
			t.Fatal("execution leaked", row)
		}
		if row.Requests == 2 && (row.Credits != 1.5 || row.Known != 1 || row.Unknown != 1 || row.UID != "account") {
			t.Fatal("usage attribution incorrect", row)
		}
	}
	request.Payload = map[string]any{"model": "qoder/model", "request_id": "not-a-session"}
	observations.Begin("qoder", request)(provider.UsageReport{})
	if len(observations.Views()) != 3 {
		t.Fatal("request ID manufactured a session")
	}
	var bounded provider.SessionObservations
	finish := make([]func(provider.UsageReport), 2000)
	for i := range finish {
		request.Payload["session_id"] = fmt.Sprint(i)
		finish[i] = bounded.Begin("qoder", request)
	}
	request.Payload["session_id"] = "overflow"
	bounded.Begin("qoder", request)(provider.UsageReport{})
	if len(bounded.Views()) != 2000 {
		t.Fatal("active observations evicted or capacity exceeded")
	}
	finish[0](provider.UsageReport{})
	bounded.Begin("qoder", request)(provider.UsageReport{})
	if len(bounded.Views()) != 2000 {
		t.Fatal("idle eviction failed")
	}
	for _, end := range finish {
		end(provider.UsageReport{})
	}
}

type routeProtocolProbe struct {
	registryProbe
	status  int
	partial bool
	calls   int
}

func (*routeProtocolProbe) Ready() bool { return true }
func (p *routeProtocolProbe) Models(context.Context) []provider.CatalogModel {
	return []provider.CatalogModel{{ID: p.prefix + "model", Name: "Fixture"}}
}
func (p *routeProtocolProbe) Serve(ctx context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
	p.calls++
	if err := provider.CheckDispatch(ctx, provider.AccountRef{Provider: p.name, LocalID: "fixture"}, p.prefix+"model"); err != nil {
		return provider.UsageReport{}, err
	}
	if p.status >= 400 {
		writeJSON(req.Writer, p.status, map[string]any{"error": map[string]any{"message": "fixture failure"}})
		return provider.UsageReport{Status: "error"}, nil
	}
	req.Writer.Header().Set("Content-Type", "text/event-stream")
	if req.Payload["stream"] == true {
		_, err := io.WriteString(req.Writer, `data: {"model":"`+p.prefix+`model","choices":[{"delta":{"content":"hello"}}]}`+"\n\n")
		if err != nil {
			return provider.UsageReport{}, err
		}
	} else {
		writeJSON(req.Writer, 200, map[string]any{"model": p.prefix + "model", "choices": []any{map[string]any{"message": map[string]any{"content": "hello"}}}})
	}
	if p.partial {
		return provider.UsageReport{Status: "incomplete"}, errors.New("fixture partial stream")
	}
	return provider.UsageReport{Status: "ok"}, nil
}
func TestUnifiedRouteFallbackAndNoReplayAfterOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		partial bool
		second  int
		code    int
	}{
		{"retry-before-response", 503, false, 1, 200}, {"request-rejection", 400, false, 0, 400}, {"partial-output", 0, true, 0, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newDNSFailoverOrch(t, &failingUpstream{})
			first := &routeProtocolProbe{registryProbe: registryProbe{name: "first", prefix: "first/"}, status: tc.status, partial: tc.partial}
			second := &routeProtocolProbe{registryProbe: registryProbe{name: "second", prefix: "second/"}}
			if err := o.runtimes.Register(first); err != nil {
				t.Fatal(err)
			}
			if err := o.runtimes.Register(second); err != nil {
				t.Fatal(err)
			}
			if _, err := o.db.SaveModelRoute(store.ModelRoute{Model: "my-route", Name: "Mine", Enabled: true, Members: []string{"first/model", "second/model"}}); err != nil {
				t.Fatal(err)
			}
			appID, err := o.db.CreateApp("route-caller", o.hashKey("route-fixture-key"), "", "", "", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			s := NewServer(o)
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"my-route","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			r.Header.Set("Authorization", "Bearer route-fixture-key")
			r.Header.Set("X-Claude-Code-Session-Id", "fixture-session")
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &Principal{AppID: appID, AppName: "route-caller"}))
			w := httptest.NewRecorder()
			s.handleChat(w, r)
			if w.Code != tc.code || first.calls != 1 || second.calls != tc.second {
				t.Fatal("routing decision", w.Code, w.Body.String(), first.calls, second.calls)
			}
			if tc.code == 200 && !strings.Contains(w.Body.String(), `"model":"my-route"`) {
				t.Fatal("physical model exposed to client", w.Body.String())
			}
			if state := s.modelsAdmission.snapshot(); state["running"] != 0 || state["queued"] != 0 || state["buffer_reserved_bytes"] != int64(0) {
				t.Fatal("lease leaked", state)
			}
			if tc.partial && len(o.routes.bindings) > 0 {
				t.Fatal("failed stream created successful affinity")
			}
			if tc.second == 1 {
				r2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"my-route","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
				r2.Header = r.Header.Clone()
				r2 = r2.WithContext(context.WithValue(r2.Context(), principalContextKey{}, &Principal{AppID: appID, AppName: "route-caller"}))
				w2 := httptest.NewRecorder()
				s.handleChat(w2, r2)
				if w2.Code != 200 || first.calls != 1 || second.calls != 2 {
					t.Fatal("successful route did not remain sticky", w2.Code, first.calls, second.calls)
				}
			}
		})
	}
}
func TestRouteWriterSplitFramesAndOpaqueState(t *testing.T) {
	for _, stream := range []bool{true, false} {
		dst := httptest.NewRecorder()
		writer := &routeWriter{dst: dst, header: make(http.Header), logical: "logical", stream: stream}
		raw := `{"model":"physical","usage":{"count":9007199254740993}}`
		if stream {
			raw = "data: " + raw + "\n\ndata: [DONE]\n\n"
		}
		for _, part := range []string{raw[:8], raw[8:17], raw[17:]} {
			if _, err := writer.Write([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}
		writer.finish()
		if !strings.Contains(dst.Body.String(), `"model":"logical"`) || !strings.Contains(dst.Body.String(), "9007199254740993") {
			t.Fatal("split response corrupted", dst.Body.String())
		}
	}
	if !routeOpaqueContinuation(map[string]any{"input": []any{map[string]any{"type": "reasoning", "encrypted_content": "opaque"}}}) {
		t.Fatal("encrypted continuation replayable")
	}
	if routeOpaqueContinuation(map[string]any{"previous_response_id": "", "input": "portable"}) {
		t.Fatal("empty continuation forced sticky state")
	}
}

func TestUnifiedRoutePortalEmptyAllowlistAndMemberScope(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	// Portal candidates must resolve to a runtime implementing
	// provider.PortalProvider, so this fixture needs a real WorkBuddy catalog
	// entry rather than a protocol probe.
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh() // Catalog transport returns fixture JSON without network access.
	s := NewServer(o)
	const member = "workbuddy/test-model"
	if s.physicalCatalog()[member] == nil {
		t.Fatal("fixture needs WorkBuddy catalog")
	}
	route := store.ModelRoute{Model: "portal-unified", Name: "Portal", Enabled: true, Members: []string{member}}
	id, err := o.db.SaveModelRoute(route)
	if err != nil {
		t.Fatal(err)
	}
	route.ID = id
	app, err := o.db.CreateApp("portal-route-grant", "route-hash", "", "", "", `["portal-unified"]`, 0)
	if err != nil {
		t.Fatal(err)
	}
	physical := o.resolveModel(member)
	p := &Principal{AppID: app, UserID: 99, PortalModels: map[string]bool{physical: true}, ModelScopes: map[string]map[string]bool{physical: {"test-account": true}}}
	if members, _ := s.routeCandidates(p, route, nil); len(members) != 0 {
		t.Fatal("empty portal key gained logical route", members)
	}
	p.AllowedModels = []string{"portal-unified"}
	if members, _ := s.routeCandidates(p, route, nil); len(members) != 1 {
		t.Fatal("explicit logical permission failed", members)
	}
	// A member added after the key grant was frozen stays invisible until the
	// key permissions are saved again. The extra member is fully portal-eligible
	// so only the frozen grant can exclude it.
	if s.physicalCatalog()["workbuddy/auto"] == nil {
		t.Fatal("fixture needs the standard auto catalog entry")
	}
	p.PortalModels["auto"] = true
	p.ModelScopes["auto"] = map[string]bool{"test-account": true}
	saved, err := o.db.ModelRoutes()
	if err != nil || len(saved) != 1 {
		t.Fatal("fixture route missing", saved, err)
	}
	saved[0].Members = []string{member, "workbuddy/auto"}
	if _, err := o.db.SaveModelRoute(saved[0]); err != nil {
		t.Fatal(err)
	}
	if members, _ := s.routeCandidates(p, saved[0], nil); len(members) != 1 || members[0] != member {
		t.Fatal("route edit widened a frozen key grant", members)
	}
	p.ModelScopes[physical] = map[string]bool{}
	if members, _ := s.routeCandidates(p, saved[0], nil); len(members) != 0 {
		t.Fatal("route widened an empty account scope", members)
	}
}

func TestUnlimitedModelAdmissionRetainsAccountingWithoutLocalCaps(t *testing.T) {
	a := newModelAdmission(&config.Config{ModelLimitsDisabled: true, MaxConcurrentRequests: 1, PortalSharedConcurrency: 1, ModelQueueSize: 1, ModelKeyQueueSize: 1, WorkBuddyAccountConcurrency: 1, ModelBufferBudgetBytes: 1})
	leases := []*modelLease{}
	for i := 0; i < 40; i++ {
		l, err := a.acquireWithBudget(context.Background(), "portal-user-1", 1, "model", 1<<20)
		if err != nil {
			t.Fatal("retired cap blocked model", err)
		}
		ref, err := l.bindProviderAccount(context.Background(), func(busy map[string]bool) (*provider.AccountRef, *apiError) {
			if len(busy) != 0 {
				t.Fatal("account concurrency cap still applied")
			}
			return &provider.AccountRef{Provider: "workbuddy", LocalID: "same"}, nil
		})
		if err != nil || ref == nil {
			t.Fatal(ref, err)
		}
		leases = append(leases, l)
	}
	if got := a.snapshot(); got["running"] != 40 || got["queued"] != 0 || got["limits_disabled"] != true {
		t.Fatal(got)
	}
	for _, l := range leases {
		l.release()
		l.release()
	}
	if got := a.snapshot(); got["running"] != 0 || len(a.byAccount) != 0 {
		t.Fatal("request accounting leaked", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.acquire(ctx, "k"); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestOptionalJSONAndRawBodyBudgetsDefaultToNoExtraCap(t *testing.T) {
	if !jsonWithinComplexity([]byte(strings.Repeat("[", 200)+strings.Repeat("]", 200)), 0, 0) {
		t.Fatal("retired depth limit applied")
	}
	if jsonWithinComplexity([]byte(`[[[]]]`), 0, 2) {
		t.Fatal("explicit depth guard ignored")
	}
	if jsonWithinComplexity([]byte(`{"a":1,"b":2}`), 2, 0) {
		t.Fatal("explicit item guard ignored")
	}
	b := &bodyBudget{}
	if !b.reserve(1024) || !b.reserve(2048) || b.usage() != 3072 {
		t.Fatal("zero body budget rejected request")
	}
	b.release(3072)
	if b.usage() != 0 {
		t.Fatal("body accounting leaked")
	}
}

func TestUnifiedStreamDoesNotUseNonStreamResponseCap(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &routeWriter{dst: recorder, header: make(http.Header), logical: "test", stream: true, limit: 4}
	data := []byte("data: {}\n\ndata: {}\n\n")
	if n, err := writer.Write(data); err != nil || n != len(data) {
		t.Fatal("valid stream limited by JSON budget", n, err)
	}
	writer.finish()
	if recorder.Body.Len() != len(data) {
		t.Fatal("stream truncated", recorder.Body.String())
	}
	writer = &routeWriter{dst: httptest.NewRecorder(), header: make(http.Header), logical: "test", limit: 4}
	if _, err := writer.Write([]byte("abcde")); err == nil {
		t.Fatal("non-stream cap removed")
	}
}

func TestAutoModelSuggestionsPreserveManualRoutesAndNames(t *testing.T) {
	catalog := map[string]map[string]any{}
	for _, id := range []string{"workbuddy/model", "qoder/model", "trae/model", "qoder/new", "trae/new", "workbuddy/auto", "qoder/auto", "workbuddy/movie", "trae/movie", "workbuddy/case", "trae/Case", "workbuddy/internal/name", "trae/internal/name"} {
		catalog[id] = map[string]any{}
	}
	catalog["workbuddy/movie"]["model_type"] = "video"
	catalog["trae/movie"]["model_type"] = "video"
	suggestions := suggestModelRoutes(nil, catalog, nil)
	if len(suggestions) != 3 {
		t.Fatal(suggestions)
	}
	byName := map[string]store.ModelRoute{}
	for _, r := range suggestions {
		byName[r.Model] = r
	}
	if len(byName["group/model"].Members) != 3 || byName["group/model"].Members[0] != "workbuddy/model" || len(byName["new"].Members) != 2 || len(byName["group/internal/name"].Members) != 2 {
		t.Fatal(suggestions)
	}
	manual := store.ModelRoute{Model: "custom", Members: []string{"workbuddy/model"}, Enabled: false}
	after := suggestModelRoutes([]store.ModelRoute{manual}, catalog, map[string]string{"new": "somewhere", "group/new": "elsewhere"})
	if len(after) != 1 || after[0].Model != "group/internal/name" {
		t.Fatal("manual or alias configuration overwritten", after)
	}
	first, _ := json.Marshal(suggestions)
	again, _ := json.Marshal(suggestModelRoutes(nil, catalog, nil))
	if string(first) != string(again) {
		t.Fatal("suggestion order unstable")
	}
}
func TestRouteVisionReflectsAuthorizedCapableMembers(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	for _, name := range []string{"first", "second"} {
		if err := o.runtimes.Register(&routeProtocolProbe{registryProbe: registryProbe{name: name, prefix: name + "/"}}); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(o)
	route := store.ModelRoute{Model: "combined", Name: "Combined", Enabled: true, Members: []string{"first/model", "second/model"}}
	catalog := map[string]map[string]any{"first/model": {"vision": false, "context_length": 200000, "max_output_tokens": 8000}, "second/model": {"supports_image": true, "context_length": 1000000, "max_output_tokens": 64000}}
	app, err := o.db.CreateApp("vision-route", "hash-vision", "", "", "", `["first/model"]`, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := &Principal{AppID: app, AllowedModels: []string{"first/model"}}
	members, aerr := s.routeCandidatesIn(p, route, nil, catalog)
	if aerr != nil || len(members) != 1 || routeCatalogEntry(route, members, catalog)["vision"] != false {
		t.Fatal("unauthorized vision exposed", members, aerr)
	}
	p.AllowedModels = append(p.AllowedModels, "second/model")
	members, aerr = s.routeCandidatesIn(p, route, nil, catalog)
	entry := routeCatalogEntry(route, members, catalog)
	if aerr != nil || entry["vision"] != true || entry["context_length"] != 200000 {
		t.Fatal("available vision hidden or window overstated", entry, aerr)
	}
	image := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}}}}}}
	members, aerr = s.routeCandidatesIn(p, route, image, catalog)
	if aerr != nil || len(members) != 1 || members[0] != "second/model" {
		t.Fatal("image dispatched to text-only member", members, aerr)
	}
}
