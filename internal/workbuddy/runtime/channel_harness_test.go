package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/upstream"
)

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
		if messages[0].(map[string]any)["content"] != strings.Replace(original, "paths the", "paths that the", 1) {
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
