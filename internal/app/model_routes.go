package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"work2api/internal/config"
	"work2api/internal/streamwatch"

	"work2api/internal/core/provider"
	"work2api/internal/store"
)

func physicalModel(id string) string {
	if !strings.Contains(id, "/") {
		return "workbuddy/" + id
	}
	return id
}

func (o *Orchestrator) modelRoute(model string) (*store.ModelRoute, error) {
	if o.db == nil {
		return nil, nil
	}
	routes, err := o.db.ModelRoutes()
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if route.Model == model {
			return &route, nil
		}
	}
	return nil, nil
}

func (s *Server) physicalCatalog() map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, entry := range s.o.runtimeModels(context.Background()) {
		out[physicalModel(str2(entry["id"]))] = entry
	}
	return out
}

func (s *Server) physicalCatalogForMembers(members []string) map[string]map[string]any {
	selected := make(map[string]bool, len(members))
	channels := make(map[string]bool, len(members))
	var runtimes []provider.Runtime
	for _, member := range members {
		selected[member] = true
		if rt, ok := s.o.runtimes.Resolve(member); ok && !channels[rt.Name()] {
			channels[rt.Name()] = true
			runtimes = append(runtimes, rt)
		}
	}
	out := make(map[string]map[string]any, len(members))
	for _, entry := range runtimeModelsFrom(context.Background(), runtimes, func(id string) bool { return selected[physicalModel(id)] }) {
		out[physicalModel(str2(entry["id"]))] = entry
	}
	return out
}

func (s *Server) routeCatalog(ctx context.Context) []map[string]any {
	routes, err := s.o.db.ModelRoutes()
	if err != nil {
		return nil
	}
	catalog := s.physicalCatalog()
	out := []map[string]any{}
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		if entry := routeCatalogEntry(route, route.Members, catalog); entry != nil {
			out = append(out, entry)
		}
	}
	return out
}

func modelHasVision(m map[string]any) bool {
	return m["vision"] == true || m["supports_image"] == true || m["modality"] == "multimodal"
}
func routeCatalogEntry(route store.ModelRoute, members []string, catalog map[string]map[string]any) map[string]any {
	vision := false
	known := 0
	contextLimit, outputLimit := 0, 0
	contextKnown, outputKnown := true, true
	for _, member := range members {
		m := catalog[member]
		if m == nil || m["model_type"] == "image" || m["model_type"] == "video" || m["model_type"] == "audio" {
			continue
		}
		known++
		vision = vision || modelHasVision(m)
		c := firstInt(m, "context_length", "context_window")
		o := firstInt(m, "max_output_tokens", "max_output")
		contextKnown = contextKnown && c > 0
		outputKnown = outputKnown && o > 0
		if c > 0 && (contextLimit == 0 || c < contextLimit) {
			contextLimit = c
		}
		if o > 0 && (outputLimit == 0 || o < outputLimit) {
			outputLimit = o
		}
	}
	if known == 0 {
		return nil
	}
	if !contextKnown {
		contextLimit = 0
	}
	if !outputKnown {
		outputLimit = 0
	}
	modality := "text"
	if vision {
		modality = "multimodal"
	}
	return map[string]any{"id": route.Model, "name": route.Name, "provider": "unified", "owned_by": "unified", "object": "model", "vision": vision, "supports_image": vision, "modality": modality, "context_length": contextLimit, "max_output_tokens": outputLimit, "members": members, "route_id": route.ID}
}

func (s *Server) routeCandidates(p *Principal, route store.ModelRoute, payload map[string]any) ([]string, *apiError) {
	return s.routeCandidatesIn(p, route, payload, s.physicalCatalog())
}
func (s *Server) routeCandidatesIn(p *Principal, route store.ModelRoute, payload map[string]any, catalog map[string]map[string]any) ([]string, *apiError) {
	return s.routeCandidatesWithFacts(p, route, inspectRouteRequest(payload), catalog)
}

type routeRequestFacts struct {
	hasImages       bool
	requestedOutput int
}

