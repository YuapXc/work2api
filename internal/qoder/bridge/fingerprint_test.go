package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/cosy"
)

func TestOptionalDesktopFingerprintPreservesLegacy(t *testing.T) {
	sess, e := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture"}, "legacy-id", "legacy-token", "windows")
	if e != nil {
		t.Fatal(e)
	}
	c := NewBearerClient(sess)
	h, e := c.buildHeaders("/models", "", "application/json", nil)
	if e != nil || h["cosy-clienttype"] != "5" || h["cosy-machineid"] != "legacy-id" {
		t.Fatal("legacy changed")
	}
	c.fingerprint = &account.CheckinFingerprint{MachineID: "real-id", MachineToken: "real-token", MachineType: "windows"}
	h, e = c.buildHeaders("/models", "", "application/json", nil)
	if e != nil || h["cosy-clienttype"] != "10" || h["cosy-machineid"] != "real-id" || h["cosy-version"] != cosy.AppVersion {
		t.Fatal("desktop identity not forwarded")
	}
}

func TestAppHeaderAndBodyVersionStayTogether(t *testing.T) {
	for _, desktop := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wire, _ := io.ReadAll(r.Body)
			plain, err := cosy.Decode(string(wire))
			if err != nil {
				t.Error(err)
			}
			var body map[string]any
			json.Unmarshal(plain, &body)
			business, _ := body["business"].(map[string]any)
			if desktop {
				if r.Header.Get("cosy-version") != cosy.AppVersion || business["version"] != cosy.AppVersion || body["chat_task"] != "FREE_INPUT" || body["version"] != "3" || body["aliyun_user_type"] != "" {
					t.Error("mixed app protocol versions")
				}
			} else if r.Header.Get("cosy-version") != cosy.Version || body["chat_task"] != nil {
				t.Error("legacy IDE protocol changed")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			frame, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": `{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", frame)
		}))
		sess, err := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture", UserType: "personal_standard"}, "fixture", "fixture", "windows")
		if err != nil {
			t.Fatal(err)
		}
		c := NewBearerClient(sess)
		if desktop {
			c.fingerprint = &account.CheckinFingerprint{MachineID: "real-id", MachineToken: "real-token", MachineType: "windows"}
		}
		b := &Bridge{sess: sess, client: c, templateBase: map[string]any{"business": map[string]any{"version": cosy.Version}}, chatStreamURL: srv.URL}
		err = b.CallQoderWithOpts(t.Context(), "codex", nil, "qfmodel", nil, CallOpts{}, func(Delta) {})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}
