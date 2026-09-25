// Package oauth implements the WorkBuddy/CodeBuddy browser device-authorization
// login, ported from workbuddy_one/oauth.py. The server exposes the non-blocking
// Begin/Poll primitives so the WebUI can render the auth URL (as a QR code) and
// poll for completion; on success the caller persists {auth, account} as an auth
// file. All three flow steps share one path shape and differ only by host and
// platform param — direct evidence that international support lives in the host
// and identity headers, not the protocol.
package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"work2api/internal/workbuddy/httpclient"
)

// Error is a recoverable-vs-not login error. IsFatal marks unrecoverable states
// (bad/expired state, 401/403) so pollers abort instead of spinning to timeout.
type Error struct {
	Msg   string
	Fatal bool
}

func (e *Error) Error() string { return e.Msg }

// siteHosts maps a site key to its login-entry host (mirrors auth.domain).
var siteHosts = map[string]string{
	"cn":             "https://www.codebuddy.cn", // 国内 CodeBuddy
	"intl":           "https://www.workbuddy.ai", // 国际 WorkBuddy
	"intl-codebuddy": "https://www.codebuddy.ai", // 国际 CodeBuddy
}

// sitePlatforms is the platform query param per site (official CLI uses "CLI";
// the WorkBuddy entry uses "workbuddy").
var sitePlatforms = map[string]string{
	"cn":             "CLI",
	"intl":           "workbuddy",
	"intl-codebuddy": "CLI",
}

const defaultSite = "cn"

// noAuthHeaders mimic the official plugin's unauthenticated markers.
var noAuthHeaders = map[string]string{
	"X-No-Authorization":   "true",
	"X-No-User-Id":         "true",
	"X-No-Enterprise-Id":   "true",
	"X-No-Department-Info": "true",
}

func siteKey(site string) string {
	k := strings.ToLower(strings.TrimSpace(site))
	if k == "" {
		return defaultSite
	}
	return k
}

// SiteHost returns the login host for a site, or an error for unknown sites (no
// silent fallback — logging into the wrong site is worse than failing).
func SiteHost(site string) (string, error) {
	k := siteKey(site)
	h, ok := siteHosts[k]
	if !ok {
		return "", &Error{Msg: "未知站点（仅支持 cn / intl / intl-codebuddy）", Fatal: true}
	}
	return h, nil
}

func sitePlatform(site string) (string, error) {
	k := siteKey(site)
	p, ok := sitePlatforms[k]
	if !ok {
		return "", &Error{Msg: "未知站点（仅支持 cn / intl / intl-codebuddy）", Fatal: true}
	}
	return p, nil
}

// Sites lists the supported site keys (for the WebUI dropdown).
func Sites() []string { return []string{"cn", "intl", "intl-codebuddy"} }

var httpClient = httpclient.New(15 * time.Second) // no proxy (mirrors trust_env=False)

// request performs one API call and unwraps the Tencent backend envelope
// {code,msg,data} (code=0 = success). fatal is set for HTTP 4xx / backend 4xx.
func request(method, site, path string, headers map[string]string, body map[string]any) (map[string]any, error) {
	host, err := SiteHost(site)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body == nil {
		body = map[string]any{}
	}
	buf, _ := json.Marshal(body)
	reader = bytes.NewReader(buf)

	req, err := http.NewRequest(method, host+path, reader)
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &Error{Msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(raw)), Fatal: resp.StatusCode < 500}
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, &Error{Msg: fmt.Sprintf("非 JSON 响应 %d: %s", resp.StatusCode, snippet(raw))}
	}
	if code, ok := data["code"]; ok {
		if n, _ := toFloat(code); n != 0 {
			// backend 4xx classes (e.g. 401/403/4xx state invalid) are fatal
			fatal := n >= 400 && n < 500
			return nil, &Error{Msg: fmt.Sprintf("后端错误 %v: %v", code, data["msg"]), Fatal: fatal}
		}
		return asMap(data["data"]), nil
	}
	return data, nil
}

// unwrap peels one optional {data:{...}} nesting layer.
func unwrap(m map[string]any) map[string]any {
	if inner, ok := m["data"].(map[string]any); ok {
		return inner
	}
	return m
}

func enterpriseHeaders(token map[string]any) map[string]string {
	h := map[string]string{}
	if d := str(token["domain"]); d != "" {
		h["X-Domain"] = d
	}
	if e := str(token["enterpriseId"]); e != "" {
		h["X-Enterprise-Id"] = e
		h["X-Tenant-Id"] = e
	}
	return h
}

// Begin starts a login and returns {state, authUrl, site} without polling. The
// WebUI renders authUrl (QR) and then calls Poll(state, site).
func Begin(site string) (map[string]any, error) {
	platform, err := sitePlatform(site)
	if err != nil {
		return nil, err
	}
	data, err := request("POST", site, "/v2/plugin/auth/state?platform="+url.QueryEscape(platform), noAuthHeaders, map[string]any{})
	if err != nil {
		return nil, err
	}
	sp := unwrap(data)
	authURL := str(sp["authUrl"])
	state := str(sp["state"])
	if authURL == "" || state == "" {
		return nil, &Error{Msg: "登录状态响应缺少 authUrl/state"}
	}
	return map[string]any{"state": state, "authUrl": authURL, "site": siteKey(site)}, nil
}

// Poll checks a login once. Returns {status:"pending"} until both token and
// account are ready, then {status:"ready", auth, account}. A fatal upstream
// error (bad state / 401 / 403) is returned as error so the caller can stop.
func Poll(state, site string) (map[string]any, error) {
	stateQ := url.QueryEscape(state)
	// 1) token
	t, err := request("GET", site, "/v2/plugin/auth/token?state="+stateQ, noAuthHeaders, nil)
	if err != nil {
		if fatal(err) {
			return nil, err
		}
		return map[string]any{"status": "pending"}, nil
	}
	token := unwrap(t)
	if str(token["accessToken"]) == "" {
		return map[string]any{"status": "pending"}, nil
	}
	// 2) account (may be prepared asynchronously)
	accHeaders := map[string]string{
		"Authorization":        "Bearer " + str(token["accessToken"]),
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}
	for k, v := range enterpriseHeaders(token) {
		accHeaders[k] = v
	}
	a, err := request("GET", site, "/v2/plugin/login/account?state="+stateQ, accHeaders, nil)
	if err != nil {
		if fatal(err) {
			return nil, err
		}
		return map[string]any{"status": "pending"}, nil
	}
	account := unwrap(a)
	if str(account["uid"]) == "" {
		return map[string]any{"status": "pending"}, nil
	}
	return map[string]any{"status": "ready", "auth": token, "account": account}, nil
}

func fatal(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Fatal
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%v", x)
	case json.Number:
		return x.String()
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		return 0, false
	}
	return 0, false
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
