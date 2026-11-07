// Package trae integrates domestic IDE and SOLO accounts with the common gateway.
package trae

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/statebackup"
	"work2api/internal/store"
	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

type accountState struct {
	Auth                     *auth.Auth
	Identities               map[string]*auth.Auth
	Catalogs                 map[string][]upstream.ModelInfo
	CatalogTimes             map[string]time.Time
	CatalogErrors            map[string]bool
	PreferredProduct         string
	Enabled                  bool
	Alias                    string
	Priority                 int
	Models                   []upstream.ModelInfo
	CatalogAt                time.Time
	Cooldown                 time.Time
	CooldownKind             upstream.ErrKind
	Failures                 int
	LastUsed                 float64
	Remaining, Total, Expiry *float64
	QuotaAt                  time.Time
	QuotaFailed              bool
	QuotaProbeAt             time.Time
	QuotaProbeDone           chan struct{}
	CreditPackages           []map[string]any
	CheckinDate              string
	CheckinPending           string
}

type Runtime struct {
	maintenanceGuard  *provider.MaintenanceGuard
	mu                sync.Mutex
	writeMu           sync.Mutex
	credentialFlights map[*auth.Auth]*credentialFlight
	credentialDirty   map[*auth.Auth]bool
	accounts          map[string]*accountState
	dir               string
	db                *store.DB
	transport         *http.Transport
	affinities        map[string]string
	affinityUsed      map[string]time.Time
	affinityOrder     map[string]uint64
	affinityClock     uint64
	logins            map[string]*loginState
	lastMaintenance   time.Time
	checkinPermit     chan struct{}
}

// One upstream UID is one ownership/quota/concurrency resource. Product
// credentials remain separate and may only serve their own verified catalog.
func (a *accountState) mergeModels() {
	byID := map[string]upstream.ModelInfo{}
	for _, product := range []string{"cn", "solo"} {
		for _, model := range a.Catalogs[product] {
			key := strings.ToLower(model.ID)
			if previous, exists := byID[key]; exists {
				model = mergeModelInfo(previous, model)
			}
			byID[key] = model
		}
	}
	a.Models = nil
	for _, model := range byID {
		a.Models = append(a.Models, model)
	}
	sort.Slice(a.Models, func(i, j int) bool { return a.Models[i].ID < a.Models[j].ID })
}
func (a *accountState) modelIdentity(model string) (*auth.Auth, string) {
	return a.modelIdentityFor(model, false)
}
func (a *accountState) modelIdentityFor(model string, image bool, available ...func(*auth.Auth) bool) (*auth.Auth, string) {
	products := []string{a.Auth.Variant}
	for _, p := range []string{"cn", "solo"} {
		if p != a.Auth.Variant {
			products = append(products, p)
		}
	}
	var fallback *auth.Auth
	var fallbackModel string
	for _, product := range products {
		identity := a.Identities[product]
		if identity == nil || len(available) > 0 && !available[0](identity) {
			continue
		}
		for _, m := range a.Catalogs[product] {
			if strings.EqualFold(m.ID, model) {
				if !image {
					return a.Identities[product], m.ID
				}
				if m.Multimodal != nil {
					if *m.Multimodal {
						return a.Identities[product], m.ID
					}
				} else if fallback == nil {
					fallback, fallbackModel = a.Identities[product], m.ID
				}
			}
		}
	}
	return fallback, fallbackModel
}

