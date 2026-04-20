package xai

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/platform/config"
	"github.com/google/uuid"
)

const (
	defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	defaultStatsigID = "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgdW5kZWZpbmVkIChyZWFkaW5nICdjaGlsZE5vZGVzJyk="
	defaultBaggage   = "sentry-environment=production,sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c"
)

var (
	sanitizeHeaderReplacer = strings.NewReplacer(
		"\u2010", "-",
		"\u2011", "-",
		"\u2012", "-",
		"\u2013", "-",
		"\u2014", "-",
		"\u2212", "-",
		"\u2018", "'",
		"\u2019", "'",
		"\u201c", "\"",
		"\u201d", "\"",
		"\u00a0", " ",
		"\u2007", " ",
		"\u202f", " ",
		"\u200b", "",
		"\u200c", "",
		"\u200d", "",
		"\ufeff", "",
	)
	whitespacePattern = regexp.MustCompile(`\s+`)
	versionPattern    = regexp.MustCompile(`(\d{2,3})`)
	chromePattern     = regexp.MustCompile(`Chrome/(\d+)`)
)

func sanitizeToken(token string) string {
	return strings.TrimPrefix(sanitizeHeaderValue(token, true), "sso=")
}

func sanitizeHeaderValue(value string, stripSpaces bool) string {
	sanitized := sanitizeHeaderReplacer.Replace(value)
	if stripSpaces {
		sanitized = whitespacePattern.ReplaceAllString(sanitized, "")
	} else {
		sanitized = strings.TrimSpace(sanitized)
	}
	return strings.Map(func(r rune) rune {
		if r > 255 {
			return -1
		}
		return r
	}, sanitized)
}

func buildSSOCookie(cfg *config.Service, token string) string {
	cookie := fmt.Sprintf("sso=%s; sso-rw=%s", sanitizeToken(token), sanitizeToken(token))
	cfCookies := ""
	cfClearance := ""
	if cfg != nil {
		cfCookies = sanitizeHeaderValue(cfg.GetString("proxy.clearance.cf_cookies", ""), false)
		cfClearance = sanitizeHeaderValue(cfg.GetString("proxy.clearance.cf_clearance", cfg.GetString("proxy.cf_clearance", "")), true)
	}
	if cfClearance != "" {
		if cfCookies == "" {
			cfCookies = "cf_clearance=" + cfClearance
		} else if strings.Contains(cfCookies, "cf_clearance=") {
			cfCookies = regexp.MustCompile(`(^|;\s*)cf_clearance=[^;]*`).ReplaceAllString(cfCookies, "${1}cf_clearance="+cfClearance)
		} else {
			cfCookies = strings.TrimRight(cfCookies, "; ") + "; cf_clearance=" + cfClearance
		}
	}
	if cfCookies != "" {
		cookie += "; " + cfCookies
	}
	return cookie
}

func buildRequestHeaders(cfg *config.Service, token, contentType, origin, referer string) http.Header {
	if contentType == "" {
		contentType = "application/json"
	}
	if origin == "" {
		origin = "https://grok.com"
	}
	if referer == "" {
		referer = "https://grok.com/"
	}

	rawUserAgent := cfg.GetString("proxy.clearance.user_agent", defaultUserAgent)
	userAgent := sanitizeHeaderValue(rawUserAgent, false)
	browser := resolveBrowser(cfg, rawUserAgent)
	accept, fetchDest := resolveAccept(contentType)

	headers := http.Header{}
	headers.Set("Accept", accept)
	headers.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Baggage", defaultBaggage)
	headers.Set("Content-Type", contentType)
	headers.Set("Origin", sanitizeHeaderValue(origin, false))
	headers.Set("Priority", "u=1, i")
	headers.Set("Referer", sanitizeHeaderValue(referer, false))
	headers.Set("Sec-Fetch-Dest", fetchDest)
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("Sec-Fetch-Site", resolveFetchSite(origin, referer))
	headers.Set("User-Agent", userAgent)
	headers.Set("Cookie", buildSSOCookie(cfg, token))
	headers.Set("x-statsig-id", statsigID(cfg))
	headers.Set("x-xai-request-id", uuid.NewString())

	for key, value := range clientHints(browser, rawUserAgent) {
		headers.Set(key, value)
	}

	return headers
}

func buildWSHeaders(cfg *config.Service, token, origin string) http.Header {
	if origin == "" {
		origin = "https://grok.com"
	}
	rawUserAgent := cfg.GetString("proxy.clearance.user_agent", defaultUserAgent)
	userAgent := sanitizeHeaderValue(rawUserAgent, false)
	browser := resolveBrowser(cfg, rawUserAgent)

	headers := http.Header{}
	headers.Set("Origin", sanitizeHeaderValue(origin, false))
	headers.Set("User-Agent", userAgent)
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	headers.Set("Cookie", buildSSOCookie(cfg, token))

	for key, value := range clientHints(browser, rawUserAgent) {
		headers.Set(key, value)
	}

	return headers
}

func resolveAccept(contentType string) (accept string, fetchDest string) {
	switch contentType {
	case "", "application/json":
		return "*/*", "empty"
	case "image/jpeg", "image/png", "video/mp4", "video/webm":
		return "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8", "document"
	default:
		return "*/*", "empty"
	}
}

