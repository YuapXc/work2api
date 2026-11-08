package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

type downloadStatus struct {
	code  int
	retry time.Duration
}

func (e *downloadStatus) Error() string { return fmt.Sprintf("GitHub更新源返回HTTP %d", e.code) }

func transientDownload(err error) bool {
	var status *downloadStatus
	if errors.As(err, &status) {
		switch status.code {
		case 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	var network net.Error
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.As(err, &network)
}

func downloadReleaseAsset(ctx context.Context, asset Asset, repository, tag, target string, limit int64) error {
	if !assetURL(asset.URL, repository, tag) {
		return errors.New("Release 下载地址不属于当前发布仓库")
	}
	address := asset.URL
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			delay := time.Duration(attempt) * 250 * time.Millisecond
			var status *downloadStatus
			if errors.As(last, &status) && status.retry > delay {
				delay = status.retry
			}
			if delay > 30*time.Second {
				return last
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			if asset.ID > 0 {
				address = fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", repository, asset.ID)
			}
		}
		last = downloadReleaseOnce(ctx, address, target, limit)
		if last == nil || !transientDownload(last) {
			return last
		}
	}
	return last
}

func downloadReleaseOnce(ctx context.Context, address, target string, limit int64) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "work2api-updater")
	resp, err := githubClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := time.Duration(0)
		if seconds, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); e == nil && seconds > 0 {
			retry = time.Duration(seconds) * time.Second
		} else if until, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
			retry = time.Until(until)
		}
		return &downloadStatus{code: resp.StatusCode, retry: retry}
	}
	if resp.ContentLength > limit {
		return errors.New("更新响应超出大小保护")
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("更新响应超出大小保护")
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return io.ErrUnexpectedEOF
	}
	return f.Sync()
}

func downloadChecksum(ctx context.Context, asset Asset, repository, tag, root string) ([]byte, error) {
	f, err := os.CreateTemp(root, "checksum-")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	_ = os.Remove(path)
	defer os.Remove(path)
	if err := downloadReleaseAsset(ctx, asset, repository, tag, path, 4096); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
