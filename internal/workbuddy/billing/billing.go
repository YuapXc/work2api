// Package billing implements credit queries and daily check-in against
// WorkBuddy/CodeBuddy's billing endpoints. Ported from workbuddy_one/billing.py.
package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"work2api/internal/workbuddy/httpclient"
	"work2api/internal/workbuddy/profiles"
)

// Credential is what billing needs from a credential manager.
type Credential interface {
	GetHeaders() (map[string]string, error)
	Endpoint() string
}

// Credit is the query result.
type Credit struct {
	Remain       float64          `json:"remain"`
	Total        float64          `json:"total"`
	ExpireAt     *float64         `json:"expire_at"`
	Packages     []map[string]any `json:"packages"`
	UnknownTotal bool             `json:"total_unknown,omitempty"`
}

// CheckinResult is the daily check-in outcome.
type CheckinResult struct {
	OK         bool     `json:"ok"`
	Message    string   `json:"message"`
	Already    bool     `json:"already"`
	Outcome    string   `json:"outcome,omitempty"`
	Credit     *float64 `json:"credit,omitempty"`
	StreakDays int      `json:"streak_days,omitempty"`
}

var paidPackageCodes = []string{
	"TCACA_code_002_AkiJS3ZHF5", "TCACA_code_005_maRGyrHhw1", "TCACA_code_003_FAnt7lcmRT",
	"TCACA_code_023_4xbGhMrE6q", "TCACA_code_026_BaESVICNoi", "TCACA_code_027_0FCGVA6vSa",
	"TCACA_code_009_0XmEQc2xOf", "TCACA_code_038_OhvqZtiPKr", "TCACA_code_036_lupO5WgNdG",
	"TCACA_code_043_6oIkxT7Z0S",
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
var errIncompleteBilling = errors.New("额度明细分页未完成")
var billingTimezone = time.FixedZone("UTC+8", 8*60*60)

func BusinessTime(t time.Time) time.Time { return t.In(billingTimezone) }

func billingHeaders(ctx context.Context, mgr Credential) (map[string]string, error) {
	var h map[string]string
	var err error
	if contextual, ok := mgr.(interface {
		GetHeadersContext(context.Context) (map[string]string, error)
	}); ok {
		h, err = contextual.GetHeadersContext(ctx)
	} else {
		h, err = mgr.GetHeaders()
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	out["User-Agent"] = browserUA
	profiles.ApplyBillingIdentity(out)
	return out, nil
}

func postJSON(ctx context.Context, mgr Credential, url string, body map[string]any) (map[string]any, error) {
	data, status, err := postJSONStatus(ctx, mgr, url, body)
	if err == nil && (status < 200 || status >= 300) {
		return nil, fmt.Errorf("billing HTTP %d", status)
	}
	return data, err
}

func postJSONStatus(ctx context.Context, mgr Credential, url string, body map[string]any) (map[string]any, int, error) {
	headers, err := billingHeaders(ctx, mgr)
	if err != nil {
		noteQuery(ctx, "credential", err)
		return nil, 0, err
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		noteQuery(ctx, "network", err)
		return nil, 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401:
		noteQuery(ctx, "unauthorized", nil)
	case resp.StatusCode == 403:
		noteQuery(ctx, "forbidden", nil)
	case resp.StatusCode >= 500:
		noteQuery(ctx, "upstream", nil)
	}
	var data map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			noteQuery(ctx, "invalid_response", nil)
		}
		return map[string]any{}, resp.StatusCode, nil
	}
	if code, ok := data["code"].(float64); ok && code == 12153 {
		noteQuery(ctx, "login_expired", nil)
	}
	return data, resp.StatusCode, nil
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
	node := billingData(data)
	if packages, ok := node["Packages"].([]any); ok && len(packages) > 0 {
		return normalizeSummaryAccounts(packages, false)
	}
	if groups, ok := node["CreditGroups"].([]any); ok {
		return normalizeSummaryAccounts(groups, true)
	}
	return nil
}

