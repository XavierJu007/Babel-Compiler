package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var proxyStreamBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

type proxyRuntimeMetrics struct {
	requestsTotal                   atomic.Uint64
	adoptedTotal                    atomic.Uint64
	adoptedOrigTokensTotal          atomic.Uint64
	adoptedSavedTokensTotal         atomic.Uint64
	fastPathHitTotal                atomic.Uint64
	passthroughTotal                atomic.Uint64
	bypassTotal                     atomic.Uint64
	dryRunTotal                     atomic.Uint64
	dryRunWouldAdoptTotal           atomic.Uint64
	dryRunOrigTokensTotal           atomic.Uint64
	dryRunPotentialSavedTokensTotal atomic.Uint64
	validationRejectedTotal         atomic.Uint64
	oversizeTotal                   atomic.Uint64
	budgetTimeoutTotal              atomic.Uint64
	upstreamErrorTotal              atomic.Uint64
	redactAsyncTotal                atomic.Uint64
	redactSyncFallbackTotal         atomic.Uint64
	redactMatchedTotal              atomic.Uint64
}

var proxyMetrics proxyRuntimeMetrics

func proxyMetricsSnapshot() map[string]uint64 {
	return map[string]uint64{
		"requests_total":                      proxyMetrics.requestsTotal.Load(),
		"adopted_total":                       proxyMetrics.adoptedTotal.Load(),
		"adopted_orig_tokens_total":           proxyMetrics.adoptedOrigTokensTotal.Load(),
		"adopted_saved_tokens_total":          proxyMetrics.adoptedSavedTokensTotal.Load(),
		"fast_path_hit_total":                 proxyMetrics.fastPathHitTotal.Load(),
		"passthrough_total":                   proxyMetrics.passthroughTotal.Load(),
		"bypass_total":                        proxyMetrics.bypassTotal.Load(),
		"dry_run_total":                       proxyMetrics.dryRunTotal.Load(),
		"dry_run_would_adopt_total":           proxyMetrics.dryRunWouldAdoptTotal.Load(),
		"dry_run_orig_tokens_total":           proxyMetrics.dryRunOrigTokensTotal.Load(),
		"dry_run_potential_saved_tokens_total": proxyMetrics.dryRunPotentialSavedTokensTotal.Load(),
		"validation_rejected_total":           proxyMetrics.validationRejectedTotal.Load(),
		"oversize_total":                      proxyMetrics.oversizeTotal.Load(),
		"budget_timeout_total":                proxyMetrics.budgetTimeoutTotal.Load(),
		"upstream_error_total":                proxyMetrics.upstreamErrorTotal.Load(),
		"redact_async_total":                  proxyMetrics.redactAsyncTotal.Load(),
		"redact_sync_fallback_total":          proxyMetrics.redactSyncFallbackTotal.Load(),
		"redact_matched_total":                proxyMetrics.redactMatchedTotal.Load(),
	}
}

func setDiagnosticHeader(h http.Header, key, val string) {
	if val == "" || h.Get(key) != "" {
		return
	}
	h.Set(key, val)
}

const maxSSELineBytes = 64 * 1024

// sseUsageSampler incrementally scans SSE lines without truncating JSON across
// Read() chunk boundaries and keeps memory strictly bounded to one line.
type sseUsageSampler struct {
	lineBuf  []byte
	overflow bool
	done     bool
	tokens   int
	parse    func(line []byte) (int, bool)
}

func (s *sseUsageSampler) Write(chunk []byte) {
	if s.done || len(chunk) == 0 {
		return
	}
	for len(chunk) > 0 && !s.done {
		nl := bytes.IndexByte(chunk, '\n')
		if nl < 0 {
			if !s.overflow {
				if len(s.lineBuf)+len(chunk) > maxSSELineBytes {
					s.lineBuf = s.lineBuf[:0]
					s.overflow = true
				} else {
					s.lineBuf = append(s.lineBuf, chunk...)
				}
			}
			return
		}
		part := chunk[:nl]
		chunk = chunk[nl+1:]
		if s.overflow {
			s.overflow = false
			s.lineBuf = s.lineBuf[:0]
			continue
		}
		var line []byte
		if len(s.lineBuf) > 0 {
			if len(s.lineBuf)+len(part) <= maxSSELineBytes {
				s.lineBuf = append(s.lineBuf, part...)
				line = s.lineBuf
			}
		} else if len(part) <= maxSSELineBytes {
			line = part
		}
		if len(line) > 0 {
			if tokens, stop := s.parse(line); tokens > 0 {
				s.tokens = tokens
				if stop {
					s.done = true
				}
			} else if stop {
				s.done = true
			}
		}
		s.lineBuf = s.lineBuf[:0]
	}
}

