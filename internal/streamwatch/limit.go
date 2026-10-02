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
