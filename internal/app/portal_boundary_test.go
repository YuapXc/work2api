package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"work2api/internal/config"
	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
)

func TestPortalAuthLimitsKeepProxyIdentitiesSeparate(t *testing.T) {
	s, _ := portalFixture(t)
	s.o.cfg.TrustedProxyCIDRs = "127.0.0.1/32"
	h := s.portalGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	request := func(peer, claimed string) int {
		r := httptest.NewRequest("POST", "/portal/api/auth/login", strings.NewReader(`{}`))
		r.RemoteAddr = peer
		r.Header.Set("X-Real-IP", claimed)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 60; i++ {
		if code := request("127.0.0.1:5000", "192.0.2.1"); code != 200 {
			t.Fatal(i, code)
		}
	}
	if request("127.0.0.1:5000", "192.0.2.1") != 429 {
		t.Fatal("IP cap missing")
	}
	for i := 0; i < 400; i++ {
		if request("127.0.0.1:5000", "192.0.2.1") != 429 {
			t.Fatal("blocked IP escaped cap")
		}
	}
	if request("127.0.0.1:5000", "192.0.2.2") != 200 {
		t.Fatal("different client blocked")
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "198.51.100.1:9"
	r.Header.Set("X-Real-IP", "192.0.2.2")
	if s.rateLimitIP(r) != "198.51.100.1" {
		t.Fatal("untrusted peer spoofed identity")
	}
	r.RemoteAddr = "127.0.0.1:9"
	r.Header.Set("X-Real-IP", "invalid, 192.0.2.2")
	if s.rateLimitIP(r) != "127.0.0.1" {
		t.Fatal("invalid proxy identity accepted")
	}
	for i := 0; i < 10; i++ {
		if !s.portalLoginLimiter.allow("user:target") {
			t.Fatal(i)
		}
	}
	if s.portalLoginLimiter.allow("user:target") {
		t.Fatal("username cap missing")
	}
	admin := newLoginRateLimiter()
	for i := 0; i < 10; i++ {
		if !admin.allow("admin-ip") {
			t.Fatal(i)
		}
	}
	if admin.allow("admin-ip") {
		t.Fatal("admin limit changed")
	}
	global := newLoginRateLimiter()
	for i := 0; i < 300; i++ {
		if !global.allowLimit("global", 300) {
			t.Fatal(i)
		}
	}
	if global.allowLimit("global", 300) {
		t.Fatal("global cap missing")
	}
	global.attempt["global"].start = time.Now().Add(-loginWindowLen)
	if !global.allowLimit("global", 300) {
		t.Fatal("expired window did not recover")
	}
}

func TestSharedQueueReservesPrivateWaitCapacity(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, ModelQueueSize: 8, PortalUserQueueSize: 4, PortalSharedQueueSize: 6})
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < 6; i++ {
		if err := a.enqueueLocked(&modelTicket{key: fmt.Sprintf("portal-user-%d", i/4), shared: true}); err != nil {
			t.Fatal(i, err)
		}
	}
	if a.enqueueLocked(&modelTicket{key: "portal-user-another", shared: true}) == nil {
		t.Fatal("shared queue exceeded cap")
	}
	if a.enqueueLocked(&modelTicket{key: "private-a"}) != nil || a.enqueueLocked(&modelTicket{key: "private-b"}) != nil {
		t.Fatal("private waiting capacity lost")
	}
	if a.enqueueLocked(&modelTicket{key: "private-c"}) == nil {
		t.Fatal("global queue exceeded cap")
	}
	b := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, ModelQueueSize: 8, PortalUserQueueSize: 4, PortalSharedQueueSize: 6})
	for i := 0; i < 4; i++ {
		if b.enqueueLocked(&modelTicket{key: "portal-user-one", shared: true}) != nil {
			t.Fatal(i)
		}
	}
	if b.enqueueLocked(&modelTicket{key: "portal-user-one", shared: true}) == nil {
		t.Fatal("per-user cap missing")
	}
}

