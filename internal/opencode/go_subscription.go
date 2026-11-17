package opencode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/opencode/goauth"
	"work2api/internal/statebackup"
	"work2api/internal/streamwatch"
)

type subscriptionState struct {
	mu           sync.RWMutex
	connection   *goauth.Connection
	active       bool
	loadError    bool
	persistError bool
	authMu       sync.Mutex
	pending      *subscriptionPending
}

type subscriptionPending struct {
	token         *goauth.Token
	id            string
	device        goauth.Device
	credential    *goauth.Credential
	organizations []goauth.Organization
	next          time.Time
}

func (rt *Runtime) subscriptionPath() string { return rt.path + ".go-oauth.json" }

func (rt *Runtime) subscription() *subscriptionState {
	rt.subscriptionMu.Lock()
	defer rt.subscriptionMu.Unlock()
	if rt.goSubscription != nil {
		return rt.goSubscription
	}
	s := &subscriptionState{}
	f, err := os.Open(rt.subscriptionPath())
	if err == nil {
		defer f.Close()
		s.active = true
		var connection goauth.Connection
		d := json.NewDecoder(io.LimitReader(f, 2<<20))
		if d.Decode(&connection) == nil && d.Decode(new(any)) == io.EOF && connection.Validate() == nil {
			s.connection = &connection
		} else {
			s.loadError = true
		}
	} else if !os.IsNotExist(err) {
		s.active, s.loadError = true, true
	}
	rt.goSubscription = s
	return s
}

func (rt *Runtime) subscriptionSnapshot() (bool, *goauth.Connection) {
	s := rt.subscription()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active, s.connection
}

func subscriptionUID(c *goauth.Connection) string {
	if c == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(c.Credential.AccountID + "\x00" + c.Credential.OrganizationID))
	return "opencode:go-oauth:" + hex.EncodeToString(sum[:])
}

func (rt *Runtime) oauthClient() (*goauth.Client, error) {
	if !rt.ready {
		return goauth.NewClient(nil), nil
	}
	clients := rt.healthyClients()
	if len(clients) == 0 {
		return nil, errors.New("没有可用的 OpenCode 传输")
	}
	return goauth.NewClient(clients[0]), nil
}

func (rt *Runtime) currentSubscriptionWriter() error {
	if r := rt.registry.Load(); r != nil && !r.IsCurrent(rt) {
		return errors.New("渠道配置已变更，请刷新后重试")
	}
	if rt.lifecycleCtx != nil && rt.lifecycleCtx.Err() != nil {
		return errors.New("渠道配置已变更，请刷新后重试")
	}
	return nil
}

// saveMu also serializes token rotation with config replacement and disconnect.
func (rt *Runtime) writeSubscription(connection *goauth.Connection) error {
	leave := statebackup.Enter()
	defer leave()
	path := rt.subscriptionPath()
	if connection == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return errors.New("无法移除 Go 订阅凭据")
		}
		return nil
	}
	if err := connection.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(connection)
	if err != nil {
		return errors.New("无法保存 Go 订阅凭据")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("无法保存 Go 订阅凭据")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".go-oauth-*")
	if err != nil {
		return errors.New("无法保存 Go 订阅凭据")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("无法保存 Go 订阅凭据")
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return errors.New("无法保存 Go 订阅凭据")
	}
	return nil
}

func (rt *Runtime) installSubscription(ctx context.Context, c *goauth.Connection) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rt.currentSubscriptionWriter(); err != nil {
		return err
	}
	if err := rt.writeSubscription(c); err != nil {
		return err
	}
	s := rt.subscription()
	s.mu.Lock()
	s.active, s.connection, s.loadError = c != nil, c, false
	s.mu.Unlock()
	return nil
}

func (rt *Runtime) subscriptionView() map[string]any {
	active, c := rt.subscriptionSnapshot()
	v := map[string]any{"connected": active, "quota_known": false}
	state := rt.subscription()
	state.mu.RLock()
	v["storage_error"] = state.persistError
	state.mu.RUnlock()
	if c != nil {
		v["email"], v["organization"], v["expires_at"], v["models"], v["needs_login"] = c.Credential.Email, c.Credential.OrganizationName, c.Credential.ExpiresAt, len(c.Routes), c.NeedsLogin
	} else if active {
		v["needs_login"] = true
	}
	return v
}