func resolveFetchSite(origin, referer string) string {
	originHost := hostname(origin)
	refererHost := hostname(referer)
	if originHost != "" && originHost == refererHost {
		return "same-origin"
	}
	return "same-site"
}

func hostname(value string) string {
	parsed, err := url.Parse(sanitizeHeaderValue(value, false))
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func resolveBrowser(cfg *config.Service, rawUserAgent string) string {
	if configured := strings.ToLower(strings.TrimSpace(cfg.GetString("proxy.clearance.browser", ""))); configured != "" {
		return configured
	}
	match := chromePattern.FindStringSubmatch(rawUserAgent)
	if len(match) == 2 {
		return "chrome" + match[1]
	}
	return "chrome120"
}

func clientHints(browser, userAgent string) map[string]string {
	browser = strings.ToLower(browser)
	userAgentLower := strings.ToLower(userAgent)
	isChromium := strings.Contains(browser, "chrome") ||
		strings.Contains(browser, "chromium") ||
		strings.Contains(browser, "edge") ||
		strings.Contains(browser, "brave") ||
		strings.Contains(userAgentLower, "chrome") ||
		strings.Contains(userAgentLower, "chromium") ||
		strings.Contains(userAgentLower, "edg")
	if !isChromium || strings.Contains(userAgentLower, "firefox") || (strings.Contains(userAgentLower, "safari") && !strings.Contains(userAgentLower, "chrome")) {
		return nil
	}

	version := majorVersion(browser, userAgent)
	if version == "" {
		return nil
	}

	brand := "Google Chrome"
	switch {
	case strings.Contains(browser, "edge") || strings.Contains(userAgentLower, "edg"):
		brand = "Microsoft Edge"
	case strings.Contains(browser, "brave"):
		brand = "Brave"
	case strings.Contains(browser, "chromium"):
		brand = "Chromium"
	}

	platform := platformFromUA(userAgent)
	arch := archFromUA(userAgent)
	mobile := "?0"
	if strings.Contains(userAgentLower, "mobile") || platform == "Android" || platform == "iOS" {
		mobile = "?1"
	}

	hints := map[string]string{
		"Sec-Ch-Ua":        fmt.Sprintf(`"%s";v="%s", "Chromium";v="%s", "Not(A:Brand";v="24"`, brand, version, version),
		"Sec-Ch-Ua-Mobile": mobile,
		"Sec-Ch-Ua-Model":  "",
	}
	if platform != "" {
		hints["Sec-Ch-Ua-Platform"] = fmt.Sprintf(`"%s"`, platform)
	}
	if arch != "" {
		hints["Sec-Ch-Ua-Arch"] = arch
		hints["Sec-Ch-Ua-Bitness"] = "64"
	}
	return hints
}

func majorVersion(browser, userAgent string) string {
	for _, source := range []string{browser, userAgent} {
		if match := versionPattern.FindStringSubmatch(source); len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

func platformFromUA(userAgent string) string {
	lower := strings.ToLower(userAgent)
	switch {
	case strings.Contains(lower, "windows"):
		return "Windows"
	case strings.Contains(lower, "mac os x"), strings.Contains(lower, "macintosh"):
		return "macOS"
	case strings.Contains(lower, "android"):
		return "Android"
	case strings.Contains(lower, "iphone"), strings.Contains(lower, "ipad"):
		return "iOS"
	case strings.Contains(lower, "linux"):
		return "Linux"
	default:
		return ""
	}
}

func archFromUA(userAgent string) string {
	lower := strings.ToLower(userAgent)
	switch {
	case strings.Contains(lower, "aarch64"), strings.Contains(lower, " arm"):
		return "arm"
	case strings.Contains(lower, "x86_64"), strings.Contains(lower, "x64"), strings.Contains(lower, "win64"), strings.Contains(lower, "intel"):
		return "x86"
	default:
		return ""
	}
}

func statsigID(cfg *config.Service) string {
	if !cfg.GetBool("features.dynamic_statsig", false) {
		return defaultStatsigID
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	if rng.Intn(2) == 0 {
		randValue := randomString(rng, "abcdefghijklmnopqrstuvwxyz0123456789", 5)
		return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("e:TypeError: Cannot read properties of null (reading 'children['%s']')", randValue)))
	}

	randValue := randomString(rng, "abcdefghijklmnopqrstuvwxyz", 10)
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("e:TypeError: Cannot read properties of undefined (reading '%s')", randValue)))
}

func randomString(rng *rand.Rand, alphabet string, size int) string {
	builder := strings.Builder{}
	builder.Grow(size)
	for index := 0; index < size; index++ {
		builder.WriteByte(alphabet[rng.Intn(len(alphabet))])
	}
	return builder.String()
}

func isResettableStatus(cfg *config.Service, status int) bool {
	if cfg == nil {
		return status == http.StatusForbidden
	}

	switch codes := cfg.Get("retry.reset_session_status_codes").(type) {
	case []any:
		for _, code := range codes {
			switch typed := code.(type) {
			case int:
				if typed == status {
					return true
				}
			case int64:
				if int(typed) == status {
					return true
				}
			case float64:
				if int(typed) == status {
					return true
				}
			}
		}
	case []int:
		for _, code := range codes {
			if code == status {
				return true
			}
		}
	case nil:
		return status == http.StatusForbidden
	default:
		return status == http.StatusForbidden
	}

	return false
}