func billingData(data map[string]any) map[string]any {
	for i := 0; i < 4; i++ {
		found := false
		for _, key := range []string{"data", "Response", "Data"} {
			if next, ok := data[key].(map[string]any); ok {
				data = next
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return data
}

func normalizeSummaryAccounts(rows []any, group bool) []any {
	var out []any
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, hasRemain := row["CycleRemainCapacity"]; !hasRemain {
			continue
		}
		next := make(map[string]any, len(row)+4)
		for key, value := range row {
			next[key] = value
		}
		for _, pair := range [][2]string{{"CycleRemainCapacity", "CycleCapacityRemainPrecise"}, {"CycleTotalCapacity", "CycleCapacitySizePrecise"}, {"CycleUsedCapacity", "CycleCapacityUsedPrecise"}} {
			if value, exists := row[pair[0]]; exists {
				next[pair[1]] = value
			}
		}
		if group {
			next["_summary_group"] = true
			next["PackageName"] = row["Name"]
			next["PackageCode"] = "group:" + str(row["Kind"])
		}
		out = append(out, next)
	}
	return out
}

func mergeResourceAccounts(summary, details []any) []any {
	if len(summary) > 0 {
		row, _ := summary[0].(map[string]any)
		if row["_summary_group"] == true {
			return summary
		}
	}
	out := append([]any(nil), details...)
	codes := map[string]bool{}
	for _, raw := range details {
		row, _ := raw.(map[string]any)
		code := str(row["PackageCode"])
		if code == "" {
			code = str(row["SubProductCode"])
		}
		codes[code] = true
	}
	for _, raw := range summary {
		row, _ := raw.(map[string]any)
		code := str(row["PackageCode"])
		if code == "" {
			code = str(row["SubProductCode"])
		}
		if len(details) > 0 && (row["_summary_group"] == true || code == "" || codes[code]) {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return x
		}
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	}
	return 0
}

func knownCredit(value any) *float64 {
	var n float64
	var err error
	switch v := value.(type) {
	case float64:
		n = v
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		n, err = strconv.ParseFloat(v, 64)
	default:
		return nil
	}
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return &n
}

func parseTS(raw any, isMs bool) *float64 {
	if raw == nil || raw == "" {
		return nil
	}
	if isMs {
		f := num(raw) / 1000.0
		if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return &f
	}
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	s = strings.TrimSpace(s)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, billingTimezone)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return nil
		}
	}
	f := float64(t.Unix())
	if f <= 0 {
		return nil
	}
	return &f
}

func summarize(accounts []any) (remain, total float64) {
	for _, a := range accounts {
		acct, ok := a.(map[string]any)
		if !ok {
			continue
		}
		cycleRemain := precise(acct, "CycleCapacityRemain")
		cycleSize := precise(acct, "CycleCapacitySize")
		capRemain := precise(acct, "CapacityRemain")
		capSize := precise(acct, "CapacitySize")
		cycleUsed := precise(acct, "CycleCapacityUsed")
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
	}
	return remain, total
}

func precise(row map[string]any, key string) float64 {
	if value, ok := row[key+"Precise"]; ok && value != nil && value != "" {
		return num(value)
	}
	return num(row[key])
}

func creditFromAccounts(accounts []any) *Credit {
	remain, total := summarize(accounts)
	pkgs := summarizePackages(accounts)
	unknown := false
	for _, raw := range accounts {
		row, _ := raw.(map[string]any)
		if precise(row, "CycleCapacitySize") <= 0 && precise(row, "CapacitySize") <= 0 && (precise(row, "CycleCapacityRemain") > 0 || precise(row, "CapacityRemain") > 0) {
			unknown = true
		}
	}
	return &Credit{Remain: remain, Total: total, ExpireAt: earliestPackageExpiry(pkgs), Packages: pkgs, UnknownTotal: unknown}
}

