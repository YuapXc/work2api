package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"strings"
)

// CLI request gzip follows hub wb_identity.gzip_body. Small bodies and other
// client identities retain their existing wire representation.
func encodeRequestBody(ctx context.Context, header http.Header, raw []byte) ([]byte, error) {
	if len(raw) < 64*1024 || !strings.EqualFold(header.Get("X-IDE-Type"), "CLI") || header.Get("Content-Encoding") != "" {
		return raw, ctx.Err()
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	for offset := 0; offset < len(raw); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(offset+64*1024, len(raw))
		if _, err := writer.Write(raw[offset:end]); err != nil {
			return nil, err
		}
		offset = end
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() >= len(raw)-len(raw)/10 {
		return raw, nil
	}
	header.Set("Content-Encoding", "gzip")
	return compressed.Bytes(), nil
}
