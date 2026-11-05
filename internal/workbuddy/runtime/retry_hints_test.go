package runtime

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
	"work2api/internal/workbuddy/upstream"
)

func TestRetryHintsAreUpstreamEvidenceOnly(t *testing.T) {
	for _, tc := range []struct {
		status int
		header http.Header
		want   bool
	}{
		{429, http.Header{"Retry-After": []string{"17"}}, true},
		{429, http.Header{"Retry-After": []string{time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)}}, true},
		{400, http.Header{"Retry-After": []string{"17"}}, false},
		{429, nil, false}, {429, http.Header{"Retry-After": []string{"NaN"}}, false}, {429, http.Header{"Retry-After": []string{"-1"}}, false},
	} {
		w := httptest.NewRecorder()
		setRetryHints(w, &upstream.UpstreamError{StatusCode: tc.status, Header: tc.header})
		reset := w.Header().Get("X-RateLimit-Reset")
		if (reset != "") != tc.want {
			t.Fatal(tc, w.Header())
		}
		if tc.want {
			n, e := strconv.ParseInt(reset, 10, 64)
			if e != nil || n <= time.Now().Unix() || w.Header().Get("Retry-After") == "" {
				t.Fatal(w.Header())
			}
		}
	}
}

func TestChannelDeniedAcceptsOnlyStructuredHyphenatedCode(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"code":"11-128","msg":"request illegal"}`, true},
		{403, `{"error":{"errCode":"11-128","message":"request illegal"}}`, true},
		{400, `{"code":11128}`, true},
		{403, `{"code":"11-124","msg":"request illegal"}`, false},
		{403, `{"msg":"11-128"}`, false},
		{400, `{"code":"11-128","msg":"first message is not system prompt"}`, false},
		{429, `{"code":"11-128"}`, false},
	} {
		if got := IsChannelDenied(tc.status, tc.body); got != tc.want {
			t.Fatalf("%d %s: got %v", tc.status, tc.body, got)
		}
		if tc.want && ClassifyUpstream(tc.status, []byte(tc.body), nil) != ErrChannelDenied {
			t.Fatal("request rejection was misclassified", tc.body)
		}
	}
}