func New(dataDir string, db *store.DB) (*Runtime, error) {
	dir := filepath.Join(dataDir, "trae")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	r := &Runtime{dir: dir, db: db, accounts: map[string]*accountState{}, affinities: map[string]string{}, affinityUsed: map[string]time.Time{}, logins: map[string]*loginState{}}
	r.transport = &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConns: 64, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 120 * time.Second}
	accounts, err := auth.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if validateAuth(a) != nil {
			continue
		}
		id := accountID(a.UID)
		if a.UID == "" {
			continue
		}
		if existing := r.accounts[id]; existing != nil {
			if current := existing.Identities[a.Variant]; current != nil && current.FilePath == filepath.Join(dir, id+"-"+a.Variant+".json") {
				continue
			}
			existing.Identities[a.Variant] = a
			if existing.Auth.Variant == a.Variant {
				existing.Auth = a
			}
			continue
		}
		state := &accountState{Auth: a, Enabled: true, Identities: map[string]*auth.Auth{a.Variant: a}, Catalogs: map[string][]upstream.ModelInfo{}, CatalogTimes: map[string]time.Time{}, CatalogErrors: map[string]bool{}}
		if row, e := db.GetAccount(id); e != nil {
			return nil, e
		} else if row != nil {
			if row["provider"] != "trae" {
				return nil, errors.New("Trae 账号标识冲突")
			}
			state.Enabled = number(row["enabled"]) != 0
			state.Alias = text(row["alias"])
			state.Priority = int(number(row["priority"]))
			state.CheckinDate = text(row["last_checkin_date"])
			state.PreferredProduct = text(row["profile"])
		}
		if sent, e := r.checkinSent(id, time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")); e == nil && sent {
			state.CheckinPending = time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
		}
		r.accounts[id] = state
	}
	for id, a := range r.accounts {
		if preferred := a.Identities[a.PreferredProduct]; preferred != nil {
			a.Auth = preferred
		}
		for product := range a.Identities {
			r.loadCatalog(id, product, a)
		}
		a.mergeModels()
	}
	return r, nil
}

func accountID(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return "trae-" + hex.EncodeToString(sum[:])
}
func validateAuth(a *auth.Auth) error {
	if a == nil || a.JWT() == "" {
		return errors.New("缺少 Trae accessToken")
	}
	if a.Variant != "cn" && a.Variant != "solo" {
		return errors.New("请选择国内 IDE 或 SOLO 授权身份")
	}
	if a.Domain != "" && a.Domain != "trae.cn" && a.Domain != "www.trae.cn" {
		return errors.New("暂不支持该 Trae 区域")
	}
	// Never allow imported metadata or OAuth callbacks to choose network targets.
	a.Domain = "trae.cn"
	a.APIHost = upstream.OAuthHost
	return nil
}
func text(v any) string { s, _ := v.(string); return s }
func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}
func nowSec() float64                   { return float64(time.Now().UnixNano()) / 1e9 }
func (r *Runtime) Name() string         { return "trae" }
func (r *Runtime) Prefix() string       { return "trae/" }
func (r *Runtime) SupportsPortal() bool { return true }

func (r *Runtime) PortalModels(_ context.Context, models map[string]bool, _ bool, _ func(string) bool) []provider.PortalModel {
	r.mu.Lock()
	catalog := r.modelsLocked(models)
	r.mu.Unlock()
	var out []provider.PortalModel
	for _, m := range catalog {
		uids, _ := m.Extra["account_uids"].([]string)
		out = append(out, provider.NewPortalModel(m.ID, uids, false))
	}
	return out
}
func (r *Runtime) Close() error {
	r.mu.Lock()
	for id, login := range r.logins {
		if login.Cancel != nil {
			login.Cancel()
		}
		login.Listener.Close()
		delete(r.logins, id)
	}
	r.mu.Unlock()
	r.transport.CloseIdleConnections()
	return nil
}
func (r *Runtime) StateSources() []provider.StateSource {
	return []provider.StateSource{{Path: r.dir, Required: true}}
}
func (r *Runtime) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a.Enabled {
			return true
		}
	}
	return false
}

type contextTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req.WithContext(t.ctx))
}
func (r *Runtime) client(ctx context.Context) *upstream.Client {
	c := upstream.New()
	t := contextTransport{ctx: ctx, base: r.transport}
	noRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	c.HTTP = &http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	c.StreamHTTP = &http.Client{Transport: t, CheckRedirect: noRedirect}
	return c
}
func (r *Runtime) ensure(ctx context.Context, a *auth.Auth) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, captured := a.TryMaintenanceIdentity()
	r.mu.Lock()
	available := r.identityAvailableLocked(a)
	r.mu.Unlock()
	if !available {
		return provider.ErrMaintenanceLogin
	}
	err := r.refreshCredentials(ctx, a, false)
	if err != nil && captured {
		r.observeMaintenance(a, "auth", before, err)
	}
	return err
}

