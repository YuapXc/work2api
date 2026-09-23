// Package profiles builds upstream identity headers per profile. Ported from
// workbuddy_one/client_profiles.py.
//
// Why it matters: the upstream uses User-Agent / X-IDE-* to decide "which
// product's client is this"; a mismatch is rejected or returns another
// product's model catalog. International WorkBuddy's product name is
// "WorkBuddy AI" (domestic has no "AI" suffix) — a one-string difference.
package profiles

import (
	"work2api/internal/workbuddy/siterouting"
)

// Pinned client versions (from verified official releases; no local desktop
// config or machine id is read).
const (
	cliVersion         = "2.149.0"
	workbuddyVersion   = "5.5.2"
	workbuddyCLIVerion = "2.137.1"
	cliUserAgent       = "CLI/" + cliVersion + " CodeBuddy/" + cliVersion
)

// sdkHeaders are fixed regardless of site: "this is a node-env SDK call".
var sdkHeaders = map[string]string{
	"x-stainless-arch":            "x64",
	"x-stainless-lang":            "js",
	"x-stainless-os":              "Linux",
	"x-stainless-package-version": "6.25.0",
	"x-stainless-retry-count":     "0",
	"x-stainless-runtime":         "node",
	"x-stainless-runtime-version": "v24.21.0",
	"X-Agent-Intent":              "craft",
	"X-Agent-Purpose":             "conversation",
	"X-Agent-Type":                "main",
	"X-Private-Data":              "false",
	"X-CodeBuddy-Request":         "1",
}

// Account is the credential's account blob (subset used for headers).
type Account map[string]any

func (a Account) str(key string) string {
	if a == nil {
		return ""
	}
	switch v := a[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		// account ids may arrive as JSON numbers
		return trimFloat(v)
	default:
		return ""
	}
}

// IdentityHeaders maps a profile to its UA + X-IDE-* headers.
func IdentityHeaders(profile string) map[string]string {
	if product, _ := siterouting.ProfileProduct(profile); product == "cli" {
		return map[string]string{
			"User-Agent":    cliUserAgent,
			"X-IDE-Type":    "CLI",
			"X-IDE-Name":    "CLI",
			"X-IDE-Version": cliVersion,
		}
	}
	name := "WorkBuddy"
	if region, _ := siterouting.ProfileRegion(profile); region == "intl" {
		name = "WorkBuddy AI"
	}
	return map[string]string{
		"User-Agent":    "WorkBuddy/" + workbuddyVersion + " " + name + "/" + workbuddyVersion + " CLI/" + workbuddyCLIVerion,
		"X-IDE-Type":    "WorkBuddy",
		"X-IDE-Name":    "WorkBuddy",
		"X-IDE-Version": workbuddyVersion,
	}
}

// AuthHeaders builds authenticated identity headers (shared by chat / refresh /
// credit / check-in).
func AuthHeaders(auth siterouting.Auth, account Account) map[string]string {
	domain := siterouting.DomainForAuth(auth)
	accessToken, _ := auth["accessToken"].(string)
	headers := map[string]string{
		"Content-Type":     "application/json",
		"Accept":           "application/json",
		"Authorization":    "Bearer " + accessToken,
		"X-User-Id":        account.str("uid"),
		"X-Enterprise-Id":  account.str("enterpriseId"),
		"X-Tenant-Id":      account.str("enterpriseId"),
		"X-Domain":         domain,
		"X-Product":        "SaaS",
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           "https://" + domain,
		"Referer":          "https://" + domain + "/",
	}
	profile, _ := siterouting.ProfileForAuth(auth)
	for k, v := range IdentityHeaders(profile) {
		headers[k] = v
	}
	return headers
}

// CredentialHeaders = SDK fixed headers + auth/identity headers.
func CredentialHeaders(auth siterouting.Auth, account Account) map[string]string {
	headers := make(map[string]string, len(sdkHeaders)+16)
	for k, v := range sdkHeaders {
		headers[k] = v
	}
	for k, v := range AuthHeaders(auth, account) {
		headers[k] = v
	}
	return headers
}

// CatalogHeaders builds model-catalog request headers. WorkBuddy must NOT send
// x-client-platform: cli (it would fetch another product's catalog); CLI must.
func CatalogHeaders(auth siterouting.Auth, account Account) map[string]string {
	headers := AuthHeaders(auth, account)
	headers["Connection"] = "close"
	profile, _ := siterouting.ProfileForAuth(auth)
	if product, _ := siterouting.ProfileProduct(profile); product == "cli" {
		headers["x-client-platform"] = "cli"
	}
	return headers
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		// integer-valued: format without decimals
		return itoa(int64(f))
	}
	return ""
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
