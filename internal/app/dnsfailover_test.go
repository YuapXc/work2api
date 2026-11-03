package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	wbruntime "work2api/internal/workbuddy/runtime"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

type failingUpstream struct {
	err   error
	calls int
	emit  bool
	lines []string
}

func TestLiveFreeAccountDispatch(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_FREE_DISPATCH") != "1" {
		t.Skip("explicit live verification only")
	}
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	var mgr *credentials.Manager
	for _, file := range credentials.FindAuthFiles("", filepath.Join("..", "..", "auths")) {
		candidate := credentials.NewManager(file)
		if siterouting.ProfileSite(candidate.Profile()) == siterouting.International {
			mgr = candidate
			break
		}
	}
	if mgr == nil {
		t.Fatal("international free-model credential unavailable")
	}
	// Simulate an exhausted balance only in this isolated, temporary runtime.
	// The real account, gateway database and credential file are not changed.
	o.wb.Pool = pool.New(map[string]pool.Credential{"live-free": mgr}, "")
	o.wb.Managers = map[string]*credentials.Manager{"live-free": mgr}
	o.wb.Catalog = models.New(o.wb.Pool, o.db)
	o.wb.Catalog.Refresh()
	const model = "deepseek-v4.1-flash"
	a := o.wb.Pool.Get("live-free")
	if !o.wb.ModelFree(a, model) {
		t.Fatal("live account does not declare this model free")
	}
	zero := 0.0
	o.wb.Pool.SetCredits(a.UID, &zero, &zero, nil, nil)
	o.wb.UpstreamClient = upstream.Shared()
	selected, err := o.pickAccountExcludingIn(model, "", nil, map[string]bool{a.UID: true})
	if err != nil || selected == nil {
		t.Fatal("free model lost at gateway selection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	body := map[string]any{"model": model, "stream": true, "max_tokens": 1024, "reasoning_effort": "off", "messages": []any{map[string]any{"role": "user", "content": "Reply with just OK."}}}
	body = o.wb.EnhanceBody(upstream.BuildUpstreamBody(body))
	var output strings.Builder
	finish := ""
	start := time.Now()
	_, callErr := o.openUpstreamScoped(ctx, selected, body, model, "", func(line string) error {
		chunk := upstream.DecodeFrame(ctx, line)
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok {
				output.WriteString(text)
			}
			if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
				finish = reason
			}
		}
		return nil
	}, nil, nil)
	if callErr != nil || output.Len() == 0 || finish != "stop" {
		var remote *upstream.UpstreamError
		if errors.As(callErr, &remote) {
			t.Logf("synthetic call rejection: %s", remote.Raw)
		}
		t.Fatalf("live free dispatch failed: error=%v finish=%s output_bytes=%d", callErr, finish, output.Len())
	}
	t.Logf("model=%s isolated_balance=0 declared_cost=0 duration=%s finish=%s output_bytes=%d", model, time.Since(start).Round(time.Millisecond), finish, output.Len())
}

func (f *failingUpstream) StreamUpstream(_ context.Context, _ map[string]string, _ map[string]any, target string, yield upstream.LineFunc) error {
	if target == "" {
		return errors.New("test account has no upstream URL")
	}
	f.calls++
	for _, line := range f.lines {
		if err := yield(line); err != nil {
			return err
		}
	}
	if f.emit {
		if err := yield(`data: {"choices":[{"delta":{"content":"partial"}}]}`); err != nil {
			return err
		}
	}
	return f.err
}
func newDNSFailoverOrch(t *testing.T, client *failingUpstream) (*Orchestrator, *pool.Account) {
	t.Helper()
	dir := t.TempDir()
	uid := "test-account"
	raw, err := json.Marshal(map[string]any{"auth": map[string]any{"access_token": "test-token", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": uid, "nickname": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uid+".info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := pool.New(map[string]pool.Credential{uid: mgr}, "")
	o := &Orchestrator{db: db, cfg: &config.Config{MaxConcurrentRequests: 4, PortalUserConcurrency: 2}, wb: &wbruntime.Runtime{Pool: p, Catalog: models.New(p, db), Managers: map[string]*credentials.Manager{uid: mgr}, UpstreamClient: client}, sessions: newSessionRouter()}
	o.wb.Configure(o.cfg, o.db, o.sessions)
	o.wb.RecordUsage = o.recordUsage
	o.runtimes = provider.NewRegistry()
	if err := o.runtimes.Register(o.wb); err != nil {
		t.Fatal(err)
	}
	acc := p.Get(uid)
	if acc == nil || acc.Profile == "" {
		t.Fatal("fixture must contain a routable pooled account")
	}
	return o, acc
}
func TestWorkBuddyToolFinishNormalization(t *testing.T) {
	for _, reason := range []string{"stop", "length", "content_filter", ""} {
		tool := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]}}]}`
		lines := []string{tool}
		if reason != "" {
			lines = append(lines, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"`+reason+`"}]}`)
		}
		lines = append(lines, "data: [DONE]")
		client := &failingUpstream{lines: lines}
		o, acc := newDNSFailoverOrch(t, client)
		var output strings.Builder
		converter := adapters.NewAnthropicStreamConverter("test-model")
		_, err := o.openUpstream(context.Background(), acc, map[string]any{}, "test-model", "", func(line string) error { output.WriteString(converter.FeedLine(line)); return nil }, nil)
		if err != nil || client.calls != 1 {
			t.Fatal(err, client.calls)
		}
		output.WriteString(converter.Finish())
		want := map[string]string{"stop": "tool_use", "": "tool_use", "length": "max_tokens", "content_filter": "refusal"}[reason]
		if !strings.Contains(output.String(), `"stop_reason":"`+want+`"`) {
			t.Fatal(reason, output.String())
		}
	}
}

func BenchmarkWorkBuddyChunkProcessing(b *testing.B) {
	line := `data: {"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("chunk", 40) + `","reasoning_content":"thinking"},"finish_reason":null}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`
	for _, cached := range []bool{false, true} {
		name := "reparse"
		if cached {
			name = "shared_frame"
		}
		b.Run(name, func(b *testing.B) {
			ctx := upstream.WithParsedFrames(context.Background())
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var validated map[string]any
				json.Unmarshal([]byte(line[6:]), &validated)
				role := false
				if cached {
					upstream.RememberFrame(ctx, line, validated)
					chunk := upstream.DecodeFrame(ctx, line)
					wbruntime.MergeUsageChunk(chunk, nil)
					wbruntime.DeltaPartsChunk(chunk)
					wbruntime.SanitizeChatChunkWithRole(line, chunk, &role)
				} else {
					wbruntime.MergeUsageFromLine(line, nil)
					wbruntime.DeltaParts(line)
					wbruntime.SanitizeChatSSEWithRole(line, &role)
				}
			}
		})
	}
}

func TestOpenUpstreamFailureAccounting(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "upstream.invalid", IsNotFound: true}
	for _, tc := range []struct {
		name        string
		err         error
		emit        bool
		wantFailure bool
	}{
		{"dns", dnsErr, false, false},
		{"wrapped dns", fmt.Errorf("dial: %w", dnsErr), false, false},
		{"dns timeout", &net.DNSError{Err: "timeout", Name: "upstream.invalid", IsTimeout: true}, false, false},
		{"connection reset", errors.New("connection reset"), false, true},
		{"local route unavailable", fmt.Errorf("dial: %w", syscall.ENETUNREACH), false, false},
		{"local route lost during stream", fmt.Errorf("read: %w", syscall.ENETDOWN), true, false},
		{"upstream 429", &upstream.UpstreamError{StatusCode: 429, Raw: []byte(`{"error":{"message":"rate limited"}}`)}, false, true},
		{"canceled", context.Canceled, false, false},
		{"stream interrupted", errors.New("connection reset"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &failingUpstream{err: tc.err, emit: tc.emit}
			o, acc := newDNSFailoverOrch(t, client)
			_, err := o.openUpstream(context.Background(), acc, map[string]any{}, "test-model", "", func(string) error { return nil }, nil)
			if !errors.Is(err, tc.err) {
				t.Fatalf("wrong propagated error: %v", err)
			}
			if client.calls != 1 {
				t.Fatalf("want one upstream call, got %d", client.calls)
			}
			got := o.wb.Pool.Get(acc.UID)
			if got == nil {
				t.Fatal("pooled account disappeared")
			}
			if (got.CooldownUntil > 0) != tc.wantFailure || (got.FailureCount > 0) != tc.wantFailure {
				t.Fatalf("unexpected account penalty: cooldown=%v failures=%d", got.CooldownUntil, got.FailureCount)
			}
		})
	}
}