func summarizePackages(accounts []any) []map[string]any {
	type packageKey struct {
		code   string
		expiry float64
	}
	groups := map[packageKey]map[string]any{}
	var order []packageKey
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
		code := str(acct["PackageCode"])
		if code == "" {
			code = str(acct["SubProductCode"])
		}
		if code == "" {
			code = "other"
		}
		expiry := parseTS(acct["DeductionEndTime"], true)
		if expiry == nil {
			expiry = parseTS(acct["ExpiredTime"], false)
		}
		if expiry == nil {
			expiry = parseTS(acct["CycleEndTime"], false)
		}
		key := packageKey{code: code}
		if expiry != nil {
			key.expiry = *expiry
		}
		g, exists := groups[key]
		if !exists {
			name := str(acct["PackageName"])
			if name == "" {
				name = code
			}
			g = map[string]any{"name": name, "remain": 0.0, "used": 0.0, "total": 0.0, "expire_at": nil}
			if expiry != nil {
				g["expire_at"] = *expiry
			}
			groups[key] = g
			order = append(order, key)
		}
		g["remain"] = g["remain"].(float64) + math.Max(remain, 0)
		g["used"] = g["used"].(float64) + math.Max(used, 0)
		g["total"] = g["total"].(float64) + math.Max(tot, 0)
	}
	var out []map[string]any
	for _, k := range order {
		g := groups[k]
		if g["total"].(float64) > 0 || g["remain"].(float64) > 0 {
			if g["total"].(float64) <= 0 {
				g["total"] = nil
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

// earliestPackageExpiry returns the soonest real per-package expiry among
// packages that still hold a positive balance — the truthful "credits will
// actually vanish" time. This is used for the account-level ExpireAt instead of
// a rolling boundary from an account summary. Returns nil when no package
// with a balance carries a known expiry.
func earliestPackageExpiry(pkgs []map[string]any) *float64 {
	var best *float64
	for _, g := range pkgs {
		if rem, _ := g["remain"].(float64); rem <= 0 {
			continue
		}
		ea, ok := g["expire_at"].(float64)
		if !ok {
			continue
		}
		if best == nil || ea < *best {
			e := ea
			best = &e
		}
	}
	return best
}

func fetchNewCredits(ctx context.Context, mgr Credential) *Credit {
	now := time.Now().In(billingTimezone)
	dayStart := now.Format("2006-01-02") + " 00:00:00"
	dayEnd := now.Format("2006-01-02") + " 23:59:59"
	base := mgr.Endpoint() + "/billing/meter"
	var accounts, summary []any
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
		pageAccounts, err := pagedAccounts(ctx, mgr, e.url, e.body)
		if err != nil {
			if errors.Is(err, errIncompleteBilling) {
				noteQuery(ctx, "incomplete", nil)
				return nil
			}
			continue
		}
		accounts = append(accounts, pageAccounts...)
		if e.url == base+"/get-user-resource-summary" {
			summary = pageAccounts
			accounts = nil
		}
	}
	accounts = mergeResourceAccounts(summary, accounts)
	if len(accounts) == 0 {
		return nil
	}
	return creditFromAccounts(accounts)
}

func fetchOldCredits(ctx context.Context, mgr Credential) *Credit {
	url := mgr.Endpoint() + "/v2/billing/meter/get-user-resource"
	now := time.Now().In(billingTimezone)
	body := map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca", "Status": []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.AddDate(10, 0, 0).Format("2006-01-02 15:04:05"),
	}
	accounts, err := pagedAccounts(ctx, mgr, url, body)
	if err != nil {
		if errors.Is(err, errIncompleteBilling) {
			noteQuery(ctx, "incomplete", nil)
		}
		return nil
	}
	if len(accounts) == 0 {
		return nil
	}
	return creditFromAccounts(accounts)
}

// Only read-only billing lists are paged. Incomplete lists must not replace a
// last-known balance. Summary endpoints without paging stay single-request.
func pagedAccounts(ctx context.Context, mgr Credential, url string, body map[string]any) ([]any, error) {
	_, paged := body["PageSize"]
	query := make(map[string]any, len(body))
	for k, v := range body {
		query[k] = v
	}
	var result []any
	var previous string
	for page := 1; page <= 100; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if paged {
			query["PageNumber"] = page
		}
		data, err := postJSON(ctx, mgr, url, query)
		if err != nil {
			if page > 1 {
				return nil, fmt.Errorf("%w: %v", errIncompleteBilling, err)
			}
			return nil, err
		}
		rows := extractAccounts(data)
		total := billingTotalCount(data)
		encoded, _ := json.Marshal(rows)
		if page > 1 && len(rows) > 0 && string(encoded) == previous {
			return nil, errIncompleteBilling
		}
		previous = string(encoded)
		result = append(result, rows...)
		if len(rows) == 0 && total > len(result) {
			return nil, errIncompleteBilling
		}
		if !paged || total > 0 && len(result) >= total || total <= 0 && len(rows) < 100 {
			return result, nil
		}
	}
	return nil, errIncompleteBilling
}

func billingTotalCount(data map[string]any) int {
	for _, path := range [][]string{{"data", "Response", "Data"}, {"data"}, {}} {
		m := data
		for _, k := range path {
			next, _ := m[k].(map[string]any)
			m = next
		}
		if n := num(m["TotalCount"]); n > 0 && n < 1<<30 {
			return int(n)
		}
	}
	return 0
}

// FetchCredits queries the account's spendable credit balance.
func FetchCredits(ctx context.Context, mgr Credential) (*Credit, error) {
	observation := &QueryError{Kind: "unavailable"}
	ctx = context.WithValue(ctx, queryKey{}, observation)
	if res := fetchNewCredits(ctx, mgr); res != nil {
		return res, nil
	}
	if res := fetchOldCredits(ctx, mgr); res != nil {
		return res, nil
	}
	return nil, observation
}

type queryKey struct{}
type QueryError struct {
	Kind  string
	cause error
}

