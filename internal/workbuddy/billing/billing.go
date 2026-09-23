// Package billing implements credit queries and daily check-in against
// WorkBuddy/CodeBuddy's billing endpoints. Ported from workbuddy_one/billing.py.
package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"work2api/internal/workbuddy/httpclient"
)

// Credential is what billing needs from a credential manager.
type Credential interface {
	GetHeaders() (map[string]string, error)
	Endpoint() string
}

// Credit is the query result.
type Credit struct {
	Remain   float64          `json:"remain"`
	Total    float64          `json:"total"`
	ExpireAt *float64         `json:"expire_at"`
	Packages []map[string]any `json:"packages"`
}

// CheckinResult is the daily check-in outcome.
type CheckinResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Already bool   `json:"already"`
}

var paidPackageCodes = []string{
	"TCACA_code_002_AkiJS3ZHF5", "TCACA_code_005_maRGyrHhw1", "TCACA_code_003_FAnt7lcmRT",
	"TCACA_code_023_4xbGhMrE6q", "TCACA_code_026_BaESVICNoi", "TCACA_code_027_0FCGVA6vSa",
	"TCACA_code_009_0XmEQc2xOf", "TCACA_code_038_OhvqZtiPKr", "TCACA_code_036_lupO5WgNdG",
}

var freePackageCodes = []string{
	"TCACA_code_001_PqouKr6QWV", "TCACA_code_008_cfWoLwvjU4", "TCACA_code_035_ArVxJcGDsm",
	"TCACA_code_006_DbXS0lrypC", "TCACA_code_039_KRcQj7wUat", "TCACA_code_040_mi9rCYg46x",
	"TCACA_code_007_nzdH5h4Nl0", "TCACA_code_028_NtpWi0jzXs", "TCACA_code_029_6wCGEWquYy",
	"TCACA_code_030_BjSt89qTvr", "TCACA_code_037_WxOD3MpI2o",
}

// browserUA bypasses the gateway WAF which blocks non-browser UAs on billing.
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

// httpClient bypasses the system proxy (mirrors upstream trust_env=False): a
// running intl proxy would otherwise break domestic billing calls.
var httpClient = httpclient.New(20 * time.Second)

func billingHeaders(mgr Credential) (map[string]string, error) {
	h, err := mgr.GetHeaders()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	out["User-Agent"] = browserUA
	return out, nil
}

func postJSON(ctx context.Context, mgr Credential, url string, body map[string]any) (map[string]any, error) {
	headers, err := billingHeaders(mgr)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return map[string]any{}, nil
	}
	return data, nil
}

func extractAccounts(data map[string]any) []any {
	if data == nil {
		return nil
	}
	paths := [][]string{{"data", "Accounts"}, {"data", "Response", "Data", "Accounts"}}
	for _, path := range paths {
		var node any = data
		ok := true
		for _, key := range path {
			m, isMap := node.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			node = m[key]
		}
		if ok {
			if list, isList := node.([]any); isList {
				return list
			}
		}
	}
	return nil
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	}
	return 0
}

func parseTS(raw any, isMs bool) *float64 {
	if raw == nil || raw == "" {
		return nil
	}
	if isMs {
		f := num(raw) / 1000.0
		if f == 0 {
			return nil
		}
		return &f
	}
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(s), time.Local)
	if err != nil {
		return nil
	}
	f := float64(t.Unix())
	return &f
}

func minTS(a, b *float64) *float64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *b < *a {
		return b
	}
	return a
}

func summarize(accounts []any) (remain, total float64, earliest *float64) {
	for _, a := range accounts {
		acct, ok := a.(map[string]any)
		if !ok {
			continue
		}
		cycleRemain := num(acct["CycleCapacityRemain"])
		cycleSize := num(acct["CycleCapacitySize"])
		capRemain := num(acct["CapacityRemain"])
		capSize := num(acct["CapacitySize"])
		cycleUsed := num(acct["CycleCapacityUsed"])
		if cycleSize > 0 {
			remain += math.Max(cycleRemain, 0)
			total += cycleSize
		} else if cycleRemain > 0 || cycleUsed > 0 {
			remain += math.Max(cycleRemain, 0)
			total += cycleSize
		} else {
			remain += math.Max(capRemain, 0)
			total += capSize
		}
		if cycleRemain <= 0 && capRemain <= 0 {
			continue
		}
		for _, kv := range []struct {
			key  string
			isMs bool
		}{{"CycleEndTime", false}, {"ExpiredTime", false}, {"DeductionEndTime", true}} {
			if ts := parseTS(acct[kv.key], kv.isMs); ts != nil {
				earliest = minTS(earliest, ts)
			}
		}
	}
	return remain, total, earliest
}

