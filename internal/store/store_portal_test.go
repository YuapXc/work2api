package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func mustDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// v15 migration must be idempotent and preserve pre-portal apps rows.
func TestPortalMigrationPreservesApps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := New(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.CreateApp("legacy", "hash1", "sk-x", "", "", "", 0); err != nil {
		t.Fatalf("create legacy app: %v", err)
	}
	// Reopen forces initSchema to run again over an existing v15 db (idempotency).
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	apps, err := db2.ListApps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("list apps after reopen: %v %v", apps, err)
	}
	if apps[0].Name != "legacy" || apps[0].UserID != 0 {
		t.Fatalf("legacy app row wrong: %+v", apps[0])
	}
	// The old global-unique constraint is gone: same name under another user OK.
	if _, err := db2.CreateApp("legacy", "hash2", "sk-y", "", "", "", 7); err != nil {
		t.Fatalf("per-user name must be allowed after migration: %v", err)
	}
}

// Key names must be unique per user, not globally (HANDOFF §5).
func TestPerUserNameUniqueness(t *testing.T) {
	db := mustDB(t)
	if _, err := db.CreateApp("mykey", "h1", "sk-1", "", "", "", 1); err != nil {
		t.Fatalf("user1 create: %v", err)
	}
	if _, err := db.CreateApp("mykey", "h2", "sk-2", "", "", "", 2); err != nil {
		t.Fatalf("user2 same name must be allowed: %v", err)
	}
	if _, err := db.CreateApp("mykey", "h3", "sk-3", "", "", "", 1); err == nil {
		t.Fatal("duplicate per-user name must conflict")
	}
}

// Invite codes: single consumption, expiry respected, race-safe via the
// conditional UPDATE.
func TestInviteCodeConsumption(t *testing.T) {
	db := mustDB(t)
	if err := db.CreateInviteCode("ABC", 0); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	ok, err := db.ConsumeInviteCode("ABC", 7)
	if err != nil || !ok {
		t.Fatalf("first consume: %v %v", ok, err)
	}
	ok, _ = db.ConsumeInviteCode("ABC", 8)
	if ok {
		t.Fatal("second consume must fail")
	}
	// unknown code
	ok, _ = db.ConsumeInviteCode("NOPE", 7)
	if ok {
		t.Fatal("unknown code must fail")
	}
	// expired code (created with a past expiry)
	if err := db.CreateInviteCode("OLD", -100); err != nil {
		t.Fatal(err)
	}
	ok, _ = db.ConsumeInviteCode("OLD", 7)
	if ok {
		t.Fatal("expired code must fail")
	}
}

