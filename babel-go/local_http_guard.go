package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
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
	if origins, present := r.Header["Origin"]; present {
		if len(origins) != 1 || !sameLocalOrigin(origins[0], host, port, r.TLS != nil) {
			reject(http.StatusForbidden, "拒绝跨源或不透明来源请求")
			return
		}
	}
	if sites, present := r.Header["Sec-Fetch-Site"]; present {
		if len(sites) != 1 || sites[0] != "same-origin" && sites[0] != "none" {
			reject(http.StatusForbidden, "拒绝跨站浏览器请求")
			return
		}
	}
	jsonRoute := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/messages"
	if jsonRoute {
		if r.ContentLength > maxLocalJSONBodyBytes {
			reject(http.StatusRequestEntityTooLarge, "请求正文超过 8 MiB 上限")
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
			r.Body = http.MaxBytesReader(w, r.Body, maxLocalJSONBodyBytes)
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

// Set headers at the actual commit point so an upstream response cannot replace
// the local browser boundary with its own HTML policy or sniffing behavior.
type localResponseWriter struct {
	http.ResponseWriter
	page, wroteHeader bool
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
