package streamwatch

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeBody struct {
	closed chan struct{}
}

func newFakeBody() *fakeBody { return &fakeBody{closed: make(chan struct{})} }

func (f *fakeBody) Read(p []byte) (int, error) {
	// 模拟挂死：永远阻塞，直到 body 被关（watchdog abort 的解除阻塞机制）
	<-f.closed
	return 0, io.EOF
}

func (f *fakeBody) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

// 挂死流：空闲超时触发，Close 解除 Read 阻塞
func TestIdleAbortUnblocksRead(t *testing.T) {
	body := newFakeBody()
	w := NewWatch(body, context.Background(), 50*time.Millisecond, time.Hour)
	defer w.Close()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 10)
		_, err := w.Reader(body).Read(buf)
		done <- err
	}()

	select {
	case err := <-done:
		// Read 被解除阻塞后返回 EOF（body 已关）；watchdog 应记录 breach
		if w.Err == nil {
			t.Fatal("watchdog 应记录 idle breach")
		}
		var be *BreachError
		if !errors.As(w.Err, &be) || be.Kind != "idle" {
			t.Fatalf("应为 idle breach，got %v", w.Err)
		}
		if err != io.EOF {
			t.Fatalf("Read 应以 EOF 结束，got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("看门狗未在超时后解除 Read 阻塞")
	}
}

// 正常流：持续 Touch 不触发
func TestTouchKeepsAlive(t *testing.T) {
	body := newFakeBody()
	w := NewWatch(body, context.Background(), 60*time.Millisecond, time.Hour)
	defer w.Close()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.Touch()
		time.Sleep(20 * time.Millisecond)
	}
	if w.Err != nil {
		t.Fatalf("持续活动不应触发看门狗: %v", w.Err)
	}
}

// 总时长上限：即使一直有活动也终止
func TestTotalCap(t *testing.T) {
	body := newFakeBody()
	w := NewWatch(body, context.Background(), time.Hour, 60*time.Millisecond)
	defer w.Close()

	go func() {
		for {
			select {
			case <-w.done:
				return
			default:
				w.Touch()
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)
	var be *BreachError
	if w.Err == nil || !errors.As(w.Err, &be) || be.Kind != "total" {
		t.Fatalf("总时长上限应触发 total breach，got %v", w.Err)
	}
}

// ctx 取消：看门狗退出，不触发 breach
func TestCtxCancel(t *testing.T) {
	body := newFakeBody()
	ctx, cancel := context.WithCancel(context.Background())
	w := NewWatch(body, ctx, 50*time.Millisecond, time.Hour)
	defer w.Close()
	cancel()
	time.Sleep(120 * time.Millisecond)
	if w.Err != nil {
		t.Fatalf("ctx 取消不应触发 breach: %v", w.Err)
	}
}

// Reader 包装正常流数据完整
func TestReaderPassthrough(t *testing.T) {
	body := io.NopCloser(strings.NewReader("hello stream"))
	w := NewWatch(body, context.Background(), time.Second, time.Hour)
	defer w.Close()
	data, err := io.ReadAll(w.Reader(body))
	if err != nil || string(data) != "hello stream" {
		t.Fatalf("passthrough 失败: %q %v", data, err)
	}
}
