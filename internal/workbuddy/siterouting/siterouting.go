// Package siterouting decides the single fixed upstream endpoint for a
// credential. Ported faithfully from workbuddy_one/site_routing.py.
//
// Core model: region (domestic/international) × product (CLI/WorkBuddy) are two
// orthogonal dimensions, four profiles total. Same-site CLI and WorkBuddy are
// independent backends (separate model catalogs), so the profile — not the
// region — is the smallest unit of routing and accounting.
//
// Security principle: a credential's domain is only a hint for *which* fixed
// endpoint to pick, never used directly as a request target. Every URL comes
// from the profileEndpoints allowlist, eliminating SSRF at the root.
package siterouting

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// Sites.
const (
	Domestic      = "domestic"
	International = "international"
)

// Fixed endpoints for the four profiles.
const (
	DomesticEndpoint      = "https://copilot.tencent.com"
	InternationalEndpoint = "https://www.codebuddy.ai"
)

// DefaultProfile: credentials with no site hint fall back to domestic CLI
// (backward compat, never guessed).
const DefaultProfile = "cn-cli"

const refreshPath = "/v2/plugin/auth/token/refresh"

// ErrInvalidHint is returned for any suspicious/unknown site hint.
var ErrInvalidHint = errors.New("站点提示无效")

// ErrConflict is returned when two hints disagree.
var ErrConflict = errors.New("凭据的站点提示相互冲突")

var profileEndpoints = map[string]string{
	"cn-cli":    DomesticEndpoint,           // 国内 CodeBuddy CLI
	"cn-work":   "https://www.workbuddy.cn", // 国内 WorkBuddy
	"intl-cli":  InternationalEndpoint,      // 国际 CodeBuddy
	"intl-work": "https://www.workbuddy.ai", // 国际 WorkBuddy
}

// domainProfiles maps a known lowercase host to its profile.
var domainProfiles = map[string]string{
	"www.codebuddy.cn":    "cn-cli",
	"www.workbuddy.cn":    "cn-work",
	"copilot.tencent.com": "cn-cli",
	"www.codebuddy.ai":    "intl-cli",
	"www.workbuddy.ai":    "intl-work",
}

// Auth is the credential's auth blob (subset used for routing).
type Auth map[string]any

func (a Auth) str(key string) string {
	if a == nil {
		return ""
	}
	if v, ok := a[key].(string); ok {
		return v
	}
	return ""
}

// ProfileRegion returns cn/intl for a profile.
func ProfileRegion(profile string) (string, error) {
	if _, ok := profileEndpoints[profile]; !ok {
		return "", errors.New("未知的账号档位")
	}
	return strings.SplitN(profile, "-", 2)[0], nil
}

// ProfileProduct returns cli/workbuddy for a profile.
func ProfileProduct(profile string) (string, error) {
	if _, err := ProfileRegion(profile); err != nil {
		return "", err
	}
	if strings.HasSuffix(profile, "-work") {
		return "workbuddy", nil
	}
	return "cli", nil
}

// ProfileSite returns domestic/international (or qoder sites) for a profile.
func ProfileSite(profile string) string {
	switch profile {
	case "qoder-cn":
		return "qoder-cn"
	case "qoder-global":
		return "qoder-global"
	}
	region, err := ProfileRegion(profile)
	if err != nil {
		return Domestic
	}
	if region == "intl" {
		return International
	}
	return Domestic
}

// knownHost validates and normalizes a known host; any suspicious input is
// rejected. When issuer is true a path is allowed (JWT iss carries a path).
func knownHost(value string, issuer bool) (string, error) {
	if value == "" {
		return "", ErrInvalidHint
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return "", ErrInvalidHint
		}
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\\?#") {
		return "", ErrInvalidHint
	}
	raw := value
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidHint
	}
	host := parsed.Hostname()
	if _, known := domainProfiles[host]; !known {
		return "", ErrInvalidHint
	}
	// netloc must equal host exactly => rejects port and userinfo
	if parsed.User != nil {
		return "", ErrInvalidHint
	}
	if !strings.EqualFold(parsed.Scheme, "https") ||
		!strings.EqualFold(parsed.Host, host) ||
		(!issuer && parsed.Path != "" && parsed.Path != "/") {
		return "", ErrInvalidHint
	}
	return host, nil
}

