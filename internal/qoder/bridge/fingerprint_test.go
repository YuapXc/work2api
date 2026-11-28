package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestCatalogClientViewsPreserveModelsAndChatIdentity(t *testing.T) {
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("cosy-clienttype") {
		case "0":
			fmt.Fprint(w, `{"chat":[{"key":"hmodel","display_name":"HungerG","enable":true,"price_factor":0,"is_vl":true}]}`)
		case "5":
			fmt.Fprint(w, `{"chat":[{"key":"hmodel","enable":true,"price_factor":9},{"key":"old","enable":true}]}`)
		case "10":
			w.WriteHeader(503)
		default:
			t.Error("unknown view")
		}
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	http.DefaultTransport = catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = target.Host
		return transport.RoundTrip(clone)
	})
	sess, _ := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture"}, "fixture", "fixture", "windows")
	c := NewBearerClient(sess)
	c.fingerprint = &account.CheckinFingerprint{MachineID: "device", MachineToken: "token", MachineType: "windows"}
	b := &Bridge{client: c, region: account.RegionCN}
	models, err := b.ListAvailableModelsContext(t.Context())
	if err != nil || len(models) != 2 || models[0].Key != "hmodel" || models[0].PriceFactor == nil || *models[0].PriceFactor != 0 {
		t.Fatal(models, err)
	}
	headers, _ := c.buildHeaders("/chat", "", "application/json", nil)
	if headers["cosy-clienttype"] != "10" || headers["cosy-machineid"] != "device" {
		t.Fatal("chat identity changed")
	}
}

type catalogTestTransport func(*http.Request) (*http.Response, error)

func (f catalogTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCatalogEmptyAndFailedViewDoesNotEraseModels(t *testing.T) {
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	primary, failed := `{"chat":[]}`, true
	http.DefaultTransport = catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		status, body := 200, primary
		if req.Header.Get("cosy-clienttype") == "5" {
			body = `{"assistant":[]}`
			if failed {
				status, body = 503, `{"message":"unavailable"}`
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	sess, _ := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture"}, "fixture", "fixture", "windows")
	bridge := &Bridge{client: NewBearerClient(sess), region: account.RegionCN}
	for _, view := range []string{primary, `{"chat":[{"key":"kmodel"},{"key":"private","source":"custom"},{"key":" "}]}`} {
		primary = view
		if models, err := bridge.ListAvailableModelsContext(t.Context()); err == nil || len(models) != 0 {
			t.Fatal("partial empty directory published", err, models)
		}
	}
	failed = false
	if models, err := bridge.ListAvailableModelsContext(t.Context()); err != nil || len(models) != 0 {
		t.Fatal("confirmed empty directory changed", err, models)
	}
}
