// Package crypto implements symmetric encryption for stored application keys,
// faithfully ported from Workbuddy2API's workbuddy_one/_crypto.py.
//
// Scheme: one-time keystream + XOR. A persistent 32-byte master key plus a
// random nonce derive an HMAC-SHA256 counter-mode keystream the length of the
// plaintext; XOR yields the ciphertext. The master key lives in its own
// permission-restricted file, so a DB leak alone cannot recover keys.
// Encrypt-then-MAC (HMAC-SHA256 over nonce+cipher) guards against tampering.
package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
)

// tokenV1 marks the authenticated (MAC-bearing) ciphertext format. Legacy
// tokens without this prefix are decrypted MAC-less for backward compat.
const tokenV1 = "v1:"

// Manager holds the master key and its file location.
type Manager struct {
	keyFile string
	mu      sync.Mutex
	key     []byte
}

// NewManager returns a crypto manager whose master key lives at
// <dataDir>/.secret_key (mirrors MASTER_KEY_FILE in _crypto.py).
func NewManager(dataDir string) *Manager {
	return &Manager{keyFile: filepath.Join(dataDir, ".secret_key")}
}

// loadMasterKey reads the master key, generating and persisting a 32-byte
// random key on first use.
func (m *Manager) loadMasterKey() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.key) > 0 {
		return m.key, nil
	}
	if data, err := os.ReadFile(m.keyFile); err == nil {
		if trimmed := trimSpace(data); len(trimmed) > 0 {
			m.key = trimmed
			return m.key, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(m.keyFile), 0o755); err != nil {
		return nil, err
	}
	// write temp then atomic replace, avoiding a half-written key file
	tmp := m.keyFile + ".tmp"
	if err := os.WriteFile(tmp, key, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, m.keyFile); err != nil {
		return nil, err
	}
	m.key = key
	return key, nil
}

// stream derives length bytes of keystream via HMAC-SHA256 counter mode.
func stream(key, nonce []byte, length int) []byte {
	out := make([]byte, 0, length+sha256.Size)
	var counter uint32
	ctr := make([]byte, 4)
	for len(out) < length {
		binary.BigEndian.PutUint32(ctr, counter)
		h := hmac.New(sha256.New, key)
		h.Write(nonce)
		h.Write(ctr)
		out = h.Sum(out)
		counter++
	}
	return out[:length]
}

// mac computes the authentication tag over nonce+cipher (encrypt-then-MAC).
func mac(key, nonce, cipher []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("wb-auth"))
	h.Write(nonce)
	h.Write(cipher)
	return h.Sum(nil)
}

// Encrypt returns tokenV1 + base64(nonce + cipher + mac).
func (m *Manager) Encrypt(plaintext string) (string, error) {
	key, err := m.loadMasterKey()
	if err != nil {
		return "", err
	}
	data := []byte(plaintext)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ks := stream(key, nonce, len(data))
	cipher := make([]byte, len(data))
	for i := range data {
		cipher[i] = data[i] ^ ks[i]
	}
	tag := mac(key, nonce, cipher)
	blob := make([]byte, 0, len(nonce)+len(cipher)+len(tag))
	blob = append(blob, nonce...)
	blob = append(blob, cipher...)
	blob = append(blob, tag...)
	return tokenV1 + base64.StdEncoding.EncodeToString(blob), nil
}

// Decrypt reverses Encrypt. Invalid input or a failed MAC yields "".
func (m *Manager) Decrypt(token string) string {
	if token == "" {
		return ""
	}
	key, err := m.loadMasterKey()
	if err != nil {
		return ""
	}
	hasV1 := len(token) >= len(tokenV1) && token[:len(tokenV1)] == tokenV1
	b64 := token
	if hasV1 {
		b64 = token[len(tokenV1):]
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	var nonce, cipher []byte
	if hasV1 {
		if len(raw) < 16+32 {
			return ""
		}
		nonce = raw[:16]
		cipher = raw[16 : len(raw)-32]
		tag := raw[len(raw)-32:]
		if subtle.ConstantTimeCompare(tag, mac(key, nonce, cipher)) != 1 {
			return ""
		}
	} else {
		if len(raw) < 16 {
			return ""
		}
		nonce = raw[:16]
		cipher = raw[16:]
	}
	ks := stream(key, nonce, len(cipher))
	plain := make([]byte, len(cipher))
	for i := range cipher {
		plain[i] = cipher[i] ^ ks[i]
	}
	return string(plain)
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