type usageFlushProbe struct {
	*httptest.ResponseRecorder
	beforeFlush func()
}

func (w *usageFlushProbe) Flush() {
	if w.beforeFlush != nil {
		w.beforeFlush()
		w.beforeFlush = nil
	}
	w.ResponseRecorder.Flush()
}

func TestUsageTimingFinalizedAfterResponseAndRetainedAfterRestart(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	path := filepath.Join(t.TempDir(), "timings.db")
	db, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	o.db = db
	s := NewServer(o)
	limits := map[string]int{"max_tokens": 32}
	timestamp := float64(time.Now().UnixNano())/1e9 - 3
	h := s.observeCalls(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamwatch.QueueWait(r.Context(), 10*time.Millisecond, false)
		streamwatch.QueueWait(r.Context(), 5*time.Millisecond, true)
		streamwatch.StartAttempt(r.Context())
		streamwatch.AttemptHeaders(r.Context(), 200)
		streamwatch.Generation(r.Context())
		time.Sleep(3 * time.Millisecond)
		streamwatch.Generation(r.Context())
		for i := 0; i < 2; i++ {
			o.recordUsage(r.Context(), store.UsageParams{Timestamp: timestamp, Status: "ok", Model: "test", Protocol: "chat", Diagnostics: &store.UsageDiagnostics{RequestedLimits: limits}})
		}
		limits["max_tokens"] = 99
		writeJSON(w, 200, map[string]any{"result": "complete"})
	}))
	flushed := false
	w := &usageFlushProbe{ResponseRecorder: httptest.NewRecorder(), beforeFlush: func() {
		rows, err := o.db.UsageRecent(10, "", "", nil, "", true, 0, "")
		if err != nil || len(rows) != 0 {
			t.Fatal("database write delayed response flush", rows, err)
		}
		flushed = true
	}}
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if !flushed || w.Header().Get("Content-Length") != fmt.Sprint(w.Body.Len()) {
		t.Fatal("response completion not independent of persistence")
	}
	rows, err := o.db.UsageRecent(10, "", "", nil, "", true, 0, "")
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, row := range rows {
		if row["ts"] != timestamp {
			t.Fatal("deferred write changed event timestamp", row["ts"])
		}
		if row["diagnostics"].(store.UsageDiagnostics).RequestedLimits["max_tokens"] != 32 {
			t.Fatal("deferred diagnostic mutated before persistence")
		}
		p := row["diagnostics"].(store.UsageDiagnostics).Performance
		if p == nil || p.RequestID == "" || p.FirstByteMS == nil || p.ExecutionMS == nil || p.QueueMS != 10 || p.AccountWaitMS != 5 || p.Attempts != 1 || len(p.Stages) != 1 {
			t.Fatal(p)
		}
		if p.FirstGenerationMS == nil || p.GenerationMS == nil || *p.GenerationMS < 1 {
			t.Fatal("generation window not persisted", p)
		}
	}
	// Reopen the exact database to prove this is not the in-memory ring.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err = reopened.UsageRecent(10, "", "", nil, "", true, 0, "")
	if err != nil || len(rows) != 2 || rows[0]["diagnostics"].(store.UsageDiagnostics).Performance == nil {
		t.Fatal(rows, err)
	}
}

type regionRetryUpstream struct{ bodies []map[string]any }

func (f *regionRetryUpstream) StreamUpstream(ctx context.Context, h map[string]string, b map[string]any, target string, yield upstream.LineFunc) error {
	f.bodies = append(f.bodies, upstream.BuildUpstreamBody(b))
	if len(f.bodies) == 1 {
		return &upstream.UpstreamError{StatusCode: 503, Raw: []byte(`{"error":{"message":"temporary failure"}}`)}
	}
	return nil
}
func TestRegionFailoverPreservesClientCapabilities(t *testing.T) {
	o, first := newDNSFailoverOrch(t, &failingUpstream{})
	mgr := o.wb.Managers[first.UID]
	o.wb.Pool = pool.New(map[string]pool.Credential{first.UID: mgr, "alternate": mgr}, "")
	first = o.wb.Pool.Get(first.UID)
	first.Profile = "cn-cli"
	o.wb.Pool.Get("alternate").Profile = "intl-cli"
	o.wb.Managers["alternate"] = mgr
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	client := &regionRetryUpstream{}
	o.wb.UpstreamClient = client
	original := "You are Claude Code.\n# Harness\n" + strings.Repeat("Session guidance. ", 100) + "\n<skills_instructions>### Available skills\nfind-skills: skill://find-skills/SKILL.md</skills_instructions>\nAvailable agent types for the Agent tool:\nreviewer: read CLAUDE.md\n## MCP Server Instructions\nUse mcp__docs__search."
	input := map[string]any{
		"model": "test-model", "system": original,
		"tools": []any{
			map[string]any{"name": "ToolSearch", "description": "Find MCP tools", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "mcp__docs__search", "description": "Search CLAUDE.md", "input_schema": map[string]any{"type": "object"}, "defer_loading": true},
		},
		"tool_choice": map[string]any{"type": "auto", "disable_parallel_tool_use": true},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "search-1", "name": "ToolSearch", "input": map[string]any{"query": "docs"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "search-1", "content": []any{map[string]any{"type": "tool_reference", "tool_name": "mcp__docs__search"}}}, map[string]any{"type": "text", "text": "<system-reminder>Plan mode: read only.</system-reminder>"}}},
		},
	}
	body, err := adapters.AnthropicRequestToChat(input)
	if err != nil {
		t.Fatal(err)
	}
	body = o.enhanceBody(body)
	before, _ := json.Marshal(body)
	if _, err := o.openUpstream(context.Background(), first, body, "test-model", "", func(string) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if len(client.bodies) != 2 {
		t.Fatal("failover did not run", len(client.bodies))
	}
	content := func(b map[string]any) string { return b["messages"].([]any)[0].(map[string]any)["content"].(string) }
	if content(client.bodies[0]) != original {
		t.Fatal("domestic prompt was mutated")
	}
	if content(client.bodies[1]) != original || content(body) != original {
		t.Fatal("international retry or original prompt was mutated")
	}
	for _, attempt := range client.bodies {
		encoded, _ := json.Marshal(attempt)
		if string(encoded) != string(before) || attempt["parallel_tool_calls"] != false {
			t.Fatal("retry changed tool metadata, discovery, or parallel control", string(encoded))
		}
	}
}

type channelRetryUpstream struct {
	calls     int
	denyCalls int
	partial   bool
	body      string
	always    bool
	bodies    []map[string]any
	urls      []string
	afterCall func(int)
}

func TestWorkBuddyImageRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		history, current, partial, always bool
		code                              int
		calls                             int
	}{
		{"history only", true, false, false, false, 11135, 2},
		{"history and current images", true, true, false, false, 11135, 2},
		{"current only", false, true, false, false, 11135, 2},
		{"no images", false, false, false, false, 11135, 1},
		{"already output", true, false, true, false, 11135, 1},
		{"retry bounded", true, true, false, true, 11135, 2},
		{"other error", true, false, false, false, 11101, 1},
		{"adjacent user parts", true, true, false, false, 11135, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, acc := newDNSFailoverOrch(t, &failingUpstream{})
			client := &channelRetryUpstream{body: fmt.Sprintf(`{"code":%d}`, tc.code), partial: tc.partial, always: tc.always}
			o.wb.UpstreamClient = client
			image := func(url string) any {
				return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
			}
			old := []any{map[string]any{"type": "text", "text": "old"}}
			if tc.history {
				old = append(old, image("https://example.test/expired.png"), image("data:image/png;base64,AA=="))
			}
			current := []any{map[string]any{"type": "text", "text": "continue"}}
			if tc.current {
				current = append(current, image("https://example.test/current.png"))
			}
			body := map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": old},
				map[string]any{"role": "assistant", "content": "previous answer", "tool_calls": []any{map[string]any{"id": "call1", "function": map[string]any{"name": "run", "arguments": `{"image_url":"business data"}`}}}},
				map[string]any{"role": "tool", "tool_call_id": "call1", "content": "result"},
				map[string]any{"role": "user", "content": current},
			}}
			if tc.name == "adjacent user parts" {
				messages := body["messages"].([]any)
				body["messages"] = []any{messages[0], messages[3]}
			}
			original, _ := json.Marshal(body)
			d := &wbruntime.RequestDiagnostics{}
			ctx := context.WithValue(context.Background(), wbruntime.RequestDiagnosticsKey{}, d)
			_, err := o.openUpstream(ctx, acc, body, "test-model", "", func(string) error { return nil }, nil)
			if client.calls != tc.calls {
				t.Fatalf("calls=%d want=%d err=%v", client.calls, tc.calls, err)
			}
			wantSuccess := tc.calls == 2 && !tc.always
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected recovery result: %v", err)
			}
			after, _ := json.Marshal(body)
			if string(after) != string(original) {
				t.Fatal("original request changed")
			}
			if tc.calls == 2 {
				if client.urls[0] != client.urls[1] {
					t.Fatal("image recovery changed upstream account region")
				}
				messages := client.bodies[1]["messages"].([]any)
				wantRemoved := 0
				if tc.history {
					wantRemoved += 2
				}
				if tc.current {
					wantRemoved++
				}
				if d.ImagesRemoved != wantRemoved {
					t.Fatalf("removed=%d want=%d", d.ImagesRemoved, wantRemoved)
				}
				for i, item := range messages {
					message := item.(map[string]any)
					originalMessage := body["messages"].([]any)[i].(map[string]any)
					if parts, ok := message["content"].([]any); ok {
						for _, part := range parts {
							if part.(map[string]any)["type"] == "image_url" {
								t.Fatal("image survived recovery")
							}
						}
						before := originalMessage["content"].([]any)
						oldText, _ := json.Marshal(before[0])
						newText, _ := json.Marshal(parts[0])
						if len(parts) != len(before) || string(oldText) != string(newText) {
							t.Fatal("text or block order changed")
						}
					} else {
						before, _ := json.Marshal(originalMessage)
						after, _ := json.Marshal(message)
						if string(before) != string(after) {
							t.Fatalf("non-image message %d changed", i)
						}
					}
				}
			}
			if !o.wb.Pool.Get(acc.UID).Enabled || o.wb.Pool.Get(acc.UID).CooldownUntil > nowSec() {
				t.Fatal("image failure penalized account")
			}
			if wantSuccess {
				client.calls, client.bodies = 0, nil
				_, _ = o.openUpstream(ctx, acc, body, "test-model", "", func(string) error { return nil }, nil)
				first, _ := json.Marshal(client.bodies[0])
				if string(first) != string(original) {
					t.Fatal("image fallback leaked into next request")
				}
			}
		})
	}
}

func TestWorkBuddySelectiveImageRecovery(t *testing.T) {
	const valid = "data:image/png;base64,iVBORw0KGgo="
	const remote = "https://example.test/normal.png"
	for _, tc := range []struct {
		name, raw                   string
		urls                        []string
		want                        []string // empty means an omitted image
		deny, calls, fixed, removed int
		mode                        string
	}{
		{"bad base64 only", `{"code":11135}`, []string{valid, "data:image/png;base64,!!", remote}, []string{valid, "", remote}, 1, 2, 0, 1, "partial"},
		{"missing padding", `{"code":11135}`, []string{valid, "data:image/png;base64,iVBORw0KGgo", remote}, []string{valid, valid, remote}, 1, 2, 1, 0, "partial"},
		{"base64 whitespace", `{"code":11135}`, []string{"data:image/png;base64,iVBO Rw0K\nGgo=", remote}, []string{valid, remote}, 1, 2, 1, 0, "partial"},
		{"actual MIME", `{"code":11135}`, []string{"data:image/jpeg;base64,iVBORw0KGgo=", remote}, []string{valid, remote}, 1, 2, 1, 0, "partial"},
		{"empty data", `{"code":11135}`, []string{valid, "data:image/png;base64,", remote}, []string{valid, "", remote}, 1, 2, 0, 1, "partial"},
		{"explicit position", `{"code":11135,"extError":{"param":"messages[0].content[2].image_url.url"}}`, []string{valid, remote}, []string{valid, ""}, 1, 2, 0, 1, "partial"},
		{"out of range position", `{"code":11135,"param":"messages[99].content[2]"}`, []string{valid, remote}, []string{"", ""}, 1, 2, 0, 2, "all"},
		{"text position ignored", `{"code":11135,"param":"messages[0].content[0]"}`, []string{valid, remote}, []string{"", ""}, 1, 2, 0, 2, "all"},
		{"conflicting positions", `{"code":11135,"param":"messages[0].content[1]","extError":{"param":"messages[0].content[2]"}}`, []string{valid, remote}, []string{"", ""}, 1, 2, 0, 2, "all"},
		{"repair then full fallback", `{"code":11135}`, []string{"data:image/png;base64,iVBORw0KGgo", remote}, []string{valid, remote}, 2, 3, 1, 2, "all"},
		{"misleading message ignored", `{"code":11101,"msg":"quoted: \"code\":11135"}`, []string{valid, remote}, nil, 1, 1, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, acc := newDNSFailoverOrch(t, &failingUpstream{})
			client := &channelRetryUpstream{body: tc.raw, denyCalls: tc.deny}
			o.wb.UpstreamClient = client
			parts := []any{map[string]any{"type": "text", "text": "inspect"}}
			for _, url := range tc.urls {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url, "detail": "high"}})
			}
			body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": parts}}}
			original, _ := json.Marshal(body)
			d := &wbruntime.RequestDiagnostics{}
			ctx := context.WithValue(context.Background(), wbruntime.RequestDiagnosticsKey{}, d)
			_, err := o.openUpstream(ctx, acc, body, "test-model", "", func(string) error { return nil }, nil)
			if client.calls != tc.calls || (err == nil) != (tc.calls > tc.deny) {
				t.Fatalf("calls=%d want=%d err=%v", client.calls, tc.calls, err)
			}
			if d.ImagesRepaired != tc.fixed || d.ImagesRemoved != tc.removed || d.ImageRecovery != tc.mode {
				t.Fatalf("unexpected diagnostics: %+v", d)
			}
			after, _ := json.Marshal(body)
			if string(original) != string(after) {
				t.Fatal("original request mutated")
			}
			if tc.want != nil {
				second := client.bodies[1]["messages"].([]any)[0].(map[string]any)["content"].([]any)
				if second[0].(map[string]any)["text"] != "inspect" {
					t.Fatal("text changed")
				}
				for i, want := range tc.want {
					part := second[i+1].(map[string]any)
					if want == "" {
						if part["type"] != "text" {
							t.Fatal("invalid image survived")
						}
						continue
					}
					image, ok := part["image_url"].(map[string]any)
					if !ok || image["url"] != want || image["detail"] != "high" {
						t.Fatalf("image %d changed incorrectly: %v", i, part)
					}
				}
			}
			if tc.calls == 3 {
				last := client.bodies[2]["messages"].([]any)[0].(map[string]any)["content"].([]any)
				for _, v := range last {
					if v.(map[string]any)["type"] != "text" {
						t.Fatal("full fallback retained image")
					}
				}
			}
		})
	}
}

