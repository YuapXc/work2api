package opencode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"work2api/internal/core/protocol"
	"work2api/internal/streamwatch"
)

type attemptError struct {
	err   error
	phase string
}

var errProxyUnavailable = errors.New("proxy route unavailable")

type proxyRouteError struct{ err error }

func (e *proxyRouteError) Error() string   { return "proxy route unavailable" }
func (e *proxyRouteError) Unwrap() []error { return []error{errProxyUnavailable, e.err} }
func classifyProxyRoute(proxy *proxyTransport, err error) error {
	if err == nil || proxy == nil || proxy.name == "direct" {
		return err
	}
	var attempt *attemptError
	var op *net.OpError
	if errors.As(err, &attempt) && attempt.phase == "dial" || errors.As(err, &op) && (op.Op == "proxyconnect" || strings.HasPrefix(op.Op, "socks")) {
		return &proxyRouteError{err}
	}
	return err
}

func (e *attemptError) Error() string { return fmt.Sprintf("%v [phase=%s]", e.err, e.phase) }
func (e *attemptError) Unwrap() error { return e.err }

// Only a missing official 2.1 route can fall back to its 2.0 equivalent.
// Reuse the identity, body and remaining first-event budget; never replay a
// generated response or interpret an authentication/payload error as a route miss.
func doCompatibleInferenceAttempt(client *http.Client, req *http.Request, from protocol.Protocol, timeout time.Duration) (*http.Response, error) {
	start := time.Now()
	resp, err := doInferenceAttempt(client, req, from, timeout)
	if err != nil || resp == nil || resp.StatusCode != http.StatusNotFound || req.GetBody == nil || req.Context().Err() != nil || req.URL.Host != "opencode.ai" || req.URL.Scheme != "https" {
		return resp, err
	}
	path := req.URL.Path
	legacy := strings.Replace(path, "/inference/openai/v1/", "/inference/v1/", 1)
	legacy = strings.Replace(legacy, "/inference/anthropic/v1/", "/inference/v1/", 1)
	if legacy == path {
		return resp, err
	}
	remaining := timeout
	if timeout > 0 {
		remaining -= time.Since(start)
		if remaining <= 0 {
			return resp, err
		}
	}
	body, bodyErr := req.GetBody()
	if bodyErr != nil {
		return resp, err
	}
	old := req.Clone(req.Context())
	old.URL.Path = legacy
	old.Body = body
	// Retain the real 404 if the compatibility connection cannot be made.
	saved := bufferedErrorResponse(resp)
	if timeout > 0 {
		remaining = timeout - time.Since(start)
		if remaining <= 0 {
			_ = body.Close()
			return saved, nil
		}
	}
	streamwatch.StartAttempt(req.Context())
	next, nextErr := doInferenceAttempt(client, old, from, remaining)
	if nextErr != nil {
		if req.Context().Err() != nil {
			_ = saved.Body.Close()
			return nil, req.Context().Err()
		}
		return saved, nil
	}
	_ = saved.Body.Close()
	return next, nil
}

// Apply the configured optional timeout to the entire uncommitted attempt,
// then disarm it atomically. A timer must never kill an established stream.
func doInferenceAttempt(client *http.Client, req *http.Request, from protocol.Protocol, timeout ...time.Duration) (*http.Response, error) {
	if len(timeout) == 0 || timeout[0] <= 0 {
		return readInferenceAttempt(client, req, from)
	}
	ctx, cancel := context.WithCancel(req.Context())
	var connected, written, answered atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { connected.Store(true) },
		WroteRequest:         func(info httptrace.WroteRequestInfo) { written.Store(info.Err == nil) },
		GotFirstResponseByte: func() { answered.Store(true) },
	}
	var mu sync.Mutex
	committed, timedOut := false, false
	timer := time.AfterFunc(timeout[0], func() {
		mu.Lock()
		defer mu.Unlock()
		if !committed {
			timedOut = true
			cancel()
		}
	})
	resp, err := readInferenceAttempt(client, req.Clone(httptrace.WithClientTrace(ctx, trace)), from)
	mu.Lock()
	committed = true
	timer.Stop()
	expired := timedOut
	mu.Unlock()
	if expired {
		err = context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		if resp != nil {
			resp.Body.Close()
		}
		phase := "first_event"
		if !connected.Load() {
			phase = "dial"
		} else if !written.Load() {
			phase = "write"
		} else if !answered.Load() {
			phase = "headers"
		}
		return nil, &attemptError{err, phase}
	}
	resp.Body = &attemptBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type attemptBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *attemptBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

func readInferenceAttempt(client *http.Client, req *http.Request, from protocol.Protocol) (*http.Response, error) {
	resp, err := client.Do(req)
	streamwatch.AttemptHTTP(req.Context(), resp)
	if err != nil || resp.StatusCode/100 != 2 || from == protocol.SystemOne || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return resp, err
	}
	watch := streamwatch.NewWatch(resp.Body, req.Context(), 0, 0)
	reader := bufio.NewReader(watch.Reader(resp.Body))
	prefix, err := protocol.ReadStreamHead(reader, from)
	watch.Close()
	if err != nil {
		resp.Body.Close()
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		var rejection *protocol.StreamHeadError
		if errors.As(err, &rejection) {
			resp.StatusCode = rejection.Status
			resp.Status = fmt.Sprintf("%d %s", rejection.Status, http.StatusText(rejection.Status))
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Del("Content-Length")
			resp.ContentLength = int64(len(rejection.Body))
			resp.Body = io.NopCloser(bytes.NewReader(rejection.Body))
			return resp, nil
		}
		return nil, err
	}
	resp.Body = &streamPrefixBody{Reader: io.MultiReader(bytes.NewReader(prefix), reader), body: resp.Body}
	return resp, nil
}

type streamPrefixBody struct {
	io.Reader
	body io.ReadCloser
}

func (b *streamPrefixBody) Close() error { return b.body.Close() }
