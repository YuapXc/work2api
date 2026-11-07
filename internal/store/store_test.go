package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work2api/internal/benchmarks"
)

func TestBenchmarkBindingsPersistAndClear(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "benchmarks.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	binding := benchmarks.Binding{EntryID: "aa-id", Identity: "observed-revision", Efforts: map[string]string{"high": "aa-high"}}
	if err := db.SaveBenchmarkBinding(ctx, "workbuddy/example", binding); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bindings, err := db.BenchmarkBindings(ctx)
	got := bindings["workbuddy/example"]
	if err != nil || got.EntryID != binding.EntryID || got.Identity != binding.Identity || got.Efforts["high"] != "aa-high" {
		t.Fatalf("binding was not restored: %+v, %v", got, err)
	}
	if err := db.SaveBenchmarkBinding(ctx, "workbuddy/example", benchmarks.Binding{}); err != nil {
		t.Fatal(err)
	}
	if bindings, err := db.BenchmarkBindings(ctx); err != nil || len(bindings) != 0 {
		t.Fatalf("binding was not cleared: %+v, %v", bindings, err)
	}
}

func TestAppCostsKeepChannelsAndOwnersSeparate(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "costs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := db.CreateUser("cost-user", "fixture", "user")
	if err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("mixed", "mixed-hash", "", "", "", "", user)
	if err != nil {
		t.Fatal(err)
	}
	known, unknown := true, false
	paid, zero, foreign := 2.0, 0.0, 99.0
	for _, item := range []UsageParams{
		{Model: "legacy-workbuddy", AppID: app, UserID: user, Credits: &paid, TokensKnown: &known, InputTokens: 10, OutputTokens: 5},
		{Model: "legacy-workbuddy", AppID: app, UserID: user, TokensKnown: &known},
		{Model: "unified/model", Diagnostics: &UsageDiagnostics{PhysicalModel: "trae/model"}, AppID: app, UserID: user, Credits: &zero, TokensKnown: &known, InputTokens: 10, OutputTokens: 5},
		{Model: "qoder/model", AppID: app, UserID: user, TokensKnown: &unknown, InputTokens: 10, OutputTokens: 5},
		{Model: "unified/missing-physical", AppID: app, UserID: user, Credits: &paid, TokensKnown: &known},
		{Model: "trae/model", AppID: app, UserID: 0, Credits: &foreign, TokensKnown: &known, InputTokens: 100},
	} {
		if err := db.LogUsage(item); err != nil {
			t.Fatal(err)
		}
	}
	for _, list := range []func() ([]App, error){db.ListApps, func() ([]App, error) { return db.UserApps(user) }} {
		apps, err := list()
		if err != nil || len(apps) != 1 {
			t.Fatal(apps, err)
		}
		a := apps[0]
		if a.Requests != 5 || a.Tokens != 45 || a.TokensKnown || a.CreditsKnown || len(a.ChannelCosts) != 4 {
			t.Fatal(a)
		}
		costs := map[string]*float64{}
		for _, c := range a.ChannelCosts {
			costs[c.Provider] = c.Credits
			if c.Provider == "workbuddy" && !c.Partial {
				t.Fatal("partial reported costs were marked complete")
			}
			if c.Provider == "trae" && c.Partial {
				t.Fatal("reported zero was marked incomplete")
			}
		}
		if costs["workbuddy"] == nil || *costs["workbuddy"] != 2 || costs["trae"] == nil || *costs["trae"] != 0 || costs["qoder"] != nil || costs["unknown"] == nil || *costs["unknown"] != 2 {
			t.Fatal(costs)
		}
	}
}