func (s *sseUsageSampler) Flush() {
	if s.done || s.overflow || len(s.lineBuf) == 0 {
		return
	}
	if tokens, _ := s.parse(s.lineBuf); tokens > 0 {
		s.tokens = tokens
	}
	s.lineBuf = s.lineBuf[:0]
}

func (s *sseUsageSampler) Tokens() int {
	s.Flush()
	return s.tokens
}

func parseSSEDataPayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	return payload
}

func newOpenAISSEUsageSampler() *sseUsageSampler {
	return &sseUsageSampler{
		parse: func(line []byte) (int, bool) {
			if !bytes.Contains(line, []byte("prompt_tokens")) {
				return 0, false
			}
			payload := parseSSEDataPayload(line)
			if payload == nil {
				return 0, false
			}
			var chunk struct {
				Usage struct {
					PromptTokens int `json:"prompt_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(payload, &chunk) == nil && chunk.Usage.PromptTokens > 0 {
				return chunk.Usage.PromptTokens, false
			}
			return 0, false
		},
	}
}

func newOpenAIResponsesSSEUsageSampler() *sseUsageSampler {
	return &sseUsageSampler{
		parse: func(line []byte) (int, bool) {
			if !bytes.Contains(line, []byte("input_tokens")) {
				return 0, false
			}
			payload := parseSSEDataPayload(line)
			if payload == nil {
				return 0, false
			}
			var evt struct {
				Type     string `json:"type"`
				Response struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"response"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(payload, &evt) == nil {
				if evt.Response.Usage.InputTokens > 0 {
					return evt.Response.Usage.InputTokens, evt.Type == "response.completed"
				}
				if evt.Usage.InputTokens > 0 {
					return evt.Usage.InputTokens, evt.Type == "response.completed"
				}
			}
			return 0, false
		},
	}
}

func newAnthropicSSEUsageSampler() *sseUsageSampler {
	return &sseUsageSampler{
		parse: func(line []byte) (int, bool) {
			if !bytes.Contains(line, []byte("message_start")) {
				return 0, false
			}
			payload := parseSSEDataPayload(line)
			if payload == nil {
				return 0, false
			}
			var evt struct {
				Type    string `json:"type"`
				Message struct {
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(payload, &evt) == nil && evt.Type == "message_start" {
				total := evt.Message.Usage.InputTokens + evt.Message.Usage.CacheCreationInputTokens + evt.Message.Usage.CacheReadInputTokens
				return total, true
			}
			return 0, false
		},
	}
}

// RFC 9110 §7.6.1 hop-by-hop headers that a proxy must never forward.
var staticHopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func connectionHeaderTokens(h http.Header) map[string]bool {
	var tokens map[string]bool
	for _, val := range h.Values("Connection") {
		for _, part := range strings.Split(val, ",") {
			if name := strings.TrimSpace(part); name != "" {
				if tokens == nil {
					tokens = make(map[string]bool)
				}
				tokens[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	return tokens
}

func isHopByHopHeader(canonicalKey string, connTokens map[string]bool) bool {
	return staticHopByHopHeaders[canonicalKey] || connTokens[canonicalKey]
}

func isBabelControlHeader(key string) bool {
	lower := strings.ToLower(key)
	return strings.HasPrefix(lower, "x-babel-") || lower == "x-upstream-baseurl"
}

// copyProxyRequestHeaders implements blacklist-based transparent forwarding:
// client headers (OpenAI-Organization, OpenAI-Beta, User-Agent, Accept,
// traceparent, custom routing headers, etc.) are preserved while hop-by-hop
// headers, framing/encoding headers, local browser Origin/Sec-Fetch-* headers,
// and Babel control headers are stripped.
func copyProxyRequestHeaders(dst, src http.Header) {
	connTokens := connectionHeaderTokens(src)
	for k, vals := range src {
		ck := http.CanonicalHeaderKey(k)
		if isHopByHopHeader(ck, connTokens) || isBabelControlHeader(ck) {
			continue
		}
		if ck == "Host" || ck == "Content-Length" || ck == "Accept-Encoding" || ck == "Origin" || strings.HasPrefix(ck, "Sec-Fetch-") {
			continue
		}
		for _, v := range vals {
			dst.Add(ck, v)
		}
	}
}

// copyProxyResponseHeaders forwards upstream response headers while stripping
// hop-by-hop headers and Content-Length (framing is managed by net/http).
func copyProxyResponseHeaders(dst, src http.Header) {
	connTokens := connectionHeaderTokens(src)
	for k, vals := range src {
		ck := http.CanonicalHeaderKey(k)
		if isHopByHopHeader(ck, connTokens) || ck == "Content-Length" {
			continue
		}
		for _, v := range vals {
			dst.Add(ck, v)
		}
	}
}

// mergeRequestQuery preserves any query parameters configured on targetURL and
// appends query parameters from the incoming proxy request (e.g. Azure api-version).
func mergeRequestQuery(targetURL, rawQuery string) string {
	if rawQuery == "" {
		return targetURL
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	if u.RawQuery == "" {
		u.RawQuery = rawQuery
	} else {
		u.RawQuery = u.RawQuery + "&" + rawQuery
	}
	return u.String()
}

func writeProxyError(w http.ResponseWriter, protocol string, status int, errType, message string) {
	if errType == "" {
		switch status {
		case http.StatusUnauthorized:
			errType = "authentication_error"
		case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
			errType = "invalid_request_error"
		default:
			errType = "api_error"
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if protocol == "anthropic" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    errType,
				"message": message,
			},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    strconv.Itoa(status),
		},
	})
}

func isEventStreamResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "text/event-stream")
}

// proxyMaxBodyLimit returns the max inbound body size allowed on /v1/* routes.
// Defaults to maxLocalJSONBodyBytes (8 MiB) to preserve the local boundary unless
// explicitly raised via BABEL_PROXY_MAX_BODY_BYTES for large multimodal payloads.
func proxyMaxBodyLimit() int64 {
	raw := strings.TrimSpace(os.Getenv("BABEL_PROXY_MAX_BODY_BYTES"))
	if raw == "" {
		return maxLocalJSONBodyBytes
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return n
	}
	return maxLocalJSONBodyBytes
}

// proxyMaxCompileBodyLimit caps how large a request body Babel will buffer and
// JSON-parse for compression; larger bodies (within proxyMaxBodyLimit) stream
// straight through to upstream untouched.
func proxyMaxCompileBodyLimit() int64 {
	raw := strings.TrimSpace(os.Getenv("BABEL_PROXY_MAX_COMPILE_BYTES"))
	if raw == "" {
		return maxLocalJSONBodyBytes
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return n
	}
	return maxLocalJSONBodyBytes
}

// buildV1PassthroughURL maps an arbitrary /v1/... request path onto upstreamBase
// without duplicating /v1 or appending onto a configured chat/messages endpoint.
func buildV1PassthroughURL(upstreamBase, reqPath, rawQuery string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(upstreamBase))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("上游地址不是有效的 HTTP(S) URL")
	}
	basePath := strings.TrimRight(u.Path, "/")
	lowerBase := strings.ToLower(basePath)
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages"} {
		if strings.HasSuffix(lowerBase, suffix) {
			basePath = basePath[:len(basePath)-len(suffix)]
			break
		}
	}
	relPath := reqPath
	if isAPIBasePath(basePath) && (strings.HasPrefix(strings.ToLower(relPath), "/v1/") || strings.EqualFold(relPath, "/v1")) {
		relPath = relPath[3:]
	}
	if !strings.HasPrefix(relPath, "/") && relPath != "" {
		relPath = "/" + relPath
	}
	u.Path = basePath + relPath
	u.RawPath = ""
	return mergeRequestQuery(u.String(), rawQuery), nil
}

func inferOpenAIBaseFromCompilerURL(raw string) string {
	resolved := compilerRequestURL(raw)
	u, err := url.Parse(resolved)
	if err != nil || u.Host == "" {
		return ""
	}
	lowerPath := strings.ToLower(strings.TrimRight(u.Path, "/"))
	if strings.HasSuffix(lowerPath, "/v1/chat/completions") {
		u.Path = u.Path[:len(strings.TrimRight(u.Path, "/"))-len("/chat/completions")]
		u.RawPath = ""
		return u.String()
	}
	return ""
}

func streamPassthroughRequest(w http.ResponseWriter, r *http.Request, protocol, targetURL string, extraHeaders func(http.Header)) {
	streamPassthroughRequestWithStatus(w, r, protocol, targetURL, "passthrough", extraHeaders)
}

func streamPassthroughRequestWithStatus(w http.ResponseWriter, r *http.Request, protocol, targetURL, babelStatus string, extraHeaders func(http.Header)) {
	streamPassthroughBodyWithStatus(w, r, protocol, targetURL, babelStatus, r.Body, r.ContentLength, extraHeaders)
}

func streamPassthroughBodyWithStatus(w http.ResponseWriter, r *http.Request, protocol, targetURL, babelStatus string, body io.Reader, contentLength int64, extraHeaders func(http.Header)) {
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, body)
	if err != nil {
		writeProxyError(w, protocol, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	if contentLength >= 0 {
		proxyReq.ContentLength = contentLength
	}
	copyProxyRequestHeaders(proxyReq.Header, r.Header)
	if extraHeaders != nil {
		extraHeaders(proxyReq.Header)
	}

	resp, err := newProxyForwardHTTPClient().Do(proxyReq)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProxyError(w, protocol, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("请求正文超过 %d 字节上限", maxErr.Limit))
			return
		}
		proxyMetrics.upstreamErrorTotal.Add(1)
		writeProxyError(w, protocol, http.StatusBadGateway, "api_error", fmt.Sprintf("上游转发失败: %v", err))
		return
	}
	defer resp.Body.Close()
	if rejectProxyRedirectForProtocol(w, protocol, resp) {
		proxyMetrics.upstreamErrorTotal.Add(1)
		return
	}

	copyProxyResponseHeaders(w.Header(), resp.Header)
	setDiagnosticHeader(w.Header(), "X-Babel-Status", babelStatus)
	w.WriteHeader(resp.StatusCode)

	if isEventStreamResponse(resp) {
		flusher, canFlush := w.(http.Flusher)
		bufPtr := proxyStreamBufPool.Get().(*[]byte)
		defer proxyStreamBufPool.Put(bufPtr)
		buf := *bufPtr
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return
				}
				if canFlush {
					flusher.Flush()
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) && r.Context().Err() == nil {
					panic(http.ErrAbortHandler)
				}
				return
			}
		}
	}

	if _, copyErr := io.Copy(w, resp.Body); copyErr != nil && r.Context().Err() == nil {
		panic(http.ErrAbortHandler)
	}
}

// v1PassthroughProxyHandler transparently forwards non-compressed /v1/* endpoints
// (e.g. GET /v1/models, POST /v1/responses, POST /v1/messages/count_tokens,
// POST /v1/embeddings) so standard LLM SDKs and IDEs work out of the box.
func v1PassthroughProxyHandler(w http.ResponseWriter, r *http.Request) {
	proxyMetrics.requestsTotal.Add(1)
	protocol := "openai"
	if strings.HasPrefix(r.URL.Path, "/v1/messages") || r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != "" {
		protocol = "anthropic"
	}

	callerAuth := strings.TrimSpace(r.Header.Get("Authorization"))
	callerAPIKey := strings.TrimSpace(r.Header.Get("x-api-key"))
	callerAzureKey := strings.TrimSpace(r.Header.Get("api-key"))
	if callerAuth == "" && callerAPIKey == "" && callerAzureKey == "" {
		writeProxyError(w, protocol, http.StatusUnauthorized, "authentication_error", "代理请求必须携带最终服务的认证头（Authorization / x-api-key / api-key）；不会使用已保存的编译密钥")
		return
	}

	cfg := loadConfig()
	p, err := resolveProxyProfile(r, cfg)
	if err != nil {
		writeProxyError(w, protocol, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	upstreamBase := strings.TrimSpace(r.Header.Get("X-Upstream-BaseURL"))
	if upstreamBase == "" {
		upstreamBase = strings.TrimSpace(p.UpstreamURL)
	}
	if upstreamBase == "" && protocol == "openai" {
		upstreamBase = inferOpenAIBaseFromCompilerURL(p.BaseURL)
	}
	if upstreamBase == "" {
		writeProxyError(w, protocol, http.StatusBadRequest, "invalid_request_error", "该代理路径需要配置 upstream_url 或携带 X-Upstream-BaseURL")
		return
	}

	targetURL, err := buildV1PassthroughURL(upstreamBase, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		writeProxyError(w, protocol, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("上游地址无效: %v", err))
		return
	}
	if err := validateOutboundURL(targetURL); err != nil {
		writeProxyError(w, protocol, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("上游地址无效: %v", err))
		return
	}

	var extra func(http.Header)
	if protocol == "anthropic" {
		extra = func(h http.Header) {
			if h.Get("anthropic-version") == "" {
				h.Set("anthropic-version", "2023-06-01")
			}
		}
	}
	proxyMetrics.passthroughTotal.Add(1)
	streamPassthroughRequestWithStatus(w, r, protocol, targetURL, "passthrough", extra)
}
