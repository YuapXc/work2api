package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"work2api/internal/qoder/cosy"
)

func queueErrorFixture() (string, string, string) {
	ticket := `{"isQueued":true,"modelKey":"qfmodel","queueType":"p3","retryAfterSeconds":30,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT"}`
	business := fmt.Sprintf(`{"code":"10605","subCode":"RESOURCE_LIMIT","message":%q}`, ticket)
	return ticket, business, fmt.Sprintf(`{"code":403,"message":%q}`, business)
}

func TestResourceErrorsAcrossCompleteFrames(t *testing.T) {
	ticket, business, outer := queueErrorFixture()
	for _, tc := range []struct {
		name, body, kind string
		status, want     int
	}{
		{"nested_envelope", outer, "upstream_queued", 403, 429},
		{"call_count_queue", `{"code":10605,"subCode":"CALL_COUNT_LIMIT","message":"{\"isQueued\":true,\"retryAfterSeconds\":17}"}`, "upstream_queued", 200, 429},
		{"call_count_only", `{"code":10605,"subCode":"CALL_COUNT_LIMIT"}`, ErrTypeCallCount, 200, 429},
		{"explicit_not_queued", `{"code":10605,"subCode":"CALL_COUNT_LIMIT","isQueued":false,"retryAfterSeconds":17}`, ErrTypeCallCount, 200, 429},
		{"unrelated_queue", `{"code":10605,"error":{"subCode":"RESOURCE_LIMIT"},"details":{"isQueued":true}}`, "resource_limit", 200, 403},

		{"business_string", business, "upstream_queued", 200, 429},
		{"business_number", `{"code":10605,"subCode":"RESOURCE_LIMIT","isQueued":true}`, "upstream_queued", 200, 429},
		{"details_string", fmt.Sprintf(`{"code":"10605","details":%q}`, ticket), "upstream_queued", 200, 429},
		{"details_object", `{"code":10605,"details":{"subCode":"RESOURCE_LIMIT","isQueued":true}}`, "upstream_queued", 200, 429},
		{"parent_resource_child_queue", `{"code":10605,"subCode":"RESOURCE_LIMIT","details":{"isQueued":true}}`, "upstream_queued", 200, 429},
		{"entitlement", `{"code":10605,"subCode":"RESOURCE_LIMIT","serviceAvailable":false}`, "resource_limit", 200, 403},
		{"code_alone", `{"code":10605}`, ErrTypeUpstream, 200, 500},
		{"numeric_unknown", `{"code":115,"message":"failure"}`, ErrTypeUpstream, 200, 500},
		{"workspace", `{"error":{"message":"Workspace allocated quota exceeded"}}`, ErrTypeWorkspaceQuota, 429, 429},
		{"workspace_business", `{"code":429,"details":{"error":{"message":"Workspace allocated quota exceeded"}}}`, ErrTypeWorkspaceQuota, 200, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"body": tc.body, "statusCodeValue": tc.status})
			d := ExtractDelta(string(raw))
			var e *UpstreamError
			if !errors.As(d.Err, &e) || e.ErrType != tc.kind || ErrorStatus(e) != tc.want {
				t.Fatalf("got %#v, want %s/%d", d.Err, tc.kind, tc.want)
			}
			if IsTransientUpstream(e.Status, e.Detail) {
				t.Fatal("business rejection became a transient retry")
			}
			if e.Detail != tc.body {
				t.Fatal("original structured error was lost")
			}
		})
	}
}

func TestResourceParserBoundsAndNegativeEvidence(t *testing.T) {
	for _, raw := range []string{
		`{"code":10605}`, `{"message":"RESOURCE_LIMIT isQueued"}`,
		`{"message":"The quota exceeded"}`, `{"isQueued":true}`,
		`{"message":"Workspace allocated quota exceeded yesterday"}`,
		`{"message":"` + strings.Repeat("x", 65536) + `","subCode":"RESOURCE_LIMIT"}`,
	} {
		if kind := resourceFailure(raw, 0); kind != "" {
			t.Fatalf("misclassified as %s", kind)
		}
	}
	raw := `{"subCode":"RESOURCE_LIMIT","isQueued":true}`
	for i := 0; i < 5; i++ {
		raw = fmt.Sprintf(`{"details":%s}`, raw)
	}
	if got := resourceFailure(raw, 0); got != "" {
		t.Fatal("depth bound ignored", got)
	}
	for _, status := range []int{401, 402} {
		e := NewUpstreamError(status, `{"error":{"message":"Workspace allocated quota exceeded"}}`)
		if ErrorStatus(e) != status || e.ErrType == ErrTypeWorkspaceQuota {
			t.Fatal("auth/payment status lost", e)
		}
	}
	for _, code := range []any{nil, false, map[string]any{}, []any{}} {
		if businessCode(code) != "" {
			t.Fatal("invalid code accepted")
		}
	}
	for _, code := range []string{`0`, `"0"`, `null`} {
		raw := fmt.Sprintf(`{"body":%q}`, `{"code":`+code+`,"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)
		d := ExtractDelta(raw)
		if d.Err != nil || d.Content != "OK" {
			t.Fatal("success frame regressed", d)
		}
	}
}

func TestResourceRejectionsDoNotReplayOrEmitSuccess(t *testing.T) {
	_, business, outer := queueErrorFixture()
	for _, tc := range []struct {
		name, body              string
		httpStatus, frameStatus int
		partial                 bool
	}{
		{"http", outer, 403, 0, false}, {"sse_envelope", outer, 200, 403, false}, {"sse_business", business, 200, 200, false},
		{"after_output", business, 200, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if tc.httpStatus != 200 {
					w.WriteHeader(tc.httpStatus)
					fmt.Fprint(w, tc.body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.partial {
					frame, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": `{"choices":[{"delta":{"content":"first"}}]}`})
					fmt.Fprintf(w, "data: %s\n\n", frame)
				}
				b, _ := json.Marshal(map[string]any{"statusCodeValue": tc.frameStatus, "body": tc.body})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}))
			defer srv.Close()
			sess, e := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture", UserType: "personal_standard"}, "fixture", "fixture", "fixture")
			if e != nil {
				t.Fatal(e)
			}
			b := &Bridge{sess: sess, client: NewBearerClient(sess), templateBase: map[string]any{}, chatStreamURL: srv.URL}
			emitted := 0
			wantEmitted := 0
			if tc.partial {
				wantEmitted = 1
			}
			err := b.CallQoderWithOpts(context.Background(), "codex", nil, "qfmodel", nil, CallOpts{}, func(Delta) { emitted++ })
			var upstream *UpstreamError
			if !errors.As(err, &upstream) || ErrorStatus(err) != 429 || hits.Load() != 1 || emitted != wantEmitted {
				t.Fatal(err, hits.Load(), emitted)
			}
		})
	}
}