func (f *channelRetryUpstream) StreamUpstream(_ context.Context, _ map[string]string, request map[string]any, target string, yield upstream.LineFunc) error {
	f.calls++
	f.bodies = append(f.bodies, request)
	f.urls = append(f.urls, target)
	if f.afterCall != nil {
		f.afterCall(f.calls)
	}
	if f.calls == 1 || f.calls <= f.denyCalls || f.always {
		if f.partial {
			if e := yield(`data: {"choices":[{"delta":{"content":"partial"}}]}`); e != nil {
				return e
			}
		}
		return &upstream.UpstreamError{StatusCode: 400, Raw: []byte(f.body)}
	}
	return nil
}

func TestChannelIdentityCompatibilityPreservesCapabilitiesAndBounds(t *testing.T) {
	const banner = "You are Claude Code, Anthropic's official CLI for Claude."
	const harnessBanner = "You are an AI agent powered by DeepSeek Harness."
	const codexBanner = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI."
	const zcodeBranch = "You are ZCode, an interactive coding agent\nMain branch (you will usually use this for PRs): main"
	const zcodeCompatible = "You are ZCode, an interactive coding agent\nDefault branch (you will usually use this for PRs): main"
	const tail = "\n### Available skills\nfind-skills: skill://find-skills/SKILL.md\nAvailable agent types for the Agent tool:\nreviewer: read CLAUDE.md\n## MCP Server Instructions\nUse MCP tools only with permission."
	const denial = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`
	for _, tc := range []struct {
		name                              string
		profile, prompt, errorBody        string
		wantRewrite                       string
		disabled, partial, always, revoke bool
		wantCalls                         int
		wantCompat                        bool
		denyCalls                         int
		lateDomestic                      bool
	}{
		{name: "domestic", profile: "cn-cli", prompt: banner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "harness domestic", profile: "cn-cli", prompt: harnessBanner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "harness domestic work", profile: "cn-work", prompt: harnessBanner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "harness international work", profile: "intl-work", prompt: harnessBanner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "harness disabled", profile: "cn-cli", prompt: harnessBanner + tail, errorBody: denial, disabled: true, wantCalls: 2},
		{name: "harness partial output", profile: "cn-cli", prompt: harnessBanner + tail, errorBody: denial, partial: true, wantCalls: 1},
		{name: "zcode branch", profile: "cn-cli", prompt: zcodeBranch + tail, errorBody: denial, wantCalls: 2, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode disabled", profile: "cn-cli", prompt: zcodeBranch + tail, errorBody: denial, disabled: true, wantCalls: 2},
		{name: "zcode international", profile: "intl-cli", prompt: zcodeBranch + tail, errorBody: denial, wantCalls: 2, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode international work", profile: "intl-work", prompt: zcodeBranch + tail, errorBody: denial, wantCalls: 2, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode domestic work", profile: "cn-work", prompt: zcodeBranch + tail, errorBody: denial, wantCalls: 2, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode content policy", profile: "cn-cli", prompt: zcodeBranch + tail, errorBody: `{"code":11128,"msg":"blocked by security policy"}`, wantCalls: 2, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode international policy", profile: "intl-work", prompt: zcodeBranch + tail, errorBody: `{"code":11128,"msg":"请求被安全策略拦截"}`, wantCalls: 2, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode code only", profile: "intl-cli", prompt: zcodeBranch + tail, errorBody: `{"code":"11128"}`, wantCalls: 2, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode fifth domestic attempt", profile: "intl-cli", prompt: zcodeBranch + tail, errorBody: denial, wantCalls: 5, wantCompat: true, wantRewrite: zcodeCompatible + tail, denyCalls: 4, lateDomestic: true},
		{name: "zcode partial output", profile: "cn-cli", prompt: zcodeBranch + tail, errorBody: denial, partial: true, wantCalls: 1},
		{name: "zcode international partial output", profile: "intl-work", prompt: zcodeBranch + tail, errorBody: denial, partial: true, wantCalls: 1},
		{name: "zcode persistent denial", profile: "cn-cli", prompt: zcodeBranch + tail, errorBody: denial, always: true, wantCalls: 5, wantCompat: true, wantRewrite: zcodeCompatible + tail},
		{name: "zcode revoked during retry", profile: "cn-cli", prompt: zcodeBranch + tail, errorBody: denial, revoke: true, wantCalls: 1},
		{name: "zcode international revoked", profile: "intl-cli", prompt: zcodeBranch + tail, errorBody: denial, revoke: true, wantCalls: 1},
		{name: "codex domestic", profile: "cn-cli", prompt: codexBanner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "codex disabled", profile: "cn-cli", prompt: codexBanner + tail, errorBody: denial, disabled: true, wantCalls: 2},
		{name: "codex international", profile: "intl-cli", prompt: codexBanner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "codex content policy", profile: "cn-cli", prompt: codexBanner + tail, errorBody: `{"code":11128,"msg":"blocked by security policy"}`, wantCalls: 2, wantCompat: true},
		{name: "codex partial output", profile: "cn-cli", prompt: codexBanner + tail, errorBody: denial, partial: true, wantCalls: 1},
		{name: "codex persistent denial", profile: "cn-cli", prompt: codexBanner + tail, errorBody: denial, always: true, wantCalls: 5, wantCompat: true},
		{name: "codex revoked during retry", profile: "cn-cli", prompt: codexBanner + tail, errorBody: denial, revoke: true, wantCalls: 1},
		{name: "disabled", profile: "cn-cli", prompt: banner + tail, errorBody: denial, disabled: true, wantCalls: 2},
		{name: "international", profile: "intl-cli", prompt: banner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "unknown banner", profile: "cn-cli", prompt: "You are Claude Code." + tail, errorBody: denial, wantCalls: 2},
		{name: "embedded banner", profile: "cn-cli", prompt: "Project note: " + banner + tail, errorBody: denial, wantCalls: 2},
		{name: "content policy", profile: "cn-cli", prompt: banner + tail, errorBody: `{"code":11128,"msg":"blocked by security policy"}`, wantCalls: 2, wantCompat: true},
		{name: "role validation", profile: "cn-cli", prompt: banner + tail, errorBody: `{"code":11128,"msg":"first message is not system prompt"}`, wantCalls: 2},
		{name: "partial output", profile: "cn-cli", prompt: banner + tail, errorBody: denial, partial: true, wantCalls: 1},
		{name: "persistent denial", profile: "cn-cli", prompt: banner + tail, errorBody: denial, always: true, wantCalls: 5, wantCompat: true},
		{name: "revoked during retry", profile: "cn-cli", prompt: banner + tail, errorBody: denial, revoke: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, first := newDNSFailoverOrch(t, &failingUpstream{})
			mgr := o.wb.Managers[first.UID]
			managers := map[string]pool.Credential{first.UID: mgr}
			alternatives := 6
			if tc.lateDomestic {
				alternatives = 3
			}
			for i := 0; i < alternatives; i++ {
				uid := fmt.Sprintf("alternate-%d", i)
				managers[uid] = mgr
				o.wb.Managers[uid] = mgr
			}
			o.wb.Pool = pool.New(managers, "")
			first = o.wb.Pool.Get(first.UID)
			first.Profile = tc.profile
			for _, a := range o.wb.Pool.Accounts() {
				if a.UID != first.UID {
					a.Profile = "intl-cli"
				}
			}
			if tc.lateDomestic {
				o.wb.Pool.Get("alternate-2").Profile = "cn-cli"
			}
			o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.wb.Catalog.Refresh()
			o.cfg.ChannelIdentityCompat = !tc.disabled
			client := &channelRetryUpstream{body: tc.errorBody, partial: tc.partial, always: tc.always, denyCalls: tc.denyCalls}
			if tc.lateDomestic {
				// Keep normal routing intact: this authorized domestic candidate
				// becomes available only for the last remaining dispatch slot.
				o.wb.Pool.SetEnabled("alternate-2", false, "fixture unavailable")
				client.afterCall = func(n int) {
					if n == 4 {
						o.wb.Pool.SetEnabled("alternate-2", true, "")
					}
				}
			}
			o.wb.UpstreamClient = client
			body := map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "system", "content": tc.prompt}, map[string]any{"role": "user", "content": "<system-reminder>Plan mode: read only.</system-reminder>"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "Skill", "description": "Use find-skills", "parameters": map[string]any{"type": "object"}}}}}
			original, _ := json.Marshal(body)
			admission := newModelAdmission(o.cfg)
			lease, leaseErr := admission.acquire(context.Background(), "private")
			if leaseErr != nil {
				t.Fatal(leaseErr)
			}
			t.Cleanup(lease.release)
			ctx := context.WithValue(context.Background(), modelLeaseKey{}, lease)
			if tc.revoke {
				ctx = context.WithValue(ctx, portalDispatchCheckKey{}, func(string, string) *apiError {
					if client.calls > 0 {
						return errBody(403, "revoked", "permission_error")
					}
					return nil
				})
			}
			o.sessions.Bind("sticky", first.UID)
			served, err := o.openUpstream(ctx, first, body, "test-model", "sticky", func(string) error { return nil }, nil)
			if client.calls != tc.wantCalls {
				t.Fatalf("calls=%d, want %d, error=%v", client.calls, tc.wantCalls, err)
			}
			if tc.wantCompat && tc.wantCalls == 2 && (err != nil || served.UID != first.UID || o.modelCooldownUntil(first.UID, "test-model") > nowSec() || o.sessions.Lookup("sticky") != first.UID) {
				t.Fatal("successful banner compatibility penalized/rotated account", err)
			}
			if tc.lateDomestic {
				wantURL, _ := siterouting.ChatURLForProfile("cn-cli")
				if err != nil || served.UID != "alternate-2" || client.urls[4] != wantURL {
					t.Fatalf("last domestic slot: account=%s URL=%s error=%v", served.UID, client.urls[4], err)
				}
			}
			for i, attempt := range client.bodies {
				expected := tc.prompt
				if tc.wantCompat && i > 0 {
					expected = "You are a coding assistant." + tail
					if tc.wantRewrite != "" {
						expected = tc.wantRewrite
					}
				}
				if attempt["messages"].([]any)[0].(map[string]any)["content"] != expected {
					t.Fatal("incorrect identity rewrite", i, attempt)
				}
				copyBody := make(map[string]any)
				for k, v := range attempt {
					copyBody[k] = v
				}
				messages := append([]any(nil), attempt["messages"].([]any)...)
				messages[0] = body["messages"].([]any)[0]
				copyBody["messages"] = messages
				encoded, _ := json.Marshal(copyBody)
				if string(encoded) != string(original) {
					t.Fatal("capabilities changed", i)
				}
			}
			after, _ := json.Marshal(body)
			if string(after) != string(original) {
				t.Fatal("original request mutated")
			}
			if tc.always || tc.revoke || tc.wantCalls == 1 {
				if err == nil {
					t.Fatal("failure unexpectedly succeeded")
				}
			}
			lease.release()
			if state := admission.snapshot(); state["running"] != 0 || state["queued"] != 0 || len(state["account_running"].(map[string]int)) != 0 {
				t.Fatal("compatibility retry leaked admission/account permits", state)
			}
		})
	}
}

func TestChannelPreflightDoesNotRepeatIdentityRecovery(t *testing.T) {
	const banner = "You are Claude Code, Anthropic's official CLI for Claude.\nAvailable skills: keep."
	const billing = "x-anthropic-billing-header: cc_version=1;\n"
	for _, profile := range []string{"cn-cli", "intl-work"} {
		for _, tc := range []struct {
			name, prefix       string
			disabled, metadata bool
			calls              int
		}{
			{name: "preflight", calls: 1},
			{name: "disabled", disabled: true, calls: 1},
			{name: "metadata retained", prefix: billing, calls: 1},
			{name: "distinct metadata recovery", prefix: billing, metadata: true, calls: 2},
		} {
			t.Run(profile+"/"+tc.name, func(t *testing.T) {
				o, account := newDNSFailoverOrch(t, &failingUpstream{})
				account.Profile = profile
				o.cfg.ChannelIdentityCompat = !tc.disabled
				o.cfg.ChannelMetadataCompat = tc.metadata
				o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
				o.wb.Catalog.Refresh()
				client := &channelRetryUpstream{body: `{"code":11128}`, always: true}
				o.wb.UpstreamClient = client
				body := o.enhanceBody(map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "system", "content": tc.prefix + banner}, map[string]any{"role": "user", "content": "hi"}}})
				_, err := o.openUpstreamScoped(context.Background(), account, body, "test-model", "", func(string) error { return nil }, nil, map[string]bool{account.UID: true})
				if err == nil || client.calls != tc.calls {
					t.Fatalf("duplicate or missing recovery: calls=%d want=%d err=%v", client.calls, tc.calls, err)
				}
				want := tc.prefix + "You are a coding assistant.\nAvailable skills: keep."
				if tc.disabled {
					want = tc.prefix + banner
				}
				if client.bodies[0]["messages"].([]any)[0].(map[string]any)["content"] != want {
					t.Fatal("first dispatch did not honor preflight configuration")
				}
			})
		}
	}
}

func TestChannelIdentityBannerMatchingIsExactAndNonMutating(t *testing.T) {
	const banner = "You are Claude Code, Anthropic's official CLI for Claude."
	for _, text := range []string{banner, banner + "\n### Available skills\nexample: docs", banner + "\r\nKeep CLAUDE.md"} {
		body := map[string]any{"messages": []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": text, "cache_control": map[string]any{"type": "ephemeral"}}, map[string]any{"type": "text", "text": "Keep all project rules"}}}}}
		before, _ := json.Marshal(body)
		result, ok := channelIdentityCompatBody(body)
		if !ok {
			t.Fatal("verified banner not recognized")
		}
		got := result["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if got[0].(map[string]any)["text"] != "You are a coding assistant."+text[len(banner):] || got[1].(map[string]any)["text"] != "Keep all project rules" || got[0].(map[string]any)["cache_control"] == nil {
			t.Fatal("content/metadata lost", got)
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("input mutated")
		}
	}
	for _, text := range []string{banner + " Do not edit files.", "Quote: " + banner, "You are Claude Code."} {
		if _, ok := channelIdentityCompatBody(map[string]any{"messages": []any{map[string]any{"role": "system", "content": text}}}); ok {
			t.Fatal("unknown template modified", text)
		}
	}
	if _, ok := channelIdentityCompatBody(map[string]any{"messages": []any{map[string]any{"role": "user", "content": banner}}}); ok {
		t.Fatal("user role rewritten")
	}
	const codex = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI."
	const suffix = " You are expected to be precise, safe, and helpful.\r\n<skills_instructions>Keep every skill description.</skills_instructions>\n<permissions instructions>Ask before destructive commands.</permissions instructions>"
	for _, intro := range []string{codex, "You are a CODING AGENT running in the Codex CLI. Codex CLI is an open source project led by OpenAI.", strings.ReplaceAll(codex, ". Codex", ".\r\nCodex")} {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": intro + suffix, "cache_control": map[string]any{"type": "ephemeral"}}}},
			map[string]any{"role": "user", "content": codex},
			map[string]any{"role": "tool", "content": codex, "tool_call_id": "call-test"},
		}}
		before, _ := json.Marshal(body)
		result, matched := channelIdentityCompatBody(body)
		if !matched {
			t.Fatal("known Codex intro not recognized", intro)
		}
		messages := result["messages"].([]any)
		block := messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		if block["text"] != "You are a coding assistant."+suffix || block["cache_control"] == nil || messages[1].(map[string]any)["content"] != codex || messages[2].(map[string]any)["content"] != codex {
			t.Fatal("Codex compatibility changed instructions or other roles")
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("Codex compatibility mutated original request")
		}
		if _, changed := channelIdentityCompatBody(result); changed {
			t.Fatal("Codex compatibility is not idempotent")
		}
	}
	for _, prompt := range []string{"Quote: " + codex, "You are Codex, a coding agent.", strings.Replace(codex, "open source", "proprietary", 1), codex + "example", "You are a coding agent running in the Codex CLI. Follow project instructions."} {
		if _, matched := channelIdentityCompatBody(map[string]any{"messages": []any{map[string]any{"role": "system", "content": prompt}}}); matched {
			t.Fatal("unknown or quoted Codex template rewritten", prompt)
		}
	}
}

func TestChannelBranchTemplateCompatibilityPreservesContext(t *testing.T) {
	const prefix = "You are ZCode, an interactive coding agent\r\nKeep the original authorization and safety policy.\r\n"
	const branch = "Main branch (you will usually use this for PRs): feature/测试"
	const tail = "\r\n<system-reminder>Available skills and MCP tools remain available.</system-reminder>"
	for _, blocks := range []bool{false, true} {
		var content any = prefix + branch + tail
		if blocks {
			content = []any{map[string]any{"type": "text", "text": content, "cache_control": map[string]any{"type": "ephemeral"}}}
		}
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": content},
			map[string]any{"role": "user", "content": branch},
			map[string]any{"role": "tool", "content": branch, "tool_call_id": "tool-1"},
		}}
		before, _ := json.Marshal(body)
		result, matched := channelIdentityCompatBody(body)
		if !matched {
			t.Fatal("known branch template not recognized")
		}
		messages := result["messages"].([]any)
		got := messages[0].(map[string]any)["content"]
		if blocks {
			block := got.([]any)[0].(map[string]any)
			if block["cache_control"] == nil {
				t.Fatal("cache metadata removed")
			}
			got = block["text"]
		}
		if got != prefix+strings.Replace(branch, "Main branch", "Default branch", 1)+tail || messages[1].(map[string]any)["content"] != branch || messages[2].(map[string]any)["content"] != branch {
			t.Fatal("branch compatibility changed other instructions or roles")
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("branch compatibility mutated input")
		}
		if _, changed := channelIdentityCompatBody(result); changed {
			t.Fatal("branch compatibility is not idempotent")
		}
	}
	for _, text := range []string{"Quote: " + branch, "> " + branch, "Main branch: main", "Main branch (you will usually use this for PRs elsewhere): main"} {
		if _, matched := channelIdentityCompatBody(map[string]any{"messages": []any{map[string]any{"role": "system", "content": text}}}); matched {
			t.Fatal("unknown/quoted branch template changed", text)
		}
	}
}

func TestChannelCompatibilityProgressionAndSessionMemory(t *testing.T) {
	const denial = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","displayMsg":{"en":"The request was blocked by security policy. Please retry later or contact support."}}`
	const banner = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	const tail = "\nAvailable skills: example\nDo not edit files without permission."
	makeBody := func(billing string) map[string]any {
		return map[string]any{"model": "test-model", "max_tokens": 50, "messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": billing}, map[string]any{"type": "text", "text": banner + tail, "cache_control": map[string]any{"type": "ephemeral"}}}},
			map[string]any{"role": "user", "content": "<system-reminder>Available skills: example</system-reminder>"},
		}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "Skill"}}}}
	}
	for _, profile := range []string{"cn-cli", "cn-work", "intl-cli", "intl-work"} {
		for _, metadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/metadata=%t", profile, metadata), func(t *testing.T) {
				client := &channelRetryUpstream{body: denial, denyCalls: 2}
				o, first := newDNSFailoverOrch(t, &failingUpstream{})
				o.wb.UpstreamClient = client
				first.Profile = profile
				o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
				o.wb.Catalog.Refresh()
				o.cfg.ChannelIdentityCompat = true
				o.cfg.ChannelMetadataCompat = metadata
				body := makeBody("x-anthropic-billing-header: cc_version=2.1.0; cch=aaa;")
				before, _ := json.Marshal(body)
				ctx, finish := o.sessions.Begin(context.Background(), "memory", "test-model", nil, body)
				defer finish()
				o.sessions.Bind("memory", first.UID)
				_, err := o.openUpstreamScoped(ctx, first, body, "test-model", "memory", func(string) error { return nil }, nil, map[string]bool{first.UID: true})
				if !metadata {
					if err == nil || client.calls != 2 || o.sessions.Lookup("memory") != "" {
						t.Fatal("disabled metadata retry or failed binding", client.calls, err)
					}
					return
				}
				if err != nil || client.calls != 3 {
					t.Fatal("progressive compatibility failed", client.calls, err)
				}
				transformed := client.bodies[2]["messages"].([]any)[0].(map[string]any)["content"].([]any)
				if transformed[0].(map[string]any)["text"] != "" || transformed[1].(map[string]any)["text"] != "You are a coding assistant."+tail {
					t.Fatal("capabilities lost", transformed)
				}
				after, _ := json.Marshal(body)
				if string(before) != string(after) {
					t.Fatal("original mutated")
				}
				body = makeBody("x-anthropic-billing-header: cc_version=2.1.0; cch=bbb;")
				fingerprint := compatibilityFingerprint(body)
				if o.sessions.Compatibility("memory", first.UID, fingerprint) != 2 {
					t.Fatal("memory missed changing attribution")
				}
				_, err = o.openUpstreamScoped(ctx, first, body, "test-model", "memory", func(string) error { return nil }, nil, map[string]bool{first.UID: true})
				if err != nil || client.calls != 4 || client.bodies[3]["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "" {
					t.Fatal("successful compatibility not reused", err)
				}
				body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "Other"}}}
				if o.sessions.Compatibility("memory", first.UID, compatibilityFingerprint(body)) != 0 || o.sessions.Compatibility("other", first.UID, fingerprint) != 0 || o.sessions.Compatibility("memory", "other", fingerprint) != 0 {
					t.Fatal("memory scope too broad")
				}
				o.sessions.UnbindMatching("memory", first.UID)
				if o.sessions.Compatibility("memory", first.UID, fingerprint) != 0 {
					t.Fatal("failed binding kept compatibility")
				}
			})
		}
	}
	t.Run("metadata retained across scoped account change", func(t *testing.T) {
		o, first := newDNSFailoverOrch(t, &failingUpstream{})
		mgr := o.wb.Managers[first.UID]
		o.wb.Pool = pool.New(map[string]pool.Credential{first.UID: mgr, "alternate": mgr}, "")
		first = o.wb.Pool.Get(first.UID)
		first.Profile = "intl-work"
		o.wb.Managers["alternate"] = mgr
		o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
		o.wb.Catalog.Refresh()
		o.cfg.ChannelIdentityCompat, o.cfg.ChannelMetadataCompat = true, true
		client := &channelRetryUpstream{body: denial, denyCalls: 3}
		o.wb.UpstreamClient = client
		body := makeBody("x-anthropic-billing-header: cc_version=2.1.0; cch=abc;")
		before, _ := json.Marshal(body)
		ctx, finish := o.sessions.Begin(context.Background(), "scoped-memory", "test-model", nil, body)
		defer finish()
		o.sessions.Bind("scoped-memory", first.UID)
		served, err := o.openUpstreamScoped(ctx, first, body, "test-model", "scoped-memory", func(string) error { return nil }, nil, map[string]bool{first.UID: true, "alternate": true})
		if err != nil || client.calls != 4 || served.UID != "alternate" {
			t.Fatal("scoped compatibility did not reach authorized alternative", client.calls, err)
		}
		third, _ := json.Marshal(client.bodies[2])
		fourth, _ := json.Marshal(client.bodies[3])
		after, _ := json.Marshal(body)
		if string(third) != string(fourth) || string(before) != string(after) || o.sessions.Compatibility("scoped-memory", served.UID, compatibilityFingerprint(body)) != 2 {
			t.Fatal("metadata compatibility reset, original mutated or successful memory lost")
		}
	})
}

