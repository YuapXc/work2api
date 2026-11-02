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
