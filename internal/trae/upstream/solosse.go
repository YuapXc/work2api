// Adapted from mmqz/cpa-multi-plugins, MIT; see NOTICE.md.
package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

type SOLOEvent struct {
	Event        string          // metadata | timing_cost | output | extra_info | token_usage | done | error
	Response     string          // output: content 增量
	Reasoning    string          // output: 思考链增量
	ToolCalls    json.RawMessage // output: 工具调用（null 或对象/数组）
	Usage        map[string]any  // token_usage
	FinishReason string          // done
	ErrorCode    int64           // error
	ErrorMessage string          // error
}
type SOLOStreamError struct {
	Code int64
	Msg  string
}

func (e *SOLOStreamError) Error() string {
	return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg)
}

var soloPlanLimitCodes = map[int64]struct{}{1005: {}, 4008: {}}
var soloModelMismatchCodes = map[int64]struct{}{4001: {}}

func IsPlanLimitCode(code int64) bool {
	_, ok := soloPlanLimitCodes[code]
	return ok
}
func IsModelMismatchCode(code int64) bool {
	_, ok := soloModelMismatchCodes[code]
	return ok
}
func (e *SOLOStreamError) Kind() ErrKind {
	if _, ok := soloPlanLimitCodes[e.Code]; ok {
		return ErrPlanLimit
	}
	if MsgIndicatesInputTooLarge(e.Msg) {
		return ErrInputTooLarge
	}
	if _, ok := soloModelMismatchCodes[e.Code]; ok {
		return ErrModelUnavailable
	}
	if e.Code == 429 {
		return ErrSoftRate
	}
	if e.Code == 401 {
		return ErrSessionDead
	}
	if e.Code >= 500 && e.Code <= 599 {
		return ErrServer
	}
	return ErrClient
}
func ParseSOLOLine(eventName, dataLine string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(dataLine), &decoded); err != nil {
		return nil, err
	}
	// The upstream interleaves progress_notice heartbeats whose payload is a
	// bare JSON string ("Processing_...") instead of an object. Nothing below
	// can read such a frame, and failing here aborted the whole turn with a 502
	// even though the answer arrived normally. Skip non-object frames; corrupt
	// JSON still fails above, and real upstream failures arrive as an explicit
	// error event.
	raw, ok := decoded.(map[string]any)
	if !ok {
		return ev, nil
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "extra_info":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
		if ev.ErrorMessage == "" {
			ev.ErrorMessage, _ = raw["msg"].(string)
		}
	}
	return ev, nil
}
