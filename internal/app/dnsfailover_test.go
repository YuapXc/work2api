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
	"testing"
	"time"
	"work2api/internal/config"
	"work2api/internal/store"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

type failingUpstream struct {
	err   error
	calls int
	emit  bool
}

func (f *failingUpstream) StreamUpstream(_ context.Context, _ map[string]string, _ map[string]any, target string, yield upstream.LineFunc) error {
	if target == "" {
		return errors.New("test account has no upstream URL")
	}
	f.calls++
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
	o := &Orchestrator{db: db, cfg: &config.Config{MaxConcurrentRequests: 4, PortalUserConcurrency: 2}, pool: p, models: models.New(p, db), managers: map[string]*credentials.Manager{uid: mgr}, modelCooldowns: map[string]cdEntry{}, sessions: newSessionRouter(), upstreamClient: client}
	acc := p.Get(uid)
	if acc == nil || acc.Profile == "" {
		t.Fatal("fixture must contain a routable pooled account")
	}
	return o, acc
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
			got := o.pool.Get(acc.UID)
			if got == nil {
				t.Fatal("pooled account disappeared")
			}
			if (got.CooldownUntil > 0) != tc.wantFailure || (got.FailureCount > 0) != tc.wantFailure {
				t.Fatalf("unexpected account penalty: cooldown=%v failures=%d", got.CooldownUntil, got.FailureCount)
			}
		})
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
	mgr := o.managers[first.UID]
	o.pool = pool.New(map[string]pool.Credential{first.UID: mgr, "alternate": mgr}, "")
	first = o.pool.Get(first.UID)
	first.Profile = "cn-cli"
	o.pool.Get("alternate").Profile = "intl-cli"
	o.managers["alternate"] = mgr
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.models.Refresh()
	client := &regionRetryUpstream{}
	o.upstreamClient = client
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
}