func (r *Runtime) Models(context.Context) []provider.CatalogModel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.modelsLocked(nil)
}

func (r *Runtime) modelsLocked(requested map[string]bool) []provider.CatalogModel {
	merged := map[string]provider.CatalogModel{}
	for id, a := range r.accounts {
		if !a.Enabled {
			continue
		}
		for _, m := range a.Models {
			name := "trae/" + strings.ToLower(m.ID)
			if requested != nil && !requested[name] {
				continue
			}
			m = displayPricing(a, m)
			entry := merged[name]
			if entry.ID == "" {
				entry = provider.CatalogModel{ID: name, Name: m.Name, Context: int(m.ContextWindow), MaxOutput: int(m.MaxTokens), Extra: map[string]any{"account_uids": []string{}}}
				applyModelCapabilities(&entry, m)
			} else {
				mergeCatalogCapabilities(&entry, m)
			}
			entry.Extra["account_uids"] = append(entry.Extra["account_uids"].([]string), id)
			entry.Extra["catalog_stale"] = entry.Extra["catalog_stale"] == true || catalogStale(a)
			merged[name] = entry
		}
	}
	keys := make([]string, 0, len(merged))
	for id := range merged {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	out := make([]provider.CatalogModel, 0, len(keys))
	for _, id := range keys {
		out = append(out, merged[id])
	}
	return out
}

func (r *Runtime) RefreshAccount(ctx context.Context, id string) error {
	r.mu.Lock()
	a := r.accounts[id]
	r.mu.Unlock()
	if a == nil {
		return errors.New("账号不存在")
	}
	r.mu.Lock()
	identities := make(map[string]*auth.Auth, len(a.Identities))
	for product, identity := range a.Identities {
		identities[product] = identity
	}
	r.mu.Unlock()
	var failures []error
	for product, identity := range identities {
		if err := r.ensureMaintenance(ctx, identity); err != nil {
			r.markCatalogFailure(id, product, a)
			failures = append(failures, err)
			continue
		}
		fetch := func(ctx context.Context) error {
			fingerprint := identity.MaintenanceIdentity()
			models, err := r.client(ctx).FetchModels(identity)
			r.observeMaintenance(identity, "catalog", fingerprint, err)
			if err != nil {
				return err
			}
			if identity.MaintenanceIdentity() != fingerprint {
				return provider.ErrMaintenancePaused
			}
			return r.commitCatalog(id, product, identity, models)
		}
		ran, err := r.authGuard().Recover(ctx, id+":"+product+":catalog", identity.MaintenanceIdentity(), fetch)
		if err == nil && !ran {
			err = fetch(ctx)
		}
		if err != nil {
			r.markCatalogFailure(id, product, a)
			failures = append(failures, errors.New("Trae 模型目录刷新失败；保留上次成功目录"))
			continue
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(failures...)
}
func (r *Runtime) RefreshModels(ctx context.Context) error {
	r.mu.Lock()
	ids := []string{}
	for id := range r.accounts {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.RefreshAccount(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Import validates identity and catalog before persisting. The authorization
// wrapper can serialize this operation with contribution ownership changes.
func (r *Runtime) AddAccountByToken(ctx context.Context, token string, opts map[string]any) (map[string]any, error) {
	return r.importAccount(ctx, token, opts, 0)
}
func (r *Runtime) AddOwnedAccount(ctx context.Context, token string, opts map[string]any, userID int64) (map[string]any, error) {
	if userID <= 0 {
		return nil, errors.New("缺少账号归属")
	}
	return r.importAccount(ctx, token, opts, userID)
}
func (r *Runtime) importAccount(ctx context.Context, token string, opts map[string]any, ownerID int64) (map[string]any, error) {
	a, err := auth.Parse([]byte(token))
	if err != nil {
		return nil, errors.New("请输入 Trae 导出的 JSON 凭据")
	}
	if variant := text(opts["product"]); variant != "" {
		if a.Variant != "" && a.Variant != variant {
			return nil, errors.New("所选产品与凭据授权身份不同，请选择凭据对应的产品")
		}
		a.Variant = variant
	}
	if err := validateAuth(a); err != nil {
		return nil, err
	}
	uid, nickname, enterprise, err := r.client(ctx).GetUserInfo(a)
	if err != nil || uid == "" {
		return nil, errors.New("Trae 凭据验证失败")
	}
	a.UID = uid
	a.Nickname = nickname
	a.EnterpriseID = enterprise
	models, err := r.client(ctx).FetchModels(a)
	if err != nil || len(models) == 0 {
		return nil, errors.New("Trae 账号暂无可验证的模型目录")
	}
	id := accountID(uid)
	// Admin import cannot overwrite a user contribution, even if its old token
	// was withdrawn or revoked. Only the owner flow explicitly opts into restore.
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	leave := statebackup.Enter()
	defer leave()
	owner, err := r.db.ContributionByAccount(id)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if owner != nil && ownerID != owner.UserID {
		return nil, errors.New("账号已有用户归属，不能覆盖")
	}
	if ownerID > 0 {
		if owner != nil && owner.Status != "active" && owner.Status != "private" && owner.Status != "verifying" {
			changed, e := r.db.SetContributionStatus(owner.ID, ownerID, owner.Status, "verifying", "本人重新授权")
			if e != nil {
				return nil, e
			}
			if !changed {
				return nil, store.ErrConflict
			}
			owner.Status = "verifying"
		}
		if owner == nil {
			// A platform credential cannot be claimed by user import.
			if existing, e := r.db.GetAccount(id); e != nil {
				return nil, e
			} else if existing != nil {
				return nil, errors.New("该账号已由平台管理，不能认领")
			}
			contribution, e := r.db.CreateContribution(ownerID, id, "trae", "domestic", "verifying")
			if e != nil {
				return nil, e
			}
			owner = &store.Contribution{ID: contribution, UserID: ownerID, AccountUID: id, Provider: "trae", Status: "verifying"}
		}
	}
	a.FilePath = filepath.Join(r.dir, id+"-"+a.Variant+".json")
	r.mu.Lock()
	previous := r.accounts[id]
	r.mu.Unlock()
	if _, err = r.db.UpsertProviderAccount(store.ProviderAccount{UID: id, Provider: "trae", Nickname: nickname, EnterpriseID: enterprise, Domain: "trae.cn", Profile: a.Variant, Auth: map[string]any{}}); err != nil {
		return nil, err
	}
	if err = a.SaveAtomic(); err != nil {
		return nil, err
	}
	if previous != nil {
		if old := previous.Identities[a.Variant]; old != nil && old.FilePath != a.FilePath && filepath.Dir(old.FilePath) == r.dir {
			if err = os.Remove(old.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	r.mu.Lock()
	state := &accountState{Auth: a, PreferredProduct: a.Variant, Enabled: true, Models: models, CatalogAt: time.Now(), Identities: map[string]*auth.Auth{}, Catalogs: map[string][]upstream.ModelInfo{}, CatalogTimes: map[string]time.Time{}, CatalogErrors: map[string]bool{}}
	if previous != nil {
		for product, identity := range previous.Identities {
			state.Identities[product] = identity
		}
		for product, catalog := range previous.Catalogs {
			state.Catalogs[product] = catalog
		}
		state.CheckinDate, state.CheckinPending = previous.CheckinDate, previous.CheckinPending
		state.Enabled = previous.Enabled
		state.Alias = previous.Alias
		state.Priority = previous.Priority
		for product, at := range previous.CatalogTimes {
			state.CatalogTimes[product] = at
		}
		for product, failed := range previous.CatalogErrors {
			state.CatalogErrors[product] = failed
		}
		state.Remaining, state.Total, state.Expiry = previous.Remaining, previous.Total, previous.Expiry
		state.QuotaAt, state.QuotaFailed, state.CreditPackages = previous.QuotaAt, previous.QuotaFailed, previous.CreditPackages
	}
	state.Identities[a.Variant] = a
	state.Catalogs[a.Variant] = models
	state.CatalogTimes[a.Variant] = state.CatalogAt
	state.mergeModels()
	r.accounts[id] = state
	r.mu.Unlock()
	if err := r.writeCatalog(id, a.Variant, models, state.CatalogAt); err != nil {
		r.mu.Lock()
		state.CatalogErrors[a.Variant] = true
		r.mu.Unlock()
		return nil, err
	}
	if ownerID > 0 && owner.Status == "verifying" {
		if err = r.db.ActivateContribution(owner.ID, ownerID); err != nil {
			return nil, err
		}
		if previous != nil && !previous.Enabled {
			if err = r.db.SetAccountState(id, map[string]any{"enabled": false}); err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"id": id, "uid": id, "label": nickname, "product": a.Variant, "provider": "trae"}, nil
}

func (r *Runtime) change(id string, fields map[string]any, apply func(*accountState)) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	leave := statebackup.Enter()
	defer leave()
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.accounts[id]
	if a == nil {
		return errors.New("账号不存在")
	}
	if err := r.db.SetAccountState(id, fields); err != nil {
		return err
	}
	apply(a)
	return nil
}
func (r *Runtime) SetAccountEnabled(id string, enabled bool) error {
	return r.change(id, map[string]any{"enabled": enabled}, func(a *accountState) { a.Enabled = enabled })
}
func (r *Runtime) RenameAccount(id, name string) error {
	if len([]rune(name)) > 64 {
		return errors.New("别名过长")
	}
	return r.change(id, map[string]any{"alias": strings.TrimSpace(name)}, func(a *accountState) { a.Alias = strings.TrimSpace(name) })
}
func (r *Runtime) SetAccountPriority(id string, n int) error {
	if n < 0 || n > 1000 {
		return errors.New("优先级需为 0–1000")
	}
	return r.change(id, map[string]any{"priority": n}, func(a *accountState) { a.Priority = n })
}
func (r *Runtime) DeleteAccount(id string) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	leave := statebackup.Enter()
	defer leave()
	owner, err := r.db.ContributionByAccount(id)
	if err != nil {
		return err
	}
	if owner != nil {
		return errors.New("用户账号请通过贡献管理撤权；不能删除归属")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.accounts[id]
	if a == nil {
		return errors.New("账号不存在")
	}
	if err := r.db.SetAccountState(id, map[string]any{"enabled": false}); err != nil {
		return err
	}
	a.Enabled = false
	for _, identity := range a.Identities {
		if err := os.Remove(identity.FilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(r.catalogPath(id, identity.Variant)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(r.checkinMarker(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := r.db.DeleteAccount(id); err != nil {
		return err
	}
	delete(r.accounts, id)
	return nil
}

func (r *Runtime) AdminData(context.Context) provider.AdminData {
	guard := r.authGuard()
	r.mu.Lock()
	defer r.mu.Unlock()
	data := provider.AdminData{DisplayName: "Trae", Capabilities: []string{"accounts", "models", "credits", "checkin", "add_account", "oauth", "local_import"}, Accounts: []map[string]any{}, Resources: []provider.ResourceSummary{}, Notes: "添加国内 IDE 或 Work/SOLO 账号；产品身份保留，模型调用自动选择符合条件的账号。"}
	data.Maintenance = []provider.MaintenanceOption{{Key: "trae_auto_quota", Label: "自动刷新 Trae 额度", Description: "启动自动读取一次；开启后每 30 分钟刷新，未知额度保持未知"}, {Key: "trae_auto_checkin", Label: "自动 Trae 签到", Description: "仅使用已授权设备身份，不轮换设备"}}
	for id, a := range r.accounts {
		label := a.Alias
		if label == "" {
			label = a.Auth.Nickname
		}
		healthy := a.Enabled && !time.Now().Before(a.Cooldown)
		row := map[string]any{"checkin_pending": a.CheckinPending == time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02") && a.CheckinDate != a.CheckinPending, "id": id, "uid": id, "provider": "trae", "label": label, "nickname": a.Auth.Nickname, "alias": a.Alias, "source": "import", "product": a.Auth.Variant, "site": "domestic", "enabled": a.Enabled, "healthy": healthy, "priority": a.Priority, "credits_remaining": a.Remaining, "credits_total": a.Total, "credits_expire_at": a.Expiry, "quota_stale": time.Since(a.QuotaAt) > 30*time.Minute, "quota_refresh_failed": a.QuotaFailed, "last_checkin_date": a.CheckinDate, "quota_unit": "credits", "credit_packages": a.CreditPackages, "catalog_stale": catalogStale(a)}
		pending := []map[string]any{}
		for _, product := range []string{"cn", "solo", "intl"} {
			identity := a.Identities[product]
			if identity == nil {
				continue
			}
			fingerprint, ready := identity.TryMaintenanceIdentity()
			if !ready {
				continue
			}
			for _, scope := range []string{"auth", "catalog", "quota", "checkin"} {
				state := guard.State(id+":"+product+":"+scope, fingerprint)
				if state.Kind != "" {
					pending = append(pending, state.Public(product+" · "+map[string]string{"auth": "授权", "catalog": "模型目录", "quota": "额度", "checkin": "签到"}[scope]))
				}
			}
		}
		if len(pending) > 0 {
			row["maintenance"] = pending
		}
		active, invalid := 0, 0
		for _, identity := range a.Identities {
			active++
			if !r.identityAvailableLocked(identity) {
				invalid++
			}
		}
		if invalid > 0 {
			row["auth_invalid"] = invalid == active
			row["auth_partial"] = invalid < active
			row["healthy"] = healthy && invalid < active
		}
		row["failure_count"] = a.Failures
		if time.Now().Before(a.Cooldown) {
			row["cooldown_until"] = float64(a.Cooldown.UnixMilli()) / 1000
			row["cooldown_kind"] = a.CooldownKind.String()
		}
		if !a.QuotaAt.IsZero() {
			row["quota_updated_at"] = float64(a.QuotaAt.Unix())
		}
		catalogStatus := map[string]any{}
		for product := range a.Identities {
			at := a.CatalogTimes[product]
			status := map[string]any{"model_count": len(a.Catalogs[product]), "refresh_failed": a.CatalogErrors[product], "stale": time.Since(at) > 30*time.Minute || a.CatalogErrors[product]}
			if !at.IsZero() {
				status["updated_at"] = float64(at.Unix())
			}
			catalogStatus[product] = status
		}
		row["catalog_status"] = catalogStatus
		products := []string{}
		for _, p := range []string{"cn", "solo"} {
			if a.Identities[p] != nil {
				products = append(products, p)
			}
		}
		row["products"] = products
		data.Accounts = append(data.Accounts, row)
		budget := provider.CreditBudget(row, "credits", label, "积分", float64(time.Now().Unix()))
		budget.EstimateUnavailable = "缺少逐次调用消费依据"
		data.Budgets = append(data.Budgets, budget)
		actions := map[string]provider.ResourceAction{"rename": {Label: "别名", Enabled: true}, "enabled": {Label: "启用", Enabled: true}, "priority": {Label: "优先级", Enabled: true}, "delete": {Label: "删除", Enabled: true, Confirmation: "删除网关导入的账号凭据？"}}
		data.Resources = append(data.Resources, provider.ResourceMetadata(row, "account", actions))
		data.Ready = data.Ready || a.Enabled
	}
	sort.Slice(data.Accounts, func(i, j int) bool { return text(data.Accounts[i]["id"]) < text(data.Accounts[j]["id"]) })
	data.Models = []map[string]any{}
	for _, model := range r.modelsLocked(nil) {
		item := map[string]any{"id": model.ID, "name": model.Name, "provider": "trae", "context_length": model.Context, "max_output_tokens": model.MaxOutput, "vision": model.Vision}
		for key, value := range model.Extra {
			if key != "account_uids" {
				item[key] = value
			}
		}
		data.Models = append(data.Models, item)
	}
	healthyCount := 0
	for _, account := range data.Accounts {
		if account["healthy"] == true {
			healthyCount++
		}
	}
	data.Status = map[string]any{"account_count": len(data.Accounts), "healthy_count": healthyCount, "model_count": len(data.Models)}
	return data
}

func (r *Runtime) Warm(ctx context.Context) {
	// Balances are memory-only. Hydrate once on startup independently of the
	// optional periodic refresh, without querying disabled accounts.
	r.mu.Lock()
	ids := []string{}
	for id, a := range r.accounts {
		if a.Enabled {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = r.refreshQuota(probeCtx, id, false)
		cancel()
	}
	_ = r.RefreshModels(ctx)
}
func (r *Runtime) Maintain(ctx context.Context, settings map[string]string, now time.Time) {
	r.mu.Lock()
	due := now.Sub(r.lastMaintenance) >= 30*time.Minute
	if due {
		r.lastMaintenance = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	_ = r.RefreshModels(ctx)
	if settings["trae_auto_quota"] == "1" {
		_, _ = r.AdminRefreshCredits(ctx)
	}
	if settings["trae_auto_checkin"] == "1" {
		_, _ = r.AdminCheckin(ctx)
	}
}

func (r *Runtime) selectAccount(req provider.ServeRequest, model, session string, tried, busy map[string]bool) (string, error) {
	return r.selectAccountFor(req, model, session, tried, busy, false)
}
func (r *Runtime) selectAccountFor(req provider.ServeRequest, model, session string, tried, busy map[string]bool, image bool) (string, error) {
	excluded, err := r.db.PrivatePoolExcludedUIDs()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	choices := []provider.Candidate{}
	waiting := false
	for id, a := range r.accounts {
		if !a.Enabled || tried[id] || time.Now().Before(a.Cooldown) {
			continue
		}
		if req.Caller.UserID > 0 || req.Caller.TestAccount != "" {
			if !req.Caller.AccountScope[id] {
				continue
			}
		} else if excluded[id] {
			continue
		}
		if identity, _ := a.modelIdentityFor(model, image, r.identityAvailableLocked); identity == nil {
			continue
		}
		if a.Remaining != nil && *a.Remaining == 0 {
			continue
		}
		if busy[id] {
			waiting = true
			continue
		}
		days := math.Inf(1)
		amount := 0.0
		if a.Expiry != nil {
			days = (*a.Expiry - nowSec()) / 86400
			if a.Remaining != nil {
				amount = *a.Remaining
			}
		}
		choices = append(choices, provider.Candidate{ID: id, Domestic: true, Remaining: a.Remaining, Total: a.Total, ExpiryDays: days, ExpiringAmount: amount, Priority: a.Priority, Failures: a.Failures, LastUsed: a.LastUsed})
	}
	if session != "" {
		sticky := r.affinities[session]
		if time.Since(r.affinityUsed[session]) > 24*time.Hour {
			delete(r.affinities, session)
			delete(r.affinityUsed, session)
			delete(r.affinityOrder, session)
			sticky = ""
		}
		for _, c := range choices {
			if c.ID == sticky {
				return sticky, nil
			}
		}
	}
	id := provider.SelectCandidate(choices, false, 7, nowSec())
	if id == "" {
		if waiting {
			return "", nil
		}
		return "", provider.NewAPIError(503, "当前没有可用且有权调用该模型的 Trae 账号", "account_unavailable")
	}
	r.accounts[id].LastUsed = nowSec()
	return id, nil
}

var _ provider.Runtime = (*Runtime)(nil)
