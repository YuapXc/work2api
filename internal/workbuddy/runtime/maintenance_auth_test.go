package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/store"
	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

func TestModelFaultDoesNotCoolOtherModels(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "fault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		status int
		raw    string
		kind   ErrKind
	}{{500, `{"code":11134}`, ErrModelProvider}, {502, `{"error":{"code":"11134"}}`, ErrModelProvider}, {429, `{"code":6004,"msg":"switch models"}`, ErrModelRate}} {
		mgrPath := filepath.Join(t.TempDir(), "auth.info")
		if err := os.WriteFile(mgrPath, []byte(`{"auth":{"accessToken":"fixture","domain":"www.codebuddy.cn"},"account":{"uid":"fixture"}}`), 0600); err != nil {
			t.Fatal(err)
		}
		mgr := credentials.NewManager(mgrPath)
		p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
		r := &Runtime{Pool: p, Managers: map[string]*credentials.Manager{"fixture": mgr}, db: db, Cooldowns: map[string]CooldownEntry{}}
		if got := ClassifyUpstream(tc.status, []byte(tc.raw), nil); got != tc.kind {
			t.Fatal(got, tc.kind)
		}
		act := r.PenalizeAccount(p.Get("fixture"), "m", &upstream.UpstreamError{StatusCode: tc.status, Raw: []byte(tc.raw)})
		if !act.ModelScoped || !act.Rotate || p.Get("fixture").CooldownUntil > nowSec() || !p.Get("fixture").Enabled || r.Cooldowns["fixture|m"].Until <= nowSec() {
			t.Fatal("account-wide penalty", act)
		}
	}
}

func TestDisabledAccountEntersMaintenanceWithoutReenabling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":1},"account":{"uid":"fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	r := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: pool.New(map[string]pool.Credential{"fixture": mgr}, "")}
	r.Pool.SetEnabled("fixture", false, "手动停用")
	acc := r.Pool.Get("fixture")
	if err := r.prepareBillingMaintenance(context.Background(), acc); err != nil {
		t.Fatalf("manual disable incorrectly pauses maintenance: %v", err)
	}
	identity, err := mgr.MaintenanceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	g := r.billingGuard()
	g.State(acc.UID, identity)
	g.Observe(acc.UID, identity, "authorization_expired", true)
	if _, err := r.checkinAccount(context.Background(), acc); !errors.Is(err, provider.ErrMaintenanceLogin) {
		t.Fatalf("known invalid auth still reached check-in: %v", err)
	}
	if r.Pool.Get("fixture").Enabled {
		t.Fatal("maintenance reenabled disabled account")
	}
}

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func TestConfirmedExpiryCannotUseStickyScopedOrAdmissionSelection(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	managers := map[string]*credentials.Manager{}
	creds := map[string]pool.Credential{}
	for _, uid := range []string{"expired", "valid"} {
		path := filepath.Join(dir, uid+".info")
		body := `{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":` + fmt.Sprint(time.Now().Add(time.Hour).UnixMilli()) + `},"account":{"uid":"` + uid + `"}}`
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		managers[uid] = credentials.NewManager(path)
		creds[uid] = managers[uid]
	}
	r := &Runtime{Managers: managers, Pool: pool.New(creds, "")}
	r.Catalog = models.NewWithCatalogClient(r.Pool, db, &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"m","credits":1}],"agents":[{"name":"cli","models":["m"]}]}}`))}, nil
	})})
	r.Configure(&config.Config{DataDir: dir}, db, NewSessionRouter())
	if _, err := r.Catalog.RefreshWithStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Sessions.Bind("sticky", "expired")
	mgr := managers["expired"]
	identity, _ := mgr.MaintenanceIdentity()
	g := r.billingGuard()
	g.State("expired", identity)
	g.Observe("expired", identity, "authorization_expired", true)
	if r.ModelCallable(r.Pool.Get("expired"), "m") || r.ModelHealthy(r.Pool.Get("expired"), "m", nowSec()) || r.AccountHealthy(r.Pool.Get("expired"), nowSec()) {
		t.Fatal("invalid credential remains callable")
	}
	selected, e := r.PickAccount("m", "sticky")
	if e != nil || selected.UID != "valid" {
		t.Fatal("sticky selection kept invalid credential", e)
	}
	selector, e := r.AccountSelector("m", "expired", nil, map[string]bool{"expired": true, "valid": true})
	if e != nil {
		t.Fatal(e)
	}
	selected, e = selector(nil)
	if e != nil || selected.UID != "valid" {
		t.Fatal("admission preferred invalid credential", e)
	}
	if _, e = r.PickInScope("m", "sticky", nil, map[string]bool{"expired": true}); e == nil {
		t.Fatal("scoped selection bypassed auth exclusion")
	}
	if _, err := r.headers(context.Background(), r.Pool.Get("expired"), nil); err == nil {
		t.Fatal("send boundary accepted known-invalid credentials")
	}
	if !r.Pool.Get("expired").Enabled {
		t.Fatal("auth state changed manual enablement")
	}
}

func TestDiagnosticUsageDoesNotEnterUserStatistics(t *testing.T) {
	r := &Runtime{}
	ctx := provider.WithInvocation(context.Background(), provider.Invocation{Diagnostic: true})
	r.LogUsage(LogArgs{Ctx: ctx, Status: "ok", Usage: map[string]any{"prompt_tokens": 1, "completion_tokens": 1}})
	// No configured DB/usage sink: a diagnostic must return before persistence.
}

func TestReadOnlyCheckReusesFreshBillingWithoutEnablingAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "check.info")
	body := `{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":` + fmt.Sprint(time.Now().Add(time.Hour).UnixMilli()) + `},"account":{"uid":"fixture"}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	identity, _ := mgr.MaintenanceIdentity()
	r := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: pool.New(map[string]pool.Credential{"fixture": mgr}, ""), creditStates: map[string]creditState{"fixture": {Manager: mgr, Identity: identity, Updated: nowSec()}}}
	r.Pool.SetEnabled("fixture", false, "手动停用")
	result := r.CheckAccount(context.Background(), "fixture", false)
	if result.Status != "valid" || r.Pool.Get("fixture").Enabled {
		t.Fatal("fresh check made extra requests or enabled account", result)
	}
}