func (f *channelRetryUpstream) StreamUpstream(_ context.Context, _ map[string]string, request map[string]any, _ string, yield upstream.LineFunc) error {
	f.calls++
	f.bodies = append(f.bodies, request)
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
	const tail = "\n### Available skills\nfind-skills: skill://find-skills/SKILL.md\nAvailable agent types for the Agent tool:\nreviewer: read CLAUDE.md\n## MCP Server Instructions\nUse MCP tools only with permission."
	const denial = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`
	for _, tc := range []struct {
		name                              string
		profile, prompt, errorBody        string
		disabled, partial, always, revoke bool
		wantCalls                         int
		wantCompat                        bool
	}{
		{name: "domestic", profile: "cn-cli", prompt: banner + tail, errorBody: denial, wantCalls: 2, wantCompat: true},
		{name: "disabled", profile: "cn-cli", prompt: banner + tail, errorBody: denial, disabled: true, wantCalls: 2},
		{name: "international", profile: "intl-cli", prompt: banner + tail, errorBody: denial, wantCalls: 2},
		{name: "unknown banner", profile: "cn-cli", prompt: "You are Claude Code." + tail, errorBody: denial, wantCalls: 2},
		{name: "embedded banner", profile: "cn-cli", prompt: "Project note: " + banner + tail, errorBody: denial, wantCalls: 2},
		{name: "content policy", profile: "cn-cli", prompt: banner + tail, errorBody: `{"code":11128,"msg":"blocked by security policy"}`, wantCalls: 1},
		{name: "role validation", profile: "cn-cli", prompt: banner + tail, errorBody: `{"code":11128,"msg":"first message is not system prompt"}`, wantCalls: 2},
		{name: "partial output", profile: "cn-cli", prompt: banner + tail, errorBody: denial, partial: true, wantCalls: 1},
		{name: "persistent denial", profile: "cn-cli", prompt: banner + tail, errorBody: denial, always: true, wantCalls: 5, wantCompat: true},
		{name: "revoked during retry", profile: "cn-cli", prompt: banner + tail, errorBody: denial, revoke: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, first := newDNSFailoverOrch(t, &failingUpstream{})
			mgr := o.managers[first.UID]
			managers := map[string]pool.Credential{first.UID: mgr}
			for i := 0; i < 6; i++ {
				uid := fmt.Sprintf("alternate-%d", i)
				managers[uid] = mgr
				o.managers[uid] = mgr
			}
			o.pool = pool.New(managers, "")
			first = o.pool.Get(first.UID)
			first.Profile = tc.profile
			for _, a := range o.pool.Accounts() {
				if a.UID != first.UID {
					a.Profile = "intl-cli"
				}
			}
			o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.models.Refresh()
			o.cfg.ChannelIdentityCompat = !tc.disabled
			client := &channelRetryUpstream{body: tc.errorBody, partial: tc.partial, always: tc.always}
			o.upstreamClient = client
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
			o.sessions.bind("sticky", first.UID)
			served, err := o.openUpstream(ctx, first, body, "test-model", "sticky", func(string) error { return nil }, nil)
			if client.calls != tc.wantCalls {
				t.Fatalf("calls=%d, want %d, error=%v", client.calls, tc.wantCalls, err)
			}
			if tc.name == "domestic" && (err != nil || served.UID != first.UID || o.modelCooldownUntil(first.UID, "test-model") > nowSec() || o.sessions.lookup("sticky") != first.UID) {
				t.Fatal("successful banner compatibility penalized/rotated account", err)
			}
			for i, attempt := range client.bodies {
				expected := tc.prompt
				if tc.wantCompat && i == 1 {
					expected = "You are a coding assistant." + tail
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
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata=%t", metadata), func(t *testing.T) {
			client := &channelRetryUpstream{body: denial, denyCalls: 2}
			o, first := newDNSFailoverOrch(t, &failingUpstream{})
			o.upstreamClient = client
			first.Profile = "cn-cli"
			o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.models.Refresh()
			o.cfg.ChannelIdentityCompat = true
			o.cfg.ChannelMetadataCompat = metadata
			body := makeBody("x-anthropic-billing-header: cc_version=2.1.0; cch=aaa;")
			before, _ := json.Marshal(body)
			ctx, finish := o.sessions.begin(context.Background(), "memory", "test-model", nil, body)
			defer finish()
			o.sessions.bind("memory", first.UID)
			_, err := o.openUpstreamScoped(ctx, first, body, "test-model", "memory", func(string) error { return nil }, nil, map[string]bool{first.UID: true})
			if !metadata {
				if err == nil || client.calls != 2 || o.sessions.lookup("memory") != "" {
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
			if o.sessions.compatibility("memory", first.UID, fingerprint) != 2 {
				t.Fatal("memory missed changing attribution")
			}
			_, err = o.openUpstreamScoped(ctx, first, body, "test-model", "memory", func(string) error { return nil }, nil, map[string]bool{first.UID: true})
			if err != nil || client.calls != 4 || client.bodies[3]["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "" {
				t.Fatal("successful compatibility not reused", err)
			}
			body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "Other"}}}
			if o.sessions.compatibility("memory", first.UID, compatibilityFingerprint(body)) != 0 || o.sessions.compatibility("other", first.UID, fingerprint) != 0 || o.sessions.compatibility("memory", "other", fingerprint) != 0 {
				t.Fatal("memory scope too broad")
			}
			o.sessions.unbindMatching("memory", first.UID)
			if o.sessions.compatibility("memory", first.UID, fingerprint) != 0 {
				t.Fatal("failed binding kept compatibility")
			}
		})
	}
}

func TestUsageDiagnosticsPreservesRequestAndUpstreamFacts(t *testing.T) {
	req := withRequestSessionIdentity(httptest.NewRequest("POST", "/v1/messages", nil), map[string]any{"max_tokens": 1200})
	d := diagnostic(req.Context())
	d.EffectiveLimits = outputLimits(map[string]any{"max_tokens": 1000})
	d.observe(`data: {"choices":[{"finish_reason":"length"}]}`)
	d.observe(`data: {"choices":[{"finish_reason":null}],"usage":{"completion_tokens":1000}}`)
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
			mgr := o.managers[first.UID]
			o.pool = pool.New(map[string]pool.Credential{first.UID: mgr, "alternate": mgr}, "")
			first = o.pool.Get(first.UID)
			o.managers["alternate"] = mgr
			o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.models.Refresh()
			client := &channelRetryUpstream{partial: tc.partial, body: `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`}
			if tc.content {
				client.body = `{"msg":"blocked by security policy"}`
			}
			o.upstreamClient = client
			var scope map[string]bool
			if tc.scoped {
				scope = map[string]bool{first.UID: true}
				if !tc.exhausted {
					scope["alternate"] = true
				}
			}
			o.sessions.bind("sticky", first.UID)
			served, err := o.openUpstreamScoped(context.Background(), first, map[string]any{}, "test-model", "sticky", func(string) error { return nil }, nil, scope)
			if client.calls != tc.wantCalls || (err == nil) != (tc.wantCalls == 2) {
				t.Fatal(client.calls, err)
			}
			if tc.wantCalls == 2 && (served.UID != "alternate" || o.sessions.lookup("sticky") != "alternate") {
				t.Fatal("fallback did not bind alternate", served.UID)
			}
			if !tc.partial && !tc.content {
				if o.modelCooldownUntil(first.UID, "test-model") > nowSec() || o.modelCooldownUntil(first.UID, "other-model") > nowSec() {
					t.Fatal("cooldown scope incorrect")
				}
				if !o.pool.Get(first.UID).Enabled || o.pool.Get(first.UID).CooldownUntil > nowSec() {
					t.Fatal("account unnecessarily disabled/cooled")
				}
			}
			if (tc.exhausted || tc.partial) && o.sessions.lookup("sticky") != "" {
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
	mgr := o.managers[first.UID]
	o.pool = pool.New(map[string]pool.Credential{first.UID: mgr, "international": mgr}, "")
	o.managers["international"] = mgr
	o.pool.Get(first.UID).Profile = "cn-cli"
	o.pool.Get("international").Profile = "intl-cli"
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.models.Refresh()
	o.sessions.bind("existing-session", "international")
	selected, e := o.pickAccount("test-model", "existing-session")
	if e != nil || selected == nil || selected.UID != "international" {
		t.Fatal("healthy sticky international session replaced", selected, e)
	}
}