func (e *QueryError) Unwrap() error { return e.cause }
func (e *QueryError) Error() string {
	switch e.Kind {
	case "login_expired":
		return "登录授权已失效，请重新授权"
	case "incomplete":
		return "额度明细读取不完整，保留上次额度"
	case "unauthorized":
		return "额度接口鉴权失败，请重新授权"
	case "credential":
		return "无法获取有效授权，请核实登录状态"
	case "network":
		return "额度查询网络异常，请稍后重试"
	case "forbidden":
		return "额度接口访问被拒绝，请核实上游权限或防护"
	case "upstream":
		return "上游额度服务暂时异常"
	case "invalid_response":
		return "额度响应格式异常"
	default:
		return "未能获取完整额度数据，请稍后重试"
	}
}
func noteQuery(ctx context.Context, kind string, cause error) {
	if q, ok := ctx.Value(queryKey{}).(*QueryError); ok {
		rank := map[string]int{"unavailable": 0, "invalid_response": 1, "incomplete": 1, "upstream": 2, "network": 3, "forbidden": 4, "unauthorized": 5, "credential": 6, "login_expired": 7}
		if rank[kind] >= rank[q.Kind] {
			q.Kind, q.cause = kind, cause
		}
	}
}

// DailyCheckin performs the daily check-in.
func DailyCheckin(ctx context.Context, mgr Credential) (CheckinResult, error) {
	url := mgr.Endpoint() + "/v2/billing/meter/daily-checkin"
	var data map[string]any
	var status int
	for attempt := 0; ; attempt++ {
		var err error
		data, status, err = postJSONStatus(ctx, mgr, url, map[string]any{})
		if err != nil {
			return CheckinResult{}, err
		}
		msg := strings.ToLower(str(data["msg"]) + " " + str(data["message"]))
		processing := strings.Contains(msg, "请求处理中") || strings.Contains(msg, "request is being processed") || strings.Contains(msg, "request processing")
		if status != http.StatusTooManyRequests || !processing || attempt >= 3 {
			break
		}
		timer := time.NewTimer([]time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return CheckinResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	// 必须存在明确的 code 字段才判成功——否则非 JSON / 网关 404 / WAF 拦截等
	// 会被 num(nil)==0 误判为「签到成功」（国际站无签到活动即属此类）。
	if status == http.StatusUnauthorized {
		probe, probeStatus, err := postJSONStatus(ctx, mgr, mgr.Endpoint()+"/billing/meter/get-user-resource-summary", map[string]any{})
		code := knownCredit(probe["code"])
		summary := billingData(probe)
		_, packages := summary["Packages"].([]any)
		_, groups := summary["CreditGroups"].([]any)
		_, accounts := summary["Accounts"].([]any)
		if err == nil && probeStatus >= 200 && probeStatus < 300 && code != nil && *code == 0 && (packages || groups || accounts) {
			return CheckinResult{Outcome: "ineligible", Message: "账号额度授权有效，但签到接口不接受当前客户端，请在官方客户端签到"}, nil
		}
		return CheckinResult{}, &QueryError{Kind: "unauthorized"}
	}
	if status >= 500 || status == http.StatusTooManyRequests {
		return CheckinResult{}, &QueryError{Kind: "upstream"}
	}
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
	validCode := false
	switch v := codeRaw.(type) {
	case float64:
		validCode = !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v
	case string:
		_, e := strconv.Atoi(v)
		validCode = e == nil
	case int:
		validCode = true
	}
	code := num(codeRaw)
	if validCode && code == 12153 {
		return CheckinResult{}, &QueryError{Kind: "login_expired"}
	}
	if validCode && code == 0 && status >= 200 && status < 300 {
		result := CheckinResult{OK: true, Message: "签到成功", Outcome: "claimed"}
		payload := billingData(data)
		result.Credit = knownCredit(payload["credit"])
		result.StreakDays = int(num(payload["streak_days"]))
		if result.Credit != nil {
			result.Message += "，获得 " + strconv.FormatFloat(*result.Credit, 'f', -1, 64) + " 积分"
		}
		if result.StreakDays > 0 {
			result.Message += fmt.Sprintf("，连续 %d 天", result.StreakDays)
		}
		return result, nil
	}
	if status >= 200 && status < 300 && validCode {
		switch int(code) {
		case 1001:
			return CheckinResult{Already: true, Outcome: "done", Message: "今日已签到"}, nil
		case 1002:
			return CheckinResult{Outcome: "ineligible", Message: "该账号暂不符合签到条件"}, nil
		case 1003:
			return CheckinResult{Outcome: "inactive", Message: "签到活动未开启"}, nil
		}
	}
	for _, k := range []string{"已签到", "already checked in", "already checked-in", "already signed in"} {
		if strings.Contains(strings.ToLower(msg), k) {
			return CheckinResult{OK: false, Message: msg, Already: true, Outcome: "done"}, nil
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("签到未完成（HTTP %d）", status)
	}
	return CheckinResult{OK: false, Message: msg}, nil
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
