package trae

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/store"
	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

func TestClaimAcceptedWaitsForConfirmationAndSurvivesRestart(t *testing.T) {
	var claims atomic.Int32
	var confirmed atomic.Bool
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/" {
			t.Error("unexpected root path")
		}

		if req.URL.Path == upstream.EpCheckinClaim {
			claims.Add(1)
			io.WriteString(w, `{"code":0}`)
			return
		}
		fmt.Fprintf(w, `{"code":0,"enable":true,"checked_in":%v}`, confirmed.Load())
	})
	a := &auth.Auth{UID: "claim-user", Variant: "cn", AccessToken: "fixture", DeviceID: "1234567890123456", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	id := accountID(a.UID)
	a.FilePath = filepath.Join(r.dir, id+"-cn.json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.UpsertProviderAccount(store.ProviderAccount{UID: id, Provider: "trae", Auth: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := r.db.SetAccountState(id, map[string]any{"enabled": false}); err != nil {
		t.Fatal(err)
	}
	r.accounts[id] = &accountState{Auth: a, Enabled: false, Identities: map[string]*auth.Auth{"cn": a}}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := r.AdminCheckin(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			if result["results"].([]map[string]any)[0]["status"] != "pending" {
				t.Error("claim falsely marked checked", result)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatal("repeated claim", claims.Load())
	}
	history, _ := r.db.CheckinHistory("")
	if len(history[id]) != 0 {
		t.Fatal("unconfirmed claim stored as success")
	}
	r2, err := New(filepath.Dir(r.dir), r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	r2.transport = r.transport
	if _, err = r2.AdminCheckin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if claims.Load() != 1 {
		t.Fatal("restart repeated accepted claim")
	}
	confirmed.Store(true)
	result, err := r2.AdminCheckin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result["results"].([]map[string]any)[0]["status"] != "already" {
		t.Fatal(result)
	}
	history, _ = r.db.CheckinHistory("")
	if len(history[id]) != 1 {
		t.Fatal("confirmed day missing")
	}
	if r2.accounts[id].Enabled {
		t.Fatal("maintenance enabled disabled account")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r2.AdminCheckin(ctx); err == nil {
		t.Fatal("canceled maintenance continued")
	}
}

func TestCheckinRewardUsesConfirmedKnownAmountsOnly(t *testing.T) {
	for _, tc := range []struct {
		name, claim, status string
		known               bool
		want                int64
		confirmed           bool
	}{
		{"claim", `{"code":0,"credits_granted":7}`, `"credits":10,"extra_credits":2`, true, 7, true},
		{"fallback", `{"code":0}`, `"credits":10,"extra_credits":2`, true, 12, true},
		{"explicit_zero", `{"code":0,"credits_granted":0}`, `"credits":10`, true, 0, true},
		{"status_zero", `{"code":0}`, `"credits":0`, true, 0, true},
		{"unknown", `{"code":0}`, `"unknown":true`, false, 0, true},
		{"pending", `{"code":0,"credits_granted":7}`, `"credits":10`, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var claims atomic.Int32
			r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == upstream.EpCheckinClaim {
					claims.Add(1)
					fmt.Fprint(w, tc.claim)
					return
				}
				fmt.Fprintf(w, `{"code":0,"enable":true,"checked_in":%v,%s}`, claims.Load() > 0 && tc.confirmed, tc.status)
			})
			a := &auth.Auth{UID: "reward", Variant: "cn", AccessToken: "fixture", DeviceID: "1234567890123456", ExpiresAt: time.Now().Add(time.Hour).Unix(), FilePath: filepath.Join(r.dir, "reward.json")}
			if e := a.SaveAtomic(); e != nil {
				t.Fatal(e)
			}
			id := accountID(a.UID)
			r.accounts[id] = &accountState{Auth: a, Enabled: true, Identities: map[string]*auth.Auth{"cn": a}}
			result, err := r.AdminCheckin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			row := result["results"].([]map[string]any)[0]
			credit, known := row["credit"].(int64)
			if known != tc.known || known && credit != tc.want || claims.Load() != 1 {
				t.Fatal(row, claims.Load())
			}
			if tc.confirmed && row["ok"] != true {
				t.Fatal(row)
			}
			if !tc.confirmed && (row["status"] != "pending" || row["ok"] == false) {
				t.Fatal(row)
			}
			next, err := r.AdminCheckin(t.Context())
			if err != nil || claims.Load() != 1 {
				t.Fatal(err, claims.Load())
			}
			if _, exists := next["results"].([]map[string]any)[0]["credit"]; exists {
				t.Fatal("reward counted twice")
			}
		})
	}
}