func (rt *Runtime) DeviceAuthorization(ctx context.Context, action string, body map[string]any) (map[string]any, error) {
	s := rt.subscription()
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if err := rt.currentSubscriptionWriter(); err != nil {
		return nil, err
	}
	if s.pending != nil && time.Now().After(s.pending.device.ExpiresAt) {
		s.pending = nil
	}
	view := func() map[string]any {
		v := rt.subscriptionView()
		if p := s.pending; p != nil {
			status := "pending"
			if p.credential != nil {
				status = "choosing_org"
			}
			v["pending"] = map[string]any{"id": p.id, "status": status, "user_code": p.device.UserCode, "verification_url": p.device.VerificationURI, "expires_at": p.device.ExpiresAt, "retry_after_seconds": max(1, int(time.Until(p.next).Seconds()+1)), "organizations": p.organizations}
		}
		return v
	}
	switch action {
	case "status":
		return view(), nil
	case "disconnect":
		if err := rt.installSubscription(ctx, nil); err != nil {
			return nil, err
		}
		s.pending = nil
		return view(), nil
	case "cancel":
		s.pending = nil
		return view(), nil
	case "start":
		client, err := rt.oauthClient()
		if err != nil {
			return nil, err
		}
		device, err := client.Start(ctx)
		if err != nil {
			return nil, err
		}
		var nonce [24]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		s.pending = &subscriptionPending{id: hex.EncodeToString(nonce[:]), device: device, next: time.Now().Add(device.Interval)}
		return view(), nil
	case "poll":
		p := s.pending
		id, _ := body["id"].(string)
		if p == nil || p.id != id {
			return nil, errors.New("Go 订阅授权已过期，请重新发起")
		}
		client, err := rt.oauthClient()
		if err != nil {
			return nil, err
		}
		if p.credential == nil {
			if p.token == nil {
				if time.Now().Before(p.next) {
					return view(), nil
				}
				p.next = time.Now().Add(p.device.Interval)
				token, err := client.Poll(ctx, p.device.DeviceCode)
				if err != nil {
					var oauth *goauth.OAuthError
					if errors.As(err, &oauth) {
						if oauth.Code == "authorization_pending" || oauth.Code == "slow_down" {
							if oauth.Code == "slow_down" {
								p.device.Interval += 5 * time.Second
								p.next = time.Now().Add(p.device.Interval)
							}
							return view(), nil
						}
						if oauth.Code == "expired_token" || oauth.Code == "access_denied" || oauth.Code == "invalid_grant" {
							s.pending = nil
						}
					}
					return nil, err
				}
				p.token = &token
			}
			token := *p.token
			user, orgs, err := client.Identify(ctx, token)
			if err != nil {
				return nil, err
			}
			p.organizations = orgs
			p.credential = &goauth.Credential{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: token.ExpiresAt, AccountID: user.ID, Email: user.Email, OrganizationID: token.OrganizationID}
		}
		organization, _ := body["organization_id"].(string)
		if organization == "" {
			organization = p.credential.OrganizationID
		}
		if organization == "" && len(p.organizations) == 1 {
			organization = p.organizations[0].ID
		}
		if organization == "" && len(p.organizations) > 1 {
			return view(), nil
		}
		credential := *p.credential
		if organization != "" {
			found := false
			for _, org := range p.organizations {
				if org.ID == organization {
					credential.OrganizationID, credential.OrganizationName, found = org.ID, org.Name, true
					break
				}
			}
			if !found {
				return nil, errors.New("请选择本次授权允许的组织")
			}
		}
		routes, err := client.Discover(ctx, credential)
		if err != nil {
			return nil, err
		}
		connection := &goauth.Connection{Credential: credential, Routes: routes}
		if err = rt.installSubscription(ctx, connection); err != nil {
			return nil, err
		}
		if !rt.ready {
			if err = rt.SaveConfigDoc(map[string]any{}); err != nil {
				return nil, err
			}
		}
		s.pending = nil
		return view(), nil
	default:
		return nil, errors.New("不支持的 Go 订阅操作")
	}
}

