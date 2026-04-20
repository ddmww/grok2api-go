package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ddmww/grok2api-go/internal/platform/config"
	utls "github.com/refraction-networking/utls"
	xproxy "golang.org/x/net/proxy"
)

var browserVersionPattern = regexp.MustCompile(`(\d{2,3})`)

func newTransport(cfg *config.Service, proxyValue string) (*http.Transport, error) {
	insecure := cfg.GetBool("proxy.egress.skip_ssl_verify", false)
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec
		TLSHandshakeTimeout:   30 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 0,
	}
	protocols := &http.Protocols{}
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	transport.Protocols = protocols

	proxyURL := strings.TrimSpace(proxyValue)
	transport.DialContext = directDialer().DialContext
	if proxyURL == "" {
		return transport, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if err := validateProxyScheme(parsed); err != nil {
		return nil, err
	}

	switch {
	case strings.HasPrefix(parsed.Scheme, "socks"):
		dialContext, err := socksDialContext(parsed)
		if err != nil {
			return nil, err
		}
		transport.DialContext = dialContext
	default:
		transport.Proxy = http.ProxyURL(parsed)
	}

	return transport, nil
}

func DialContext(cfg *config.Service, proxyValue string) (func(context.Context, string, string) (net.Conn, error), error) {
	proxyURL := strings.TrimSpace(proxyValue)
	if proxyURL == "" {
		return directDialer().DialContext, nil
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if err := validateProxyScheme(parsed); err != nil {
		return nil, err
	}

	if strings.HasPrefix(parsed.Scheme, "socks") {
		return socksDialContext(parsed)
	}
	return directDialer().DialContext, nil
}

func directDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
}

func socksDialContext(proxyURL *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	switch strings.ToLower(proxyURL.Scheme) {
	case "socks", "socks5h":
		proxyURL = cloneURLWithScheme(proxyURL, "socks5")
	case "socks4a":
		proxyURL = cloneURLWithScheme(proxyURL, "socks4")
	}
	dialer, err := xproxy.FromURL(proxyURL, xproxy.Direct)
	if err != nil {
		return nil, err
	}
	if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
		return contextDialer.DialContext, nil
	}
	return func(_ context.Context, network, addr string) (net.Conn, error) {
		return dialer.Dial(network, addr)
	}, nil
}

func cloneURLWithScheme(value *url.URL, scheme string) *url.URL {
	if value == nil {
		return nil
	}
	copied := *value
	copied.Scheme = scheme
	return &copied
}

func wrapUTLS(_ context.Context, rawConn net.Conn, _ string, _ bool, _ string) (net.Conn, error) {
	return rawConn, nil
}

func clientHelloID(browser string) utls.ClientHelloID {
	match := browserVersionPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(browser)))
	if len(match) != 2 {
		return utls.HelloChrome_Auto
	}

	switch match[1] {
	case "120":
		return utls.HelloChrome_120
	case "121", "122", "123", "124", "125", "126", "127", "128", "129", "130":
		return utls.HelloChrome_120
	case "131", "132":
		return utls.HelloChrome_131
	default:
		return utls.HelloChrome_Auto
	}
}

func validateProxyScheme(proxyURL *url.URL) error {
	switch proxyURL.Scheme {
	case "http", "https", "socks", "socks5", "socks5h", "socks4", "socks4a":
		return nil
	default:
		return errors.New("unsupported proxy scheme")
	}
}
