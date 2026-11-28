package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/store"
	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

func TestModelFaultDoesNotCoolOtherModels(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "fault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		status int
		raw    string
		kind   ErrKind
	}{{500, `{"code":11134}`, ErrModelProvider}, {502, `{"error":{"code":"11134"}}`, ErrModelProvider}, {429, `{"code":6004,"msg":"switch models"}`, ErrModelRate}} {
		mgrPath := filepath.Join(t.TempDir(), "auth.info")
		if err := os.WriteFile(mgrPath, []byte(`{"auth":{"accessToken":"fixture","domain":"www.codebuddy.cn"},"account":{"uid":"fixture"}}`), 0600); err != nil {
			t.Fatal(err)
		}
		mgr := credentials.NewManager(mgrPath)
		p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
		r := &Runtime{Pool: p, Managers: map[string]*credentials.Manager{"fixture": mgr}, db: db, Cooldowns: map[string]CooldownEntry{}}
		if got := ClassifyUpstream(tc.status, []byte(tc.raw), nil); got != tc.kind {
			t.Fatal(got, tc.kind)
		}
		act := r.PenalizeAccount(p.Get("fixture"), "m", &upstream.UpstreamError{StatusCode: tc.status, Raw: []byte(tc.raw)})
		if !act.ModelScoped || !act.Rotate || p.Get("fixture").CooldownUntil > nowSec() || !p.Get("fixture").Enabled || r.Cooldowns["fixture|m"].Until <= nowSec() {
			t.Fatal("account-wide penalty", act)
		}
	}
}

func TestDisabledAccountEntersMaintenanceWithoutReenabling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":1},"account":{"uid":"fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	r := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: pool.New(map[string]pool.Credential{"fixture": mgr}, "")}
	r.Pool.SetEnabled("fixture", false, "手动停用")
	acc := r.Pool.Get("fixture")
	if err := r.prepareBillingMaintenance(context.Background(), acc); err != nil {
		t.Fatalf("manual disable incorrectly pauses maintenance: %v", err)
	}
	identity, err := mgr.MaintenanceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	g := r.billingGuard()
	g.State(acc.UID, identity)
	g.Observe(acc.UID, identity, "authorization_expired", true)
	if _, err := r.checkinAccount(context.Background(), acc); !errors.Is(err, provider.ErrMaintenanceLogin) {
		t.Fatalf("known invalid auth still reached check-in: %v", err)
	}
	if r.Pool.Get("fixture").Enabled {
		t.Fatal("maintenance reenabled disabled account")
	}
}

type maintenanceTransport func(*http.Request) (*http.Response, error)

