package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"work2api/internal/toolschema"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/reasoning"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

func TestLiveDomesticSchemaToolRoundTrip(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_CN_SCHEMA") != "1" {
		t.Skip("explicit domestic synthetic tool verification only")
	}
	var mgr *credentials.Manager
	for _, file := range append(credentials.FindAuthFiles(filepath.Join("..", "..", "..", "auths"), ""), credentials.FindAuthFiles("", "")...) {
		candidate := credentials.NewManager(file)
		if siterouting.ProfileSite(candidate.Profile()) != siterouting.Domestic {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		_, err := candidate.GetChatHeadersContext(ctx)
		cancel()
		if err == nil {
			mgr = candidate
			break
		}
	}
	if mgr == nil {
		t.Fatal("domestic credential unavailable")
	}
	var tools any
	json.Unmarshal([]byte(`[{"type":"function","function":{"name":"echo_check","description":"Echo a synthetic value.","parameters":{"type":"object","required":["value"],"properties":{"value":{"enum":["ok",1]},"unused":false},"additionalProperties":false}}}]`), &tools)
	body := reasoning.Sanitize(upstream.BuildUpstreamBody(map[string]any{"model": "deepseek-v4.1-flash", "max_tokens": 128, "thinking": map[string]any{"type": "disabled"}, "tools": toolschema.WorkBuddyTools(tools), "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "echo_check"}}, "messages": []any{map[string]any{"role": "system", "content": "Use echo_check once with value ok; after its result, reply exactly OK."}, map[string]any{"role": "user", "content": "Run the synthetic echo check."}}}), nil)
	endpoint, _ := siterouting.ChatURLForProfile(mgr.Profile())
	var message map[string]any
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		headers, err := mgr.GetChatHeadersContext(ctx)
		if err != nil {
			cancel()
			t.Fatal("credential preparation failed")
		}
		start := time.Now()
		reply, err := upstream.CollectStream("deepseek-v4.1-flash", func(yield upstream.LineFunc) error {
			return upstream.Shared().StreamUpstream(ctx, headers, body, endpoint, yield)
		})
		cancel()
		if err != nil {
			t.Fatalf("domestic synthetic tool round %d failed: %T", i+1, err)
		}
		choices, _ := reply["choices"].([]any)
		if len(choices) != 1 {
			t.Fatal("missing response")
		}
		choice := choices[0].(map[string]any)
		message, _ = choice["message"].(map[string]any)
		if i == 0 {
			calls, _ := message["tool_calls"].([]any)
			if len(calls) != 1 {
				t.Fatal("missing tool call")
			}
			call := calls[0].(map[string]any)
			fn := call["function"].(map[string]any)
			var args map[string]any
			if fn["name"] != "echo_check" || json.Unmarshal([]byte(ToStr(fn["arguments"])), &args) != nil || args["value"] != "ok" {
				t.Fatal("tool contract failed")
			}
			body["messages"] = append(body["messages"].([]any), message, map[string]any{"role": "tool", "tool_call_id": call["id"], "content": "ok"})
			delete(body, "tool_choice")
		} else if choice["finish_reason"] != "stop" || strings.TrimSpace(ToStr(message["content"])) != "OK" {
			t.Fatal("tool result round did not complete")
		}
		t.Logf("domestic_schema_tool round=%d completed=true elapsed_ms=%d", i+1, time.Since(start).Milliseconds())
	}
}

func TestLiveDomesticConversationGrouping(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_CN_GROUPING") != "1" {
		t.Skip("explicit domestic synthetic verification only")
	}
	var mgr *credentials.Manager
	for _, file := range append(credentials.FindAuthFiles(filepath.Join("..", "..", "..", "auths"), ""), credentials.FindAuthFiles("", "")...) {
		candidate := credentials.NewManager(file)
		if siterouting.ProfileSite(candidate.Profile()) != siterouting.Domestic {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		_, err := candidate.GetChatHeadersContext(ctx)
		cancel()
		if err == nil {
			mgr = candidate
			break
		}
	}
	if mgr == nil {
		t.Fatal("domestic credential unavailable")
	}
	body := reasoning.Sanitize(upstream.BuildUpstreamBody(map[string]any{"model": "deepseek-v4.1-flash", "max_tokens": 32, "thinking": map[string]any{"type": "disabled"}, "messages": []any{map[string]any{"role": "system", "content": "Answer briefly."}, map[string]any{"role": "user", "content": "Reply exactly OK."}}}), nil)
	request := (&http.Request{Header: http.Header{"X-Claude-Code-Session-Id": []string{"synthetic-grouping-check"}}}).WithContext(t.Context())
	request = WithRequestSessionIdentity(request, body)
	url, _ := siterouting.ChatURLForProfile(mgr.Profile())
	var conversation, turn, physical string
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
		headers, err := mgr.GetChatHeadersContext(ctx)
		if err != nil {
			cancel()
			t.Fatal("credential preparation failed")
		}
		applyUpstreamConversation(ctx, &pool.Account{UID: "synthetic", Profile: mgr.Profile()}, headers, "deepseek-v4.1-flash")
		if i > 0 && (conversation != headers["X-Conversation-ID"] || turn != headers["X-Conversation-Request-ID"] || physical == headers["X-Request-ID"]) {
			cancel()
			t.Fatal("conversation/turn grouping or physical request uniqueness failed")
		}
		conversation, turn, physical = headers["X-Conversation-ID"], headers["X-Conversation-Request-ID"], headers["X-Request-ID"]
		start := time.Now()
		reply, err := upstream.CollectStream("deepseek-v4.1-flash", func(yield upstream.LineFunc) error {
			return upstream.Shared().StreamUpstream(ctx, headers, body, url, yield)
		})
		cancel()
		if err != nil {
			t.Fatalf("domestic grouping request %d did not complete: %T", i+1, err)
		}
		choices, _ := reply["choices"].([]any)
		if len(choices) != 1 {
			t.Fatal("missing response")
		}
		choice := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if choice["finish_reason"] != "stop" || strings.TrimSpace(ToStr(message["content"])) != "OK" {
			t.Fatal("incomplete synthetic response")
		}
		t.Logf("domestic_grouping request=%d completed=true elapsed_ms=%d", i+1, time.Since(start).Milliseconds())
	}
}

func TestUpstreamConversationAndTurnIsolation(t *testing.T) {
	makeBody := func(messages []any) map[string]any { return map[string]any{"messages": messages} }
	user := map[string]any{"role": "user", "content": "same question"}
	base := makeBody([]any{user})
	loop := makeBody([]any{user, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}}, map[string]any{"role": "tool", "content": "output"}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "ignored"}, map[string]any{"type": "text", "text": "<system-reminder>notice</system-reminder>"}}}})
	if requestTurnFingerprint(base) != requestTurnFingerprint(loop) {
		t.Fatal("tool round started another user turn")
	}
	loop["messages"] = append(loop["messages"].([]any), user)
	if requestTurnFingerprint(base) == requestTurnFingerprint(loop) {
		t.Fatal("repeated user question collapsed")
	}
	stamp := func(body map[string]any, scope, agent, uid string) map[string]string {
		r := &http.Request{Header: http.Header{}}
		r.Header.Set("X-Claude-Code-Session-Id", "session")
		r.Header.Set("Authorization", scope)
		r.Header.Set("X-Claude-Code-Agent-Id", agent)
		r = WithRequestSessionIdentity(r.WithContext(context.Background()), body)
		headers := map[string]string{"X-Request-ID": "physical-request", "X-Conversation-Message-ID": "physical-request", "X-Conversation-Request-ID": "random", "X-Conversation-ID": "random"}
		applyUpstreamConversation(r.Context(), &pool.Account{UID: uid, Profile: "cn-cli"}, headers, "deepseek-v4.1-flash")
		return headers
	}
	a, b := stamp(base, "key-a", "", "account-a"), stamp(base, "key-a", "", "account-a")
	if !reflect.DeepEqual(a, b) || a["X-Conversation-ID"] == "random" || a["X-Request-ID"] != "physical-request" || a["X-Conversation-Request-ID"] == "physical-request" {
		t.Fatal("identity grouping or per-request identifiers lost", a, b)
	}
	next := stamp(loop, "key-a", "", "account-a")
	if a["X-Conversation-ID"] != next["X-Conversation-ID"] || a["X-Conversation-Request-ID"] == next["X-Conversation-Request-ID"] {
		t.Fatal("conversation and turn confused")
	}
	for _, other := range []map[string]string{stamp(base, "key-b", "", "account-a"), stamp(base, "key-a", "subagent", "account-a"), stamp(base, "key-a", "", "account-b")} {
		if a["X-Conversation-ID"] == other["X-Conversation-ID"] || a["X-Conversation-Request-ID"] == other["X-Conversation-Request-ID"] {
			t.Fatal("identity crossed principal, subagent or account")
		}
	}
	image := func(data string) map[string]any {
		return makeBody([]any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]any{"data": data}}}}})
	}
	if requestTurnFingerprint(image("one")) == requestTurnFingerprint(image("two")) {
		t.Fatal("image-only turns collapsed")
	}
}

