package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type downloadTransport func(*http.Request) (*http.Response, error)

func (f downloadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReleaseDownloadFallsBackToSameAssetAndCleansFailures(t *testing.T) {
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = downloadTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Accept") != "application/octet-stream" {
			t.Error("asset metadata requested instead of bytes")
		}
		status, body := 503, "busy"
		if calls == 2 {
			if r.URL.String() != "https://api.github.com/repos/example/project/releases/assets/42" {
				t.Error("wrong asset fallback", r.URL)
			}
			status, body = 200, "verified archive"
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), ContentLength: int64(len(body))}, nil
	})
	path := filepath.Join(t.TempDir(), "download")
	asset := Asset{ID: 42, URL: "https://github.com/example/project/releases/download/v1.0.0/test.zip"}
	if err := downloadReleaseAsset(context.Background(), asset, "example/project", "v1.0.0", path, 1024); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("unexpected retries", calls)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "verified archive" {
		t.Fatal(string(data), err)
	}
	if err := downloadReleaseAsset(context.Background(), asset, "another/project", "v1.0.0", filepath.Join(t.TempDir(), "wrong"), 1024); err == nil || calls != 2 {
		t.Fatal("foreign repository accepted")
	}
	for _, mode := range []string{"oversize", "truncated", "forbidden", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			calls = 0
			http.DefaultTransport = downloadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				code := 200
				length := int64(20)
				body := "short"
				if mode == "forbidden" {
					code = 403
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), ContentLength: length, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			ctx := context.Background()
			if mode == "cancel" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			path := filepath.Join(t.TempDir(), "failed")
			limit := int64(100)
			if mode == "oversize" {
				limit = 4
			}
			if err := downloadReleaseAsset(ctx, asset, "example/project", "v1.0.0", path, limit); err == nil {
				t.Fatal("expected failure")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("retained failed archive", err)
			}
			if mode == "forbidden" && calls != 1 || mode == "cancel" && calls != 0 {
				t.Fatal("retried permanent failure", calls)
			}
		})
	}
}

func TestFileArchiveExtractionIsBoundedAndRejectsUnsafeEntries(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		for _, mode := range []string{"valid", "duplicate", "traversal", "missing", "oversize", "canceled", "cancel-during"} {
			t.Run(format+mode, func(t *testing.T) {
				var wire bytes.Buffer
				name := ExecutableName()
				if mode == "traversal" {
					name = "../" + name
				}
				if mode == "missing" {
					name = "README.md"
				}
				count := 1
				if mode == "duplicate" {
					count = 2
				}
				if format == "zip" {
					w := zip.NewWriter(&wire)
					for i := 0; i < count; i++ {
						f, err := w.Create(name)
						if err != nil {
							t.Fatal(err)
						}
						f.Write([]byte("binary"))
					}
					if err := w.Close(); err != nil {
						t.Fatal(err)
					}
					if mode == "oversize" {
						t.Skip("oversized declared payload is covered by the tar fixture")
					}
				} else {
					gz := gzip.NewWriter(&wire)
					w := tar.NewWriter(gz)
					for i := 0; i < count; i++ {
						size := int64(6)
						if mode == "oversize" {
							size = 128<<20 + 1
						}
						if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0700, Size: size, Typeflag: tar.TypeReg}); err != nil {
							t.Fatal(err)
						}
						w.Write([]byte("binary"))
					}
					_ = w.Close()
					_ = gz.Close()
				}
				dir := t.TempDir()
				archive := filepath.Join(dir, "archive")
				if err := os.WriteFile(archive, wire.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, "binary")
				var err error
				if mode == "canceled" || mode == "cancel-during" {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if mode == "canceled" {
						cancel()
						err = extractBinaryFileContext(ctx, archive, format, target)
					} else {
						source := &cancelArchive{Reader: bytes.NewReader(wire.Bytes()), cancel: cancel}
						err = extractBinary(ctx, source, int64(wire.Len()), format, target)
					}
					if !errors.Is(err, context.Canceled) {
						t.Fatal("cancellation lost", err)
					}
				} else {
					err = ExtractBinaryFile(archive, format, target)
				}
				if mode == "valid" {
					if err != nil {
						t.Fatal(err)
					}
					data, _ := os.ReadFile(target)
					if string(data) != "binary" {
						t.Fatal("bad extracted bytes")
					}
				} else {
					if err == nil {
						t.Fatal("accepted unsafe archive")
					}
					if _, e := os.Stat(target); !errors.Is(e, os.ErrNotExist) {
						t.Fatal("retained rejected executable", e)
					}
				}
			})
		}
	}
}

type cancelArchive struct {
	*bytes.Reader
	cancel context.CancelFunc
}

func (r *cancelArchive) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}
func (r *cancelArchive) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.Reader.ReadAt(p, off)
	r.cancel()
	return n, err
}

func TestHashFileHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashFileContext(ctx, filepath.Join(t.TempDir(), "missing")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled hash opened file", err)
	}
}