func TestUsageDiagnosticsPreservesRequestAndUpstreamFacts(t *testing.T) {
	req := withRequestSessionIdentity(httptest.NewRequest("POST", "/v1/messages", nil), map[string]any{"max_tokens": 1200})
	d := diagnostic(req.Context())
	d.EffectiveLimits = outputLimits(map[string]any{"max_tokens": 1000})
	d.Observe(`data: {"choices":[{"finish_reason":"length"}]}`)
	d.Observe(`data: {"choices":[{"finish_reason":null}],"usage":{"completion_tokens":1000}}`)
	facts := usageDiagnostics(req.Context())
	if facts.RequestedLimits["max_tokens"] != 1200 || facts.EffectiveLimits["max_tokens"] != 1000 || facts.FinishReason != "length" || !facts.Started {
		t.Fatal(facts)
	}
}

func TestChannelCompatibilityAfterAnthropicConversion(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=2.1.0; cch=abc;"
	const rules = "\nAvailable skills: example\nUse MCP only with permission."
	for _, identity := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
		"You are an agent for Claude Code, Anthropic's official CLI for Claude. Given the user's message, use the permitted tools.",
	} {
		original := map[string]any{"model": "test-model", "max_tokens": 50, "system": []any{map[string]any{"type": "text", "text": billing}, map[string]any{"type": "text", "text": identity + rules}}, "messages": []any{map[string]any{"role": "user", "content": "<system-reminder>Available skills: example</system-reminder>"}}, "tools": []any{map[string]any{"name": "Skill", "description": "Invoke example", "input_schema": map[string]any{"type": "object"}}}}
		chat, err := adapters.AnthropicRequestToChat(original)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(chat)
		first, ok := channelCompatibilityBody(chat, 1)
		if !ok {
			t.Fatal("native identity missed", identity)
		}
		text := first["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(text, billing+"\nYou are a coding assistant.") || !strings.HasSuffix(text, rules) {
			t.Fatal("native capabilities changed", text)
		}
		if strings.Contains(identity, "Given the user's message,") && !strings.Contains(text, "Given the user's message, use the permitted tools.") {
			t.Fatal("subagent instruction lost")
		}
		second, ok := channelCompatibilityBody(chat, 2)
		if !ok {
			t.Fatal("native attribution missed")
		}
		if second["messages"].([]any)[0].(map[string]any)["content"] != strings.TrimPrefix(text, billing+"\n") {
			t.Fatal("unexpected metadata transformation")
		}
		after, _ := json.Marshal(chat)
		if string(before) != string(after) {
			t.Fatal("native request mutated")
		}
	}
}
func TestChannelDenialRotatesWithinScopeAndDoesNotReplayOutput(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		scoped, partial, exhausted, content bool
		wantCalls                           int
	}{
		{name: "private", wantCalls: 2}, {name: "authorized scope", scoped: true, wantCalls: 2}, {name: "scope excludes alternative", scoped: true, exhausted: true, wantCalls: 1}, {name: "partial output", partial: true, wantCalls: 1}, {name: "content policy", content: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, first := newDNSFailoverOrch(t, &failingUpstream{})
			mgr := o.wb.Managers[first.UID]
			o.wb.Pool = pool.New(map[string]pool.Credential{first.UID: mgr, "alternate": mgr}, "")
			first = o.wb.Pool.Get(first.UID)
			o.wb.Managers["alternate"] = mgr
			o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.wb.Catalog.Refresh()
			client := &channelRetryUpstream{partial: tc.partial, body: `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`}
			if tc.content {
				client.body = `{"msg":"blocked by security policy"}`
			}
			o.wb.UpstreamClient = client
			var scope map[string]bool
			if tc.scoped {
				scope = map[string]bool{first.UID: true}
				if !tc.exhausted {
					scope["alternate"] = true
				}
			}
			o.sessions.Bind("sticky", first.UID)
			served, err := o.openUpstreamScoped(context.Background(), first, map[string]any{}, "test-model", "sticky", func(string) error { return nil }, nil, scope)
			if client.calls != tc.wantCalls || (err == nil) != (tc.wantCalls == 2) {
				t.Fatal(client.calls, err)
			}
			if tc.wantCalls == 2 && (served.UID != "alternate" || o.sessions.Lookup("sticky") != "alternate") {
				t.Fatal("fallback did not bind alternate", served.UID)
			}
			if !tc.partial && !tc.content {
				if o.modelCooldownUntil(first.UID, "test-model") > nowSec() || o.modelCooldownUntil(first.UID, "other-model") > nowSec() {
					t.Fatal("cooldown scope incorrect")
				}
				if !o.wb.Pool.Get(first.UID).Enabled || o.wb.Pool.Get(first.UID).CooldownUntil > nowSec() {
					t.Fatal("account unnecessarily disabled/cooled")
				}
			}
			if (tc.exhausted || tc.partial) && o.sessions.Lookup("sticky") != "" {
				t.Fatal("failed sticky binding survived exhausted scope")
			}
			if tc.content && o.modelCooldownUntil(first.UID, "test-model") > nowSec() {
				t.Fatal("content rejection penalized account")
			}
		})
	}
}

