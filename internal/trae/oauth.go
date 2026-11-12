package trae

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

type loginState struct {
	Product, Verifier, Machine, Device, Callback string
	Expires                                      time.Time
	Listener                                     net.Listener
	Code                                         string
	RefreshToken                                 string
	EnterpriseConsole                            bool
	Account                                      map[string]any
	Busy                                         bool
	OwnerID                                      int64
	Context                                      context.Context
	Cancel                                       context.CancelFunc
}

func randomID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (r *Runtime) OAuthOptions() []map[string]any {
	return []map[string]any{{"key": "product", "label": "产品", "values": []map[string]any{{"value": "cn", "label": "国内 Trae IDE"}, {"value": "solo", "label": "国内 Work/SOLO"}}}}
}
func (r *Runtime) OAuthBegin(opts map[string]any) (map[string]any, error) {
	return r.beginLogin(opts, 0)
}
func (r *Runtime) BeginOwnedLogin(opts map[string]any, userID int64) (map[string]any, error) {
	return r.beginLogin(opts, userID)
}
func (r *Runtime) beginLogin(opts map[string]any, ownerID int64) (map[string]any, error) {
	product := text(opts["product"])
	if product != "cn" && product != "solo" {
		return nil, errors.New("请选择国内 IDE 或 SOLO")
	}
	r.mu.Lock()
	for id, state := range r.logins {
		if time.Now().After(state.Expires) {
			state.Cancel()
			state.Listener.Close()
			delete(r.logins, id)
		}
	}
	full := len(r.logins) >= 64
	owned := 0
	for _, login := range r.logins {
		if login.OwnerID == ownerID && login.Account == nil {
			owned++
		}
	}
	if ownerID > 0 && owned >= 2 {
		full = true
	}
	r.mu.Unlock()
	if full {
		return nil, errors.New("授权任务过多，请稍后重试")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	id := randomID()
	verifier := randomID()
	sum := sha256.Sum256([]byte(verifier))
	callback := "http://" + listener.Addr().String() + "/authorize"
	state := &loginState{Product: product, Verifier: verifier, Machine: randomID(), Device: randomID(), Callback: callback, Expires: time.Now().Add(10 * time.Minute), Listener: listener, OwnerID: ownerID}
	state.Context, state.Cancel = context.WithDeadline(context.Background(), state.Expires)
	params := url.Values{"auth_from": {map[bool]string{true: "solo", false: "trae"}[product == "solo"]}, "client_id": {upstream.ClientIDFor(product)}, "login_trace_id": {id}, "auth_callback_url": {callback}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "machine_id": {state.Machine}, "device_id": {state.Device}, "app_version": {upstream.IdeVersion}, "plugin_version": {"1.0.0"}, "device_brand": {upstream.DeviceBrand}, "device_type": {"windows"}, "os_version": {upstream.OSVersion}, "env": {"prod"}, "app_type": {"desktop"}}
	if product == "solo" {
		params.Set("hide_saas_login", "false")
	}
	for key, value := range map[string]string{"login_version": "1", "login_channel": "native_ide", "auth_type": "local", "redirect": "0", "x_device_id": state.Device, "x_machine_id": state.Machine, "x_device_brand": upstream.DeviceBrand, "x_device_type": "windows", "x_os_version": upstream.OSVersion, "x_env": "prod", "x_app_version": upstream.IdeVersion, "x_app_type": "desktop"} {
		params.Set(key, value)
	}
	// buddy-proxy's domestic login implementation identifies this sentinel:
	// numeric plugin versions may lose the callback parameters during login.
	params.Set("plugin_version", "trae-handoff-1.0")
	r.mu.Lock()
	owned = 0
	for _, login := range r.logins {
		if login.OwnerID == ownerID && login.Account == nil {
			owned++
		}
	}
	if len(r.logins) >= 64 || ownerID > 0 && owned >= 2 {
		r.mu.Unlock()
		state.Cancel()
		listener.Close()
		return nil, errors.New("授权任务过多")
	}
	r.logins[id] = state
	r.mu.Unlock()
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origin := req.Header.Get("Origin")
		if origin != "" && origin != "https://www.trae.cn" {
			http.Error(w, "回调来源不匹配", 403)
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if req.Method == "OPTIONS" && req.URL.Path == "/authorize" {
			w.WriteHeader(204)
			return
		}
		if req.Method != "GET" || req.URL.Path != "/authorize" {
			http.NotFound(w, req)
			return
		}
		candidate := callback + "?" + req.URL.RawQuery
		if err := r.captureCallback(id, candidate, ownerID); err != nil {
			http.Error(w, "授权回调无效："+err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "授权回调已接收，请返回 work2api 完成验证。")
	})}
	go func() { _ = server.Serve(listener) }()
	go func() { <-state.Context.Done(); _ = server.Close() }()
	return map[string]any{"login_id": id, "login_url": "https://www.trae.cn/authorization?" + params.Encode(), "callback_url": callback, "manual_callback": true}, nil
}