func summarizePackages(accounts []any) []map[string]any {
	groups := map[string]map[string]any{}
	var order []string
	for _, a := range accounts {
		acct, ok := a.(map[string]any)
		if !ok {
			continue
		}
		p := func(base string) float64 {
			if v := acct[base+"Precise"]; v != nil && v != "" {
				return num(v)
			}
			return num(acct[base])
		}
		cycleRemain := p("CycleCapacityRemain")
		cycleSize := p("CycleCapacitySize")
		capRemain := p("CapacityRemain")
		capSize := p("CapacitySize")
		cycleUsed := p("CycleCapacityUsed")
		capUsed := p("CapacityUsed")
		var remain, used, tot float64
		if cycleSize > 0 {
			remain, used, tot = cycleRemain, cycleUsed, cycleSize
		} else if cycleRemain > 0 || cycleUsed > 0 {
			remain, used, tot = cycleRemain, cycleUsed, cycleSize
		} else {
			remain, used, tot = capRemain, capUsed, capSize
		}
		key := str(acct["PackageCode"])
		if key == "" {
			key = str(acct["SubProductCode"])
		}
		if key == "" {
			key = "other"
		}
		g, exists := groups[key]
		if !exists {
			name := str(acct["PackageName"])
			if name == "" {
				name = key
			}
			g = map[string]any{"name": name, "remain": 0.0, "used": 0.0, "total": 0.0, "expire_at": (*float64)(nil)}
			groups[key] = g
			order = append(order, key)
		}
		g["remain"] = g["remain"].(float64) + math.Max(remain, 0)
		g["used"] = g["used"].(float64) + math.Max(used, 0)
		g["total"] = g["total"].(float64) + math.Max(tot, 0)
		if remain > 0 {
			cur, _ := g["expire_at"].(*float64)
			for _, kv := range []struct {
				key  string
				isMs bool
			}{{"CycleEndTime", false}, {"ExpiredTime", false}, {"DeductionEndTime", true}} {
				cur = minTS(cur, parseTS(acct[kv.key], kv.isMs))
			}
			g["expire_at"] = cur
		}
	}
	var out []map[string]any
	for _, k := range order {
		g := groups[k]
		if g["total"].(float64) > 0 {
			// normalize expire_at to a plain value for JSON
			if ea, _ := g["expire_at"].(*float64); ea != nil {
				g["expire_at"] = *ea
			} else {
				g["expire_at"] = nil
			}
			out = append(out, g)
		}
	}
	// sort by remain desc
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1]["remain"].(float64) < out[j]["remain"].(float64); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func fetchNewCredits(ctx context.Context, mgr Credential) *Credit {
	now := time.Now()
	dayStart := now.Format("2006-01-02") + " 00:00:00"
	dayEnd := now.Format("2006-01-02") + " 23:59:59"
	base := mgr.Endpoint() + "/billing/meter"
	var accounts []any
	endpoints := []struct {
		url  string
		body map[string]any
	}{
		{base + "/get-user-resource-summary", map[string]any{}},
		{base + "/get-user-resource-paid-packages", map[string]any{
			"PageNumber": 1, "PageSize": 100, "PackageCodes": paidPackageCodes,
			"Status": []int{0, 3}, "NeedRenewInfo": true, "IsDisplayTotalInfo": true,
		}},
		{base + "/get-user-resource-free-packages", map[string]any{
			"PageNumber": 1, "PageSize": 100, "PackageCodes": freePackageCodes,
			"Status": []int{0, 3}, "SlicePeriodStartTime": dayStart, "SlicePeriodEndTime": dayEnd,
			"IsDisplayTotalInfo": true,
		}},
	}
	for _, e := range endpoints {
		j, err := postJSON(ctx, mgr, e.url, e.body)
		if err != nil {
			continue
		}
		accounts = append(accounts, extractAccounts(j)...)
	}
	if len(accounts) == 0 {
		return nil
	}
	remain, total, expireAt := summarize(accounts)
	return &Credit{Remain: math.Round(remain), Total: math.Round(total), ExpireAt: expireAt, Packages: summarizePackages(accounts)}
}

func fetchOldCredits(ctx context.Context, mgr Credential) *Credit {
	url := mgr.Endpoint() + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca", "Status": []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(10, 0, 0).Format("2006-01-02 15:04:05"),
	}
	j, err := postJSON(ctx, mgr, url, body)
	if err != nil {
		return nil
	}
	accounts := extractAccounts(j)
	if len(accounts) == 0 {
		return nil
	}
	remain, total, expireAt := summarize(accounts)
	return &Credit{Remain: math.Round(remain), Total: math.Round(total), ExpireAt: expireAt, Packages: summarizePackages(accounts)}
}

// FetchCredits queries the account's spendable credit balance.
func FetchCredits(ctx context.Context, mgr Credential) (*Credit, error) {
	if res := fetchNewCredits(ctx, mgr); res != nil {
		return res, nil
	}
	if res := fetchOldCredits(ctx, mgr); res != nil {
		return res, nil
	}
	return nil, errors.New("额度查询失败：新计费三接口与旧接口均不可用")
}

// DailyCheckin performs the daily check-in.
func DailyCheckin(ctx context.Context, mgr Credential) (CheckinResult, error) {
	url := mgr.Endpoint() + "/v2/billing/meter/daily-checkin"
	data, err := postJSON(ctx, mgr, url, map[string]any{})
	if err != nil {
		return CheckinResult{}, err
	}
	// 必须存在明确的 code 字段才判成功——否则非 JSON / 网关 404 / WAF 拦截等
	// 会被 num(nil)==0 误判为「签到成功」（国际站无签到活动即属此类）。
	codeRaw, hasCode := data["code"]
	msg := str(data["msg"])
	if msg == "" {
		msg = str(data["message"])
	}
	if !hasCode {
		if msg == "" {
			msg = "签到接口无有效响应（该站点可能不支持签到）"
		}
		return CheckinResult{OK: false, Message: msg}, nil
	}
	code := num(codeRaw)
	if code == 0 {
		return CheckinResult{OK: true, Message: "签到成功"}, nil
	}
	for _, k := range []string{"已签到", "already", "checkin"} {
		if strings.Contains(msg, k) {
			return CheckinResult{OK: false, Message: msg, Already: true}, nil
		}
	}
	return CheckinResult{OK: false, Message: msg}, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