func inspectRouteRequest(payload map[string]any) routeRequestFacts {
	return routeRequestFacts{requestHasImages(payload), firstInt(payload, "max_tokens", "max_output_tokens", "max_completion_tokens")}
}

func (p *Principal) routeRequestFacts(payload map[string]any) routeRequestFacts {
	if p.routeFacts != nil {
		return *p.routeFacts
	}
	return inspectRouteRequest(payload)
}

func (s *Server) routeCandidatesWithFacts(p *Principal, route store.ModelRoute, facts routeRequestFacts, catalog map[string]map[string]any) ([]string, *apiError) {
	if p == nil {
		p = &Principal{}
	}
	if !route.Enabled {
		return nil, errBody(503, "统一模型已停用", "model_unavailable")
	}
	settings, err := s.o.db.GetSettings()
	if err != nil {
		return nil, errBody(503, "模型权限查询失败", "server_error")
	}
	aliases := parseModelAliases(settings["model_aliases"])
	resolve := func(model string) string {
		if real, ok := aliases[model]; ok {
			model = real
		}
		return strings.TrimPrefix(model, "workbuddy/")
	}
	if p.UserID > 0 {
		for _, m := range parseJSONStringArray(settings["portal_disabled_models"]) {
			if m == route.Model {
				return nil, errBody(403, "统一模型已禁用", "model_not_allowed")
			}
		}
	}
	grants := map[string]bool{}
	if p.AppID > 0 && len(p.AllowedModels) > 0 {
		grants, err = s.o.db.AppRouteMembers(p.AppID, route.ID)
		if err != nil {
			return nil, errBody(503, "统一模型授权查询失败", "server_error")
		}
	}
	logicalAllowed := false
	for _, m := range p.AllowedModels {
		if m == route.Model {
			logicalAllowed = true
		}
	}
	out := []string{}
	explicitMembers := map[string]bool{}
	for _, allowed := range p.AllowedModels {
		if allowed != route.Model {
			explicitMembers[physicalModel(resolve(allowed))] = true
		}
	}
	for _, member := range route.Members {
		entry := catalog[member]
		if entry == nil || entry["model_type"] == "image" || entry["model_type"] == "video" || entry["model_type"] == "audio" {
			continue
		}
		resolved := resolve(member)
		rt, ok := s.o.runtimes.Resolve(resolved)
		if !ok || !rt.Ready() {
			continue
		}
		keyAllowed := p.UserID > 0 && p.ModelPolicy == "follow" || p.AppID == 0 || p.UserID == 0 && len(p.AllowedModels) == 0 || len(p.AllowedModels) > 0 && modelAllowed(p, member, resolved) && !logicalAllowed
		// A separately granted physical member remains usable even when another
		// route member was added after the logical grant was captured.
		if explicitMembers[member] {
			keyAllowed = true
		}
		if logicalAllowed && grants[member] {
			keyAllowed = true
		}
		if !keyAllowed {
			continue
		}
		if p.UserID > 0 {
			portal, ok := rt.(provider.PortalProvider)
			if !ok || !portal.SupportsPortal() || !p.PortalModels[resolved] || len(p.ModelScopes[resolved]) == 0 {
				continue
			}
		}
		if facts.hasImages && !modelHasVision(entry) {
			continue
		}
		if max := firstInt(entry, "max_output_tokens", "max_output"); max > 0 {
			if facts.requestedOutput > max {
				continue
			}
		}
		out = append(out, member)
	}
	return out, nil
}

func requestHasImages(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		kind, _ := x["type"].(string)
		if kind == "image" || kind == "image_url" || kind == "input_image" {
			return true
		}
		for key, value := range x {
			if key == "arguments" || key == "parameters" {
				continue
			}
			if requestHasImages(value) {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if requestHasImages(item) {
				return true
			}
		}
	}
	return false
}

func catalogNameExists(name string, catalog map[string]map[string]any) bool {
	return catalog[physicalModel(name)] != nil || catalog["workbuddy/"+name] != nil
}