// One upstream account may only ever have one contribution (UNIQUE).
func TestContributionUniqueness(t *testing.T) {
	db := mustDB(t)
	u1, err := db.CreateUser("c1", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := db.CreateUser("c2", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContribution(u1, "wb-uid-1", "workbuddy", "cn", "active"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := db.CreateContribution(u2, "wb-uid-1", "workbuddy", "cn", "active"); err == nil {
		t.Fatal("second contributor for same account must conflict")
	}
	uids, err := db.ActiveContributionUIDs(u1)
	if err != nil || !uids["wb-uid-1"] {
		t.Fatalf("active uids: %v %v", uids, err)
	}
	// revoke with expected-from guard
	ok, err := db.SetContributionStatus(1, u1, "active", "revoked", "用户撤回")
	if err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	uids, _ = db.ActiveContributionUIDs(u1)
	if len(uids) != 0 {
		t.Fatalf("uids after revoke: %v", uids)
	}
	// foreign user cannot touch another's contribution
	if _, err := db.CreateContribution(u1, "wb-uid-2", "workbuddy", "cn", "active"); err != nil {
		t.Fatal(err)
	}
	c, err := db.ContributionByAccount("wb-uid-2")
	if err != nil || c == nil {
		t.Fatalf("by account: %v %v", c, err)
	}
	ok, _ = db.SetContributionStatus(c.ID, 99, "active", "revoked", "越权")
	if ok {
		t.Fatal("foreign revoke must not apply")
	}
}

// Group scheduling scope: only enabled groups' accounts, only granted users.
func TestGroupAccountsOf(t *testing.T) {
	db := mustDB(t)
	u42, err := db.CreateUser("u42", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	u43, err := db.CreateUser("u43", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	g1, err := db.CreateResourceGroup("shared-wb", "workbuddy", "")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := db.CreateResourceGroup("shared-2", "workbuddy", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"a", "b", "c"} {
		if _, err := db.CreateContribution(u42, uid, "workbuddy", "cn", "active"); err != nil {
			t.Fatal(err)
		}
	}
	for _, uid := range []string{"a", "b"} {
		if err := db.AddGroupAccount(g1, uid); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddGroupAccount(g2, "c"); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantGroup(u42, g1); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantGroup(u42, g2); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantGroup(u43, g2); err != nil {
		t.Fatal(err)
	}
	// g2 starts disabled (enabled=0 default); enable both groups
	if err := db.SetGroupEnabled(g1, true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetGroupEnabled(g2, true); err != nil {
		t.Fatal(err)
	}
	uids, err := db.GroupAccountsOf(u42)
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 3 {
		t.Fatalf("user42 scope: %v", uids)
	}
	uids, _ = db.GroupAccountsOf(u43)
	if len(uids) != 1 || !uids["c"] {
		t.Fatalf("user43 scope: %v", uids)
	}
	uids, _ = db.GroupAccountsOf(9999)
	if len(uids) != 0 {
		t.Fatalf("ungranted user must have empty scope: %v", uids)
	}
	// disable g2 again → scope shrinks
	if err := db.SetGroupEnabled(g2, false); err != nil {
		t.Fatal(err)
	}
	uids, _ = db.GroupAccountsOf(u42)
	if len(uids) != 2 {
		t.Fatalf("after disable: %v", uids)
	}
}

// User session lifecycle: create, validate, slide, revoke-all.
func TestUserSessions(t *testing.T) {
	db := mustDB(t)
	uid, err := db.CreateUser("alice", "hashpw", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUserSession("tok", uid, 9999999999); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetUserSession("tok")
	if err != nil || s == nil || s.UserID != uid {
		t.Fatalf("get session: %v %v", s, err)
	}
	if err := db.ExtendUserSession("tok", 1); err != nil {
		t.Fatal(err)
	}
	s, _ = db.GetUserSession("tok")
	if s != nil {
		t.Fatal("expired session must be dropped on read")
	}
	n, err := db.DeleteUserSessions(uid)
	if err != nil || n != 0 {
		t.Fatalf("delete sessions: %v %v", n, err)
	}
}

// Duplicate username / admin bootstrap guard.
func TestUsersAndBootstrap(t *testing.T) {
	db := mustDB(t)
	if _, err := db.CreateUser("bob", "h", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser("bob", "h", "user"); err != ErrConflict {
		t.Fatalf("duplicate username: %v", err)
	}
	u, err := db.GetUserByUsername("bob")
	if err != nil || u == nil || u.Role != "admin" {
		t.Fatalf("get user: %v %v", u, err)
	}
	has, err := db.HasAnyUser()
	if err != nil || !has {
		t.Fatalf("has any user: %v %v", has, err)
	}
	// disabled user stays queryable but flagged
	if err := db.SetUserStatus(u.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	u, _ = db.GetUserByUsername("bob")
	if u.Status != "disabled" {
		t.Fatalf("status: %v", u.Status)
	}
}

func TestLegacyMigrationRecoversCopyAndPreservesSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE apps(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT UNIQUE,key_hash TEXT UNIQUE,key_prefix TEXT,note TEXT,enabled INTEGER DEFAULT 1,created_at REAL,key_enc TEXT,user_id INTEGER,allowed_models TEXT DEFAULT '');
 INSERT INTO apps(id,name,key_hash,key_prefix,created_at) VALUES(4,'old','oldhash','sk-x',1),(9,'deleted','h9','sk-y',1);
 DELETE FROM apps WHERE id=9;
 CREATE TABLE apps_v2 AS SELECT * FROM apps;
 PRAGMA user_version=14;`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateApp("new", "newhash", "sk-new", "", "", "", 0)
	if err != nil || id != 10 {
		t.Fatalf("sequence id=%d err=%v", id, err)
	}
	if _, err := db.DeleteApp(id); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err = db.CreateApp("newer", "newerhash", "sk-next", "", "", "", 0)
	if err != nil || id != 11 {
		t.Fatalf("reopen sequence id=%d err=%v", id, err)
	}
	apps, err := db.ListApps()
	if err != nil || len(apps) != 2 || apps[0].ID != 4 {
		t.Fatalf("preservation: %+v %v", apps, err)
	}
}
func TestAtomicPortalCapsAndBootstrap(t *testing.T) {
	db := mustDB(t)
	uid, err := db.CreateUser("ordinary", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	successes := make(chan int64, 20)
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := db.CreateAppForUserLimited(uid, fmt.Sprintf("k%d", i), fmt.Sprintf("h%d", i), "", 2)
			if err == nil {
				successes <- id
			} else if !errors.Is(err, ErrKeyLimit) {
				failures <- err
			}
		}(i)
	}
	wg.Wait()
	close(successes)
	close(failures)
	if len(successes) != 2 {
		t.Fatalf("created %d keys", len(successes))
	}
	for err := range failures {
		t.Error(err)
	}
	admins := make(chan int64, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := db.CreateInitialAdmin(fmt.Sprintf("admin%d", i), "hash")
			if err == nil {
				admins <- id
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(admins)
	if len(admins) != 1 {
		t.Fatalf("created %d admins", len(admins))
	}
	has, err := db.HasAdminUser()
	if !has || err != nil {
		t.Fatalf("hasadmin=%v %v", has, err)
	}
}
func TestSessionsBoundedAndPasswordAtomicRevocation(t *testing.T) {
	db := mustDB(t)
	uid, err := db.CreateUser("sessionuser", "oldhash", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUserSession("expired", uid, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxUserSessions+4; i++ {
		if err := db.CreateUserSession(fmt.Sprint(i), uid, float64(time.Now().Unix()+3600)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM user_sessions WHERE user_id=?", uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != MaxUserSessions {
		t.Fatalf("sessions=%d", n)
	}
	if err := db.UpdateUserPassword(uid, "newhash"); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM user_sessions WHERE user_id=?", uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sessions after password=%d", n)
	}
}
func TestDailyQuotaReservationsIgnoreLogsAndHandleConcurrency(t *testing.T) {
	db := mustDB(t)
	uid, err := db.CreateUser("quotauser", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	const day = 123.0
	var wg sync.WaitGroup
	success := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := db.ReserveUserDailyQuota(uid, day, 1, 2, 3, 4, 8, 12)
			if err == nil {
				success <- true
			} else if !errors.Is(err, ErrDailyQuota) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(success) != 4 {
		t.Fatalf("reservations=%d", len(success))
	}
	if err := db.LogUsage(UsageParams{UserID: uid, Status: "error"}); err != nil {
		t.Fatal(err)
	}
	req, in, out, err := db.UserDailyQuota(uid, day)
	if err != nil || req != 4 || in != 8 || out != 12 {
		t.Fatalf("quota=%d,%d,%d %v", req, in, out, err)
	}
	if err := db.SettleUserDailyQuota(uid, day, 1, 2, 3, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveUserDailyQuota(uid, day, 1, 2, 3, 4, 8, 12); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveUserDailyQuota(uid, day+86400, 50, 50, 50, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
}
func TestStableAppUsageAndInactivePoolExclusion(t *testing.T) {
	db := mustDB(t)
	u1, _ := db.CreateUser("usageone", "h", "user")
	u2, _ := db.CreateUser("usagetwo", "h", "user")
	a1, _ := db.CreateApp("same", "sameh1", "sk-1", "", "", "", u1)
	a2, _ := db.CreateApp("same", "sameh2", "sk-2", "", "", "", u2)
	if err := db.LogUsage(UsageParams{AppID: a1, UserID: u1, AppName: "same", InputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	if err := db.LogUsage(UsageParams{AppID: a2, UserID: u2, AppName: "same", InputTokens: 8}); err != nil {
		t.Fatal(err)
	}
	apps, err := db.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Tokens != 3 || apps[1].Tokens != 8 {
		t.Fatalf("stats %+v", apps)
	}
	g, _ := db.CreateResourceGroup("scope", "workbuddy", "[\"m\"]")
	db.SetGroupEnabled(g, true)
	db.GrantGroup(u1, g)
	db.AddGroupAccount(g, "private")
	db.AddGroupAccount(g, "revoked")
	db.AddGroupAccount(g, "active")
	db.CreateContribution(u2, "revoked", "workbuddy", "cn", "revoked")
	db.CreateContribution(u2, "active", "workbuddy", "cn", "active")
	uids, err := db.GroupAccountsOf(u1)
	if err != nil || len(uids) != 1 || !uids["active"] {
		t.Fatalf("scope %+v %v", uids, err)
	}
}

func TestSessionIssueRejectsStalePasswordAndDisabledUser(t *testing.T) {
	db := mustDB(t)
	uid, err := db.CreateUser("lograce", "oldhash", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateUserPassword(uid, "newhash"); err != nil {
		t.Fatal(err)
	}
	expiry := float64(time.Now().Unix() + 3600)
	if err := db.CreateUserSessionForPassword("oldtoken", uid, expiry, "oldhash"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale issue=%v", err)
	}
	if err := db.CreateUserSessionForPassword("newtoken", uid, expiry, "newhash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserStatus(uid, "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUserSessionForPassword("disabledtoken", uid, expiry, "newhash"); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled issue=%v", err)
	}
}

func TestUnknownUsageFlags(t *testing.T) {
	db := mustDB(t)
	uid, _ := db.CreateUser("unknownusage", "h", "user")
	id, _ := db.CreateApp("unknown", "unknownhash", "sk-x", "", "", "", uid)
	known, err := db.UserDailyUsageKnown(uid, 0)
	if err != nil || !known {
		t.Fatalf("empty known=%v %v", known, err)
	}
	unknown := false
	if err := db.LogUsage(UsageParams{AppID: id, UserID: uid, AppName: "unknown", TokensKnown: &unknown}); err != nil {
		t.Fatal(err)
	}
	known, err = db.UserDailyUsageKnown(uid, 0)
	if err != nil || known {
		t.Fatalf("unknown known=%v %v", known, err)
	}
	apps, err := db.UserApps(uid)
	if err != nil || len(apps) != 1 || apps[0].TokensKnown || apps[0].CreditsKnown {
		t.Fatalf("user flags %+v %v", apps, err)
	}
	apps, err = db.ListApps()
	if err != nil || len(apps) != 1 || apps[0].TokensKnown || apps[0].CreditsKnown {
		t.Fatalf("admin flags %+v %v", apps, err)
	}
}
func TestActivationRequiresPersistedAccount(t *testing.T) {
	db := mustDB(t)
	uid, _ := db.CreateUser("activation", "h", "user")
	id, _ := db.CreateContribution(uid, "accountmissing", "workbuddy", "cn", "verifying")
	if err := db.ActivateContributionWithGroups(id, uid); !errors.Is(err, ErrConflict) {
		t.Fatalf("activate missing account=%v", err)
	}
	c, err := db.ContributionByAccount("accountmissing")
	if err != nil || c.Status != "verifying" {
		t.Fatalf("rolledback %+v %v", c, err)
	}
	if _, err := db.db.Exec("INSERT INTO accounts(uid,enabled,disabled_reason) VALUES('accountmissing',0,'revoked')"); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateContributionWithGroups(id, uid); err != nil {
		t.Fatal(err)
	}
	var enabled int
	var reason string
	if err := db.db.QueryRow("SELECT enabled,disabled_reason FROM accounts WHERE uid='accountmissing'").Scan(&enabled, &reason); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || reason != "" {
		t.Fatalf("account=%d %q", enabled, reason)
	}
}

func TestAtomicInvitedRegistration(t *testing.T) {
	db := mustDB(t)
	if err := db.CreateInviteCode("ONE", 0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	success := make(chan int64, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := db.CreateInvitedUser(fmt.Sprintf("invited%d", i), "hash", "ONE")
			if err == nil {
				success <- id
			} else if !errors.Is(err, ErrInvalidInvite) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(success)
	if len(success) != 1 {
		t.Fatalf("successful registrations=%d", len(success))
	}
	winner := <-success
	users, err := db.ListUsers()
	if err != nil || len(users) != 1 || users[0].ID != winner || users[0].Role != "user" {
		t.Fatalf("users=%+v %v", users, err)
	}
	invite, err := db.FindInviteCode("ONE")
	if err != nil || invite == nil || invite["used_by"] != winner {
		t.Fatalf("invite=%+v %v", invite, err)
	}
	if _, err := db.CreateInvitedUser("usedcode", "hash", "ONE"); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("used invite=%v", err)
	}
	if _, err := db.CreateInvitedUser("unknowncode", "hash", "UNKNOWN"); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("unknown invite=%v", err)
	}
	if err := db.CreateInviteCode("EXPIRED", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateInvitedUser("expiredcode", "hash", "EXPIRED"); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("expired invite=%v", err)
	}
	users, err = db.ListUsers()
	if err != nil || len(users) != 1 {
		t.Fatalf("invalid invite left users=%+v %v", users, err)
	}
	if err := db.CreateInviteCode("FRESH", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateInvitedUser(users[0].Username, "hash", "FRESH"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username=%v", err)
	}
	invite, err = db.FindInviteCode("FRESH")
	if err != nil || invite == nil || invite["used_by"] != nil {
		t.Fatalf("duplicate consumed invite=%+v %v", invite, err)
	}
}