func TestBudgetSamplesExcludeUnknownTokensErrorsAndOldHistory(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := 1700000000.0
	known, unknown := true, false
	paid, zero := 2.0, 0.0
	for _, tc := range []struct {
		age    float64
		tokens int
		known  *bool
		cost   *float64
		status string
	}{
		{1, 1000, &known, &paid, "ok"}, {1, 200, &known, &zero, "ok"}, {1, 300, &known, nil, "ok"},
		{8, 3000, &known, &paid, "ok"}, {31, 9000, &known, &paid, "ok"}, {-1, 9000, &known, &paid, "ok"},
		{1, 9000, &unknown, &paid, "ok"}, {1, 9000, &known, &paid, "error"},
		{1, 500, nil, &paid, "ok"}, {1, 9000, nil, nil, "ok"}, {1, 9000, nil, &zero, "ok"},
	} {
		id, err := db.LogUsageWithID(UsageParams{Model: "m", AccountUID: "a", InputTokens: tc.tokens, TokensKnown: tc.known, Credits: tc.cost, Status: tc.status})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec("UPDATE usage_logs SET ts=?,tokens_known=? WHERE id=?", now-tc.age*86400, tc.known, id); err != nil {
			t.Fatal(err)
		}
	}
	samples, err := db.UsageBudgetSamples(now)
	if err != nil || len(samples) != 2 {
		t.Fatalf("samples=%#v err=%v", samples, err)
	}
	for _, s := range samples {
		if s.Recent {
			if s.Tokens != 1500 || s.Credits != 4 || s.Calls != 2 || s.LegacyCalls != 1 || s.ZeroTokens != 200 || s.ZeroCalls != 1 || s.UnknownCalls != 1 || s.UnknownTokens != 300 {
				t.Fatalf("recent=%#v", s)
			}
		} else if s.Tokens != 3000 || s.Credits != 2 || s.Calls != 1 {
			t.Fatalf("fallback=%#v", s)
		}
	}
}