func TestHarnessIdentityCompatibilityIsNarrow(t *testing.T) {
	const intro = "You are an AI agent powered by DeepSeek Harness."
	for _, tail := range []string{"", "\nKeep all skills and permission rules.", " Use the read tool.", "\r\nContinue."} {
		got, changed := CompatibleIdentity(intro + tail)
		if !changed || got != "You are a coding assistant."+tail {
			t.Fatal("lost the instructions after the known opening")
		}
	}
	for _, text := range []string{
		"Quoted: " + intro,
		intro + "example",
		"You are a coding agent powered by the deepseek-v4.1-flash model.",
		"You are an AI assistant built on DeepSeek Harness.",
		"You are an AI agent powered by another application.",
	} {
		got, changed := CompatibleIdentity(text)
		if changed || got != text {
			t.Fatal("rewrote an unverified declaration or quotation")
		}
	}
}

func TestKnownTemplatesSharePreflightAndRecovery(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=1;\n"
	const tail = "\nAvailable skills: keep. MCP: keep. Never execute unauthorized tools."
	for _, template := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
		"You are an agent for Claude Code, Anthropic's official CLI for Claude. Given the user's message, keep the tool contract.",
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI.",
		"You are ZCode, an interactive coding agent\nMain branch (you will usually use this for PRs): main",
		"You are an AI agent powered by DeepSeek Harness.",
		"Tokens prefixed with @ are paths the user explicitly referenced.",
		"TOKENS PREFIXED WITH @ ARE PATHS THE USER EXPLICITLY REFERENCED.",
		"Tokens prefixed with @ are paths the user explicitly referenced.\n\nRelative paths resolve from the workspace root.",
		dsFileParagraph,
	} {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": billing + template + tail},
			map[string]any{"role": "user", "content": template},
			map[string]any{"role": "developer", "content": []any{map[string]any{"type": "text", "text": template + tail}}},
		}}
		prepared, changed := NeutralizeChannelFingerprints(body)
		if !changed || reflect.DeepEqual(body, prepared) {
			t.Fatal("known rule was not applied before sending", template)
		}
		if _, duplicate := ChannelCompatibilityBody(prepared, 1); duplicate {
			t.Fatal("recovery would resend equivalent template", template)
		}
		if !strings.HasPrefix(prepared["messages"].([]any)[0].(map[string]any)["content"].(string), billing) {
			t.Fatal("preflight removed attribution")
		}
		recovered, changed := ChannelCompatibilityBody(prepared, 2)
		if !changed {
			t.Fatal("distinct opt-in metadata recovery lost")
		}
		if _, duplicate := ChannelCompatibilityBody(recovered, 2); duplicate {
			t.Fatal("metadata recovery is not idempotent")
		}
		if recovered["messages"].([]any)[1].(map[string]any)["content"] != template {
			t.Fatal("business content changed")
		}
	}
}