func (f maintenanceTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func TestConfirmedExpiryCannotUseStickyScopedOrAdmissionSelection(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	managers := map[string]*credentials.Manager{}
	creds := map[string]pool.Credential{}
	for _, uid := range []string{"expired", "valid"} {
		path := filepath.Join(dir, uid+".info")
		body := `{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":` + fmt.Sprint(time.Now().Add(time.Hour).UnixMilli()) + `},"account":{"uid":"` + uid + `"}}`
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		managers[uid] = credentials.NewManager(path)
		creds[uid] = managers[uid]
	}
	r := &Runtime{Managers: managers, Pool: pool.New(creds, "")}
	r.Catalog = models.NewWithCatalogClient(r.Pool, db, &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"m","credits":1}],"agents":[{"name":"cli","models":["m"]}]}}`))}, nil
	})})
	r.Configure(&config.Config{DataDir: dir}, db, NewSessionRouter())
	if _, err := r.Catalog.RefreshWithStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Sessions.Bind("sticky", "expired")
	mgr := managers["expired"]
	identity, _ := mgr.MaintenanceIdentity()
	g := r.billingGuard()
	g.State("expired", identity)
	g.Observe("expired", identity, "authorization_expired", true)
	if r.ModelCallable(r.Pool.Get("expired"), "m") || r.ModelHealthy(r.Pool.Get("expired"), "m", nowSec()) || r.AccountHealthy(r.Pool.Get("expired"), nowSec()) {
		t.Fatal("invalid credential remains callable")
	}
	selected, e := r.PickAccount("m", "sticky")
	if e != nil || selected.UID != "valid" {
		t.Fatal("sticky selection kept invalid credential", e)
	}
	selector, e := r.AccountSelector("m", "expired", nil, map[string]bool{"expired": true, "valid": true})
	if e != nil {
		t.Fatal(e)
	}
	selected, e = selector(nil)
	if e != nil || selected.UID != "valid" {
		t.Fatal("admission preferred invalid credential", e)
	}
	if _, e = r.PickInScope("m", "sticky", nil, map[string]bool{"expired": true}); e == nil {
		t.Fatal("scoped selection bypassed auth exclusion")
	}
	if _, err := r.headers(context.Background(), r.Pool.Get("expired"), nil); err == nil {
		t.Fatal("send boundary accepted known-invalid credentials")
	}
	if !r.Pool.Get("expired").Enabled {
		t.Fatal("auth state changed manual enablement")
	}
}

func TestDiagnosticUsageDoesNotEnterUserStatistics(t *testing.T) {
	r := &Runtime{}
	ctx := provider.WithInvocation(context.Background(), provider.Invocation{Diagnostic: true})
	r.LogUsage(LogArgs{Ctx: ctx, Status: "ok", Usage: map[string]any{"prompt_tokens": 1, "completion_tokens": 1}})
	// No configured DB/usage sink: a diagnostic must return before persistence.
}

func TestReadOnlyCheckReusesFreshBillingWithoutEnablingAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "check.info")
	body := `{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":` + fmt.Sprint(time.Now().Add(time.Hour).UnixMilli()) + `},"account":{"uid":"fixture"}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	identity, _ := mgr.MaintenanceIdentity()
	r := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: pool.New(map[string]pool.Credential{"fixture": mgr}, ""), creditStates: map[string]creditState{"fixture": {Manager: mgr, Identity: identity, Updated: nowSec()}}}
	r.Pool.SetEnabled("fixture", false, "手动停用")
	result := r.CheckAccount(context.Background(), "fixture", false)
	if result.Status != "valid" || r.Pool.Get("fixture").Enabled {
		t.Fatal("fresh check made extra requests or enabled account", result)
	}
}

func TestCreditRejectionQueuesOneFreshReadAfterOlderRefresh(t *testing.T) {
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	managers := map[string]*credentials.Manager{}
	creds := map[string]pool.Credential{}
	for _, uid := range []string{"credit", "rate"} {
		path := filepath.Join(dir, uid+".info")
		body := fmt.Sprintf(`{"auth":{"accessToken":"fixture","refreshToken":"refresh","domain":"www.workbuddy.cn","expiresAt":%d},"account":{"uid":"%s"}}`, time.Now().Add(time.Hour).UnixMilli(), uid)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		managers[uid] = credentials.NewManager(path)
		creds[uid] = managers[uid]
	}
	r := &Runtime{Managers: managers, Pool: pool.New(creds, ""), db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() { cancel(); _ = r.Close() }()
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	var calls, other atomic.Int32
	r.creditQuery = func(ctx context.Context, mgr billing.Credential) (*billing.Credit, error) {
		if mgr != managers["credit"] {
			other.Add(1)
		}
		if calls.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &billing.Credit{Remain: 15, Total: 20}, nil
	}
	rate, credit := r.Pool.Get("rate"), r.Pool.Get("credit")
	r.PenalizeAccount(rate, "test", &upstream.UpstreamError{StatusCode: 429})
	r.creditRechecks.mu.Lock()
	rateTriggered := r.creditRechecks.done != nil
	r.creditRechecks.mu.Unlock()
	if rateTriggered {
		t.Fatal("generic rate limit triggered a billing read")
	}
	manualDone := make(chan struct{})
	go func() {
		defer close(manualDone)
		r.RefreshCreditsFor(ctx, db, credit)
	}()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("manual read did not start")
	}
	for range 6 {
		if act := r.PenalizeAccount(credit, "test", &upstream.UpstreamError{StatusCode: 402}); !act.Rotate {
			t.Fatal("quota recheck prevented immediate failover")
		}
	}
	r.creditRechecks.mu.Lock()
	done := r.creditRechecks.done
	r.creditRechecks.mu.Unlock()
	if done == nil || calls.Load() != 1 {
		t.Fatal("recheck did not wait for the same account's ongoing read")
	}
	close(releaseFirst)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("queued quota recheck did not finish")
	}
	<-manualDone
	if calls.Load() != 2 || other.Load() != 0 || !r.Pool.Get("credit").Healthy(nowSec()) || r.Pool.Get("rate").Healthy(nowSec()) {
		t.Fatalf("reads=%d other=%d, cooldown state inconsistent", calls.Load(), other.Load())
	}
	r.recheckCredits(ctx, creditRecheck{account: credit, manager: credentials.NewManager("old"), requested: nowSec()})
	if calls.Load() != 2 {
		t.Fatal("stale credential scheduled an extra query")
	}
}