func TestBudgetSamplesPreserveBillingSnapshots(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "budget-prices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	known := true
	paid := 1.0
	zero := 0.0
	for _, price := range []*float64{nil, &zero, &paid} {
		if err := db.LogUsage(UsageParams{Model: "m", AccountUID: "a", InputTokens: 1000, TokensKnown: &known, Credits: &paid, Status: "ok", Diagnostics: &UsageDiagnostics{BillingDomain: "domestic", BillingCoefficient: price, Performance: &UsagePerformance{HTTPStatus: 200, TotalMS: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	samples, err := db.UsageBudgetSamples(float64(time.Now().Unix()) + 1)
	if err != nil || len(samples) != 3 {
		t.Fatalf("samples=%#v err=%v", samples, err)
	}
	seen := map[string]bool{}
	for _, sample := range samples {
		if sample.Domain != "domestic" || sample.Calls != 1 {
			t.Fatal(sample)
		}
		if sample.Coefficient == nil {
			seen["unknown"] = true
		} else if *sample.Coefficient == 0 {
			seen["zero"] = true
		} else if *sample.Coefficient == 1 {
			seen["one"] = true
		}
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
}

func TestOnlineSnapshotIsIndependent(t *testing.T) {
	dir := t.TempDir()
	db, err := New(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SaveSettings(map[string]string{"keepalive_hour": "21"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "snapshot.db")
	if err := db.Snapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSettings(map[string]string{"keepalive_hour": "22"}); err != nil {
		t.Fatal(err)
	}
	copy, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	settings, err := copy.GetSettings()
	if err != nil || settings["keepalive_hour"] != "21" {
		t.Fatal(settings, err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// settings defaults
	s, err := db.GetSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s["checkin_hours"] != "9,21" {
		t.Fatalf("default setting wrong: %v", s["checkin_hours"])
	}

	// app create + find
	id, err := db.CreateApp("app1", "hash123", "sk-abc", "note", "enc", "", 0)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if id == 0 {
		t.Fatal("no app id")
	}
	app, err := db.FindAppByKey("hash123")
	if err != nil || app == nil {
		t.Fatalf("find app: %v %v", app, err)
	}
	if app["name"] != "app1" {
		t.Fatalf("app name: %v", app["name"])
	}

	// usage log + summary
	if err := db.LogUsage(UsageParams{Model: "m1", Protocol: "chat", AccountUID: "u1",
		InputTokens: 10, OutputTokens: 20, AppName: "app1"}); err != nil {
		t.Fatalf("log usage: %v", err)
	}
	sum, err := db.UsageSummary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum["total_requests"].(int64) != 1 || sum["total_tokens"].(int64) != 30 {
		t.Fatalf("summary wrong: %v", sum)
	}

	// account upsert + list
	auth := map[string]any{
		"auth":    map[string]any{"domain": "www.codebuddy.cn", "accessToken": "x"},
		"account": map[string]any{"uid": "u1", "nickname": "nick"},
	}
	if _, err := db.UpsertProviderAccount(ProviderAccount{UID: "u1", Provider: "workbuddy", Nickname: "nick", Domain: "www.codebuddy.cn", Profile: "cn-cli", Auth: auth}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	accs, err := db.ListAccounts()
	if err != nil || len(accs) != 1 {
		t.Fatalf("list accounts: %v %v", accs, err)
	}
	if accs[0]["profile"] != "cn-cli" {
		t.Fatalf("profile: %v", accs[0]["profile"])
	}
}

func TestUsageDiagnosticsMigrationAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.LogUsage(UsageParams{Model: "old", Status: "incomplete"}); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the previous schema to exercise an actual additive migration.
	if _, err = db.db.Exec("DROP INDEX idx_usage_request_group; ALTER TABLE usage_logs DROP COLUMN diagnostics; PRAGMA user_version=17"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	facts := &UsageDiagnostics{FinishReason: "length", RequestedLimits: map[string]int{"max_tokens": 1200}, EffectiveLimits: map[string]int{"max_tokens": 1000}, Compatibility: "identity", Started: true, Performance: &UsagePerformance{RequestID: "test-request", Attempts: 1, QueueMS: 7}}
	reasoning, creation := 7, 0
	if err = db.LogUsage(UsageParams{Model: "new", Status: "incomplete", InputTokens: 20, OutputTokens: 10, ReasoningTokens: &reasoning, CacheCreationTokens: &creation, Diagnostics: facts}); err != nil {
		t.Fatal(err)
	}
	for _, light := range []bool{true, false} {
		rows, err := db.UsageRecent(10, "", "", nil, "", light, 0, "")
		if err != nil || len(rows) != 2 {
			t.Fatal(rows, err)
		}
		value, ok := rows[0]["diagnostics"].(UsageDiagnostics)
		if !ok || value.FinishReason != "length" || value.EffectiveLimits["max_tokens"] != 1000 || value.Performance == nil || value.Performance.QueueMS != 7 || value.TokenDetails == nil || value.TokenDetails.Reasoning == nil || *value.TokenDetails.Reasoning != 7 || value.TokenDetails.CacheCreation == nil || *value.TokenDetails.CacheCreation != 0 || rows[1]["diagnostics"] != nil {
			t.Fatal(rows)
		}
	}
	row, err := db.GetUsage(2)
	if err != nil {
		t.Fatal(err)
	}
	if row["diagnostics"].(UsageDiagnostics).RequestedLimits["max_tokens"] != 1200 {
		t.Fatal(row)
	}
	if facts.TokenDetails != nil {
		t.Fatal("usage logging mutated the caller diagnostics")
	}
	var version int
	if err = db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatal(version, err)
	}
}

// TestSettingsCacheInvalidation: GetSettings is memoized, but SaveSettings must
// invalidate the cache so a later read reflects the write (and returned maps are
// independent copies the caller can mutate without corrupting the cache).
func TestSettingsCacheInvalidation(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	first, _ := db.GetSettings() // populates cache with defaults
	if first["cost_aware_routing"] != "1" {
		t.Fatalf("default cost_aware_routing = %q, want 1", first["cost_aware_routing"])
	}
	// mutating the returned copy must not leak into the cache
	first["cost_aware_routing"] = "tampered"

	if again, _ := db.GetSettings(); again["cost_aware_routing"] != "1" {
		t.Fatalf("cache corrupted by caller mutation: %q", again["cost_aware_routing"])
	}

	if err := db.SaveSettings(map[string]string{"cost_aware_routing": "0"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if after, _ := db.GetSettings(); after["cost_aware_routing"] != "0" {
		t.Fatalf("stale after save: %q, want 0", after["cost_aware_routing"])
	}
}

func TestUsageRowCap(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// 空表：span 为 (0,0)，截断为 no-op
	if lo, hi, _ := db.UsageRowSpan(); lo != 0 || hi != 0 {
		t.Fatalf("empty span = (%d,%d), want (0,0)", lo, hi)
	}
	if n, _ := db.CleanupUsageRows(2); n != 0 {
		t.Fatalf("cleanup on empty = %d, want 0", n)
	}

	for i := 0; i < 5; i++ {
		if err := db.LogUsage(UsageParams{Model: "m", Protocol: "chat", AccountUID: "u", Status: "ok"}); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}
	// maxRows<=0 是 no-op
	if n, _ := db.CleanupUsageRows(0); n != 0 {
		t.Fatalf("cleanup(0) = %d, want 0", n)
	}
	// 保留最新 2 条，删除最旧 3 条
	n, err := db.CleanupUsageRows(2)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 3 {
		t.Fatalf("deleted %d, want 3", n)
	}
	if c, _ := db.UsageRowCount(); c != 2 {
		t.Fatalf("remaining %d, want 2", c)
	}
	// 已在上限内：再次截断不删
	if n, _ := db.CleanupUsageRows(2); n != 0 {
		t.Fatalf("second cleanup = %d, want 0", n)
	}
}

func TestProviderAccountCollisionDoesNotTransferOwnership(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	record := ProviderAccount{UID: "same-id", Provider: "workbuddy", Nickname: "original", Profile: "cn-cli", Auth: map[string]any{"original": true}}
	if _, err := db.UpsertProviderAccount(record); err != nil {
		t.Fatal(err)
	}
	record.Provider = "qoder"
	record.Nickname = "replacement"
	if _, err := db.UpsertProviderAccount(record); err == nil {
		t.Fatal("foreign provider replaced account")
	}
	row, err := db.GetAccount("same-id")
	if err != nil {
		t.Fatal(err)
	}
	if row["provider"] != "workbuddy" || row["nickname"] != "original" {
		t.Fatal("account ownership or metadata changed")
	}
	record.Provider = "workbuddy"
	record.Nickname = "new alias source"
	if _, err := db.UpsertProviderAccount(record); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetAccount("same-id")
	if err != nil || row["nickname"] != "new alias source" {
		t.Fatal("same-provider update failed", err)
	}
}

func TestModelRouteGrantsFreezeAndDeletionRevokes(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	route := ModelRoute{Model: "my-model", Name: "Mine", Enabled: true, Members: []string{"workbuddy/a"}}
	id, err := db.SaveModelRoute(route)
	if err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("route-key", "fixture-hash", "", "", "", `["my-model"]`, 0)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := db.ModelRoutes()
	if err != nil || len(routes) != 1 {
		t.Fatal(routes, err)
	}
	routes[0].Members[0] = "mutated"
	again, _ := db.ModelRoutes()
	if again[0].Members[0] != "workbuddy/a" {
		t.Fatal("cache alias modified persisted configuration")
	}
	route.ID = id
	route.Revision = 1
	route.Members = append(route.Members, "trae/b")
	if _, err = db.SaveModelRoute(route); err != nil {
		t.Fatal(err)
	}
	grants, _ := db.AppRouteMembers(app, id)
	if !grants["workbuddy/a"] || grants["trae/b"] {
		t.Fatal("route edit expanded key authorization", grants)
	}
	if _, err = db.SaveModelRoute(route); err == nil {
		t.Fatal("stale configuration overwrote current revision")
	}
	if err = db.SetAppModels(app, `["my-model"]`); err != nil {
		t.Fatal(err)
	}
	grants, _ = db.AppRouteMembers(app, id)
	if !grants["trae/b"] {
		t.Fatal("explicit permission save did not refresh grant")
	}
	if err = db.DeleteModelRoute(id, 2); err != nil {
		t.Fatal(err)
	}
	route.ID = 0
	route.Revision = 0
	newID, err := db.SaveModelRoute(route)
	if err != nil {
		t.Fatal(err)
	}
	grants, _ = db.AppRouteMembers(app, newID)
	if len(grants) != 0 {
		t.Fatal("same-name recreation resurrected authorization", grants)
	}
}

func TestRequestLogsGroupRetriesAndEnforceOwnership(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	requestID := "0123456789abcdef0123456789abcdef"
	insert := func(user, app int64, status string) int64 {
		t.Helper()
		id, err := db.LogUsageWithID(UsageParams{UserID: user, AppID: app, Model: "workbuddy/test", Status: status, Diagnostics: &UsageDiagnostics{RequestedModel: "unified/test", PhysicalModel: "workbuddy/test", Performance: &UsagePerformance{RequestID: requestID}}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := insert(1, 2, "error")
	last := insert(1, 2, "ok")
	foreign := insert(3, 4, "ok")
	otherKey := insert(1, 5, "error")
	legacy, err := db.LogUsageWithID(UsageParams{UserID: 1, AppID: 2, Model: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.db.Exec("UPDATE usage_logs SET diagnostics='invalid json' WHERE id=?", legacy); err != nil {
		t.Fatal(err)
	}
	user := int64(1)
	list, total, err := db.UsageRequests(&user, 20, 0, "", "", nil, "", "")
	if err != nil || total != 3 || len(list) != 3 {
		t.Fatalf("groups=%v total=%d err=%v", list, total, err)
	}
	if list[2]["id"] != last || list[2]["logged_attempts"] != int64(2) {
		t.Fatal("retry not grouped", list)
	}
	for _, id := range []int64{last, first} {
		row, err := db.UsageRequest(id, &user)
		if err != nil {
			t.Fatal(err)
		}
		attempts := row["attempt_records"].([]map[string]any)
		if len(attempts) != 2 || attempts[0]["id"] != first || attempts[1]["id"] != last {
			t.Fatal("foreign key/user leaked", attempts)
		}
	}
	if row, err := db.UsageRequest(foreign, &user); err != nil || row != nil {
		t.Fatal("foreign record accessible", row, err)
	}
	if row, err := db.UsageRequest(otherKey, &user); err != nil || len(row["attempt_records"].([]map[string]any)) != 1 {
		t.Fatal(row, err)
	}
	list, total, err = db.UsageRequests(&user, 1, 1, "", "", nil, "", "")
	if err != nil || total != 3 || len(list) != 1 || list[0]["id"] != otherKey {
		t.Fatal("pagination", list, total, err)
	}
	list, total, err = db.UsageRequests(&user, 20, 0, "", "unified/test", nil, "ok", "")
	if err != nil || total != 1 || list[0]["id"] != last {
		t.Fatal("failed retry leaked into status filter", list, total, err)
	}
}

func TestUsageTokenDetailsRejectImpossibleCounts(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, n := range []int{-1, 11} {
		id, err := db.LogUsageWithID(UsageParams{InputTokens: 10, OutputTokens: 10, ReasoningTokens: &n, CacheCreationTokens: &n})
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetUsage(int(id))
		if err != nil {
			t.Fatal(err)
		}
		if diag, ok := row["diagnostics"].(UsageDiagnostics); ok && diag.TokenDetails != nil {
			t.Fatal("impossible counts retained", row)
		}
	}
}

func TestUsageMaintenanceKeepsAccountingAndBoundsContent(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "maintenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	credit := 2.0
	known := true
	ids := []int64{}
	for i := 0; i < 5; i++ {
		id, err := db.LogUsageWithID(UsageParams{Model: "test", InputTokens: 10, OutputTokens: 5, TokensKnown: &known, Credits: &credit, InputContent: strings.Repeat("a", 40), OutputContent: "reply", Diagnostics: &UsageDiagnostics{FinishReason: "stop", Performance: &UsagePerformance{RequestID: "test-id"}}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := db.db.Exec("UPDATE usage_logs SET content_bytes=NULL WHERE id=?", ids[3]); err != nil {
		t.Fatal(err)
	}
	old := float64(time.Now().Add(-8 * 24 * time.Hour).Unix())
	if _, err := db.db.Exec("UPDATE usage_logs SET ts=? WHERE id=?", old, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := db.MaintainUsage(t.Context(), 30, 100, 7, 90); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		row, err := db.GetUsage(int(id))
		if err != nil {
			t.Fatal(err)
		}
		if row["input_tokens"] != int64(10) || row["output_tokens"] != int64(5) {
			t.Fatal("accounting changed", row)
		}
		diag := row["diagnostics"].(UsageDiagnostics)
		if diag.FinishReason != "stop" || diag.Performance.RequestID != "test-id" {
			t.Fatal("diagnostics lost", diag)
		}
		if (row["input_content"] == "") != (i < 3) || diag.ContentCleared != (i < 3) {
			t.Fatal("oldest content policy failed", i, row)
		}
	}
	var queryPlan string
	if err := db.db.QueryRow("EXPLAIN QUERY PLAN SELECT COALESCE(SUM(content_bytes),0) FROM usage_logs").Scan(new(int), new(int), new(int), &queryPlan); err != nil || !strings.Contains(queryPlan, "idx_usage_content_bytes") {
		t.Fatal("budget reads full bodies", queryPlan, err)
	}
	var measured int64
	if err := db.db.QueryRow("SELECT SUM(content_bytes) FROM usage_logs").Scan(&measured); err != nil || measured != 90 {
		t.Fatal("size backfill or accounting failed", measured, err)
	}
	if n, _ := db.UsageRowCount(); n != 5 {
		t.Fatal("content cleanup removed rows", n)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.MaintainUsage(canceled, 1, 1, 1, 1); err != context.Canceled {
		t.Fatal(err)
	}
	if err := db.MaintainUsage(t.Context(), 0, 2, 0, 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.UsageRowCount(); n != 2 {
		t.Fatal("row cleanup failed", n)
	}
}

func TestMediaTaskPersistsAndTerminalUsageIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	task := MediaTask{ID: "video_test", Provider: "workbuddy", AccountUID: "account", Model: "movie", Kind: "video", Metadata: map[string]any{"seconds": 3}}
	if err = db.CreateMediaTask(task, "key"); err != nil {
		t.Fatal(err)
	}
	if err = db.UpdateMediaTask(task.ID, "upstream", "queued", map[string]any{"id": task.ID, "status": "queued"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateMediaTask(MediaTask{ID: "unsubmitted", Provider: "workbuddy", Model: "movie", Kind: "video"}, "key"); err != nil {
		t.Fatal(err)
	}
	if err = db.DeferMediaTaskCheck(task.ID, "暂时不可查询", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	saved, err := db.MediaTask(task.ID)
	if err != nil || saved.UpstreamID != "upstream" {
		t.Fatal(saved, err)
	}
	if saved.CheckFailures != 1 || saved.CheckError != "暂时不可查询" {
		t.Fatal("query backoff did not survive restart", saved)
	}
	if due, err := db.PendingMediaTasks(float64(time.Now().Unix())); err != nil || len(due) != 0 {
		t.Fatal("backoff or unknown submission ignored", due, err)
	}
	if due, err := db.PendingMediaTasks(float64(time.Now().Add(2 * time.Minute).Unix())); err != nil || len(due) != 1 || due[0] != task.ID {
		t.Fatal("submitted task not recoverable", due, err)
	}
	credit := 51.43
	if err = db.UpdateMediaTask(task.ID, "upstream", "completed", map[string]any{"id": task.ID, "status": "completed"}, &credit, ""); err != nil {
		t.Fatal(err)
	}
	if err = db.UpdateMediaTask(task.ID, "wrong", "in_progress", map[string]any{}, nil, ""); err != nil {
		t.Fatal(err)
	}
	saved, err = db.MediaTask(task.ID)
	if err != nil || saved.Status != "completed" || saved.UpstreamID != "upstream" || saved.CheckFailures != 0 || saved.CheckError != "" {
		t.Fatal(saved, err)
	}
	row, err := db.GetUsage(int(saved.UsageID))
	if err != nil || row["status"] != "ok" || row["credits"] != credit {
		t.Fatal(row, err)
	}
	count, _ := db.UsageRowCount()
	if count != 2 {
		t.Fatal("polling duplicated usage", count)
	}
}

func TestMediaPollingMigrationPreservesExistingTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media-migrate.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateMediaTask(MediaTask{ID: "existing", Provider: "workbuddy", Model: "movie", Kind: "video"}, "key"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMediaTask("existing", "upstream", "queued", map[string]any{"status": "queued"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	_, err = db.db.Exec(`DROP INDEX idx_media_tasks_pending;
	ALTER TABLE media_tasks DROP COLUMN next_check_at;
	ALTER TABLE media_tasks DROP COLUMN check_failures;
	ALTER TABLE media_tasks DROP COLUMN check_error;
	PRAGMA user_version=25`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.MediaTask("existing")
	if err != nil || task == nil || task.UpstreamID != "upstream" || task.CheckFailures != 0 {
		t.Fatal("migration lost existing task", task, err)
	}
	if due, err := db.PendingMediaTasks(float64(time.Now().Unix())); err != nil || len(due) != 1 || due[0] != task.ID {
		t.Fatal("old submitted task not scheduled", due, err)
	}
}

func TestMediaDoesNotInvalidateExistingKeyTokenTotals(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "media-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.CreateApp("key", "hash", "", "", "", "[]", 0)
	if err != nil {
		t.Fatal(err)
	}
	known := true
	credit := 1.0
	if err = db.LogUsage(UsageParams{AppID: id, Model: "chat", Protocol: "chat", InputTokens: 10, OutputTokens: 5, TokensKnown: &known, Credits: &credit}); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateMediaTask(MediaTask{ID: "video", Provider: "workbuddy", AccountUID: "a", AppID: id, Model: "movie", Kind: "video"}, "key"); err != nil {
		t.Fatal(err)
	}
	apps, err := db.ListApps()
	if err != nil || len(apps) != 1 || apps[0].Tokens != 15 || !apps[0].TokensKnown {
		t.Fatal(apps, err)
	}
}

func TestMediaResultRetentionKeepsUsageAndTaskState(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "retention-media.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.CreateMediaTask(MediaTask{ID: "old", Provider: "workbuddy", AccountUID: "a", Model: "wb-asr", Kind: "audio"}, "key"); err != nil {
		t.Fatal(err)
	}
	credit := 1.0
	if err = db.UpdateMediaTask("old", "upstream", "completed", map[string]any{"text": "sensitive transcript"}, &credit, ""); err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	_, err = db.db.Exec("UPDATE media_tasks SET updated_at=? WHERE id='old'", float64(time.Now().Add(-8*24*time.Hour).Unix()))
	db.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = db.MaintainUsage(context.Background(), 90, 100000, 7, 512<<20); err != nil {
		t.Fatal(err)
	}
	task, err := db.MediaTask("old")
	if err != nil || task.Status != "completed" || task.Response["result_cleared"] != true || task.Response["text"] != nil {
		t.Fatal(task, err)
	}
	row, err := db.GetUsage(int(task.UsageID))
	if err != nil || row["credits"] != credit {
		t.Fatal(row, err)
	}
}
