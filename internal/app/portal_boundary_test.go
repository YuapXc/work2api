package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work2api/internal/config"
	"work2api/internal/core/provider"
	appcrypto "work2api/internal/crypto"
	"work2api/internal/portal/portalauth"
	"work2api/internal/qoder/account"
	"work2api/internal/statebackup"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
	"work2api/internal/trae"
)

func TestAdminUsageExportMatchesGroupedLogsAndCleansUp(t *testing.T) {
	s, _ := portalFixture(t)
	s.o.cfg.AdminToken = "fixture-export-token"
	s.o.cfg.DataDir = t.TempDir()
	for _, mgr := range s.o.wb.Managers {
		s.o.cfg.DBPath = filepath.Join(filepath.Dir(mgr.Path()), "test.db")
		break
	}
	t.Cleanup(s.CloseUsageExports)
	handler := s.Handler()
	owner := &http.Cookie{Name: cookieName, Value: s.sessions.create()}
	other := &http.Cookie{Name: cookieName, Value: s.sessions.create()}
	request := func(method, path, body string, session *http.Cookie, byteRange string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:41000"
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if session != nil {
			r.AddCookie(session)
		}
		if byteRange != "" {
			r.Header.Set("Range", byteRange)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	zero, zeroMS, zeroCost := 0, int64(0), float64(0)
	known, unknown := true, false
	p := store.UsageParams{
		Timestamp: float64(time.Date(2026, 11, 7, 19, 15, 0, 0, time.UTC).Unix()),
		Model:     "test", Protocol: "responses", AppName: "  =formula", AccountUID: "test-account",
		InputTokens: 12, OutputTokens: 7, TokensKnown: &known, CachedTokens: &zero, Credits: &zeroCost,
		InputContent: "search-needle", Status: "error", Error: "private error",
		Diagnostics: &store.UsageDiagnostics{RequestedModel: "group/test", PhysicalModel: "trae/test",
			Performance: &store.UsagePerformance{RequestID: strings.Repeat("a", 32), FirstGenerationMS: &zeroMS, HTTPStatus: 200}},
	}
	insert := func(p store.UsageParams) {
		t.Helper()
		if err := s.o.db.LogUsage(p); err != nil {
			t.Fatal(err)
		}
	}
	insert(p)
	p.Status = "ok"
	p.Diagnostics.FinishReason = "stop"
	insert(p)
	p.Diagnostics = &store.UsageDiagnostics{RequestedModel: "group/test", PhysicalModel: "opencode/test"}
	p.TokensKnown, p.CachedTokens, p.Credits = &unknown, nil, nil
	insert(p)
	p.InputContent = "other"
	insert(p)

	const base = "/admin/usage/exports"
	const filters = `{"protocol":"responses","model":"group/test","search":"search-needle","timezone":"Asia/Shanghai"}`
	start := func() usageExport {
		t.Helper()
		w := request("POST", base, filters, owner, "")
		var job usageExport
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &job) != nil || job.ID == "" {
			t.Fatalf("start: %d %s", w.Code, w.Body)
		}
		return job
	}
	job := start()
	path := base + "/" + job.ID
	download := path + "/download"
	s.usageExports.mu.Lock()
	done := s.usageExports.job.done
	csvPath := s.usageExports.job.path
	s.usageExports.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not finish")
	}
	w := request("GET", path, "", owner, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil || job.Status != "ready" || job.Count != 2 || job.Expired() {
		t.Fatalf("export status: %d %s", w.Code, w.Body)
	}
	list := request("GET", "/admin/usage/recent?group=request&protocol=responses&model=group%2Ftest&search=search-needle", "", owner, "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"total":2`) {
		t.Fatalf("grouped list: %d %s", list.Code, list.Body)
	}
	if w := request("GET", download, "", nil, ""); w.Code != 401 {
		t.Fatalf("unauthenticated download: %d", w.Code)
	}
	if w := request("GET", download, "", other, ""); w.Code != 404 {
		t.Fatalf("another admin session downloaded export: %d", w.Code)
	}
	if w := request("GET", download, "", owner, "bytes=0-2"); w.Code != 206 || w.Body.String() != "\ufeff" {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	w = request("GET", download, "", owner, "")
	content := w.Body.String()
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" ||
		!strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || !strings.HasPrefix(content, "\ufeff") ||
		strings.Contains(content, "search-needle") || strings.Contains(content, "private error") {
		t.Fatalf("download: %d %v %q", w.Code, w.Header(), content)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(content, "\ufeff"))).ReadAll()
	if err != nil || len(rows) != 3 || len(rows[0]) != 22 ||
		rows[1][3] != "opencode/test" || rows[1][7] != "" || rows[1][10] != "" ||
		rows[2][0] != "2026-11-08 03:15:00" || rows[2][3] != "trae/test" || rows[2][5] != "'  =formula" ||
		rows[2][7] != "12" || rows[2][9] != "0" || rows[2][10] != "0" || rows[2][16] != "0" || rows[2][21] != "stop" {
		t.Fatalf("CSV: %v %v", rows, err)
	}
	if w := request("DELETE", path, "", owner, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"cancelled"`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(csvPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel did not remove file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen := 0
	err = s.o.db.VisitUsageExport(ctx, s.o.cfg.DBPath, "responses", "group/test", nil, "", "search-needle", func(_ map[string]any) error {
		seen++
		if seen == 1 {
			p.InputContent = "search-needle"
			return s.o.db.LogUsage(p)
		}
		return nil
	})
	if err != nil || seen != 2 {
		t.Fatalf("export snapshot changed during concurrent log write: %d rows, %v", seen, err)
	}
}

func TestTraeOAuthCallbackBindsProductOwnerAndTask(t *testing.T) {
	s, u := portalFixture(t)
	rt, err := trae.New(t.TempDir(), s.o.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	login, err := rt.BeginOwnedLogin(map[string]any{"product": "cn"}, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := login["login_id"].(string)
	callback, _ := url.Parse(login["callback_url"].(string))
	if callback.Path != "/authorize" {
		t.Fatal("callback path violates the native authorization page contract")
	}
	callback.RawQuery = url.Values{"authCode": {"test-only-code"}}.Encode()
	if rt.CompleteOwnedLogin(id, callback.String(), u.ID) == nil {
		t.Fatal("callback without task nonce accepted")
	}
	q := url.Values{"loginTraceID": {id}, "auth_code_info": {`{"auth_code":"test-only-code"}`}}
	callback.RawQuery = q.Encode()
	if rt.OAuthComplete(id, callback.String()) == nil {
		t.Fatal("admin accepted another owner's login")
	}
	if rt.CompleteOwnedLogin(id, callback.String(), u.ID+1) == nil {
		t.Fatal("other user accepted login")
	}
	q.Set("state", "wrong-task")
	callback.RawQuery = q.Encode()
	if rt.CompleteOwnedLogin(id, callback.String(), u.ID) == nil {
		t.Fatal("conflicting state accepted")
	}
	q.Del("state")
	callback.RawQuery = q.Encode()
	if err = rt.CompleteOwnedLogin(id, callback.String(), u.ID); err != nil {
		t.Fatal("native alias callback rejected", err)
	}
	callback.Host = "attacker.invalid"
	if rt.CompleteOwnedLogin(id, callback.String(), u.ID) == nil {
		t.Fatal("different callback origin accepted")
	}
	if err = rt.CancelOwnedLogin(id, u.ID); err != nil {
		t.Fatal(err)
	}
	if rt.CompleteOwnedLogin(id, callback.String(), u.ID) == nil {
		t.Fatal("cancelled login accepted")
	}
}

func TestTraeSharingRemainsProviderBound(t *testing.T) {
	s, u := portalFixture(t)
	other, err := portalauth.CreateUser(s.o.db, "wb-contributor", "testpassword", "user")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		id, provider string
		owner        int64
	}{{"trae-test", "trae", u.ID}, {"wb-test", "workbuddy", other}, {"wb-platform", "workbuddy", 0}} {
		if _, err := s.o.db.UpsertProviderAccount(store.ProviderAccount{UID: entry.id, Provider: entry.provider, Auth: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		if entry.owner > 0 {
			if _, err := s.o.db.CreateContribution(entry.owner, entry.id, entry.provider, "domestic", "active"); err != nil {
				t.Fatal(err)
			}
		}
	}
	g, err := s.o.db.CreateResourceGroup("Trae pool", "trae", `["trae/test"]`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.o.db.SetGroupEnabled(g, true); err != nil {
		t.Fatal(err)
	}
	if err = s.o.db.SetDefaultPortalGroup(g, true); err != nil {
		t.Fatal(err)
	}
	users, err := s.o.db.GroupUserIDs(g)
	if err != nil || len(users) != 1 || users[0] != u.ID {
		t.Fatal("automatic grant crossed providers", users, err)
	}
	if err = s.o.db.SetPlatformAccountSharing("wb-platform", "shared", []int64{g}); err == nil {
		t.Fatal("cross-provider platform sharing accepted")
	}
	if err = s.o.db.AddGroupAccount(g, "wb-test"); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.o.db.GroupAccountUIDsWithActiveContribution(g)
	if err != nil || len(accounts) != 1 || accounts[0] != "trae-test" {
		t.Fatal("corrupt cross-provider membership became eligible", accounts, err)
	}
	for _, models := range [][]string{{"workbuddy/test"}, {"test"}} {
		p := &Principal{AllowedModels: models}
		if !modelAllowed(p, "workbuddy/test", "test") || !modelAllowed(p, "test", "test") || modelAllowed(p, "trae/test", "trae/test") {
			t.Fatal("WorkBuddy prefix changed key authorization", models)
		}
	}
}

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
	for _, mgr := range o.wb.Managers {
		o.cfg.DataDir = filepath.Dir(mgr.Path())
		break
	}
	o.cfg.DBPath = filepath.Join(o.cfg.DataDir, "test.db")
	oldRoot := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	defer account.SetDataRoot(oldRoot)
	if err := o.runtimes.Register(&backupSourceProbe{path: account.DataRoot()}); err != nil {
		t.Fatal(err)
	}
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
	s.o.logUsage(logArgs{UserID: u.ID, AppID: 42, AppName: "key", Status: "ok", T0: time.Now(), Input: "private prompt", Output: "private answer", Reasoning: "private reasoning"})
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
	known := true
	if err := s.o.db.LogUsage(store.UsageParams{UserID: u.ID, AppID: 42, Model: "test", InputTokens: 13, OutputTokens: 7, TokensKnown: &known, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.LogUsage(store.UsageParams{UserID: 0, Model: "test", InputTokens: 1000, OutputTokens: 1000, TokensKnown: &known, Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.portalUsage(w, portalRequest(u, "/portal/api/usage", ""))
	if w.Code != 200 || !containsJSONNullTokens(w.Body.String()) {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Today struct {
			Input  int64 `json:"recorded_input_tokens"`
			Output int64 `json:"recorded_output_tokens"`
			Known  bool  `json:"tokens_known"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Today.Input != 13 || result.Today.Output != 7 || result.Today.Known {
		t.Fatal("partial daily totals hidden or foreign usage included", w.Body.String())
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
	acc := s.o.wb.Pool.Get("test-account")
	s.o.wb.Pool.OnFailure(acc.UID, 60)
	cooldown := acc.CooldownUntil
	s.o.logUsage(logArgs{UserID: u.ID, Acc: acc, UpdatePool: true, Status: "incomplete", T0: time.Now(), Quota: p.quota,
		Usage: map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(7), "prompt_cache_hit_tokens": float64(4)}})
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

type backupSourceProbe struct{ path string }

func TestQoderContributionTaskValidityUsesBalancedLock(t *testing.T) {
	s := &Server{portalTasks: map[string]*portalContributionTask{}}
	task := &portalContributionTask{expires: time.Now().Add(time.Minute)}
	if s.portalTaskLive(task) {
		t.Fatal("removed task is live")
	}
	s.portalTasks["reservation"] = task
	if !s.portalTaskLive(task) {
		t.Fatal("valid task rejected")
	}
	delete(s.portalTasks, "reservation")
	s.portalTasks["login-id"] = task
	if !s.portalTaskLive(task) {
		t.Fatal("task remapping lost identity")
	}
	task.expires = time.Now().Add(-time.Minute)
	if s.portalTaskLive(task) {
		t.Fatal("expired task is live")
	}
	s.portalMu.Lock()
	s.portalMu.Unlock()
}

func (*backupSourceProbe) Name() string                                   { return "fixture-source" }
func (*backupSourceProbe) Prefix() string                                 { return "fixture-source/" }
func (*backupSourceProbe) Ready() bool                                    { return false }
func (*backupSourceProbe) Models(context.Context) []provider.CatalogModel { return nil }
func (*backupSourceProbe) Serve(context.Context, provider.ServeRequest) (provider.UsageReport, error) {
	return provider.UsageReport{}, provider.ErrUnsupported
}
func (p *backupSourceProbe) StateSources() []provider.StateSource {
	return []provider.StateSource{{Path: p.path, Required: true}}
}
