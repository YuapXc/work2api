package store

import (
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// settings defaults
	s, err := db.GetSettings()
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if s["checkin_hours"] != "9,21" {
		t.Fatalf("default setting wrong: %v", s["checkin_hours"])
	}

	// app create + find
	id, err := db.CreateApp("app1", "hash123", "sk-abc", "note", "enc")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if id == 0 {
		t.Fatal("no app id")
	}
	app, err := db.FindAppByKey("hash123")
	if err != nil || app == nil {
		t.Fatalf("find app: %v %v", app, err)
	}
	if app["name"] != "app1" {
		t.Fatalf("app name: %v", app["name"])
	}

	// usage log + summary
	if err := db.LogUsage(UsageParams{Model: "m1", Protocol: "chat", AccountUID: "u1",
		InputTokens: 10, OutputTokens: 20, AppName: "app1"}); err != nil {
		t.Fatalf("log usage: %v", err)
	}
	sum, err := db.UsageSummary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum["total_requests"].(int64) != 1 || sum["total_tokens"].(int64) != 30 {
		t.Fatalf("summary wrong: %v", sum)
	}

	// account upsert + list
	auth := map[string]any{
		"auth":    map[string]any{"domain": "www.codebuddy.cn", "accessToken": "x"},
		"account": map[string]any{"uid": "u1", "nickname": "nick"},
	}
	if _, err := db.UpsertAccount(auth); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	accs, err := db.ListAccounts()
	if err != nil || len(accs) != 1 {
		t.Fatalf("list accounts: %v %v", accs, err)
	}
	if accs[0]["profile"] != "cn-cli" {
		t.Fatalf("profile: %v", accs[0]["profile"])
	}
}