// NormalizeDomain normalizes a known HTTPS host, rejecting port/userinfo/path.
func NormalizeDomain(value string) (string, error) {
	return knownHost(value, false)
}

// tokenIssuer extracts the JWT iss claim (base64-decodes payload only, no
// signature verification). Parse failures return "".
func tokenIssuer(token string) string {
	if strings.Count(token, ".") != 2 {
		return ""
	}
	payload := strings.Split(token, ".")[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return ""
	}
	if iss, ok := claims["iss"].(string); ok {
		return iss
	}
	return ""
}

// hints extracts (domain, issuer) known-host hints from a credential.
func hints(auth Auth) (domain, issuer string) {
	if d := auth.str("domain"); d != "" {
		if h, err := NormalizeDomain(d); err == nil {
			domain = h
		}
	}
	if iss := tokenIssuer(auth.str("accessToken")); iss != "" {
		if h, err := knownHost(iss, true); err == nil {
			issuer = h
		}
	}
	return domain, issuer
}

// SiteForAuth determines the site; conflicting hints are rejected.
func SiteForAuth(auth Auth) (string, error) {
	domain, issuer := hints(auth)
	sites := map[string]struct{}{}
	for _, host := range []string{domain, issuer} {
		if host != "" {
			sites[ProfileSite(domainProfiles[host])] = struct{}{}
		}
	}
	if len(sites) > 1 {
		return "", ErrConflict
	}
	for s := range sites {
		return s, nil
	}
	return Domestic, nil
}

// ProfileForAuth determines the profile; falls back to domestic CLI when no
// hint is present (legacy accounts).
func ProfileForAuth(auth Auth) (string, error) {
	if _, err := SiteForAuth(auth); err != nil {
		return "", err
	}
	domain, issuer := hints(auth)
	// copilot.tencent.com is the domestic shared entry; prefer the more
	// specific branded account domain.
	profiles := map[string]struct{}{}
	for _, host := range []string{domain, issuer} {
		if host != "" && host != "copilot.tencent.com" {
			profiles[domainProfiles[host]] = struct{}{}
		}
	}
	if len(profiles) > 1 {
		return "", errors.New("凭据的产品提示相互冲突")
	}
	for p := range profiles {
		return p, nil
	}
	return DefaultProfile, nil
}

// EndpointForProfile maps a profile to its fixed upstream endpoint.
func EndpointForProfile(profile string) (string, error) {
	ep, ok := profileEndpoints[profile]
	if !ok {
		return "", errors.New("未知的账号档位")
	}
	return ep, nil
}

// EndpointForAuth maps a credential to its fixed upstream endpoint.
func EndpointForAuth(auth Auth) (string, error) {
	profile, err := ProfileForAuth(auth)
	if err != nil {
		return "", err
	}
	return profileEndpoints[profile], nil
}

// RefreshURLForAuth maps a credential to its token refresh URL (must share the
// chat origin, else international accounts cannot refresh).
func RefreshURLForAuth(auth Auth) (string, error) {
	profile, err := ProfileForAuth(auth)
	if err != nil {
		return "", err
	}
	return profileEndpoints[profile] + refreshPath, nil
}

// DomainForAuth returns the X-Domain header value.
func DomainForAuth(auth Auth) string {
	domain, issuer := hints(auth)
	if domain != "" {
		return domain
	}
	if issuer != "" {
		return issuer
	}
	return "www.codebuddy.cn"
}

// ChatURLForProfile returns the chat completions URL for a profile.
func ChatURLForProfile(profile string) (string, error) {
	ep, err := EndpointForProfile(profile)
	if err != nil {
		return "", err
	}
	return ep + "/v2/chat/completions", nil
}

// CatalogURLForProfile returns the model catalog URL (/v3/config works on both
// domestic and international sites).
func CatalogURLForProfile(profile string) (string, error) {
	ep, err := EndpointForProfile(profile)
	if err != nil {
		return "", err
	}
	return ep + "/v3/config", nil
}
