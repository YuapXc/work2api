package provider

import "strconv"

// APIError preserves safe pre-dispatch status and body across channel boundaries.
type APIError struct {
	Status int
	Body   map[string]any
}

func (e *APIError) Error() string { return "api error " + strconv.Itoa(e.Status) }
func NewAPIError(status int, message, typ string) *APIError {
	return &APIError{Status: status, Body: map[string]any{"error": map[string]any{"message": message, "type": typ}}}
}
