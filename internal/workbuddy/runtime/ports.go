package runtime

import (
	"errors"
	"net"
	goruntime "runtime"
	"syscall"
	"time"
)

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
