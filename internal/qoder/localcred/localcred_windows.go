//go:build windows

// Package localcred recovers a Qoder desktop login from the local Electron
// safeStorage vault so the user does not have to re-OAuth. Unlike the CodeBuddy
// "$wbEncrypted" case (whose key lives inside the extension and is unrecoverable),
// Qoder desktop uses Chromium's standard os_crypt v10 scheme: the AES-256-GCM
// master key sits in "Local State" wrapped with Windows DPAPI for the CURRENT
// user, so our same-user process can unwrap it and decrypt "auth.v1.dat", which
// holds {token: "dt-…", refreshToken: "drt-…"} in plaintext once decrypted.
package localcred

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// Credential is a Qoder desktop login recovered from the safeStorage vault.
type Credential struct {
	Region       string // "cn" | "global"
	DeviceToken  string // dt-…  (qoder2api device token)
	RefreshToken string // drt-… (device refresh token)
	Source       string // app-data dir it was recovered from
}

// appDirs maps each Qoder desktop variant to its %APPDATA% folder + region.
var appDirs = []struct{ region, folder string }{
	{"cn", "com.qodercn.app.stable"},
	{"global", "com.qoder.app.stable"},
}

// Detect recovers credentials from every installed Qoder desktop variant. A
// missing variant is skipped silently; the returned error is the first real
// (non-not-exist) failure when nothing was recovered.
func Detect() ([]Credential, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil, errors.New("APPDATA 环境变量未设置")
	}
	var out []Credential
	var firstErr error
	for _, a := range appDirs {
		cred, err := decryptDir(filepath.Join(appData, a.folder), a.region)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if cred.DeviceToken != "" {
			out = append(out, cred)
		}
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

func decryptDir(base, region string) (Credential, error) {
	var c Credential
	dat, err := os.ReadFile(filepath.Join(base, "auth.v1.dat"))
	if err != nil {
		return c, err
	}
	key, err := masterKey(base)
	if err != nil {
		return c, err
	}
	pt, err := decryptV10(dat, key)
	if err != nil {
		return c, err
	}
	var payload struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(pt, &payload); err != nil {
		return c, err
	}
	c.Region = region
	c.DeviceToken = strings.TrimSpace(payload.Token)
	c.RefreshToken = strings.TrimSpace(payload.RefreshToken)
	c.Source = base
	return c, nil
}

// masterKey reads Local State, base64-decodes os_crypt.encrypted_key, strips the
// "DPAPI" prefix and DPAPI-unwraps it into the 32-byte AES key.
func masterKey(base string) ([]byte, error) {
	ls, err := os.ReadFile(filepath.Join(base, "Local State"))
	if err != nil {
		return nil, err
	}
	var j struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(ls, &j); err != nil {
		return nil, err
	}
	wrapped, err := base64.StdEncoding.DecodeString(j.OsCrypt.EncryptedKey)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < 5 || string(wrapped[:5]) != "DPAPI" {
		return nil, errors.New("os_crypt 密钥非 DPAPI 格式（可能是新版 App-Bound 加密，无法离线解密）")
	}
	return dpapiUnprotect(wrapped[5:])
}

// decryptV10 decrypts a Chromium os_crypt "v10" blob: 3-byte prefix + 12-byte
// GCM nonce + ciphertext + 16-byte GCM tag.
func decryptV10(dat, key []byte) ([]byte, error) {
	if len(dat) < 3+12+16 || string(dat[:3]) != "v10" {
		return nil, errors.New("auth.v1.dat 非 v10 格式")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, dat[3:15], dat[15:], nil)
}

// --- Windows DPAPI (crypt32.CryptUnprotectData, current-user scope) ---

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCryptUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiUnprotect(in []byte) ([]byte, error) {
	inBlob := dataBlob{cbData: uint32(len(in))}
	if len(in) > 0 {
		inBlob.pbData = &in[0]
	}
	var out dataBlob
	r, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&inBlob)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	res := make([]byte, out.cbData)
	copy(res, unsafe.Slice(out.pbData, out.cbData))
	return res, nil
}