func TestRegionBiasPreservesExistingInternationalSession(t *testing.T) {
	o, first := newDNSFailoverOrch(t, &failingUpstream{})
	mgr := o.wb.Managers[first.UID]
	o.wb.Pool = pool.New(map[string]pool.Credential{first.UID: mgr, "international": mgr}, "")
	o.wb.Managers["international"] = mgr
	o.wb.Pool.Get(first.UID).Profile = "cn-cli"
	o.wb.Pool.Get("international").Profile = "intl-cli"
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	o.sessions.Bind("existing-session", "international")
	selected, e := o.pickAccount("test-model", "existing-session")
	if e != nil || selected == nil || selected.UID != "international" {
		t.Fatal("healthy sticky international session replaced", selected, e)
	}
}

// This uses the real upstream HTTP parser, rather than only testing a converter.
func TestWorkBuddyResponseIntegrity(t *testing.T) {
	tool := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\""}}]}}]}`
	tail := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":null,"function":{"name":null,"arguments":"ok\"}"}}]},"finish_reason":"tool_calls"}]}`
	content := `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`
	reason := `{"choices":[{"index":0,"delta":{"reasoning_content":"thinking"},"finish_reason":"stop"}]}`
	cases := []struct {
		name, wire, mime string
		bad              bool
	}{
		{"complete tools", "data: " + tool + "\n\ndata: " + tail + "\n\ndata: [DONE]\n\n", "text/event-stream", false},
		{"missing tool finish compatibility", "data: " + strings.Replace(tool, `{\"q\":\"`, `{}`, 1) + "\n\ndata: [DONE]\n\n", "text/event-stream", false},
		{"truncated tools", "data: " + tool + "\n\ndata: [DONE]\n\n", "text/event-stream", true},
		{"bad tool finish", "data: " + tool + "\n\ndata: " + strings.Replace(tail, "tool_calls\"}]}", "stop\"}]}", 1) + "\n\ndata: [DONE]\n\n", "text/event-stream", true},
		{"changed id", "data: " + tool + "\n\ndata: " + strings.Replace(tail, `"id":null`, `"id":"call-2"`, 1) + "\n\ndata: [DONE]\n\n", "text/event-stream", true},
		{"real truncation", "data: " + tool + "\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "text/event-stream", false},
		{"reasoning only SSE", "data: " + reason + "\n\ndata: [DONE]\n\n", "text/event-stream", false},
		{"reasoning only JSON", reason, "application/json", false},
		{"JSON message tools", `{"choices":[{"message":{"tool_calls":[{"id":"call-json","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, "application/json", false},
		{"empty JSON tool call", `{"choices":[{"message":{"tool_calls":[{"id":"call-json","function":{"name":"lookup","arguments":""}}]}}]}`, "application/json", true},
		{"terminal usage", "data: " + content + "\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\ndata: [DONE]\n\n", "text/event-stream", false},
		{"complete finish EOF", "data: " + content, "text/event-stream", false},
		{"UTF-8 BOM SSE", "\ufeffdata: " + content + "\r\n\r\ndata: [DONE]\r\n\r\n", "text/event-stream", false},
		{"DONE without finish", `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\ndata: [DONE]\n\n", "text/event-stream", true},
		{"multi-line CR events", "data: {\rdata: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\r\rdata: [DONE]\r\r", "text/event-stream", false},
		{"output after finish", "data: " + content + "\n\ndata: " + content + "\n\ndata: [DONE]\n\n", "text/event-stream", true},
		{"fractional index", "data: " + strings.Replace(content, `"index":0`, `"index":0.5`, 1) + "\n\ndata: [DONE]\n\n", "text/event-stream", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.mime)
				_, _ = w.Write([]byte(tc.wire))
			}))
			defer srv.Close()
			var lines []string
			err := upstream.Shared().StreamUpstream(context.Background(), nil, map[string]any{}, srv.URL, func(line string) error { lines = append(lines, line); return nil })
			if (err != nil) != tc.bad {
				t.Fatalf("bad=%v err=%v lines=%v", tc.bad, err, lines)
			}
			if tc.bad {
				for _, line := range lines {
					if strings.Contains(line, `"finish_reason"`) || strings.Contains(line, "[DONE]") {
						t.Fatal("invalid response published successful terminal", line)
					}
				}
			}
			if !tc.bad && (len(lines) == 0 || lines[len(lines)-1] != "data: [DONE]") {
				t.Fatal("missing DONE", lines)
			}
			if tc.name == "terminal usage" && !strings.Contains(strings.Join(lines, "\n"), `"cached_tokens":3`) {
				t.Fatal("terminal cache usage lost", lines)
			}
		})
	}
}