func TestHarnessIdentityPreparedBeforeDispatch(t *testing.T) {
	const intro = "You are an AI agent powered by DeepSeek Harness."
	const instructions = "\nAvailable skills: keep. MCP: keep. Never execute unauthorized tools."
	for _, prefix := range []string{"", "\n\t", "Runtime context:\n", "x-anthropic-billing-header: cc_version=1;\n"} {
		text := prefix + strings.ToUpper(intro) + instructions
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": text},
			map[string]any{"role": "user", "content": intro},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "report", "arguments": `{"text":"` + intro + `"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": intro},
			map[string]any{"role": "developer", "content": []any{map[string]any{"type": "text", "text": text, "cache_control": map[string]any{"type": "ephemeral"}}}},
		}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "report", "description": intro}}}}
		before, _ := json.Marshal(body)
		out, changed := NeutralizeChannelFingerprints(body)
		if !changed {
			t.Fatal("known identity missed before dispatch")
		}
		messages := out["messages"].([]any)
		want := prefix + "You are a coding assistant." + instructions
		if messages[0].(map[string]any)["content"] != want || messages[4].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != want {
			t.Fatal("identity replacement lost surrounding instructions")
		}
		if !reflect.DeepEqual(messages[1:4], body["messages"].([]any)[1:4]) || !reflect.DeepEqual(out["tools"], body["tools"]) {
			t.Fatal("changed business content or tool contract")
		}
		if _, again := NeutralizeChannelFingerprints(out); again {
			t.Fatal("preflight not idempotent")
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("mutated input")
		}
	}
	for _, text := range []string{"Quoted: " + intro, `"` + intro + `"`, intro + "example", "You are an AI assistant built on DeepSeek Harness.", "You are a coding agent powered by the deepseek-v4.1-flash model."} {
		body := map[string]any{"messages": []any{map[string]any{"role": "system", "content": text}}}
		if _, changed := NeutralizeChannelFingerprints(body); changed {
			t.Fatal("rewrote an unverified identity or embedded quotation")
		}
	}
}

func TestHarnessCompatibilityPreservesToolsAndHistory(t *testing.T) {
	const intro = "You are an AI agent powered by DeepSeek Harness."
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": intro + "\nAvailable skills: keep-this. Never execute unapproved tools."}}},
			map[string]any{"role": "user", "content": intro},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call1", "type": "function", "function": map[string]any{"name": "custom", "arguments": `{"value":"` + intro + `"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call1", "content": intro},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "custom", "description": intro, "parameters": map[string]any{"type": "object"}}}},
	}
	before, _ := json.Marshal(body)
	got, changed := ChannelCompatibilityBody(body, 1)
	if !changed {
		t.Fatal("known leading system declaration was missed")
	}
	oldMessages := body["messages"].([]any)
	newMessages := got["messages"].([]any)
	if !reflect.DeepEqual(oldMessages[1:], newMessages[1:]) || !reflect.DeepEqual(body["tools"], got["tools"]) {
		t.Fatal("changed tools, business arguments or conversation history")
	}
	text := newMessages[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasSuffix(text, "\nAvailable skills: keep-this. Never execute unapproved tools.") {
		t.Fatal("lost skill or permission instructions")
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("mutated original body")
	}
	if _, again := ChannelCompatibilityBody(got, 1); again {
		t.Fatal("compatibility is not idempotent")
	}
}

func TestHarnessCaseAndRoleCompatibilityAcrossProtocols(t *testing.T) {
	intro := "YOU ARE AN AI AGENT POWERED BY DEEPSEEK HARNESS.\nAvailable skills: keep. MCP: keep. Never run unauthorized tools."
	chat := upstream.BuildUpstreamBody(map[string]any{"messages": []any{map[string]any{"role": "developer", "content": intro}, map[string]any{"role": "user", "content": intro}}})
	responses, e := adapters.ResponsesRequestToChat(map[string]any{"input": []any{map[string]any{"role": "developer", "content": intro}, map[string]any{"role": "user", "content": "hi"}}})
	if e != nil {
		t.Fatal(e)
	}
	messages, e := adapters.AnthropicRequestToChat(map[string]any{"system": intro, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if e != nil {
		t.Fatal(e)
	}
	for _, body := range []map[string]any{chat, responses, messages} {
		before, _ := json.Marshal(body)
		prepared, ready := NeutralizeChannelFingerprints(body)
		if !ready || !strings.HasPrefix(prepared["messages"].([]any)[0].(map[string]any)["content"].(string), "You are a coding assistant.") {
			t.Fatal("protocol did not prepare the verified identity before dispatch")
		}
		out, changed := ChannelCompatibilityBody(body, 1)
		if !changed {
			t.Fatal("lost known identity")
		}
		text := out["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(text, "You are a coding assistant.") || !strings.Contains(text, "Available skills: keep. MCP: keep.") {
			t.Fatal(text)
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("input mutated")
		}
	}
	out, _ := ChannelCompatibilityBody(chat, 1)
	if out["messages"].([]any)[1].(map[string]any)["content"] != intro {
		t.Fatal("user content changed")
	}
	for _, text := range []string{"Quoted: " + intro, "YOU ARE AN AI AGENT POWERED BY DEEPSEEK HARNESS.example"} {
		if _, changed := CompatibleIdentity(text); changed {
			t.Fatal("false positive")
		}
	}
}

func TestFileReferenceTemplateCompatibility(t *testing.T) {
	original := "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root."
	for _, role := range []string{"system", "developer"} {
		body := map[string]any{"messages": []any{map[string]any{"role": role, "content": original}, map[string]any{"role": "user", "content": original}, map[string]any{"role": "assistant", "content": original}}}
		out, changed := ChannelCompatibilityBody(body, 1)
		if !changed {
			t.Fatal("template not adapted")
		}
		messages := out["messages"].([]any)
		if messages[0].(map[string]any)["content"] != strings.Replace(original, "resolve from", "resolve against", 1) {
			t.Fatal("template semantics changed")
		}
		if messages[1].(map[string]any)["content"] != original || messages[2].(map[string]any)["content"] != original {
			t.Fatal("history changed")
		}
		if body["messages"].([]any)[0].(map[string]any)["content"] != original {
			t.Fatal("source mutated")
		}
		if _, again := ChannelCompatibilityBody(out, 1); again {
			t.Fatal("not idempotent")
		}
	}
	if fileReferenceTemplate.MatchString("Quoted: " + original) {
		t.Fatal("rewrote embedded quotation")
	}
}

func TestHarnessGUICompatibilityPreservesConversation(t *testing.T) {
	const phrase = "DeepSeek Harness Web GUI"
	const template = "The user is accessing the " + phrase + ". References to this GUI refer to the current interface.\nAvailable skills: keep. Never run unauthorized tools."
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": template},
		map[string]any{"role": "user", "content": phrase},
		map[string]any{"role": "assistant", "content": phrase, "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "read", "arguments": `{"text":"DeepSeek Harness Web GUI"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": phrase},
		map[string]any{"role": "developer", "content": []any{map[string]any{"type": "text", "text": template, "cache_control": map[string]any{"type": "ephemeral"}}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}}}},
	}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "description": phrase}}}}
	before, _ := json.Marshal(body)
	out, changed := NeutralizeChannelFingerprints(body)
	if !changed {
		t.Fatal("GUI template not rewritten before first attempt")
	}
	messages := out["messages"].([]any)
	original := body["messages"].([]any)
	if messages[0].(map[string]any)["content"] != strings.ReplaceAll(template, phrase, "web interface") {
		t.Fatal("system instructions changed beyond GUI phrase")
	}
	for _, i := range []int{1, 2, 3} {
		if !reflect.DeepEqual(messages[i], original[i]) {
			t.Fatal("conversation or tool arguments changed")
		}
	}
	blocks := messages[4].(map[string]any)["content"].([]any)
	oldBlocks := original[4].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["text"] != strings.ReplaceAll(template, phrase, "web interface") || !reflect.DeepEqual(blocks[0].(map[string]any)["cache_control"], oldBlocks[0].(map[string]any)["cache_control"]) || !reflect.DeepEqual(blocks[1], oldBlocks[1]) || !reflect.DeepEqual(out["tools"], body["tools"]) {
		t.Fatal("metadata or tool definitions changed")
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("source mutated")
	}
	if _, again := NeutralizeChannelFingerprints(out); again {
		t.Fatal("rewrite not idempotent")
	}
	for _, negative := range []string{"DeepSeek Harness Web GUIExtension", "deepseek-harness/web/gui", "deepseek_harness_web_gui", "DeepSeek Harness", "Web GUI"} {
		untouched := map[string]any{"messages": []any{map[string]any{"role": "system", "content": negative}}}
		if _, changed := NeutralizeChannelFingerprints(untouched); changed {
			t.Fatal("unrelated name rewritten")
		}
	}
}

