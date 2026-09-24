package opencode

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
)

// userAgent identifies as the OpenCode CLI so Zen accepts the request.
func userAgent() string {
	return fmt.Sprintf("opencode/1.18.31 (%s %s; %s)", runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// drainAndClose discards and closes a response body so its connection can be
// reused, then released.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
