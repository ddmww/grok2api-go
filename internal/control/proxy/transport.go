package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
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
	if proxyURL == "" {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return directDialer().DialContext(ctx, network, addr)
		}
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := directDialer().DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return wrapUTLS(ctx, conn, addr, insecure, cfg.GetString("proxy.clearance.browser", "chrome136"))
		}
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
	case strings.HasPrefix(parsed.Scheme, "socks5"):
		dialContext, err := socksDialContext(parsed)
		if err != nil {
			return nil, err
		}
		transport.DialContext = dialContext
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, dialErr := dialContext(ctx, network, addr)
			if dialErr != nil {
				return nil, dialErr
			}
			return wrapUTLS(ctx, conn, addr, insecure, cfg.GetString("proxy.clearance.browser", "chrome136"))
		}
	default:
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			if req.URL.Scheme == "http" {
				return parsed, nil
			}
			return nil, nil
		}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return directDialer().DialContext(ctx, network, addr)
		}
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, dialErr := dialTLSThroughProxy(ctx, parsed, addr, insecure)
			if dialErr != nil {
				return nil, dialErr
			}
			return wrapUTLS(ctx, conn, addr, insecure, cfg.GetString("proxy.clearance.browser", "chrome136"))
		}
	}

	return transport, nil
}

func directDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
}

func socksDialContext(proxyURL *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
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

func dialTLSThroughProxy(ctx context.Context, proxyURL *url.URL, targetAddr string, insecure bool) (net.Conn, error) {
	proxyConn, err := dialProxy(ctx, proxyURL, insecure)
	if err != nil {
		return nil, err
	}

	connectReq := (&http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}).WithContext(ctx)
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		connectReq.SetBasicAuth(proxyURL.User.Username(), password)
	}
	if err := connectReq.Write(proxyConn); err != nil {
		proxyConn.Close()
		return nil, err
	}

	reader := bufio.NewReader(proxyConn)
	resp, err := http.ReadResponse(reader, connectReq)
	if err != nil {
		proxyConn.Close()
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		proxyConn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return &bufferedConn{Conn: proxyConn, reader: reader}, nil
}

func dialProxy(ctx context.Context, proxyURL *url.URL, insecure bool) (net.Conn, error) {
	rawConn, err := directDialer().DialContext(ctx, "tcp", canonicalAddr(proxyURL))
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme != "https" {
		return rawConn, nil
	}

	tlsConn := tls.Client(rawConn, &tls.Config{
		InsecureSkipVerify: insecure, //nolint:gosec
		ServerName:         proxyURL.Hostname(),
		NextProtos:         []string{"http/1.1"},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func canonicalAddr(value *url.URL) string {
	port := value.Port()
	switch {
	case port != "":
		return value.Host
	case value.Scheme == "https":
		return net.JoinHostPort(value.Hostname(), "443")
	default:
		return net.JoinHostPort(value.Hostname(), "80")
	}
}

func wrapUTLS(ctx context.Context, rawConn net.Conn, addr string, insecure bool, browser string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	cfg := &utls.Config{
		InsecureSkipVerify: insecure, //nolint:gosec
		ServerName:         host,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	uconn := utls.UClient(rawConn, cfg, clientHelloID(browser))
	if err := uconn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	return uconn, nil
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

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		return c.Conn.Read(p)
	}
	if c.reader.Buffered() == 0 {
		c.reader = nil
		return c.Conn.Read(p)
	}
	return c.reader.Read(p)
}

func validateProxyScheme(proxyURL *url.URL) error {
	switch proxyURL.Scheme {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return errors.New("unsupported proxy scheme")
	}
}
