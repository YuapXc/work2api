package app

import (
	"errors"
	"net"
	"syscall"
)

// isLocalDNSFailure 判定一次上游失败是否由「本机域名解析失败」引起。
//
// 移植自 buddy-proxy #62（2026-10-02 事故）：一次约 1 秒的本机 DNS 抖动让所有
// 候选通道同时失败、全部被打上冷却——期间客户端重试全部命中冷却跳过，一个上游
// 请求都发不出去，整模型锁死。DNS 不是账号身份故障；即使只有一个上游域名
// 解析失败，也不该处罚账号。识别后当前请求快速失败，不打冷却。
//
// Go 侧比 Python 好做：标准库把解析失败统一为 *net.DNSError（Windows 的
// WSAHOST_NOT_FOUND / DNS_ERROR_RCODE_NAME_ERROR 也由 net 包归一为
// errNoSuchHost → DNSError），errors.As 沿链下钻即可。超时形态的 DNS 失败
// （IsTimeout=true）同样计入——那是机器级抖动的另一种常见表现。
//
// 识别范围刻意窄：只有解析失败。连接被拒/重置仍照常冷却，那些更可能是特定
// 上游的状态。
func isLocalDNSFailure(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

// Only typed resolver and local routing/address failures are exempt. Refusal,
// reset, EOF, TLS, HTTP errors and ambiguous timeouts remain upstream failures.
func isLocalNetworkFailure(err error) bool {
	if isLocalDNSFailure(err) {
		return true
	}
	for _, code := range []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.EADDRNOTAVAIL} {
		if errors.Is(err, code) {
			return true
		}
	}
	return isWindowsLocalNetworkFailure(err)
}
