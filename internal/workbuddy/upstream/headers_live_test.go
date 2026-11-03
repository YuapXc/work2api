package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/reasoning"
	wbruntime "work2api/internal/workbuddy/runtime"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

// TestLiveInternationalConversationHeaders uses synthetic content only and
// reports no credentials or account identifiers. Opt in explicitly.
func TestLiveInternationalConversationHeaders(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_INTL_HEADERS") != "1" {
		t.Skip("explicit live verification only")
	}
	runLiveConversationHeaders(t, siterouting.International, os.Getenv("WORK2API_LIVE_INTL_CLIENTS") == "1")
}

func TestLiveDomesticClientCompatibility(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_CN_CLIENTS") != "1" {
		t.Skip("explicit live verification only")
	}
	runLiveConversationHeaders(t, siterouting.Domestic, true)
}

func runLiveConversationHeaders(t *testing.T, site string, probeClients bool) {
	t.Helper()
	var mgr *credentials.Manager
	project := filepath.Join("..", "..", "..", "auths")
	files := append(credentials.FindAuthFiles(project, ""), credentials.FindAuthFiles("", "")...)
	for _, file := range files {
		candidate := credentials.NewManager(file)
		if siterouting.ProfileSite(candidate.Profile()) == site {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, err := candidate.GetChatHeadersContext(ctx)
			cancel()
			if err != nil {
				continue // Desktop encrypted or stale credentials are not usable test identities.
			}
			mgr = candidate
			break
		}
	}
	if mgr == nil {
		t.Fatal("credential unavailable for requested site")
	}
	url, err := siterouting.ChatURLForProfile(mgr.Profile())
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.Repeat("Reference item: amber birch cedar delta elm fern.\n", 300)
	body := upstream.BuildUpstreamBody(map[string]any{
		"model": "deepseek-v4.1-flash", "max_tokens": 1024, "reasoning_effort": "off",
		"messages": []any{map[string]any{"role": "system", "content": "Answer briefly."}, map[string]any{"role": "user", "content": prefix + "Reply with just OK."}},
	})
	body = reasoning.InjectThinking(body)
	modes := []string{"legacy", "current", "current-repeat"}
	if probeClients {
		modes = []string{"current", "harness-agent", "harness-coding", "harness-built"}
		if site == siterouting.Domestic {
			modes = append(modes, "harness-agent-compatible")
		}
	}
	if site == siterouting.Domestic && os.Getenv("WORK2API_LIVE_CN_TOOLS") == "1" {
		modes = []string{"harness-tool-compatible"}
	}
	if os.Getenv("WORK2API_LIVE_PROFILE_VERSIONS") == "1" {
		modes = []string{"version-previous", "version-current"}
	}
	if site == siterouting.Domestic && os.Getenv("WORK2API_LIVE_CN_PREFLIGHT") == "1" {
		modes = []string{"current", "harness-preflight-original", "harness-preflight-compatible"}
	}
	for _, mode := range modes {
		requestBody := body
		if strings.HasPrefix(mode, "harness-") {
			intro := "You are an AI agent powered by DeepSeek Harness."
			if strings.HasPrefix(mode, "harness-preflight-") {
				intro = "Runtime context:\n\t" + strings.ToUpper(intro)
			}
			if mode == "harness-coding" {
				intro = "You are a coding agent powered by the deepseek-v4.1-flash model."
			}
			if mode == "harness-built" {
				intro = "You are an AI assistant built on DeepSeek Harness."
			}
			requestBody = upstream.BuildUpstreamBody(map[string]any{
				"model": "deepseek-v4.1-flash", "max_tokens": 1024, "reasoning_effort": "off",
				"messages": []any{map[string]any{"role": "system", "content": intro + "\nAnswer briefly."}, map[string]any{"role": "user", "content": prefix + "Reply with just OK."}},
			})
			requestBody = reasoning.InjectThinking(requestBody)
			if mode == "harness-tool-compatible" {
				requestBody["messages"] = []any{map[string]any{"role": "system", "content": intro + "\nUse the supplied tool to report status."}, map[string]any{"role": "user", "content": "Call report_status with status OK."}}
				requestBody["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "report_status", "description": "Report a synthetic test status.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string"}}, "required": []any{"status"}}}}}
				requestBody["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "report_status"}}
				requestBody = reasoning.NormalizeToolChoice(requestBody)
			}
			if strings.HasSuffix(mode, "-compatible") {
				var changed bool
				if strings.HasPrefix(mode, "harness-preflight-") {
					requestBody, changed = wbruntime.NeutralizeChannelFingerprints(requestBody)
				} else {
					requestBody, changed = wbruntime.ChannelCompatibilityBody(requestBody, 1)
				}
				if !changed {
					t.Fatal("verified Harness opening was not adapted")
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		headers, err := mgr.GetChatHeadersContext(ctx)
		if err != nil {
			cancel()
			t.Fatal("credential preparation failed")
		}
		if mode == "legacy" {
			delete(headers, "X-Conversation-ID")
		}
		if mode == "version-previous" {
			product, _ := siterouting.ProfileProduct(mgr.Profile())
			if product == "cli" {
				headers["User-Agent"] = "CLI/2.149.0 CodeBuddy/2.149.0"
				headers["X-IDE-Version"] = "2.149.0"
			} else {
				name := "WorkBuddy"
				if site == siterouting.International {
					name = "WorkBuddy AI"
				}
				headers["User-Agent"] = "WorkBuddy/5.5.2 " + name + "/5.5.2 CLI/2.137.1"
				headers["X-IDE-Version"] = "5.5.2"
			}
		}
		start := time.Now()
		var usage map[string]any
		content := 0
		finish := ""
		first := time.Duration(0)
		toolName, toolArguments := "", ""
		err = upstream.Shared().StreamUpstream(ctx, headers, requestBody, url, func(line string) error {
			if !strings.HasPrefix(line, "data: ") {
				return nil
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
				return nil
			}
			if u, ok := chunk["usage"].(map[string]any); ok {
				usage = upstream.MergeUsage(usage, u)
			}
			choices, _ := chunk["choices"].([]any)
			for _, raw := range choices {
				choice, _ := raw.(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				calls, _ := delta["tool_calls"].([]any)
				if len(calls) > 0 && first == 0 {
					first = time.Since(start)
				}
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					function, _ := call["function"].(map[string]any)
					name, _ := function["name"].(string)
					arguments, _ := function["arguments"].(string)
					toolName += name
					toolArguments += arguments
				}
				if text, _ := delta["content"].(string); text != "" {
					content += len(text)
					if first == 0 {
						first = time.Since(start)
					}
				}
				if reason, _ := choice["finish_reason"].(string); reason != "" {
					finish = reason
				}
			}
			return nil
		})
		cancel()
		if err != nil {
			var remote *upstream.UpstreamError
			if errors.As(err, &remote) {
				var payload map[string]any
				_ = json.Unmarshal(remote.Raw, &payload)
				t.Logf("mode=%s status=%d code=%v msg=%v duration=%s", mode, remote.StatusCode, payload["code"], payload["msg"], time.Since(start).Round(time.Millisecond))
				if remote.StatusCode == 429 {
					t.Fatal("rate limited; stop live requests")
				}
			} else {
				t.Logf("mode=%s transport failure: %v", mode, err)
			}
			if mode != "legacy" && (!strings.HasPrefix(mode, "harness-") || strings.HasSuffix(mode, "-compatible")) {
				t.Fatal("current headers did not complete generation")
			}
			continue
		}
		t.Logf("mode=%s finish=%s bytes=%d first=%s duration=%s usage=%v", mode, finish, content, first.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), usage)
		if mode == "harness-tool-compatible" {
			var arguments map[string]any
			if finish != "tool_calls" || toolName != "report_status" || json.Unmarshal([]byte(toolArguments), &arguments) != nil || arguments["status"] != "OK" {
				t.Fatal("native tool name, arguments or finish reason lost")
			}
			continue
		}
		if content == 0 || finish != "stop" {
			t.Error("incomplete synthetic response")
		}
	}
}
