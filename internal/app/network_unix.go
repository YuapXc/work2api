//go:build !windows

package app

func isWindowsLocalNetworkFailure(error) bool { return false }