func (r *Runtime) captureCallback(id, callback string, ownerID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.logins[id]
	if state == nil || state.OwnerID != ownerID || time.Now().After(state.Expires) {
		return errors.New("授权任务已失效")
	}
	u, err := url.Parse(callback)
	if err != nil {
		return errors.New("回调格式错误")
	}
	expected, _ := url.Parse(state.Callback)
	if u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path || u.User != nil || u.Fragment != "" {
		return errors.New("回调目标不匹配")
	}
	query := u.Query()
	trace := ""
	for _, key := range []string{"login_trace_id", "loginTraceID", "loginTraceId", "trace_id", "state"} {
		if value := query.Get(key); value != "" {
			if trace != "" && trace != value {
				return errors.New("回调任务标识冲突")
			}
			trace = value
		}
	}
	// The upstream requires /authorize. The random trace must bind this fixed
	// path to its task; the per-task listener supplies a second origin boundary.
	if trace != id {
		return errors.New("回调任务不匹配")
	}
	for _, key := range []string{"error", "error_code", "err", "errorCode"} {
		if query.Get(key) != "" {
			return errors.New("上游拒绝授权，请重新登录")
		}
	}
	if query.Get("isRedirect") == "false" {
		return errors.New("上游未完成授权")
	}
	code := ""
	for _, key := range []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"} {
		if value := query.Get(key); value != "" {
			if code != "" && code != value {
				return errors.New("授权码参数冲突")
			}
			code = value
		}
	}
	if code == "" {
		for _, key := range []string{"authCodeInfo", "auth_code_info", "AuthCodeInfo"} {
			value := query.Get(key)
			var info map[string]any
			if json.Unmarshal([]byte(value), &info) != nil {
				if decoded, e := url.QueryUnescape(value); e == nil {
					_ = json.Unmarshal([]byte(decoded), &info)
				}
			}
			for _, name := range []string{"authCode", "auth_code", "AuthCode", "authorization_code", "code"} {
				if code = text(info[name]); code != "" {
					break
				}
			}
			if code != "" {
				break
			}
		}
	}
	refresh := callbackRefreshToken(query)
	if code == "" && refresh == "" || len(code) > 8192 || len(refresh) > 32768 {
		return errors.New("回调缺少授权码")
	}
	if state.Busy {
		return errors.New("账号正在验证，请勿重复提交")
	}
	if state.Code != "" && state.Code != code {
		return errors.New("授权码已提交")
	}
	if state.RefreshToken != "" && state.RefreshToken != refresh {
		return errors.New("登录态已提交")
	}
	enterpriseHost := query.Get("host")
	var userInfo map[string]any
	_ = json.Unmarshal([]byte(query.Get("userInfo")), &userInfo)
	if enterpriseHost == "" {
		enterpriseHost, _ = userInfo["Host"].(string)
	}
	if enterpriseHost == "" {
		enterpriseHost, _ = userInfo["host"].(string)
	}
	if enterpriseHost == "" {
		enterpriseHost, _ = userInfo["APIHost"].(string)
	}
	if enterpriseHost == "" {
		enterpriseHost, _ = userInfo["apiHost"].(string)
	}
	if parsed, e := url.Parse(enterpriseHost); e == nil && parsed.Scheme == "https" && parsed.Hostname() == "console.enterprise.trae.cn" && parsed.User == nil {
		state.EnterpriseConsole = true
	}
	state.Code = code
	state.RefreshToken = refresh
	return nil
}

