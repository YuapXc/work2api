package app

import (
	"path/filepath"
	"testing"
	"time"

	"work2api/internal/store"
)

func TestCreditAlertsPackagePointer(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{o: &Orchestrator{db: db}}

	// 内存态（pool.AllAccounts 直通）：expire_at 是 *float64
	var f float64 = float64(time.Now().Add(36 * time.Hour).Unix())
	pkg := map[string]any{"name": "Free Plan Subscription", "remain": 37.38, "expire_at": &f}
	acc := map[string]any{"uid": "79980c4f-x", "enabled": true, "label": "test", "credit_packages": []any{pkg}}
	alerts := s.creditAlerts([]map[string]any{acc})
	found := false
	for _, a := range alerts {
		if a["kind"] == "expiry" {
			found = true
			t.Logf("alert: %v", a["message"])
		}
	}
	if !found {
		t.Fatal("*float64 形态的包到期应触发按包提醒")
	}

	// JSON 态：expire_at 已归一化为 float64
	pkg2 := map[string]any{"name": "Bonus Pack", "remain": 60.0, "expire_at": float64(time.Now().Add(48 * time.Hour).Unix())}
	acc2 := map[string]any{"uid": "05d11bc8-x", "enabled": true, "label": "t2", "credit_packages": []any{pkg2}}
	alerts2 := s.creditAlerts([]map[string]any{acc2})
	found2 := false
	for _, a := range alerts2 {
		if a["kind"] == "expiry" {
			found2 = true
			t.Logf("alert2: %v", a["message"])
		}
	}
	if !found2 {
		t.Fatal("float64 形态的包到期应触发按包提醒")
	}

	// 已耗尽包（remain=0）不提醒
	pkg3 := map[string]any{"name": "Exhausted", "remain": 0.0, "expire_at": float64(time.Now().Add(24 * time.Hour).Unix())}
	acc3 := map[string]any{"uid": "c3922514-x", "enabled": true, "label": "t3", "credit_packages": []any{pkg3}}
	for _, a := range s.creditAlerts([]map[string]any{acc3}) {
		if a["kind"] == "expiry" {
			t.Fatal("已耗尽包不应提醒")
		}
	}
}