func TestCreditRejectionQueuesOneFreshReadAfterOlderRefresh(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	managers := map[string]*credentials.Manager{}
	creds := map[string]pool.Credential{}
	for _, uid := range []string{"credit", "rate"} {
		path := filepath.Join(dir, uid+".info")
		body := fmt.Sprintf(`{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":%d},"account":{"uid":"%s"}}`, time.Now().Add(time.Hour).UnixMilli(), uid)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		managers[uid] = credentials.NewManager(path)
		creds[uid] = managers[uid]
	}
	r := &Runtime{Managers: managers, Pool: pool.New(creds, ""), db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() { cancel(); _ = r.Close() }()
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	var calls, other atomic.Int32
	r.creditQuery = func(ctx context.Context, mgr billing.Credential) (*billing.Credit, error) {
		if mgr != managers["credit"] {
			other.Add(1)
		}
		if calls.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &billing.Credit{Remain: 15, Total: 20}, nil
	}
	rate, credit := r.Pool.Get("rate"), r.Pool.Get("credit")
	r.PenalizeAccount(rate, "test", &upstream.UpstreamError{StatusCode: 429})
	r.creditRechecks.mu.Lock()
	rateTriggered := r.creditRechecks.done != nil
	r.creditRechecks.mu.Unlock()
	if rateTriggered {
		t.Fatal("generic rate limit triggered a billing read")
	}
	manualDone := make(chan struct{})
	go func() {
		defer close(manualDone)
		r.RefreshCreditsFor(ctx, db, credit)
	}()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("manual read did not start")
	}
	for range 6 {
		if act := r.PenalizeAccount(credit, "test", &upstream.UpstreamError{StatusCode: 402}); !act.Rotate {
			t.Fatal("quota recheck prevented immediate failover")
		}
	}
	r.creditRechecks.mu.Lock()
	done := r.creditRechecks.done
	r.creditRechecks.mu.Unlock()
	if done == nil || calls.Load() != 1 {
		t.Fatal("recheck did not wait for the same account's ongoing read")
	}
	close(releaseFirst)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("queued quota recheck did not finish")
	}
	<-manualDone
	if calls.Load() != 2 || other.Load() != 0 || !r.Pool.Get("credit").Healthy(nowSec()) || r.Pool.Get("rate").Healthy(nowSec()) {
		t.Fatalf("reads=%d other=%d, cooldown state inconsistent", calls.Load(), other.Load())
	}
	r.recheckCredits(ctx, creditRecheck{account: credit, manager: credentials.NewManager("old"), requested: nowSec()})
	if calls.Load() != 2 {
		t.Fatal("stale credential scheduled an extra query")
	}
}

func TestMediaLostResponseIsNotReplayedAndDispatchIsRechecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.info")
	raw := fmt.Sprintf(`{"auth":{"accessToken":"fixture","domain":"www.codebuddy.cn","expiresAt":%d},"account":{"uid":"media"}}`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	calls := 0
	r := &Runtime{Managers: map[string]*credentials.Manager{"media": mgr}, Pool: pool.New(map[string]pool.Credential{"media": mgr}, "")}
	r.MediaClient = &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) { calls++; return nil, io.ErrUnexpectedEOF })}
	request := provider.MediaRequest{Operation: "video", Model: "movie", AccountID: "media", Payload: map[string]any{"model": "movie", "prompt": "test"}}
	if _, err := r.CallMedia(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("submission replayed", err, calls)
	}
	ctx := provider.WithInvocation(context.Background(), provider.Invocation{Check: func(context.Context, provider.AccountRef, string) error { return errors.New("revoked") }})
	if _, err := r.CallMedia(ctx, request); err == nil || calls != 1 {
		t.Fatal("revoked request reached network", err, calls)
	}
}

func TestTranscriptionUploadSeparatesStorageAuthorization(t *testing.T) {
	r := &Runtime{}
	calls := []string{}
	r.MediaClient = &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Method+" "+req.URL.Host+req.URL.Path)
		body := `{"code":0,"data":{"items":[{"upload_url":"https://bucket.cos.ap-beijing.myqcloud.com/upload","download_url":"https://bucket.cos.ap-beijing.myqcloud.com/audio.wav"}]}}`
		if req.Method == "PUT" {
			if req.Header.Get("Authorization") != "" || req.Header.Get("X-User-Id") != "" {
				t.Fatal("account credential leaked to object storage")
			}
			body = ""
		} else if req.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatal("presign missing auth")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	u, err := r.uploadMediaInput(context.Background(), "https://copilot.tencent.com", map[string]string{"Authorization": "Bearer fixture"}, map[string]any{"data": "UklGRg==", "mime": "audio/wav"})
	if err != nil || !strings.HasSuffix(u, "audio.wav") || len(calls) != 2 {
		t.Fatal(u, calls, err)
	}
	r.MediaClient = &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"items":[{"upload_url":"https://127.0.0.1/upload","download_url":"https://127.0.0.1/file"}]}}`))}, nil
	})}
	if _, err = r.uploadMediaInput(context.Background(), "https://copilot.tencent.com", nil, map[string]any{"data": "UklGRg==", "mime": "audio/wav"}); err == nil {
		t.Fatal("unsafe storage origin accepted")
	}
}