type preambleFailureUpstream struct {
	calls   int
	content bool
}

func (f *preambleFailureUpstream) StreamUpstream(_ context.Context, _ map[string]string, _ map[string]any, _ string, yield upstream.LineFunc) error {
	f.calls++
	if err := yield(`data: {"choices":[{"delta":{"role":"assistant","function_call":{"name":"","arguments":""}}}]}`); err != nil {
		return err
	}
	if f.calls == 1 {
		if f.content {
			if err := yield(`data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`); err != nil {
				return err
			}
		}
		return &upstream.UpstreamError{StatusCode: 503, Raw: []byte(`{"error":{"message":"temporary"}}`)}
	}
	if err := yield(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`); err != nil {
		return err
	}
	return yield("data: [DONE]")
}
func TestWorkBuddyPreambleFailover(t *testing.T) {
	for _, content := range []bool{false, true} {
		t.Run(fmt.Sprint(content), func(t *testing.T) {
			o, first := newDNSFailoverOrch(t, &failingUpstream{})
			mgr := o.wb.Managers[first.UID]
			o.wb.Pool = pool.New(map[string]pool.Credential{first.UID: mgr, "alternate": mgr}, "")
			first = o.wb.Pool.Get(first.UID)
			o.wb.Managers["alternate"] = mgr
			o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.wb.Catalog.Refresh()
			client := &preambleFailureUpstream{content: content}
			o.wb.UpstreamClient = client
			var lines []string
			_, err := o.openUpstream(context.Background(), first, map[string]any{"model": "test-model"}, "test-model", "", func(line string) error { lines = append(lines, line); return nil }, nil)
			if content {
				if err == nil || client.calls != 1 {
					t.Fatal("replayed thinking", client.calls, err)
				}
			} else {
				if err != nil || client.calls != 2 || len(lines) != 3 {
					t.Fatal("preamble blocked fallback or leaked", client.calls, err, lines)
				}
			}
		})
	}
}
func TestWorkBuddySSEMetadataRetention(t *testing.T) {
	role := false
	first := sanitizeChatSSEWithRole(`data: {"choices":[{"delta":{"role":"assistant"}}]}`, &role)
	if first == "" {
		t.Fatal("first role lost")
	}
	if got := sanitizeChatSSEWithRole(`data: {"choices":[{"delta":{"role":"assistant","function_call":{"name":"","arguments":""}}}]}`, &role); got != "" {
		t.Fatal("duplicate dummy preamble retained", got)
	}
	got := sanitizeChatSSEWithRole(`data: {"choices":[{"delta":{"content":"","tool_calls":[]}}],"usage":{"prompt_tokens":7}}`, &role)
	if !strings.Contains(got, `"prompt_tokens":7`) || !strings.Contains(got, `"choices":[]`) {
		t.Fatal("usage dropped", got)
	}
	if err := upstream.ValidateChoiceCount(map[string]any{"n": 1.5}); err == nil {
		t.Fatal("fractional n accepted")
	}
	for _, body := range []map[string]any{{}, {"n": 1}, {"n": float64(1)}} {
		if err := upstream.ValidateChoiceCount(body); err != nil {
			t.Fatal("normal choice count rejected", err)
		}
	}
}

func TestResponsesFallbackReusesSuccessfulCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, banner, want string }{
		{"identity", "You are Claude Code, Anthropic's official CLI for Claude.", "You are a coding assistant."},
		{"file-reference", "Tokens prefixed with @ are paths the user explicitly referenced.", "Tokens prefixed with @ are paths that the user explicitly referenced."},
	} {
		t.Run(tc.name, func(t *testing.T) { checkResponsesCompatibilityReuse(t, tc.banner, tc.want) })
	}
}
func checkResponsesCompatibilityReuse(t *testing.T, banner, wantPrompt string) {
	o, account := newDNSFailoverOrch(t, &failingUpstream{})
	client := &channelRetryUpstream{body: `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`}
	o.wb.UpstreamClient = client
	o.cfg.ChannelIdentityCompat = true
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	principal := &Principal{AppID: 17, EffectiveModel: "test-model"}
	server := &Server{o: o}
	raw := map[string]any{"model": "test-model", "instructions": banner, "input": []any{map[string]any{"role": "user", "content": "stable first turn"}}}
	chat := map[string]any{"model": "test-model", "max_tokens": 50, "messages": []any{map[string]any{"role": "system", "content": banner}, map[string]any{"role": "user", "content": "stable first turn"}}}
	previous := 0
	for turn, want := range []int{2, 1} {
		request := httptest.NewRequest("POST", "/v1/responses", nil)
		prepared, finish, err := server.prepareProviderRequest(request, raw, principal)
		if err != nil {
			t.Fatal(err)
		}
		key := requestSessionKey(prepared, principal, "test-model", nil)
		if key == "" {
			t.Fatal("missing fallback identity")
		}
		_, err = o.openUpstreamScoped(prepared.Context(), account, chat, "test-model", key, func(string) error { return nil }, nil, map[string]bool{account.UID: true})
		finish()
		if err != nil || client.calls-previous != want {
			t.Fatalf("turn %d attempts=%d want=%d err=%v", turn, client.calls-previous, want, err)
		}
		previous = client.calls
		raw["input"] = []any{map[string]any{"role": "user", "content": "stable first turn"}, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "next turn"}}
	}
	if client.bodies[2]["messages"].([]any)[0].(map[string]any)["content"] != wantPrompt {
		t.Fatal("second turn resent rejected template")
	}
}

func TestScopedChannelDenialKeepsSafeClassification(t *testing.T) {
	o, account := newDNSFailoverOrch(t, &failingUpstream{})
	client := &channelRetryUpstream{body: `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"private-upstream-marker"}`, always: true}
	o.wb.UpstreamClient = client
	o.cfg.ChannelIdentityCompat = true
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	body := map[string]any{"model": "test-model", "messages": []any{map[string]any{"role": "system", "content": "You are Claude Code, Anthropic's official CLI for Claude."}, map[string]any{"role": "user", "content": "synthetic"}}}
	_, err := o.openUpstreamScoped(context.Background(), account, body, "test-model", "", func(string) error { return nil }, nil, map[string]bool{account.UID: true})
	remote, ok := err.(*upstream.UpstreamError)
	if !ok || remote.StatusCode != 400 || !strings.Contains(string(remote.Raw), "11128") || !strings.Contains(string(remote.Raw), "channel_rejected") || strings.Contains(string(remote.Raw), "private-upstream-marker") {
		t.Fatalf("lost safe refusal classification: %v", err)
	}
	if client.calls > 5 {
		t.Fatal("recovery exceeded existing budget")
	}
}