func (rt *Runtime) subscriptionConnection(ctx context.Context, force ...*goauth.Connection) (*goauth.Connection, error) {
	forceRefresh := len(force) > 0 && force[0] != nil
	failedToken := ""
	if forceRefresh {
		failedToken = force[0].Credential.AccessToken
	}
	active, c := rt.subscriptionSnapshot()
	if !active || c == nil || c.NeedsLogin {
		return nil, provider.NewAPIError(503, "Go 订阅需要重新登录", "account_unavailable")
	}
	if (!forceRefresh || c.Credential.AccessToken != failedToken) && time.Until(c.Credential.ExpiresAt) > time.Minute {
		return c, nil
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	if err := rt.currentSubscriptionWriter(); err != nil {
		return nil, err
	}
	active, c = rt.subscriptionSnapshot()
	if !active || c == nil || c.NeedsLogin {
		return nil, provider.NewAPIError(503, "Go 订阅需要重新登录", "account_unavailable")
	}
	if (!forceRefresh || c.Credential.AccessToken != failedToken) && time.Until(c.Credential.ExpiresAt) > time.Minute {
		return c, nil
	}
	client, err := rt.oauthClient()
	if err != nil {
		return nil, err
	}
	credential, err := client.Refresh(ctx, c.Credential)
	next := *c
	if err != nil {
		var oauth *goauth.OAuthError
		if errors.As(err, &oauth) && (oauth.Code == "unauthorized" || oauth.Code == "sso_required" || oauth.Code == "access_denied" || oauth.Code == "invalid_grant" || oauth.Code == "expired_token") {
			next.NeedsLogin = true
			_ = rt.writeSubscription(&next)
			s := rt.subscription()
			s.mu.Lock()
			s.connection = &next
			s.mu.Unlock()
		}
		return nil, err
	}
	next.Credential = credential
	changedOrganization := credential.OrganizationID != c.Credential.OrganizationID
	if changedOrganization {
		next.NeedsLogin = true
	}
	persistErr := rt.writeSubscription(&next)
	state := rt.subscription()
	state.mu.Lock()
	state.connection = &next
	state.persistError = persistErr != nil
	state.mu.Unlock()
	if persistErr != nil && rt.logger != nil {
		rt.logger.Error("Go 订阅续期已保留在内存，凭据文件保存失败")
	}
	if changedOrganization {
		return nil, provider.NewAPIError(503, "Go 订阅组织已变化，请重新登录", "account_unavailable")
	}
	return &next, nil
}

func (rt *Runtime) goKeyCount() int {
	active, _ := rt.subscriptionSnapshot()
	if active || rt.goNodes == nil {
		return 0
	}
	return rt.goNodes.Len()
}

func (rt *Runtime) resolveRoute(model string, caller provider.Caller) (Route, error) {
	base, err := rt.catalog.Route(model, rt.zenNodes.Len() > 0, rt.goKeyCount() > 0, rt.cfg.Anonymous)
	active, c := rt.subscriptionSnapshot()
	if active && c == nil && rt.cfg.Prefer == TierGo {
		return Route{}, provider.NewAPIError(503, "Go 订阅需要重新登录", "account_unavailable")
	}
	if !active || c == nil {
		return base, err
	}
	if err == nil && rt.catalog.IsFreeModel(model) {
		return base, nil
	}
	remote, ok := c.Routes[model]
	if !ok || caller.UserID > 0 && !caller.AccountScope[subscriptionUID(c)] {
		return base, err
	}
	if err == nil && rt.cfg.Prefer == TierZen {
		return base, nil
	}
	return Route{ID: model, Tier: TierGo, Protocol: remote.Protocol, KeyTiers: []Tier{TierGo}}, nil
}

func (rt *Runtime) subscriptionModels(base []provider.CatalogModel, requested map[string]bool) []provider.CatalogModel {
	active, c := rt.subscriptionSnapshot()
	if !active || c == nil {
		return base
	}
	indexes := make(map[string]int, len(base))
	for i, m := range base {
		indexes[m.ID] = i
	}
	for model, route := range c.Routes {
		id := runtimePrefix + model
		if requested != nil && !requested[id] {
			continue
		}
		if i, exists := indexes[id]; exists {
			old, _ := base[i].Extra["account_uids"].([]string)
			base[i].Extra["account_uids"] = append(old, subscriptionUID(c))
			continue
		}
		md := rt.catalog.MetadataForTier(model, TierGo)
		base = append(base, provider.CatalogModel{ID: id, Name: model, Vision: hasImageModality(md.InputModalities), Modalities: md.InputModalities, Context: md.ContextWindow, MaxOutput: md.MaxOutput,
			Extra: map[string]any{"account_uids": []string{subscriptionUID(c)}, "tier": "go", "native_protocol": string(route.Protocol), "free": false, "upstream_free": false, "pricing_known": false, "subscription": true, "reasoning": md.Reasoning, "tool_call": md.ToolCall, "structured_output": md.StructuredOutput}})
	}
	sort.Slice(base, func(i, j int) bool { return base[i].ID < base[j].ID })
	return base
}

func (rt *Runtime) doSubscriptionUpstream(ctx context.Context, route Route, bodies routeBody, ids RequestIDs, attemptOffset int) (*http.Response, error, int) {
	c, err := rt.subscriptionConnection(ctx)
	if err != nil {
		return nil, err, 0
	}
	remote, ok := c.Routes[route.ID]
	if !ok || remote.Protocol != route.Protocol {
		return nil, provider.NewAPIError(400, "该模型不在当前 Go 订阅目录中", "model_unavailable"), 0
	}
	initialCredential := c.Credential
	uid := subscriptionUID(c)
	check := func() error {
		active, current := rt.subscriptionSnapshot()
		if !active || current == nil || current.NeedsLogin || subscriptionUID(current) != uid {
			return provider.NewAPIError(503, "Go 订阅连接已变更", "account_unavailable")
		}
		return provider.CheckDispatch(ctx, provider.AccountRef{Provider: runtimeName, LocalID: uid}, runtimePrefix+route.ID)
	}
	if err = check(); err != nil {
		return nil, err, 0
	}
	body, err := bodies(TierGo)
	if err != nil {
		return nil, err, 0
	}
	if route.Protocol == protocol.Responses {
		body, err = provider.PrepareDispatchBody(ctx, provider.AccountRef{Provider: runtimeName, LocalID: uid}, runtimePrefix+remote.ModelID, body)
		if err != nil {
			return nil, err, 0
		}
	}
	if remote.ModelID != route.ID {
		var payload map[string]json.RawMessage
		if err = json.Unmarshal(body, &payload); err != nil {
			return nil, err, 0
		}
		payload["model"], err = json.Marshal(remote.ModelID)
		if err != nil {
			return nil, err, 0
		}
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err, 0
		}
	}
	var proxy *proxyTransport
	for _, candidate := range rt.transports.items {
		if candidate.healthy.Load() {
			proxy = candidate
			break
		}
	}
	if proxy == nil {
		return nil, errors.New("没有可用的 OpenCode 传输"), 0
	}
	client := *proxy.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	attempts := 0
	attemptLimit := min(2, max(0, rt.cfg.Retry.MaxAttempts-attemptOffset))
	for attempts < attemptLimit {
		if attempts > 0 {
			if err = check(); err != nil {
				return nil, err, attempts
			}
			c, err = rt.subscriptionConnection(ctx, c)
			if err != nil {
				return nil, err, attempts
			}
		}
		if err = check(); err != nil {
			return nil, err, attempts
		}
		req, err := newUpstreamRequest(ctx, remote.BaseURL, remote.Protocol, body, ids, "")
		if err != nil {
			return nil, err, attempts
		}
		req.Header.Set("Authorization", "Bearer "+c.Credential.AccessToken)
		if c.Credential.OrganizationID != "" {
			req.Header.Set("x-org-id", c.Credential.OrganizationID)
		}
		if ids.AccountUID != nil {
			*ids.AccountUID = uid
		}
		if dispatched := provider.CurrentInvocation(ctx).Dispatched; dispatched != nil {
			dispatched(provider.AccountRef{Provider: runtimeName, LocalID: uid}, runtimePrefix+remote.ModelID)
		}
		streamwatch.StartAttempt(ctx)
		attempts++
		resp, err := doInferenceAttempt(&client, req, remote.Protocol, time.Duration(rt.cfg.Performance.AttemptTimeoutSeconds)*time.Second)
		if resp != nil {
			streamwatch.AttemptResult(ctx, resp.StatusCode)
		}
		if ctx.Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return nil, ctx.Err(), attempts
		}
		if err != nil {
			rt.syncProxyResult(ctx, proxy, remote.BaseURL, upstreamStatus(resp), err)
		} else if resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 400 {
			proxy.healthy.Store(true)
		}
		if resp != nil && resp.StatusCode == 401 && attempts < attemptLimit {
			resp.Body.Close()
			continue
		}
		if resp != nil && resp.StatusCode == 401 {
			rt.markSubscriptionUnauthorized(c)
		}
		if resp != nil {
			sanitizeSubscriptionResponse(resp, c.Credential, initialCredential)
		}
		if err != nil {
			err = &subscriptionResponseError{cause: err, message: redactSubscriptionValue(err.Error(), c.Credential, initialCredential)}
		}
		return resp, err, attempts
	}
	return nil, errors.New("Go 订阅调用未完成"), attempts
}