func TestMediaLostResponseIsNotReplayedAndDispatchIsRechecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "media.info")
	raw := fmt.Sprintf(`{"auth":{"accessToken":"fixture","domain":"www.codebuddy.cn","expiresAt":%d},"account":{"uid":"media"}}`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	calls := 0
	r := &Runtime{Managers: map[string]*credentials.Manager{"media": mgr}, Pool: pool.New(map[string]pool.Credential{"media": mgr}, "")}
	r.MediaClient = &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) { calls++; return nil, io.ErrUnexpectedEOF })}
	request := provider.MediaRequest{Operation: "video", Model: "movie", AccountID: "media", Payload: map[string]any{"model": "movie", "prompt": "test"}}
	if _, err := r.CallMedia(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("submission replayed", err, calls)
	}
	ctx := provider.WithInvocation(context.Background(), provider.Invocation{Check: func(context.Context, provider.AccountRef, string) error { return errors.New("revoked") }})
	if _, err := r.CallMedia(ctx, request); err == nil || calls != 1 {
		t.Fatal("revoked request reached network", err, calls)
	}
}

func TestTranscriptionUploadSeparatesStorageAuthorization(t *testing.T) {
	r := &Runtime{}
	calls := []string{}
	r.MediaClient = &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Method+" "+req.URL.Host+req.URL.Path)
		body := `{"code":0,"data":{"items":[{"upload_url":"https://bucket.cos.ap-beijing.myqcloud.com/upload","download_url":"https://bucket.cos.ap-beijing.myqcloud.com/audio.wav"}]}}`
		if req.Method == "PUT" {
			if req.Header.Get("Authorization") != "" || req.Header.Get("X-User-Id") != "" {
				t.Fatal("account credential leaked to object storage")
			}
			body = ""
		} else if req.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatal("presign missing auth")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	u, err := r.uploadMediaInput(context.Background(), "https://copilot.tencent.com", map[string]string{"Authorization": "Bearer fixture"}, map[string]any{"data": "UklGRg==", "mime": "audio/wav"})
	if err != nil || !strings.HasSuffix(u, "audio.wav") || len(calls) != 2 {
		t.Fatal(u, calls, err)
	}
	r.MediaClient = &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"items":[{"upload_url":"https://127.0.0.1/upload","download_url":"https://127.0.0.1/file"}]}}`))}, nil
	})}
	if _, err = r.uploadMediaInput(context.Background(), "https://copilot.tencent.com", nil, map[string]any{"data": "UklGRg==", "mime": "audio/wav"}); err == nil {
		t.Fatal("unsafe storage origin accepted")
	}
}

type signatureFixtureStream func(context.Context, map[string]string, map[string]any, string, upstream.LineFunc) error

func (f signatureFixtureStream) StreamUpstream(ctx context.Context, h map[string]string, b map[string]any, u string, y upstream.LineFunc) error {
	return f(ctx, h, b, u, y)
}
func TestChatSignaturesRestoreOnlyTheirOwnTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.info")
	os.WriteFile(path, []byte(fmt.Sprintf(`{"auth":{"accessToken":"fixture","domain":"www.workbuddy.cn","expiresAt":%d},"account":{"uid":"fixture"}}`, time.Now().Add(time.Hour).UnixMilli())), 0600)
	mgr := credentials.NewManager(path)
	p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
	r := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p}
	acc := p.Get("fixture")
	body := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": "question"}}}
	req, _ := http.NewRequest("POST", "http://example.invalid", nil)
	req.Header.Set("Authorization", "Bearer caller-a")
	req = WithRequestSessionIdentity(req, body)
	ctx := req.Context()
	r.UpstreamClient = signatureFixtureStream(func(ctx context.Context, h map[string]string, b map[string]any, u string, y upstream.LineFunc) error {
		for _, line := range []string{`{"choices":[{"index":0,"delta":{"reasoning_content":"thought","extra_fields":{"reasoning_detail":{"content_signature":"signature"},"_meta":{"a":1}}}}]}`, `{"choices":[{"index":0,"delta":{"content":"answer","extra_fields":{"reasoning_detail":{"text":"thought"},"_meta":{"b":2}}}}]}`, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`} {
			if err := y("data: " + line); err != nil {
				return err
			}
		}
		return y("data: [DONE]")
	})
	if _, err := r.Stream(ctx, acc, body, func(string) error { return nil }, AttemptOptions{}); err != nil {
		t.Fatal(err)
	}
	second := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "next"}}}
	scope := r.chatSignatureScope(ctx, acc, "test")
	restored, marks := r.chatSignatures.restore(scope, second)
	m := restored["messages"].([]any)[0].(map[string]any)
	extra, _ := m["extra_fields"].(map[string]any)
	detail, _ := extra["reasoning_detail"].(map[string]any)
	meta, _ := extra["_meta"].(map[string]any)
	if len(marks) != 1 || m["reasoning"] != "thought" || detail["content_signature"] != "signature" || detail["text"] != "thought" || len(meta) != 2 {
		t.Fatal("signature merge lost fields", m)
	}
	for _, messages := range [][]any{{map[string]any{"role": "assistant", "content": "answer", "reasoning": "different thought", "extra_fields": nil}}, {map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "assistant", "content": "answer"}}} {
		restored, marks := r.chatSignatures.restore(scope, map[string]any{"messages": messages})
		if len(marks) != len(messages) {
			t.Fatal("upstream restoration range narrowed")
		}
		if len(messages) == 1 && restored["messages"].([]any)[0].(map[string]any)["reasoning"] != "different thought" {
			t.Fatal("client reasoning changed")
		}
	}
	clientOwned := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "answer", "reasoning": "own thought", "extra_fields": map[string]any{"own": true}}}}
	if _, marks := r.chatSignatures.restore(scope, clientOwned); len(marks) != 0 {
		t.Fatal("client fields overwritten")
	}
	if _, exists := second["messages"].([]any)[0].(map[string]any)["extra_fields"]; exists {
		t.Fatal("caller input mutated")
	}
	for _, foreign := range []string{scope + "other-key", r.chatSignatureScope(ctx, acc, "other-model")} {
		if _, found := r.chatSignatures.restore(foreign, second); len(found) > 0 {
			t.Fatal("scope crossed")
		}
	}
	mixed := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{"ans", map[string]any{"type": "text", "text": "wer"}}}}}
	if _, marks := r.chatSignatures.restore(scope, mixed); len(marks) != 1 {
		t.Fatal("mixed text blocks lost their signature")
	}
	for _, partial := range []bool{false, true} {
		sends := 0
		r.UpstreamClient = signatureFixtureStream(func(ctx context.Context, h map[string]string, b map[string]any, u string, y upstream.LineFunc) error {
			sends++
			if partial {
				if err := y(`data: {"choices":[{"delta":{"content":"first"}}]}`); err != nil {
					return err
				}
				return &upstream.UpstreamError{StatusCode: 400, Raw: []byte(`{"code":11159}`)}
			}
			return &upstream.UpstreamError{StatusCode: 400, Raw: []byte(`{"error":{"message":"invalid request"},"request":{"messages":[{"content":"thinking_signature_invalid"}]}}`)}
		})
		started, err := r.Stream(ctx, acc, second, func(string) error { return nil }, AttemptOptions{})
		if err == nil || sends != 1 || started != partial {
			t.Fatal("unexpected signature replay", partial, sends, started, err)
		}
	}
	var attempts int
	r.UpstreamClient = signatureFixtureStream(func(ctx context.Context, h map[string]string, b map[string]any, u string, y upstream.LineFunc) error {
		attempts++
		m := b["messages"].([]any)[0].(map[string]any)
		if attempts == 1 {
			if m["extra_fields"] == nil {
				t.Fatal("not restored")
			}
			return &upstream.UpstreamError{StatusCode: 400, Raw: []byte(`{"code":11159,"message":"BAD_SIGNATURE"}`)}
		}
		if m["extra_fields"] != nil {
			t.Fatal("rejected local signature retained")
		}
		return y(`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	})
	if _, err := r.Stream(ctx, acc, second, func(string) error { return nil }, AttemptOptions{}); err != nil || attempts != 2 {
		t.Fatal("bounded recovery failed", err, attempts)
	}
	if _, marks := r.chatSignatures.restore(scope, second); len(marks) > 0 {
		t.Fatal("refused signature not evicted")
	}
}
func TestSpeechWireFormatsAndRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "speech.info")
	os.WriteFile(path, []byte(fmt.Sprintf(`{"auth":{"accessToken":"fixture","domain":"www.workbuddy.cn","expiresAt":%d},"account":{"uid":"fixture"}}`, time.Now().Add(time.Hour).UnixMilli())), 0600)
	mgr := credentials.NewManager(path)
	r := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: pool.New(map[string]pool.Credential{"fixture": mgr}, "")}
	var calls int
	refuse := false
	asrFailure := false
	r.MediaClient = &http.Client{Transport: maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("User-Agent") != "node" || req.Header.Get("Authorization") != "Bearer fixture" || req.Header.Get("X-User-Id") != "fixture" {
			t.Fatal("wrong app headers")
		}
		if refuse {
			return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("refused"))}, nil
		}
		if req.URL.Path == "/agenttool/v1/tts" {
			var b map[string]string
			json.NewDecoder(req.Body).Decode(&b)
			if len([]byte(b["text"])) > 1000 {
				t.Fatal("chunk too large")
			}
			pcm := []byte{0, 0, 1, 0}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("event: audio\ndata: " + base64.StdEncoding.EncodeToString(pcm) + "\n\nevent: done\ndata: {}\n\n"))}, nil
		}
		if req.URL.Path != "/agenttool/v1/asr" {
			t.Fatal("unexpected route")
		}
		reader, err := req.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "audio" || part.Header.Get("Content-Type") != "audio/wav" || part.FileName() != "voice.wav" {
			t.Fatal("wrong audio form")
		}
		data, _ := io.ReadAll(part)
		if !bytes.Equal(data, []byte("wave")) {
			t.Fatal("audio changed")
		}
		body := `{"code":0,"text":"hello"}`
		if asrFailure {
			body = `{"code":15053,"msg":"ASR recognition failed"}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	text := strings.Repeat("你好。", 500)
	parts := spokenParts(text)
	for _, part := range parts {
		if len(part) > 1000 {
			t.Fatal("UTF8 split oversized")
		}
	}
	for _, format := range []string{"wav", "pcm"} {
		payload, err := audioBody("speech", speechModel, map[string]any{"input": "你好", "response_format": format})
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.CallAudio(t.Context(), provider.MediaRequest{Operation: "speech", Model: speechModel, AccountID: "fixture", Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if format == "wav" {
			if string(result.Data[:4]) != "RIFF" || binary.LittleEndian.Uint32(result.Data[24:]) != 16000 || len(result.Data) != 48 {
				t.Fatal("WAV invalid")
			}
		} else if result.ContentType != "audio/pcm" || len(result.Data) != 6 {
			t.Fatal("PCM not 24kHz")
		}
	}
	for _, tc := range []struct {
		format string
		limit  int64
		size   int
		ok     bool
	}{{"wav", 100, 92, true}, {"pcm", 80, 72, true}, {"wav", 80, 0, false}} {
		r.cfg = &config.Config{MaxResponseBytes: tc.limit}
		payload, _ := audioBody("speech", speechModel, map[string]any{"input": strings.Repeat("x", 12000), "response_format": tc.format})
		result, err := r.CallAudio(t.Context(), provider.MediaRequest{Operation: "speech", Model: speechModel, AccountID: "fixture", Payload: payload})
		if (err == nil) != tc.ok || err == nil && len(result.Data) != tc.size {
			t.Fatal("audio output limit", tc, err, len(result.Data))
		}
	}
	r.cfg = nil
	for _, tc := range []struct {
		format string
		size   int64
	}{{"wav", 48}, {"pcm", 6}} {
		p, _ := audioBody("speech", speechModel, map[string]any{"input": "x", "response_format": tc.format})
		for _, limit := range []int64{tc.size, tc.size - 1} {
			r.cfg = &config.Config{MaxResponseBytes: limit}
			got, err := r.CallAudio(t.Context(), provider.MediaRequest{Operation: "speech", Model: speechModel, AccountID: "fixture", Payload: p})
			if (err == nil) != (limit == tc.size) || err == nil && int64(len(got.Data)) != tc.size {
				t.Fatal(tc, limit, err)
			}
		}
	}
	r.cfg = nil
	if _, err := audioBody("speech", speechModel, map[string]any{"input": "text", "response_format": "mp3"}); err == nil {
		t.Fatal("MP3 mislabeled")
	}
	payload, err := audioBody("transcription", transcribeModel, map[string]any{"audio_data": map[string]any{"raw": []byte("wave"), "mime": "audio/wav", "name": "voice.wav"}})
	if err != nil {
		t.Fatal(err)
	}
	request := provider.MediaRequest{Operation: "transcription", Model: transcribeModel, AccountID: "fixture", Payload: payload}
	result, err := r.CallAudio(t.Context(), request)
	if err != nil || string(result.Data) != `{"text":"hello"}` {
		t.Fatal("ASR result invalid", err, string(result.Data))
	}
	asrFailure = true
	if _, err = r.CallAudio(t.Context(), request); err == nil {
		t.Fatal("ASR business error ignored")
	}
	refuse = true
	before := calls
	if _, err = r.CallAudio(t.Context(), request); err == nil || calls != before+1 {
		t.Fatal("bare401 renewed or replayed")
	}
	before = calls
	ctx := provider.WithInvocation(t.Context(), provider.Invocation{Check: func(context.Context, provider.AccountRef, string) error { return errors.New("revoked") }})
	if _, err = r.CallAudio(ctx, request); err == nil || calls != before {
		t.Fatal("revocation bypassed")
	}
	ctx = provider.WithInvocation(t.Context(), provider.Invocation{Check: func(context.Context, provider.AccountRef, string) error {
		next := credentials.NewManager(path)
		r.Pool.AddAccount("fixture", next)
		r.SetManager("fixture", next)
		return nil
	}})
	if _, err = r.CallAudio(ctx, request); err == nil || calls != before {
		t.Fatal("dispatched with replaced credentials")
	}
}

func TestChatSignatureStorageIsBoundedAndSkipsOversizedCapture(t *testing.T) {
	var cache chatSignatureStore
	for i := 0; i < 2050; i++ {
		tap := &chatSignatureTap{extra: map[string]any{"signature": "value"}, ids: []string{fmt.Sprint(i)}}
		cache.keep("scope", tap)
	}
	if cache.order.Len() != 2048 || cache.used <= 0 || cache.used > chatSignatureBytes {
		t.Fatal("signature budget exceeded")
	}
	if _, marks := cache.restore("scope", map[string]any{"messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "0"}}}}}); len(marks) != 0 {
		t.Fatal("oldest signature retained")
	}
	tap := &chatSignatureTap{extra: map[string]any{"signature": "value"}, failed: true}
	before := cache.order.Len()
	cache.keep("scope", tap)
	if cache.order.Len() != before || !tap.failed {
		t.Fatal("oversized response cached")
	}
}

func TestSpeechAndSignatureUpstreamEdges(t *testing.T) {
	for _, tc := range []struct{ line, reasoning string }{
		{`{"choices":[{"delta":{"reasoning":"a"},"message":{"reasoning":"b","extra_fields":{"signature":"s"}}}]}`, "ab"},
		{`{"choices":[{"delta":{"reasoning":"a","extra_fields":{"signature":"s"}},"message":{"reasoning":"a","extra_fields":{"signature":"s"}}}]}`, "a"},
	} {
		var tap chatSignatureTap
		tap.observe(t.Context(), "data: "+tc.line)
		if tap.reasoning.String() != tc.reasoning || tap.extra["signature"] != "s" {
			t.Fatal("signed reply fields changed", tap.reasoning.String(), tap.extra)
		}
	}
	if _, err := audioBody("speech", speechModel, map[string]any{"input": "\ufeff\u2003"}); err == nil {
		t.Fatal("blank spoken text accepted")
	}
	if parts := spokenParts("\ufeff\u2003hello\ufeff"); len(parts) != 1 || parts[0] != "hello" {
		t.Fatal("unexpected spoken text", parts)
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer secret")
	headers.Set("X-API-Key", "private-value")
	for _, raw := range []string{`{"code":15053,"msg":"speech failed"}`, `{"error":{"message":"token secret and private-value must not leak"}}`, `{"debug":{"account":"hidden"}}`, "plain refusal"} {
		err := speechFailure(502, []byte(raw), "upstream", headers)
		body, _ := json.Marshal(err.(*provider.APIError).Body)
		if strings.Contains(string(body), "secret") || strings.Contains(string(body), "private-value") || strings.Contains(string(body), "hidden") {
			t.Fatal("credential leaked")
		}
	}
	var got bytes.Buffer
	err := readSpoken(t.Context(), io.NopCloser(strings.NewReader("event: error\ndata: recognition unavailable\n\n")), &got, "wav", 1024, nil)
	var errorBody []byte
	if ae, ok := err.(*provider.APIError); ok {
		errorBody, _ = json.Marshal(ae.Body)
	}
	if err == nil || got.Len() != 0 || !strings.Contains(string(errorBody), "recognition unavailable") {
		t.Fatal("error event message lost", err)
	}
	for _, raw := range []string{"thinking_signature_invalid", `{"error":{"message":"Invalid signature in thinking"}}`, `{"error":{"details":"{\"internal_code\":11159}"}}`, "block is bound to a different conversation"} {
		if !rejectsChatSignature([]byte(raw)) {
			t.Fatal("signature recovery missing", raw)
		}
	}
	for _, raw := range []string{"rate limit", "unknown error", `{"error":{"message":"invalid request"},"request":{"messages":[{"content":"thinking_signature_invalid"}]}}`} {
		if rejectsChatSignature([]byte(raw)) {
			t.Fatal("unrelated recovery")
		}
	}
}
