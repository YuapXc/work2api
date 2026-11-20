package trae

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/statebackup"
	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

func (r *Runtime) sendPreparedChat(ctx context.Context, identity *auth.Auth, body []byte, console bool) (io.ReadCloser, int, []byte, http.Header, error, bool) {
	var connected, handshakeFailed atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			handshakeFailed.Store(err != nil && (transientConnectError(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)))
		},
	}
	stream, status, raw, headers, err := r.client(httptrace.WithClientTrace(ctx, trace)).ChatPreparedStreamWithHeaders(identity, body, console)
	var dial *net.OpError
	var dns *net.DNSError
	beforeSend := err != nil && ctx.Err() == nil && !connected.Load() && (handshakeFailed.Load() ||
		!errors.As(err, &dns) && errors.As(err, &dial) && dial.Op == "dial" && transientConnectError(dial))
	return stream, status, raw, headers, err, beforeSend
}

func transientConnectError(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout() || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

func retryable(kind upstream.ErrKind) bool {
	return kind == upstream.ErrSoftRate || kind == upstream.ErrServer || kind == upstream.ErrSessionDead || kind == upstream.ErrPlanLimit
}

func retryAfter(value string, now time.Time) time.Duration {
	var delay time.Duration
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > 86400 {
			seconds = 86400
		}
		delay = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		delay = at.Sub(now)
	}
	if delay < 0 {
		return 0
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return delay
}

// Return a useful classification and numeric code, without echoing an upstream
// body that may contain credentials, signed links, or fragments of user input.
func rejectionError(kind upstream.ErrKind, status int, code int64) error {
	message, errorType := "Trae 上游拒绝请求", "upstream_error"
	switch kind {
	case upstream.ErrInputTooLarge:
		status, message, errorType = 400, "Trae 上游拒绝过大的输入，请压缩上下文或缩小请求", "context_length_exceeded"
	case upstream.ErrModelUnavailable:
		status, message, errorType = 400, "该模型不在当前 Trae 产品聊天通道可用，请刷新模型目录并选择可用模型", "model_unavailable"
	case upstream.ErrPlanLimit:
		status, message, errorType = 429, "Trae 账号的积分或模型配额不足，请等待额度恢复或使用其他有权账号", "quota_exceeded"
	case upstream.ErrSoftRate:
		status, message, errorType = 429, "Trae 上游限流，请稍后重试", "rate_limit_exceeded"
	case upstream.ErrSessionDead:
		status, message, errorType = 503, "Trae 账号授权已失效，自动刷新未恢复，请重新授权", "account_unavailable"
	case upstream.ErrServer:
		status, message = 502, "Trae 上游服务暂不可用，请稍后重试"
	}
	if status < 400 || status > 599 {
		status = 502
	}
	if code != 0 {
		message += fmt.Sprintf("（上游 code=%d）", code)
	}
	return provider.NewAPIError(status, message, errorType)
}

func (r *Runtime) forceRefresh(ctx context.Context, id string, identity *auth.Auth) error {
	if accountID(identity.UID) != id {
		return errors.New("Trae 账号授权已变更")
	}
	before, captured := identity.TryMaintenanceIdentity()
	err := r.refreshCredentials(ctx, identity, true)
	if err != nil && captured {
		r.observeMaintenance(identity, "auth", before, err)
	}
	return err
}

func (r *Runtime) fallbackEnterprise(ctx context.Context, id string, identity *auth.Auth) bool {
	if !r.switchToConsumer(identity) {
		return false
	}
	if ctx.Err() == nil {
		_ = r.RefreshAccount(ctx, id)
		_ = r.refreshQuota(ctx, id, false)
	}
	return true
}

func (r *Runtime) invalidateConsoleRoute(identity *auth.Auth) {
	id := accountID(identity.UID)
	r.mu.Lock()
	a := r.accounts[id]
	if a == nil || a.Identities[identity.Variant] != identity {
		r.mu.Unlock()
		return
	}
	var siblings []*auth.Auth
	for product, candidate := range a.Identities {
		if candidate.EnterpriseConsole {
			siblings = append(siblings, candidate)
			delete(a.Catalogs, product)
			delete(a.CatalogTimes, product)
		}
	}
	a.mergeModels()
	a.Remaining, a.Total, a.Expiry = nil, nil, nil
	a.QuotaUnit, a.QuotaReset, a.QuotaUnlimited = "credits", nil, false
	a.CreditPackages, a.QuotaAt, a.QuotaProbeAt = nil, time.Time{}, time.Time{}
	if a.CooldownKind == upstream.ErrPlanLimit {
		a.Cooldown, a.CooldownKind = time.Time{}, upstream.ErrNone
	}
	r.mu.Unlock()
	for _, candidate := range siblings {
		if _, err := candidate.FallbackToConsumer(); err != nil {
			log.Printf("trae console fallback persistence failed: %T", err)
		}
	}
}

func (r *Runtime) switchToConsumer(identity *auth.Auth) bool {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	leave := statebackup.Enter()
	defer leave()
	r.mu.Lock()
	a := r.accounts[accountID(identity.UID)]
	current := a != nil && a.Identities[identity.Variant] == identity
	r.mu.Unlock()
	if !current && identity.FilePath != "" {
		return false
	}
	changed, saveErr := identity.FallbackToConsumer()
	if !changed {
		return false
	}
	if saveErr != nil {
		log.Printf("trae console fallback persistence failed: %T", saveErr)
	}
	if current {
		r.invalidateConsoleRoute(identity)
	}
	return true
}
