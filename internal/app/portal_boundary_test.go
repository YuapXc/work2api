package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work2api/internal/config"
	appcrypto "work2api/internal/crypto"
	"work2api/internal/portal/portalauth"
	"work2api/internal/qoder/account"
	"work2api/internal/statebackup"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
)

func TestCallMetricsKeepAttemptsAndStreamErrorsSeparate(t *testing.T) {
	s, _ := portalFixture(t)
	h := s.observeCalls(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamwatch.QueueWait(r.Context(), 30*time.Millisecond, false)
		streamwatch.QueueWait(r.Context(), 10*time.Millisecond, true)
		streamwatch.StartAttempt(r.Context())
		streamwatch.AttemptResult(r.Context(), 429)
		streamwatch.StartAttempt(r.Context())
		streamwatch.AttemptResult(r.Context(), 200)
		_, _ = w.Write([]byte("data: error\n\n"))
		streamwatch.Outcome(r.Context(), "error")
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("X-Request-Id", "client-controlled")
	h.ServeHTTP(w, r)
	if id := w.Header().Get("X-Request-Id"); len(id) != 32 || id == "client-controlled" {
		t.Fatal(id)
	}
	m := s.calls.snapshot()
	if m["requests"] != int64(1) || m["attempts"] != int64(2) || m["upstream_429"] != int64(1) || m["errors"] != int64(1) {
		t.Fatal(m)
	}
	recent := m["recent"].([]callObservation)
	if len(recent) != 1 || recent[0].Status != 200 || recent[0].Outcome != "error" || recent[0].Queue != 30 || recent[0].Account != 10 || recent[0].First == nil {
		t.Fatal(recent)
	}
	for i := 0; i < 200; i++ {
		s.calls.record(streamwatch.TimingSnapshot{Start: time.Now(), Outcome: "ok"}, 200)
	}
	if len(s.calls.snapshot()["recent"].([]callObservation)) != 128 {
		t.Fatal("recent metadata unbounded")
	}
}

func TestCompleteBackupRestoresDatabaseAndSecrets(t *testing.T) {
	s, user := portalFixture(t)
	o := s.o
	oldPackageRoot := config.PackageRoot
	config.PackageRoot = t.TempDir()
	defer func() { config.PackageRoot = oldPackageRoot }()
	for _, mgr := range o.managers {
		o.cfg.DataDir = filepath.Dir(mgr.Path())
		break
	}
	o.cfg.DBPath = filepath.Join(o.cfg.DataDir, "test.db")
	oldRoot := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	defer account.SetDataRoot(oldRoot)
	for _, key := range []string{"BACKUP_EXTRA_PATHS", "OPENCODE_CONFIG", "ENV_FILE"} {
		t.Setenv(key, "")
	}
	cm := appcrypto.NewManager(o.cfg.DataDir)
	sealed, err := cm.Encrypt("sk-backup-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.db.CreateApp("backup-fixture", "backup-hash", "sk-back", "", sealed, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(account.DataRoot(), "secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(account.DataRoot(), "secrets", "fixture.token"), []byte("dt-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	leave := statebackup.Enter()
	err = o.runBackup(context.Background())
	leave()
	if !errors.Is(err, errBackupBusy) {
		t.Fatal("active calls should defer capture", err)
	}
	if err := os.WriteFile(filepath.Join(o.cfg.DataDir, ".instance.lock"), []byte("process-only"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := o.runBackup(context.Background()); err != nil {
		t.Fatal(err)
	}
	files := completedBackups(filepath.Join(o.cfg.DataDir, "backups"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	r := tar.NewReader(z)
	contents := map[string][]byte{}
	for {
		h, e := r.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		raw, e := io.ReadAll(r)
		if e != nil {
			t.Fatal(e)
		}
		contents[h.Name] = raw
	}
	var manifest backupManifest
	if err := json.Unmarshal(contents["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	for name, expected := range manifest.Files {
		if strings.HasSuffix(name, ".instance.lock") {
			t.Fatal("process lock entered backup", name)
		}
		actual := sha256.Sum256(contents[name])
		if hex.EncodeToString(actual[:]) != expected {
			t.Fatal("member checksum", name)
		}
	}
	restore := t.TempDir()
	if err := os.WriteFile(filepath.Join(restore, "snapshot.db"), contents["database.sqlite"], 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.New(filepath.Join(restore, "snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := db.GetUser(user.ID)
	if err != nil || u == nil || u.Username != user.Username {
		t.Fatal(u, err)
	}
	a, err := db.FindAppByKey("backup-hash")
	if err != nil || a == nil {
		t.Fatal(a, err)
	}
	keyFound, tokenFound, authFound := false, false, false
	for name, raw := range contents {
		if strings.HasSuffix(name, "/.secret_key") {
			keyFound = true
			if err := os.WriteFile(filepath.Join(restore, ".secret_key"), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasSuffix(name, "/fixture.token") && string(raw) == "dt-fixture" {
			tokenFound = true
		}
		if strings.HasSuffix(name, "/test-account.info") {
			authFound = true
		}
	}
	if !keyFound || !tokenFound || !authFound {
		t.Fatal("backup omitted credentials", keyFound, tokenFound, authFound)
	}
	storedCipher, err := db.GetAppKeyEnc(a["id"].(int64))
	if err != nil {
		t.Fatal(err)
	}
	if appcrypto.NewManager(restore).Decrypt(storedCipher) != "sk-backup-fixture" {
		t.Fatal("restored master key cannot decrypt database key")
	}
	entries, _ := filepath.Glob(filepath.Join(o.cfg.DataDir, "backups", ".pending-*"))
	if len(entries) != 0 {
		t.Fatal("staging leaked", entries)
	}
}

func TestBackupRetentionPreservesManualAndInvalidCopies(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 9; i++ {
		name := "managed-" + time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC).Format("20060102T150405.000000000Z") + ".tar.gz"
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		digest, err := backupDigest(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".sha256", []byte(digest+"  "+name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manual := filepath.Join(root, "manual.tar.gz")
	invalid := filepath.Join(root, "managed-20200101T000000.000000000Z.tar.gz")
	for _, path := range []string{manual, invalid} {
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(invalid+".sha256", []byte(strings.Repeat("0", 64)+"  "+filepath.Base(invalid)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pruneBackups(root); err != nil {
		t.Fatal(err)
	}
	if len(completedBackups(root)) != 7 {
		t.Fatal("retention")
	}
	for _, path := range []string{manual, invalid} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("unverified/manual backup deleted", err)
		}
	}
}

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

func TestIncompleteResponseRetainsUsageWithoutAccountPenalty(t *testing.T) {
	s, u := portalFixture(t)
	s.o.cfg.PortalDailyOutputTokens = 100
	p := &Principal{UserID: u.ID}
	if !s.reservePortalBudget(httptest.NewRecorder(), p, map[string]any{"model": "model-a", "max_tokens": float64(50)}) {
		t.Fatal("reservation failed")
	}
	acc := s.o.pool.Get("test-account")
	s.o.pool.OnFailure(acc.UID, 60)
	cooldown := acc.CooldownUntil
	s.o.logUsage(logArgs{userID: u.ID, acc: acc, updatePool: true, status: "incomplete", t0: time.Now(), quota: p.quota,
		usage: map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(7), "prompt_cache_hit_tokens": float64(4)}})
	p.quota.settle()
	_, input, output, err := s.o.db.UserDailyQuota(u.ID, p.quota.day)
	if err != nil || input != 10 || output != 7 {
		t.Fatal(input, output, err)
	}
	if acc.FailureCount != 0 || acc.CooldownUntil != cooldown {
		t.Fatal("truncation penalized account", acc)
	}
	rows, err := s.o.db.UsageRecent(10, "", "", nil, "", false, 0, "")
	if err != nil || len(rows) != 1 || rows[0]["status"] != "incomplete" {
		t.Fatal(rows, err)
	}
	summary, err := s.o.db.UsageSummary()
	if err != nil || summary["cache"].(map[string]any)["known_rows"] != int64(1) {
		t.Fatal(summary, err)
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
