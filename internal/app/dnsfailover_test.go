package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"work2api/internal/config"
	"work2api/internal/store"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

type failingUpstream struct {
	err   error
	calls int
	emit  bool
}

func (f *failingUpstream) StreamUpstream(_ context.Context, _ map[string]string, _ map[string]any, target string, yield upstream.LineFunc) error {
	if target == "" {
		return errors.New("test account has no upstream URL")
	}
	f.calls++
	if f.emit {
		if err := yield(`data: {"choices":[{"delta":{"content":"partial"}}]}`); err != nil {
			return err
		}
	}
	return f.err
}
func newDNSFailoverOrch(t *testing.T, client *failingUpstream) (*Orchestrator, *pool.Account) {
	t.Helper()
	dir := t.TempDir()
	uid := "test-account"
	raw, err := json.Marshal(map[string]any{"auth": map[string]any{"access_token": "test-token", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": uid, "nickname": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uid+".info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := pool.New(map[string]pool.Credential{uid: mgr}, "")
	o := &Orchestrator{db: db, cfg: &config.Config{}, pool: p, models: models.New(p, db), managers: map[string]*credentials.Manager{uid: mgr}, modelCooldowns: map[string]cdEntry{}, sessions: newSessionRouter(), upstreamClient: client}
	acc := p.Get(uid)
	if acc == nil || acc.Profile == "" {
		t.Fatal("fixture must contain a routable pooled account")
	}
	return o, acc
}
func TestOpenUpstreamFailureAccounting(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "upstream.invalid", IsNotFound: true}
	for _, tc := range []struct {
		name        string
		err         error
		emit        bool
		wantFailure bool
	}{
		{"dns", dnsErr, false, false},
		{"wrapped dns", fmt.Errorf("dial: %w", dnsErr), false, false},
		{"dns timeout", &net.DNSError{Err: "timeout", Name: "upstream.invalid", IsTimeout: true}, false, false},
		{"connection reset", errors.New("connection reset"), false, true},
		{"upstream 429", &upstream.UpstreamError{StatusCode: 429, Raw: []byte(`{"error":{"message":"rate limited"}}`)}, false, true},
		{"canceled", context.Canceled, false, false},
		{"stream interrupted", errors.New("connection reset"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &failingUpstream{err: tc.err, emit: tc.emit}
			o, acc := newDNSFailoverOrch(t, client)
			_, err := o.openUpstream(context.Background(), acc, map[string]any{}, "test-model", "", func(string) error { return nil }, nil)
			if !errors.Is(err, tc.err) {
				t.Fatalf("wrong propagated error: %v", err)
			}
			if client.calls != 1 {
				t.Fatalf("want one upstream call, got %d", client.calls)
			}
			got := o.pool.Get(acc.UID)
			if got == nil {
				t.Fatal("pooled account disappeared")
			}
			if (got.CooldownUntil > 0) != tc.wantFailure || (got.FailureCount > 0) != tc.wantFailure {
				t.Fatalf("unexpected account penalty: cooldown=%v failures=%d", got.CooldownUntil, got.FailureCount)
			}
		})
	}
}
