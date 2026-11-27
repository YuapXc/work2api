package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"time"
)

type coalesceKey struct{}

func WithReasoningCoalescing(ctx context.Context) context.Context {
	return context.WithValue(ctx, coalesceKey{}, true)
}

type reasoningPacket struct {
	line  string
	chunk map[string]any
}

func pureReasoning(chunk map[string]any) (string, string) {
	choices, _ := chunk["choices"].([]any)
	if len(choices) != 1 || chunk["usage"] != nil {
		return "", ""
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != nil && choice["finish_reason"] != "" || choice["logprobs"] != nil {
		return "", ""
	}
	delta, _ := choice["delta"].(map[string]any)
	field, text := "", ""
	for key, value := range delta {
		if key != "reasoning" && key != "reasoning_content" {
			return "", ""
		}
		part, ok := value.(string)
		if !ok {
			return "", ""
		}
		if part != "" {
			if field != "" {
				return "", ""
			}
			field, text = key, part
		}
	}
	for key := range chunk {
		switch key {
		case "id", "object", "created", "model", "choices", "system_fingerprint", "usage":
		default:
			return "", ""
		}
	}
	return field, text
}
func coalescedSSE(resp *http.Response, yield LineFunc, names map[string]string) error {
	parent := resp.Request.Context()
	ctx, cancel := context.WithCancel(WithParsedFrames(parent))
	copyResp := *resp
	copyResp.Request = resp.Request.Clone(ctx)
	packets := make(chan reasoningPacket)
	result := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		result <- streamSSE(&copyResp, func(line string) error {
			packet := reasoningPacket{line, DecodeFrame(ctx, line)}
			select {
			case packets <- packet:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, names)
	}()
	defer func() { cancel(); resp.Body.Close(); <-stopped }()
	var held map[string]any
	var text strings.Builder
	var field, lastField string
	var timer *time.Timer
	var tick <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	emit := func(line string, chunk map[string]any) error { RememberFrame(parent, line, chunk); return yield(line) }
	flush := func() error {
		if held == nil {
			return nil
		}
		choice := held["choices"].([]any)[0].(map[string]any)
		choice["delta"].(map[string]any)[field] = text.String()
		raw, err := json.Marshal(held)
		chunk := held
		held = nil
		text.Reset()
		tick = nil
		if timer != nil {
			timer.Stop()
		}
		if err != nil {
			return err
		}
		return emit("data: "+string(raw), chunk)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			if err := flush(); err != nil {
				return err
			}
		case err := <-result:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e := flush(); e != nil {
				return e
			}
			return err
		case packet := <-packets:
			f, part := pureReasoning(packet.chunk)
			if len(part) >= 8192 || len(packet.line) > 65536 {
				f = ""
			}
			if f == "" || f != lastField {
				if err := flush(); err != nil {
					return err
				}
				lastField = f
				if err := emit(packet.line, packet.chunk); err != nil {
					return err
				}
				continue
			}
			if held != nil {
				compatible := true
				for _, key := range []string{"id", "object", "created", "model", "system_fingerprint"} {
					compatible = compatible && reflect.DeepEqual(held[key], packet.chunk[key])
				}
				oldChoice := held["choices"].([]any)[0].(map[string]any)
				newChoice := packet.chunk["choices"].([]any)[0].(map[string]any)
				compatible = compatible && reflect.DeepEqual(oldChoice["index"], newChoice["index"])
				if !compatible || text.Len()+len(part) > 8192 {
					if err := flush(); err != nil {
						return err
					}
				}
			}
			if held == nil {
				held = map[string]any{}
				for k, v := range packet.chunk {
					held[k] = v
				}
				choice := map[string]any{}
				for k, v := range packet.chunk["choices"].([]any)[0].(map[string]any) {
					choice[k] = v
				}
				delta := map[string]any{}
				for k, v := range choice["delta"].(map[string]any) {
					delta[k] = v
				}
				choice["delta"] = delta
				held["choices"] = []any{choice}
				field = f
				if timer == nil {
					timer = time.NewTimer(300 * time.Millisecond)
				} else {
					timer.Reset(300 * time.Millisecond)
				}
				tick = timer.C
			}
			text.WriteString(part)
			if text.Len() >= 8192 {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
}
