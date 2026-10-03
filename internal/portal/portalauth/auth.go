// Package portalauth implements the user-portal identity layer: bcrypt password
// hashing, persistent revocable user sessions (distinct cookie from the admin
// surface — HANDOFF §4), and the eligibility/permission computation shared by
// the model endpoints and the portal API.
package portalauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"work2api/internal/store"
)

// Session cookie for the user portal. Deliberately a different name from the
// admin cookie (w2a_admin_session) so the two surfaces can never cross-
// authenticate.
const UserCookieName = "w2a_portal_session"

const (
	// SessionTTL mirrors the admin session (24h sliding), in seconds.
	SessionTTL = 24 * 3600
	// username bounds: cheap sanity before the expensive bcrypt call.
	maxUsernameLen = 64
	maxPasswordLen = 128
	minPasswordLen = 8
)

// ErrBadCredentials deliberately does not distinguish "no such user" from
// "wrong password" (username enumeration defense).
var ErrBadCredentials = errors.New("用户名或密码错误")

// HashPassword wraps bcrypt.GenerateFromPassword (cost 10). bcrypt truncates
// at 72 bytes; the length cap rejects oversized input before hashing instead
// of silently truncating.
func HashPassword(password string) (string, error) {
	if n := utf8.RuneCountInString(password); n < minPasswordLen || n > maxPasswordLen || len(password) > 72 {
		return "", errors.New("密码需至少 8 个字符且不超过 72 字节")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword is constant-time inside bcrypt.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func validUsername(name string) bool {
	n := len(name)
	if n < 2 || n > maxUsernameLen {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// CreateUser validates + hashes and inserts; role must come from server logic,
// never from user input (HANDOFF §4). Returns the user id.
func CreateUser(db *store.DB, username, password, role string) (int64, error) {
	username, hash, err := UserPasswordHash(username, password)
	if err != nil {
		return 0, err
	}
	return db.CreateUser(username, hash, role)
}

func UserPasswordHash(username, password string) (string, string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !validUsername(username) {
		return "", "", errors.New("用户名需为 2–64 位字母、数字、- _ . 组合")
	}
	hash, err := HashPassword(password)
	return username, hash, err
}

// dummyBcryptHash is a valid bcrypt hash of an unknown password; comparing
// against it equalizes the timing of unknown-user logins with real ones.
const dummyBcryptHash = "$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0X8fS0VPLkxuA2jPkFZBpF0VJ1O"

// Login authenticates and issues a persistent session. Equal-cost path for
// unknown users (dummy bcrypt compare) so timing can't enumerate usernames.
func Login(db *store.DB, username, password string) (*store.User, string, error) {
	user, err := db.GetUserByUsername(strings.ToLower(strings.TrimSpace(username)))
	if err != nil {
		return nil, "", err
	}
	if user == nil {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(password))
		return nil, "", ErrBadCredentials
	}
	if !VerifyPassword(user.PasswordHash, password) || user.Status != "active" {
		return nil, "", ErrBadCredentials
	}
	token, err := newSessionToken()
	if err != nil {
		return nil, "", err
	}
	expires := float64(nowUnix() + SessionTTL)
	if err := db.CreateUserSessionForPassword(HashToken(token), user.ID, expires, user.PasswordHash); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, "", ErrBadCredentials
		}
		return nil, "", err
	}
	return user, token, nil
}

// SessionUser resolves a raw cookie token to its live user. Disabled users are
// rejected here (immediate effect on the next request — HANDOFF 资格矩阵).
func SessionUser(db *store.DB, token string) (*store.User, error) {
	if token == "" {
		return nil, ErrBadCredentials
	}
	s, err := db.GetUserSession(HashToken(token))
	if err != nil || s == nil {
		return nil, ErrBadCredentials
	}
	user, err := db.GetUser(s.UserID)
	if err != nil || user == nil || user.Status != "active" {
		return nil, ErrBadCredentials
	}
	// sliding expiry
	_ = db.ExtendUserSession(HashToken(token), float64(nowUnix()+SessionTTL))
	return user, nil
}

// Logout revokes the session.
func Logout(db *store.DB, token string) {
	if token != "" {
		_ = db.DeleteUserSession(HashToken(token))
	}
}

// ChangePassword re-verifies the old password, rotates the hash, and revokes
// every existing session (HANDOFF §4: password change must drop sessions). The
// caller re-issues the current session if the user should stay logged in.
func ChangePassword(db *store.DB, userID int64, oldPassword, newPassword string) error {
	user, err := db.GetUser(userID)
	if err != nil || user == nil {
		return ErrBadCredentials
	}
	if !VerifyPassword(user.PasswordHash, oldPassword) {
		return ErrBadCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := db.UpdateUserPassword(userID, hash, user.PasswordHash); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ErrBadCredentials
		}
		return err
	}
	return nil
}

// HashToken is the one-way form of a session token (SHA-256 hex): the raw
// token lives only in the cookie, the DB stores only this.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