func suggestModelRoutes(routes []store.ModelRoute, catalog map[string]map[string]any, aliases map[string]string) []store.ModelRoute {
	groups := map[string][]string{}
	order := map[string]int{"workbuddy": 0, "qoder": 1, "opencode": 2, "trae": 3}
	for id, entry := range catalog {
		channel, name, ok := strings.Cut(id, "/")
		if _, known := order[channel]; !ok || !known || name == "" || name == "auto" || name == "default" {
			continue
		}
		if entry["model_type"] == "image" || entry["model_type"] == "video" || entry["model_type"] == "audio" {
			continue
		}
		groups[name] = append(groups[name], id)
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	used := map[string]bool{}
	covered := map[string]bool{}
	for _, route := range routes {
		used[route.Model] = true
		for _, m := range route.Members {
			covered[m] = true
		}
	}
	out := []store.ModelRoute{}
	for _, name := range names {
		members := groups[name]
		if len(members) < 2 {
			continue
		}
		manual := false
		for _, m := range members {
			manual = manual || covered[m]
		}
		if manual {
			continue
		}
		model := name
		if catalogNameExists(model, catalog) || aliases[model] != "" {
			model = "group/" + name
		}
		if used[model] || aliases[model] != "" || catalog[model] != nil || !routeNamePattern.MatchString(model) || strings.Contains(model, "//") {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			a, _, _ := strings.Cut(members[i], "/")
			b, _, _ := strings.Cut(members[j], "/")
			if order[a] != order[b] {
				return order[a] < order[b]
			}
			return members[i] < members[j]
		})
		out = append(out, store.ModelRoute{Model: model, Name: name, Enabled: true, Members: members})
		used[model] = true
	}
	return out
}

