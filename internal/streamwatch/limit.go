package streamwatch

import (
	"context"
	"errors"
	"io"
)

type responseLimitKey struct{}

var ErrResponseTooLarge = errors.New("upstream response exceeds configured byte limit")

func WithResponseLimit(ctx context.Context, limit int64) context.Context {
	return context.WithValue(ctx, responseLimitKey{}, limit)
}

func ResponseLimit(ctx context.Context) int64 {
	if n, ok := ctx.Value(responseLimitKey{}).(int64); ok && n > 0 {
		return n
	}
	return 64 << 20
}

func LimitReader(ctx context.Context, r io.Reader) io.Reader {
	return &responseReader{r: r, left: ResponseLimit(ctx)}
}

// ReadBody applies the same idle/duration and byte bounds to JSON and error
// bodies as to SSE. A smaller diagnostic limit may be supplied by callers.
func ReadBody(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	if limit > 0 && limit < ResponseLimit(ctx) {
		ctx = WithResponseLimit(ctx, limit)
	}
	watch := NewWatch(body, ctx, 0, 0)
	defer watch.Close()
	data, err := io.ReadAll(watch.Reader(body))
	if breach := watch.Err(); breach != nil {
		return nil, breach
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}

type responseReader struct {
	r    io.Reader
	left int64
}

func (r *responseReader) Read(p []byte) (int, error) {
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.r.Read(p)
	if int64(n) > r.left {
		return 0, ErrResponseTooLarge
	}
	r.left -= int64(n)
	return n, err
}