func TestHarnessGUICompatibilityAcrossProtocols(t *testing.T) {
	const text = "Runtime: DEEPSEEK HARNESS WEB GUI. Available skills: keep."
	chat := upstream.BuildUpstreamBody(map[string]any{"messages": []any{map[string]any{"role": "developer", "content": text}}})
	responses, err := adapters.ResponsesRequestToChat(map[string]any{"instructions": text, "input": []any{map[string]any{"role": "user", "content": "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := adapters.AnthropicRequestToChat(map[string]any{"system": text, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{chat, responses, messages} {
		first, changed := NeutralizeChannelFingerprints(body)
		if !changed {
			t.Fatal("initial template preparation missed")
		}
		fallback, matched := ChannelCompatibilityBody(body, 1)
		if !matched {
			t.Fatal("compatibility fallback missed")
		}
		for _, out := range []map[string]any{first, fallback} {
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "DEEPSEEK HARNESS WEB GUI") || !strings.Contains(string(raw), "web interface. Available skills: keep.") {
				t.Fatal("protocol lost GUI replacement or skills")
			}
		}
	}
}

func TestMediaVideoParametersPreserveInputAndRespectCatalog(t *testing.T) {
	entry := map[string]any{"video_generation": map[string]any{"minSeconds": float64(3), "maxSeconds": float64(15), "defaultSeconds": float64(5), "defaultResolution": "720P", "resolutions": []any{"720P", "1080P"}, "aspectRatios": []any{"16:9", "1:1"}, "defaultAspectRatio": "16:9"}}
	p := map[string]any{"prompt": "test", "seconds": "3", "resolution": "720p", "image_url": "https://example.invalid/frame.png"}
	before, _ := json.Marshal(p)
	body, err := buildMediaBody("video", "movie", p, entry)
	if err != nil {
		t.Fatal(err)
	}
	extra := body["extra_parameters"].(map[string]any)
	if body["seconds"] != float64(3) || extra["resolution"] != "720P" || extra["aspect_ratio"] != nil || body["watermark"] != true {
		t.Fatal(body)
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("caller payload mutated")
	}
	p["seconds"] = float64(2)
	if _, err := buildMediaBody("video", "movie", p, entry); err == nil {
		t.Fatal("unsupported duration accepted")
	}
	p["seconds"] = float64(3)
	p["resolution"] = "4K"
	if _, err := buildMediaBody("video", "movie", p, entry); err == nil {
		t.Fatal("unsupported resolution accepted")
	}
	if _, err := buildMediaBody("image-edit", "hunyuan-image-alpha-edit", map[string]any{"prompt": "test", "image": []any{"file:///private/image.png"}}, nil); err == nil {
		t.Fatal("server file reference accepted")
	}
	if _, err := buildMediaBody("image", "hunyuan-image-alpha", map[string]any{"prompt": "test", "response_format": "b64_json"}, nil); err == nil {
		t.Fatal("unsupported output format silently accepted")
	}
}

func TestMediaImageModelBranchesMatchReference(t *testing.T) {
	for _, name := range []string{"hunyuan-image-alpha", "gemini-image", "hy-image-3.5"} {
		b, err := buildMediaBody("image", name, map[string]any{"prompt": "test", "quality": "high"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if name == "hy-image-3.5" {
			if b["response_format"] != "b64_json" || b["quality"] != "high" {
				t.Fatal(b)
			}
		} else if b["quality"] != nil || b["response_format"] != nil {
			t.Fatal("model branch contaminated", b)
		}
	}
}

func TestMediaVideoReferenceMaterialsFollowOfficialWire(t *testing.T) {
	entry := map[string]any{"video_generation": map[string]any{"minSeconds": float64(4), "maxSeconds": float64(15), "maxReferenceImages": float64(9), "maxReferenceVideos": float64(3), "maxReferenceAudios": float64(3), "maxReferenceMaterials": float64(12), "allowMixedReferenceMaterials": true, "framesExclusiveWithReferenceMaterials": true, "referenceAudioRequiresVisualMaterial": true, "audioMode": "always-on"}}
	p := map[string]any{"prompt": "", "seconds": float64(4), "reference_images": []any{"https://example.invalid/a.png"}, "reference_audios": []any{"https://example.invalid/a.wav"}}
	b, err := buildMediaBody("video", "minimax-video-h3", p, entry)
	if err != nil {
		t.Fatal(err)
	}
	refs := b["reference_images"].([]any)
	if refs[0].(map[string]any)["image_url"] != "https://example.invalid/a.png" {
		t.Fatal(b)
	}
	if b["extra_parameters"].(map[string]any)["enable_audio"] != true {
		t.Fatal(b)
	}
	p["image_url"] = "https://example.invalid/frame.png"
	if _, err = buildMediaBody("video", "minimax-video-h3", p, entry); err == nil {
		t.Fatal("incompatible frames and references accepted")
	}
	delete(p, "image_url")
	delete(p, "reference_images")
	p["prompt"] = "test"
	if _, err = buildMediaBody("video", "minimax-video-h3", p, entry); err == nil {
		t.Fatal("reference audio without visual material accepted")
	}
}

func TestBuddyProxyDSHCompatibilityParity(t *testing.T) {
	const rules = "\nAvailable skills: keep all descriptions. Preserve tools and permissions."
	for _, intro := range []string{
		"You are a coding agent powered by DeepSeek Harness.",
		"You are an AI assistant powered by DeepSeek Harness.",
		"You are an AI agent powered by DeepSeek.",
		"You are an AI agent. DeepSeek Harness.",
		"Powered by DeepSeek Harness, you are an AI agent.",
		strings.Repeat("DeepSeek Harness runtime. ", 20),
	} {
		body := map[string]any{"messages": []any{map[string]any{"role": "system", "content": intro + rules}, map[string]any{"role": "developer", "content": intro + rules}}}
		out, changed := NeutralizeChannelFingerprints(body)
		if changed || !reflect.DeepEqual(body, out) {
			t.Fatal("broadened upstream verified trigger", intro)
		}
	}
}

func TestAssistantDeclarationCompatibilityPreservesBusinessContent(t *testing.T) {
	const intro = "You are an AI agent powered by DeepSeek Harness."
	const tail = "\nAvailable skills: read, patch. Working directory: /srv/DSH/project"
	arguments := `{"text":"` + intro + `"}`
	body := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "Quoted: " + intro},
			map[string]any{"type": "text", "text": intro + tail, "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/DSH.png"}},
		}, "tool_calls": []any{map[string]any{"function": map[string]any{"name": "read", "arguments": arguments}}}},
		map[string]any{"role": "user", "content": intro},
		map[string]any{"role": "tool", "content": intro},
	}}
	before, _ := json.Marshal(body)
	out, changed := NeutralizeChannelFingerprints(body)
	if !changed {
		t.Fatal("assistant declaration not adapted")
	}
	messages := out["messages"].([]any)
	assistant := messages[0].(map[string]any)
	blocks := assistant["content"].([]any)
	if blocks[1].(map[string]any)["text"] != "You are a coding assistant."+tail {
		t.Fatal("instructions lost", blocks)
	}
	original := body["messages"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(blocks[0], original["content"].([]any)[0]) || !reflect.DeepEqual(blocks[2], original["content"].([]any)[2]) || !reflect.DeepEqual(assistant["tool_calls"], original["tool_calls"]) || !reflect.DeepEqual(messages[1:], body["messages"].([]any)[1:]) || !reflect.DeepEqual(blocks[1].(map[string]any)["cache_control"], original["content"].([]any)[1].(map[string]any)["cache_control"]) {
		t.Fatal("business content changed")
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("request mutated")
	}
	if _, again := NeutralizeChannelFingerprints(out); again {
		t.Fatal("not idempotent")
	}
	for _, text := range []string{"Quoted: " + intro, "```\n" + intro + "\n```", "My path is /srv/DSH/project", "DeepSeek Harness Web GUI", "Tokens prefixed with @ are paths the user explicitly referenced."} {
		untouched := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": text}}}
		if _, changed := NeutralizeChannelFingerprints(untouched); changed {
			t.Fatal("ordinary history changed", text)
		}
	}
}