func (s *Server) adminModelRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := s.o.db.ModelRoutes()
	if err != nil {
		writeAPIErr(w, errBody(503, "配置读取失败", "server_error"))
		return
	}
	catalog := s.physicalCatalog()
	models := []map[string]any{}
	for id, entry := range catalog {
		if entry["model_type"] == "image" || entry["model_type"] == "video" || entry["model_type"] == "audio" {
			continue
		}
		models = append(models, map[string]any{"id": id, "name": entry["name"], "provider": entry["provider"], "vision": entry["vision"], "context_length": entry["context_length"], "max_output_tokens": entry["max_output_tokens"]})
	}
	sort.Slice(models, func(i, j int) bool { return str2(models[i]["id"]) < str2(models[j]["id"]) })
	w.Header().Set("Cache-Control", "no-store")
	settings, err := s.o.db.GetSettings()
	if err != nil {
		writeAPIErr(w, errBody(503, "配置读取失败", "server_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"routes": routes, "models": models, "suggestions": suggestModelRoutes(routes, catalog, parseModelAliases(settings["model_aliases"]))})
}

var routeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func (s *Server) adminSaveModelRoute(w http.ResponseWriter, r *http.Request) {
	var route store.ModelRoute
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if d.Decode(&route) != nil || d.Decode(&struct{}{}) != io.EOF {
		writeAPIErr(w, errBody(400, "统一模型配置格式错误", "invalid_request_error"))
		return
	}
	route.Model, route.Name = strings.TrimSpace(route.Model), strings.TrimSpace(route.Name)
	if !routeNamePattern.MatchString(route.Model) || strings.Contains(route.Model, "//") || len([]rune(route.Name)) > 100 || len(route.Members) == 0 {
		writeAPIErr(w, errBody(400, "请输入有效调用名称，并选择实际模型成员", "invalid_request_error"))
		return
	}
	if route.Name == "" {
		route.Name = route.Model
	}
	if _, ok := s.o.runtimes.ForModel(route.Model); ok || strings.HasPrefix(route.Model, "workbuddy/") || route.Model == "auto" {
		writeAPIErr(w, errBody(400, "调用名称不能占用渠道命名空间或 auto", "invalid_request_error"))
		return
	}
	catalog := s.physicalCatalog()
	settings, settingsErr := s.o.db.GetSettings()
	if settingsErr != nil {
		writeAPIErr(w, errBody(503, "模型配置读取失败", "server_error"))
		return
	}
	if catalogNameExists(route.Model, catalog) || parseModelAliases(settings["model_aliases"])[route.Model] != "" {
		writeAPIErr(w, errBody(409, "名称与现有模型或别名冲突，请换一个调用名称", "invalid_request_error"))
		return
	}
	existing, err := s.o.modelRoute(route.Model)
	if err != nil {
		writeAPIErr(w, errBody(503, "配置读取失败", "server_error"))
		return
	}
	if existing != nil && existing.ID != route.ID {
		writeAPIErr(w, errBody(409, "统一模型名称已存在", "invalid_request_error"))
		return
	}
	seen := map[string]bool{}
	for _, m := range route.Members {
		if m != physicalModel(m) || seen[m] {
			writeAPIErr(w, errBody(400, "成员须使用带渠道前缀的唯一模型 ID", "invalid_request_error"))
			return
		}
		seen[m] = true
		if entry := catalog[m]; entry != nil && (entry["model_type"] == "image" || entry["model_type"] == "video" || entry["model_type"] == "audio") {
			writeAPIErr(w, errBody(400, "聊天合并路由不支持媒体生成模型", "invalid_request_error"))
			return
		}
		// Keep previously configured temporarily unavailable members during edits.
		retained := false
		if existing != nil {
			for _, old := range existing.Members {
				if old == m {
					retained = true
				}
			}
		}
		if catalog[m] == nil && !retained {
			writeAPIErr(w, errBody(400, "成员不在当前模型目录："+m, "invalid_request_error"))
			return
		}
	}
	id, err := s.o.db.SaveModelRoute(route)
	if err != nil {
		writeAPIErr(w, errBody(409, "保存失败，配置可能已变更，请刷新后重试", "configuration_conflict"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "message": "已保存；新增成员不会自动扩大已有受限 Key 的授权"})
}

func (s *Server) adminDeleteModelRoute(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	revision, _ := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if id <= 0 || revision <= 0 {
		writeAPIErr(w, errBody(400, "缺少配置版本", "invalid_request_error"))
		return
	}
	if s.o.db.DeleteModelRoute(id, revision) != nil {
		writeAPIErr(w, errBody(409, "配置已变更，请刷新后重试", "configuration_conflict"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type routeBinding struct {
	Member string
	At     time.Time
}
type routeAffinity struct {
	mu       sync.Mutex
	bindings map[string]routeBinding
}

func (a *routeAffinity) member(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.bindings[key]
	if time.Since(b.At) > 30*time.Minute {
		return ""
	}
	return b.Member
}
func (a *routeAffinity) bind(key, member string) {
	if key == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bindings == nil {
		a.bindings = map[string]routeBinding{}
	}
	now := time.Now()
	for k, v := range a.bindings {
		if now.Sub(v.At) > 30*time.Minute {
			delete(a.bindings, k)
		}
	}
	if len(a.bindings) >= 2000 {
		var oldKey string
		old := now
		for k, v := range a.bindings {
			if v.At.Before(old) {
				oldKey, old = k, v.At
			}
		}
		delete(a.bindings, oldKey)
	}
	a.bindings[key] = routeBinding{member, now}
}

// Error responses are held until a retry decision. Successful streams are
// forwarded immediately; once their headers/body are sent there is no replay.
type routeWriter struct {
	dst       http.ResponseWriter
	header    http.Header
	status    int
	started   bool
	errorBody bytes.Buffer
	line      []byte
	logical   string
	stream    bool
	limit     int
}

func (w *routeWriter) Unwrap() http.ResponseWriter { return w.dst }
func (w *routeWriter) Header() http.Header         { return w.header }
func (w *routeWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *routeWriter) start() {
	if w.started {
		return
	}
	for k, v := range w.header {
		w.dst.Header()[k] = append([]string(nil), v...)
	}
	w.dst.Header().Del("Content-Length")
	status := w.status
	if status == 0 {
		status = 200
	}
	w.dst.WriteHeader(status)
	w.started = true
}
func rewriteRouteModel(data []byte, logical string) []byte {
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&obj) != nil {
		return data
	}
	changed := false
	if _, ok := obj["model"]; ok {
		obj["model"] = logical
		changed = true
	}
	if response, ok := obj["response"].(map[string]any); ok {
		if _, ok := response["model"]; ok {
			response["model"] = logical
			changed = true
		}
	}
	if !changed {
		return data
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return raw
}
func (w *routeWriter) Write(data []byte) (int, error) {
	if w.status >= 400 {
		if w.errorBody.Len()+len(data) > 1<<20 {
			return 0, io.ErrShortBuffer
		}
		return w.errorBody.Write(data)
	}
	limit := w.limit
	if w.stream {
		limit = streamwatch.DefaultMaxLineBytes
	}
	if !w.stream && limit > 0 && len(w.line)+len(data) > limit {
		return 0, fmt.Errorf("统一模型响应超过已配置的响应缓冲上限")
	}
	w.start()
	if !w.stream {
		w.line = append(w.line, data...)
		return len(data), nil
	}
	w.line = append(w.line, data...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			if len(w.line) > limit {
				return 0, fmt.Errorf("SSE单行超过解析上限")
			}
			break
		}
		if i > limit {
			return 0, fmt.Errorf("SSE单行超过解析上限")
		}
		line := w.line[:i+1]
		if bytes.HasPrefix(line, []byte("data:")) {
			raw := bytes.TrimSpace(line[5:])
			if !bytes.Equal(raw, []byte("[DONE]")) {
				line = append(append([]byte("data: "), rewriteRouteModel(raw, w.logical)...), '\n')
			}
		}
		if _, err := w.dst.Write(line); err != nil {
			return 0, err
		}
		w.line = w.line[i+1:]
	}
	return len(data), nil
}
func (w *routeWriter) Flush() { _ = w.FlushError() }

func (w *routeWriter) FlushError() error {
	if w.status >= 400 {
		return nil
	}
	w.start()
	return http.NewResponseController(w.dst).Flush()
}
func (w *routeWriter) finish() {
	if !w.started {
		w.start()
		_, _ = w.dst.Write(w.errorBody.Bytes())
	}
	if len(w.line) > 0 {
		data := w.line
		if !w.stream {
			data = rewriteRouteModel(data, w.logical)
		}
		_, _ = w.dst.Write(data)
		w.line = nil
	}
}

// Signed or encrypted provider state must stay on the channel which produced it.
func routeOpaqueContinuation(payload map[string]any) bool {
	if id, _ := payload["previous_response_id"].(string); id != "" {
		return true
	}
	var opaque func(any) bool
	opaque = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if value, _ := x["encrypted_content"].(string); value != "" {
				return true
			}
			if x["type"] == "thinking" || x["type"] == "redacted_thinking" {
				if value, _ := x["signature"].(string); value != "" {
					return true
				}
				if x["type"] == "redacted_thinking" {
					return true
				}
			}
			for key, value := range x {
				if key != "arguments" && key != "parameters" && opaque(value) {
					return true
				}
			}
		case []any:
			for _, value := range x {
				if opaque(value) {
					return true
				}
			}
		}
		return false
	}
	return opaque(payload["input"]) || opaque(payload["messages"])
}

func cloneRoutePayload(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	out := make(map[string]any, len(body))
	for key, value := range body {
		out[key] = cloneRouteValue(value)
	}
	return out
}

// Route inputs have already been decoded as JSON. Copy mutable containers so
// adapters cannot change another attempt's history; immutable strings can share
// their backing storage without re-encoding the entire conversation.
func cloneRouteValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneRoutePayload(value)
	case []any:
		if value == nil {
			return []any(nil)
		}
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneRouteValue(item)
		}
		return out
	default:
		return value
	}
}

func (s *Server) dispatchModelRoute(w http.ResponseWriter, r *http.Request, proto provider.Protocol, payload map[string]any, p *Principal) bool {
	route, err := s.o.modelRoute(p.RouteModel)
	if err != nil || route == nil {
		writeAPIErr(w, errBody(503, "统一模型配置不可用", "model_unavailable"))
		return true
	}
	facts := p.routeRequestFacts(payload)
	members, aerr := s.routeCandidatesWithFacts(p, *route, facts, s.physicalCatalogForMembers(route.Members))
	if aerr != nil {
		writeAPIErr(w, aerr)
		return true
	}
	if len(members) == 0 {
		writeAPIErr(w, errBody(403, "没有符合当前权限和能力的成员", "model_not_allowed"))
		return true
	}
	key := requestSessionKey(r, p, route.Model, payload)
	sticky := s.o.routes.member(key)
	for i, m := range members {
		if m == sticky {
			copy(members[1:i+1], members[:i])
			members[0] = m
			break
		}
	}
	// Provider-owned server continuation identifiers cannot be replayed to an
	// arbitrary channel after the sticky member is unavailable.
	if routeOpaqueContinuation(payload) {
		if sticky == "" || members[0] != sticky {
			writeAPIErr(w, errBody(400, "服务端续接需要原渠道会话；请使用实际渠道模型或完整对话历史", "invalid_request_error"))
			return true
		}
		members = members[:1]
	}
	if len(members) > 2 {
		members = members[:2]
	}
	for i, member := range members {
		if r.Context().Err() != nil {
			return true
		}
		current, readErr := s.o.modelRoute(route.Model)
		if readErr != nil || current == nil {
			writeAPIErr(w, errBody(503, "统一模型配置不可用", "model_unavailable"))
			return true
		}
		freshMembers, permissionErr := s.routeCandidatesWithFacts(p, *current, facts, s.physicalCatalogForMembers(current.Members))
		if permissionErr != nil {
			writeAPIErr(w, permissionErr)
			return true
		}
		allowed := false
		for _, candidate := range freshMembers {
			allowed = allowed || candidate == member
		}
		if !allowed {
			if i < len(members)-1 {
				continue
			}
			writeAPIErr(w, errBody(403, "成员资格已改变，请重新发起请求", "model_not_allowed"))
			return true
		}
		body := cloneRoutePayload(payload)
		body["model"] = member
		attempt := *p
		attempt.RouteModel = ""
		attempt.EffectiveModel = s.o.resolveModel(member)
		if p.UserID > 0 {
			attempt.AccountScope = p.ModelScopes[attempt.EffectiveModel]
		}
		attemptRequest := r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &attempt))
		req, finish, prepErr := s.prepareProviderRequest(attemptRequest, body, &attempt)
		if prepErr != nil {
			status, doc := errToHTTP(prepErr)
			writeJSON(w, status, doc)
			return true
		}
		if diagnostic := diagnostic(req.Context()); diagnostic != nil {
			diagnostic.RequestedModel = route.Model
			diagnostic.PhysicalModel = member
		}
		proxy := &routeWriter{dst: w, header: make(http.Header), logical: route.Model, stream: boolVal(payload["stream"]), limit: positiveOr(int(s.o.cfg.MaxResponseBytes), config.DefaultMaxResponseBytes)}
		result := &routeAttemptResult{}
		req = req.WithContext(context.WithValue(req.Context(), routeAttemptResultKey{}, result))
		s.dispatchRuntimeTo(proxy, req, proto, body, &attempt)
		finish()
		if proxy.started || proxy.status < 400 {
			proxy.finish()
			if proxy.status < 400 && result.ok {
				s.o.routes.bind(key, member)
			}
			return true
		}
		if i == len(members)-1 || proxy.status != 429 && proxy.status < 500 {
			proxy.finish()
			return true
		}
	}
	return true
}

type routeAttemptResultKey struct{}
type routeAttemptResult struct{ ok bool }
