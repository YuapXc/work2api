package streamwatch

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func SSEWriter(w http.ResponseWriter) (func(string) error, bool) {
	_, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.WriteHeader(http.StatusOK)
	return func(s string) error {
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := w.Write([]byte(s)); err != nil {
			return errors.Join(context.Canceled, err)
		}
		if err := controller.Flush(); err != nil {
			return errors.Join(context.Canceled, err)
		}
		return nil
	}, true
}
