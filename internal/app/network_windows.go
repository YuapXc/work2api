package app

import (
	"errors"
	"syscall"
)

func isWindowsLocalNetworkFailure(err error) bool {
	for _, code := range []syscall.Errno{10050, 10051, 10065, 10049} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}
