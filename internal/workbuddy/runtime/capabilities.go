package runtime

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/upstream"
)

func (r *Runtime) Name() string         { return "workbuddy" }
func (r *Runtime) Prefix() string       { return "" }
func (r *Runtime) Ready() bool          { return r.Pool != nil && len(r.Pool.Accounts()) > 0 }
func (r *Runtime) SupportsPortal() bool { return true }
func (r *Runtime) Models(ctx context.Context) []provider.CatalogModel {
	var out []provider.CatalogModel
	for _, m := range r.Catalog.ListCached() {
		m["catalog_source"] = r.Catalog.Source()
		m["catalog_stale"] = r.Catalog.Stale()
		out = append(out, provider.CatalogModel{ID: ToStr(m["id"]), Name: ToStr(m["name"]), Extra: m})
	}
	return out
}
func (r *Runtime) RefreshModels(ctx context.Context) error {
	return r.modelRefresh.Do(ctx, "catalog", func(ctx context.Context) error {
		_, err := r.Catalog.RefreshWithStatus(ctx)
		if errors.Is(err, models.ErrPartialRefresh) {
			return provider.ErrPartialRefresh
		}
		return err
	})
}
func (r *Runtime) PrepareRequest(ctx context.Context, req provider.ServeRequest) (context.Context, func(), error) {
	if err := upstream.ValidateChoiceCount(req.Payload); err != nil {
		return ctx, func() {}, errBody(400, "WorkBuddy 仅支持 n=1；并行 Agent 请分别发送独立请求", "invalid_request_error")
	}
	request := WithRequestSessionIdentity((&http.Request{Header: req.Headers}).WithContext(ctx), req.Payload)
	model := ToStr(req.Payload["model"])
	returnCtx, finish := r.Sessions.Begin(request.Context(), RequestSessionKey(request, &req.Caller, model, req.Payload), model, &req.Caller, req.Payload)
	return returnCtx, finish, nil
}

var _ provider.Runtime = (*Runtime)(nil)
var _ provider.RequestPreparer = (*Runtime)(nil)

func (r *Runtime) ExportCredentials() (map[string]any, error) {
	// workbuddy: from the live managers so refreshed tokens are included.
	wbAccounts := []map[string]any{}
	for _, acc := range r.Pool.Accounts() {
		mgr := r.Manager(acc.UID)
		if mgr == nil {
			continue
		}
		session, err := mgr.RawSession()
		if err != nil {
			continue
		}
		entry := map[string]any{"uid": acc.UID, "session": map[string]any{
			"auth": session.Auth, "account": session.Account,
		}}
		wbAccounts = append(wbAccounts, entry)
	}
	return map[string]any{"accounts": wbAccounts, "note": "每个 entry 存为 auths/workbuddy-<uid>.info（session 原样落盘）即可在目标项目导入"}, nil
}

func (r *Runtime) StateSources() []provider.StateSource {
	out := []provider.StateSource{{Path: r.ProjectAuths}}
	r.ManagerMu.RLock()
	defer r.ManagerMu.RUnlock()
	for _, m := range r.Managers {
		if m != nil {
			out = append(out, provider.StateSource{Path: m.Path()})
		}
	}
	return out
}

func LockPaths() []string {
	paths := []string{}
	for _, dir := range credentials.AuthDirs("", filepath.Join(config.PackageRoot, "auths")) {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			paths = append(paths, filepath.Join(dir, ".instance.lock"))
		}
	}
	return append(paths, filepath.Join(config.PackageRoot, "auths", ".instance.lock"))
}

func (r *Runtime) HealthSnapshot() map[string]any {
	healthy := r.Pool.HealthyCount(nil)
	row := map[string]any{"name": r.Name(), "ready": healthy > 0, "callable_accounts": healthy, "model_count": len(r.Catalog.ListCached()), "model_source": r.Catalog.Source()}
	if healthy == 0 {
		row["reason"] = "没有启用且有额度的非冷却账号"
	}
	return row
}
