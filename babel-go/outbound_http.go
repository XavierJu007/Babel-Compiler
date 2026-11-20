package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func isOutboundLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Check the actual destination, without resolving names or rewriting paths.
// A hostname that happens to resolve to a private address is not a safe HTTP
// exception: its next DNS answer could route the same plaintext off this host.
func validateOutboundURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("请求地址不是有效的 HTTP(S) URL")
	}
	return validateOutboundDestination(u)
}

func validateOutboundDestination(u *url.URL) error {
	if u == nil || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("请求地址须为不含用户信息或片段的完整 HTTP(S) URL")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return fmt.Errorf("请求地址端口须为 1 至 65535 的整数")
		}
	}
	if u.Scheme == "http" && !isOutboundLoopback(u.Hostname()) {
		return fmt.Errorf("远程请求必须使用 HTTPS；仅本机环回地址 127.0.0.0/8、[::1] 或 localhost 允许 HTTP")
	}
	return nil
}

type outboundTransport struct {
	base *http.Transport
}

func (t *outboundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validateOutboundDestination(req.URL); err != nil {
		// RoundTripper owns the request body even when rejecting before dialing.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (t *outboundTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

func newSecureOutboundTransport() *outboundTransport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &outboundTransport{base: &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if isOutboundLoopback(req.URL.Hostname()) {
				return nil, nil
			}
			// A normal CONNECT proxy remains supported. HTTPS still verifies the
			// destination's certificate inside that tunnel before sending data.
			return http.ProxyFromEnvironment(req)
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if strings.EqualFold(host, "localhost") {
				// Keep the HTTP exception on this machine even if hosts/DNS is
				// changed. Explicit [::1] remains available for IPv6-only services.
				address = net.JoinHostPort("127.0.0.1", port)
			}
			return dialer.DialContext(ctx, network, address)
		},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Standard chain, hostname and validity verification stays enabled.
			// RootCAs is nil to use the operating system's trusted authorities.
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}

// Reuse connections while keeping our security policy independent of mutable
// package-wide http.DefaultTransport settings. There is no insecure TLS mode.
var sharedOutboundTransport = newSecureOutboundTransport()

// A configured endpoint is the complete destination. Replaying prompts or keys
// at a provider-supplied Location crosses that boundary, even if Go strips some
// authentication headers. Callers handle the original 3xx response explicitly.
func newOutboundHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: sharedOutboundTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
