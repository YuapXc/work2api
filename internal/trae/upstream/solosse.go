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
	PlanPresent  bool
	ErrorMessage string // error
}
type SOLOStreamError struct {
	PlanPresent bool
	Code        int64
	Msg         string
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
	if e.Code == 429 {
		return ErrSoftRate
	}
	if e.Code == 401 {
		return ErrSessionDead
	}
	if e.Code == 1005 && e.PlanPresent {
		return ErrModelPlan
	}
	if _, ok := soloPlanLimitCodes[e.Code]; ok {
		return ErrPlanLimit
	}
	if MsgIndicatesInputTooLarge(e.Msg) {
		return ErrInputTooLarge
	}
	if _, ok := soloModelMismatchCodes[e.Code]; ok {
		return ErrModelUnavailable
	}
	if e.Code >= 500 && e.Code <= 599 {
		return ErrServer
	}
	return ErrClient
}

func ParseSOLOLine(eventName, dataLine string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(eventName)}
	if strings.TrimSpace(dataLine) == "" {
		return ev, nil
	}
	// A known control heartbeat may carry a JSON string. Do not extend this
	// exemption to output/error/done or accept malformed JSON as model text.
	if ev.Event == "progress_notice" {
		var heartbeat any
		if err := json.Unmarshal([]byte(dataLine), &heartbeat); err != nil {
			return nil, err
		}
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	if ev.Event == "" && BusinessCode([]byte(dataLine)) == 4245 {
		ev.Event = "error"
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
		ev.PlanPresent = HasPlan([]byte(dataLine))
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