func TestSharedAdmissionReservesPrivateCapacityAndRecovers(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, PortalSharedConcurrency: 3})
	var leases []*modelLease
	for i := 0; i < 3; i++ {
		l, err := a.acquireWithLimit(context.Background(), fmt.Sprintf("portal-user-%d", i), 1, "")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		l, err := a.acquireWithLimit(ctx, "portal-user-4", 1, "")
		if l != nil {
			l.release()
		}
		result <- err
	}()
	awaitQueue(t, a, 1)
	private, err := a.acquire(context.Background(), "private-key")
	if err != nil {
		t.Fatal(err)
	}
	if a.snapshot()["running"] != 4 {
		t.Fatal(a.snapshot())
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	private.release()
	for _, l := range leases {
		l.release()
	}
	if a.sharedActive != 0 || a.snapshot()["running"] != 0 || a.snapshot()["queued"] != 0 {
		t.Fatal(a.snapshot(), a.sharedActive)
	}
}

func TestPortalUsageOmitsBodiesAndMarksMissingUsage(t *testing.T) {
	s, u := portalFixture(t)
	s.o.logUsage(logArgs{userID: u.ID, appID: 42, appName: "key", status: "ok", t0: time.Now(), input: "private prompt", output: "private answer", reasoning: "private reasoning"})
	rows, err := s.o.db.UsageRecent(10, "", "", nil, "", false, 0, "")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	row := rows[0]
	for _, field := range []string{"input_content", "output_content", "reasoning_content"} {
		if row[field] != "" {
			t.Fatal("body persisted", field, row[field])
		}
	}
	if row["tokens_known"] != int64(0) || row["app_id"] != int64(42) {
		t.Fatal(row)
	}
	w := httptest.NewRecorder()
	s.portalUsage(w, portalRequest(u, "/portal/api/usage", ""))
	if w.Code != 200 || !containsJSONNullTokens(w.Body.String()) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func containsJSONNullTokens(body string) bool {
	return strings.Contains(body, `"input_tokens":null`) && strings.Contains(body, `"output_tokens":null`)
}

func TestConfiguredQuotaForwardsOneOutputReservation(t *testing.T) {
	s, u := portalFixture(t)
	s.o.cfg.PortalDailyOutputTokens = 100
	p := &Principal{UserID: u.ID}
	payload := map[string]any{"model": "model-a", "max_tokens": float64(50), "max_completion_tokens": float64(25), "max_output_tokens": float64(1)}
	if !s.reservePortalBudget(httptest.NewRecorder(), p, payload) {
		t.Fatal("reservation failed")
	}
	if p.quota.output != 1 || payload["max_tokens"] != 1 || payload["max_completion_tokens"] != 1 {
		t.Fatal(p.quota, payload)
	}
	p.quota.settle()
	p.quota.settle()
	requests, _, output, err := s.o.db.UserDailyQuota(u.ID, p.quota.day)
	if err != nil || requests != 1 || output != 1 {
		t.Fatal(requests, output, err)
	}
}

func TestStalePasswordChangeCannotOverwriteReset(t *testing.T) {
	s, u := portalFixture(t)
	hash, err := portalauth.HashPassword("administrator-reset")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.UpdateUserPassword(u.ID, hash); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.CreateUserSessionForPassword("fresh-session", u.ID, float64(time.Now().Add(time.Hour).Unix()), hash); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.UpdateUserPassword(u.ID, "stale-password", u.PasswordHash); !errors.Is(err, store.ErrConflict) {
		t.Fatal(err)
	}
	latest, err := s.o.db.GetUser(u.ID)
	if err != nil || latest.PasswordHash != hash {
		t.Fatal(latest, err)
	}
	if session, err := s.o.db.GetUserSession("fresh-session"); err != nil || session == nil {
		t.Fatal("stale change revoked new session", session, err)
	}
}
