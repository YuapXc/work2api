// Package httpx holds small HTTP response helpers shared across the server.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSONStatus writes value as JSON with the given status code.
func WriteJSONStatus(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