func (rt *Runtime) subscriptionAccount() map[string]any {
	active, c := rt.subscriptionSnapshot()
	if !active {
		return nil
	}
	uid, label, healthy := "opencode:go-oauth:unavailable", "Go 订阅 · 需要重新登录", false
	if c != nil {
		uid = subscriptionUID(c)
		label = "Go 订阅 · " + c.Credential.OrganizationName
		healthy = !c.NeedsLogin
	}
	return map[string]any{"id": uid, "label": label, "source": "native", "credential": true, "enabled": true, "healthy": healthy, "subscription": true, "quota_unit": "订阅", "credits_remaining": nil, "credits_total": nil}
}

var _ provider.DeviceAuthorizationRuntime = (*Runtime)(nil)

func (rt *Runtime) refreshSubscriptionCatalog(ctx context.Context) error {
	if registry := rt.registry.Load(); registry == nil || !registry.IsCurrent(rt) {
		return provider.ErrMaintenancePaused
	}
	active, _ := rt.subscriptionSnapshot()
	if !active {
		return nil
	}
	c, err := rt.subscriptionConnection(ctx)
	if err != nil {
		return err
	}
	client, err := rt.oauthClient()
	if err != nil {
		return err
	}
	routes, err := client.Discover(ctx, c.Credential)
	if err != nil {
		return err
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	if err = rt.currentSubscriptionWriter(); err != nil {
		return err
	}
	state := rt.subscription()
	state.mu.RLock()
	current := state.connection
	state.mu.RUnlock()
	if current != c {
		return provider.ErrMaintenancePaused
	}
	if maps.Equal(c.Routes, routes) {
		return nil
	}
	next := *c
	next.Routes = routes
	if err = rt.writeSubscription(&next); err != nil {
		return err
	}
	state.mu.Lock()
	state.connection = &next
	state.persistError = false
	state.mu.Unlock()
	return nil
}

func (rt *Runtime) markSubscriptionUnauthorized(expected *goauth.Connection) {
	saveMu.Lock()
	defer saveMu.Unlock()
	if rt.currentSubscriptionWriter() != nil {
		return
	}
	state := rt.subscription()
	state.mu.RLock()
	current := state.connection
	state.mu.RUnlock()
	if current != expected {
		return
	}
	next := *current
	next.NeedsLogin = true
	persistErr := rt.writeSubscription(&next)
	state.mu.Lock()
	state.connection = &next
	state.persistError = persistErr != nil
	state.mu.Unlock()
}

type subscriptionResponseError struct {
	cause   error
	message string
}

func (e *subscriptionResponseError) Error() string { return e.message }
func (e *subscriptionResponseError) Unwrap() error { return e.cause }

func redactSubscriptionValue(value string, credentials ...goauth.Credential) string {
	var secrets []string
	for _, credential := range credentials {
		secrets = append(secrets, credential.AccessToken, credential.RefreshToken)
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		value = strings.ReplaceAll(value, secret, "***")
		encoded, _ := json.Marshal(secret)
		value = strings.ReplaceAll(value, string(encoded[1:len(encoded)-1]), "***")
	}
	return value
}
func sanitizeSubscriptionResponse(response *http.Response, credentials ...goauth.Credential) {
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Request = nil
	response.Status = redactSubscriptionValue(response.Status, credentials...)
	if response.StatusCode >= 400 {
		var body []byte
		var err error
		if response.Body != nil {
			body, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
		}
		if err != nil || len(body) >= 1<<20 {
			body = []byte(`{"error":{"message":"Go subscription upstream request was rejected"}}`)
			response.Header.Set("Content-Type", "application/json")
			response.Header.Del("Content-Encoding")
		} else {
			body = []byte(redactSubscriptionValue(string(body), credentials...))
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		response.ContentLength = int64(len(body))
		response.TransferEncoding = nil
		response.Header.Del("Content-Length")
	}
	for _, header := range []http.Header{response.Header, response.Trailer} {
		for name, values := range header {
			safeValues := make([]string, len(values))
			for i, value := range values {
				safeValues[i] = redactSubscriptionValue(value, credentials...)
			}
			safeName := redactSubscriptionValue(name, credentials...)
			if safeName != name {
				delete(header, name)
			}
			header[safeName] = safeValues
		}
	}
}
