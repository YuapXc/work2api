//go:build !windows

package localcred

import "errors"

// Credential mirrors the Windows type so callers compile on all platforms.
type Credential struct {
	Region       string
	DeviceToken  string
	RefreshToken string
	Source       string
}

// Detect is Windows-only: the Qoder desktop safeStorage vault is DPAPI-wrapped.
func Detect() ([]Credential, error) {
	return nil, errors.New("本地 Qoder 凭据探测仅支持 Windows")
}
