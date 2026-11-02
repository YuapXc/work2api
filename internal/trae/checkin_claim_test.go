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