func callbackRefreshToken(query url.Values) string {
	for _, key := range []string{"credential", "userJwt"} {
		blob := query.Get(key)
		if len(blob) > 65536 {
			continue
		}
		var document map[string]any
		if json.Unmarshal([]byte(blob), &document) != nil {
			if decoded, err := url.QueryUnescape(blob); err == nil {
				_ = json.Unmarshal([]byte(decoded), &document)
			}
		}
		if nested, ok := document["userJwt"].(map[string]any); ok {
			if token := text(nested["RefreshToken"]); token != "" {
				return token
			}
		}
		for _, name := range []string{"RefreshToken", "refreshToken", "refresh_token"} {
			if token := text(document[name]); token != "" {
				return token
			}
		}
		if key == "credential" {
			inner, _ := url.ParseQuery(blob)
			if token := inner.Get("refreshToken"); token != "" {
				return token
			}
			if json.Unmarshal([]byte(inner.Get("userJwt")), &document) == nil {
				if token := text(document["RefreshToken"]); token != "" {
					return token
				}
			}
		}
	}
	for _, name := range []string{"refreshToken", "refresh_token", "RefreshToken", "refresh-token"} {
		if token := query.Get(name); token != "" {
			return token
		}
	}
	return ""
}
func (r *Runtime) OAuthComplete(id, callback string) error { return r.captureCallback(id, callback, 0) }
func (r *Runtime) CompleteOwnedLogin(id, callback string, userID int64) error {
	return r.captureCallback(id, callback, userID)
}
func (r *Runtime) CancelOwnedLogin(id string, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.logins[id]
	if state == nil || state.OwnerID != userID {
		return errors.New("授权任务不存在")
	}
	if state.Busy {
		return errors.New("账号正在验证，请等待验证结果后再操作")
	}
	state.Listener.Close()
	delete(r.logins, id)
	state.Cancel()
	return nil
}
func (r *Runtime) OAuthPoll(id string) (map[string]any, error) { return r.pollLogin(id, 0) }
func (r *Runtime) PollOwnedLogin(id string, userID int64) (map[string]any, error) {
	return r.pollLogin(id, userID)
}
func (r *Runtime) pollLogin(id string, ownerID int64) (map[string]any, error) {
	r.mu.Lock()
	state := r.logins[id]
	if state == nil || state.OwnerID != ownerID || time.Now().After(state.Expires) {
		r.mu.Unlock()
		return nil, errors.New("授权任务已失效")
	}
	if state.Account != nil {
		account := state.Account
		r.mu.Unlock()
		return map[string]any{"status": "ready", "account": account}, nil
	}
	if state.Code == "" && state.RefreshToken == "" || state.Busy {
		r.mu.Unlock()
		return map[string]any{"status": "pending"}, nil
	}
	state.Busy = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); state.Busy = false; r.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(state.Context, 30*time.Second)
	defer cancel()
	var raw []byte
	var err error
	if state.RefreshToken != "" {
		identity := &auth.Auth{RefreshToken: state.RefreshToken, Variant: state.Product, Domain: "trae.cn", APIHost: upstream.OAuthHost, MachineID: state.Machine, DeviceID: state.Device}
		if err = r.client(ctx).RefreshToken(identity); err != nil {
			return nil, errors.New("Trae 登录态交换失败，请重新授权")
		}
		raw, _ = json.Marshal(map[string]any{"accessToken": identity.AccessToken, "refreshToken": identity.RefreshToken, "expiresAt": identity.ExpiresAt, "domain": "trae.cn", "variant": state.Product, "enterpriseConsole": state.EnterpriseConsole, "machineId": state.Machine, "deviceId": state.Device})
	} else {
		public, private, err := auth.GenerateDeviceKeyPair()
		if err != nil {
			return nil, err
		}
		device := map[string]any{"DeviceID": state.Device, "MachineID": state.Machine, "PlatformCode": upstream.PlatformCodeFor(state.Product), "DeviceType": "PC", "DeviceName": "", "DeviceModel": upstream.DeviceBrand, "DeviceBrand": upstream.DeviceBrand, "ClientVersion": upstream.IdeVersion, "DevicePublicKey": public, "OSInfo": "windows", "OSVersion": upstream.OSVersion}
		body, _ := json.Marshal(map[string]any{"ClientID": upstream.ClientIDFor(state.Product), "AuthCode": state.Code, "CodeVerifier": state.Verifier, "DeviceInfo": device, "IDEVersion": upstream.IdeVersion})
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.trae.cn/trae/api/v3/oauth/ExchangeToken", bytes.NewReader(body))
		upstream.OAuthHeaders(req)
		response, err := r.client(ctx).HTTP.Do(req)
		if err != nil {
			return nil, errors.New("Trae 授权交换失败")
		}
		defer response.Body.Close()
		var document struct {
			Result struct {
				Token, RefreshToken                string
				TokenExpireAt, TokenExpireDuration int64
			}
		}
		if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&document) != nil || document.Result.Token == "" {
			return nil, errors.New("Trae 授权交换被拒绝，请重新授权")
		}
		expiry := document.Result.TokenExpireAt
		if expiry > 1e12 {
			expiry /= 1000
		}
		if expiry == 0 {
			expiry = time.Now().Unix() + document.Result.TokenExpireDuration
		}
		raw, _ = json.Marshal(map[string]any{"accessToken": document.Result.Token, "refreshToken": document.Result.RefreshToken, "expiresAt": expiry, "domain": "trae.cn", "variant": state.Product, "enterpriseConsole": state.EnterpriseConsole, "machineId": state.Machine, "deviceId": state.Device, "devicePublicKey": public, "devicePrivateKey": private})
	}
	var account map[string]any
	if ownerID > 0 {
		account, err = r.AddOwnedAccount(ctx, string(raw), map[string]any{"product": state.Product}, ownerID)
	} else {
		account, err = r.AddAccountByToken(ctx, string(raw), map[string]any{"product": state.Product})
	}
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	state.Account = account
	state.Code = ""
	state.RefreshToken = ""
	state.Verifier = ""
	state.Cancel()
	state.Listener.Close()
	r.mu.Unlock()
	return map[string]any{"status": "ready", "account": account}, nil
}
