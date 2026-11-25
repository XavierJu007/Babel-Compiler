package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const maxLocalJSONBodyBytes = 8 * 1024 * 1024

const localPageCSP = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
const localDataCSP = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; sandbox"

// Babel has no network-user authentication. Keep its management and proxy
// authority on an explicit loopback listener rather than exposing it on a LAN.
func loopbackListenAddress(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("服务地址须为明确的环回地址和端口，例如 127.0.0.1:8080: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1" // Do not let a hostname resolver choose a nonlocal bind.
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", "", fmt.Errorf("本机服务仅允许 127.0.0.0/8、[::1] 或 localhost 环回监听")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", "", fmt.Errorf("服务端口须为 0 至 65535 的整数")
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return "", "", fmt.Errorf("服务端口须为 0 至 65535 的整数")
	}
	return ip.String(), strconv.Itoa(number), nil
}

func newServerHandler(listenAddr string) (http.Handler, error) {
	host, port, err := loopbackListenAddress(listenAddr)
	if err != nil {
		return nil, err
	}
	return &localHTTPGuard{next: newServerMux(), host: host, port: port}, nil
}

type localHTTPGuard struct {
	next       http.Handler
	host, port string
}

func (g *localHTTPGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	protected := &localResponseWriter{ResponseWriter: w, page: r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead)}
	reject := func(status int, detail string) {
		protected.page = false
		protected.Header().Set("Content-Type", "application/json; charset=utf-8")
		protected.WriteHeader(status)
		_ = json.NewEncoder(protected).Encode(map[string]string{"detail": detail})
	}
	host, port, ok := localRequestAuthority(r.Host)
	if !ok || port != g.port || host != "localhost" && host != g.host {
		reject(http.StatusForbidden, "拒绝与本机监听地址不匹配的 Host")
		return
	}
	isProxyRoute := strings.HasPrefix(r.URL.Path, "/v1/")
	allowedProxyOrigin := ""
	if origins, present := r.Header["Origin"]; present {
		if len(origins) != 1 {
			reject(http.StatusForbidden, "拒绝跨源或不透明来源请求")
			return
		}
		if sameLocalOrigin(origins[0], host, port, r.TLS != nil) {
			// Same-origin browser request is always allowed.
		} else if isProxyRoute && isAllowedProxyCORSOrigin(origins[0]) {
			allowedProxyOrigin = origins[0]
		} else {
			reject(http.StatusForbidden, "拒绝跨源或不透明来源请求")
			return
		}
	}
	if sites, present := r.Header["Sec-Fetch-Site"]; present {
		if len(sites) != 1 || (sites[0] != "same-origin" && sites[0] != "none" && allowedProxyOrigin == "") {
			reject(http.StatusForbidden, "拒绝跨站浏览器请求")
			return
		}
	}
	if allowedProxyOrigin != "" {
		protected.corsOrigin = allowedProxyOrigin
		if r.Method == http.MethodOptions {
			protected.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			reqHeaders := sanitizeCORSRequestHeaders(r.Header.Get("Access-Control-Request-Headers"))
			if reqHeaders == "" {
				reqHeaders = "Authorization, Content-Type, x-api-key, api-key, anthropic-version, anthropic-beta, OpenAI-Organization, OpenAI-Beta, X-Babel-Profile, X-Babel-Domains, X-Babel-Validation-Mode, X-Babel-Reader-Profile, X-Babel-Audit-Profile, X-Babel-Bypass, X-Babel-Compile-Budget, X-Babel-Skip-Cache-Control, X-Babel-Compress-Developer, X-Upstream-BaseURL"
			}
			protected.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			protected.Header().Set("Access-Control-Max-Age", "600")
			protected.WriteHeader(http.StatusNoContent)
			return
		}
	}
	apiRoute := strings.HasPrefix(r.URL.Path, "/api/")
	proxyJSONRoute := r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/messages"
	if apiRoute || proxyJSONRoute {
		maxBytes := int64(maxLocalJSONBodyBytes)
		if proxyJSONRoute {
			maxBytes = proxyMaxBodyLimit()
		}
		if r.ContentLength > maxBytes {
			reject(http.StatusRequestEntityTooLarge, fmt.Sprintf("请求正文超过 %d 字节上限", maxBytes))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || contentType != "application/json" {
				reject(http.StatusUnsupportedMediaType, "此接口仅接受 application/json 请求")
				return
			}
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
	}
	g.next.ServeHTTP(protected, r)
	if !protected.wroteHeader {
		protected.WriteHeader(http.StatusOK)
	}
}

func localRequestAuthority(authority string) (string, string, bool) {
	u, err := url.Parse("http://" + authority)
	if err != nil || u.User != nil || u.Host == "" || u.Host != authority || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", "", false
	}
	return host, strconv.Itoa(number), true
}

func sameLocalOrigin(origin, requestHost, requestPort string, tls bool) bool {
	u, err := url.Parse(origin)
	wantScheme := "http"
	if tls {
		wantScheme = "https"
	}
	if err != nil || u.Scheme != wantScheme || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host, port, ok := localRequestAuthority(u.Host)
	if tls && u.Port() == "" {
		port = "443"
	}
	return ok && host == requestHost && port == requestPort
}

// isAllowedProxyCORSOrigin checks explicit opt-in origins configured via
// BABEL_PROXY_ALLOWED_ORIGINS (comma-separated). Wildcards ("*") and opaque
// "null" origins are always rejected so the local boundary stays explicit.
func isAllowedProxyCORSOrigin(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" || origin == "*" || strings.EqualFold(origin, "null") {
		return false
	}
	raw := strings.TrimSpace(os.Getenv("BABEL_PROXY_ALLOWED_ORIGINS"))
	if raw == "" {
		return false
	}
	for _, entry := range strings.Split(raw, ",") {
		candidate := strings.TrimSpace(entry)
		if candidate == "" || candidate == "*" || strings.EqualFold(candidate, "null") {
			continue
		}
		if strings.EqualFold(candidate, origin) {
			return true
		}
	}
	return false
}

func sanitizeCORSRequestHeaders(raw string) string {
	var safe []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		valid := true
		for i := 0; i < len(name); i++ {
			c := name[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				valid = false
				break
			}
		}
		if valid {
			safe = append(safe, name)
		}
	}
	return strings.Join(safe, ", ")
}

// Set headers at the actual commit point so an upstream response cannot replace
// the local browser boundary with its own HTML policy, sniffing, or duplicate CORS headers.
type localResponseWriter struct {
	http.ResponseWriter
	page, wroteHeader bool
	corsOrigin        string
}

func headerContainsToken(values []string, token string) bool {
	for _, val := range values {
		for _, part := range strings.Split(val, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (w *localResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	policy := localDataCSP
	if w.page {
		policy = localPageCSP
	}
	w.Header().Set("Content-Security-Policy", policy)
	if w.corsOrigin != "" {
		// Replace any upstream Access-Control-* headers so browsers never see
		// duplicate Access-Control-Allow-Origin values (which Fetch rejects).
		w.Header().Set("Access-Control-Allow-Origin", w.corsOrigin)
		w.Header().Del("Access-Control-Allow-Credentials")
		w.Header().Set("Access-Control-Expose-Headers", "X-Babel-Status, X-Babel-Compile-Ms")
		if !headerContainsToken(w.Header().Values("Vary"), "Origin") {
			w.Header().Add("Vary", "Origin")
		}
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *localResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *localResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *localResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
