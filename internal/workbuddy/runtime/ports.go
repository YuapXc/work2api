package runtime

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	goruntime "runtime"
	"syscall"
	"time"
)

func transportFailure(err error) bool {
	if LocalNetworkFailure(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var network net.Error
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	if errors.As(err, &network) || errors.As(err, &certificate) || errors.As(err, &record) {
		return true
	}
	for _, code := range []syscall.Errno{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ECONNREFUSED} {
		if errors.Is(err, code) {
			return true
		}
	}
	if goruntime.GOOS == "windows" {
		for _, code := range []syscall.Errno{10054, 10053, 10060, 10061} {
			if errors.Is(err, code) {
				return true
			}
		}
	}
	return false
}

func LocalNetworkFailure(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	for _, code := range []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.EADDRNOTAVAIL} {
		if errors.Is(err, code) {
			return true
		}
	}
	if goruntime.GOOS == "windows" {
		for _, code := range []syscall.Errno{10050, 10051, 10065, 10049} {
			if errors.Is(err, code) {
				return true
			}
		}
	}
	return false
}

func nowSec() float64 { return float64(time.Now().UnixNano()) / 1e9 }
