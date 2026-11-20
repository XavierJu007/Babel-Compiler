package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundURLRequiresTLSExceptExplicitLoopback(t *testing.T) {
	for _, raw := range []string{
		"https://provider.example/v1/chat/completions", "https://10.0.0.5/custom?route=a",
		"http://127.0.0.1:11434/v1", "http://127.0.0.2:8080/custom",
		"http://[::1]:8080/v1", "http://localhost:11434/v1", "http://LOCALHOST:11434/v1",
	} {
		if err := validateOutboundURL(raw); err != nil {
			t.Errorf("valid destination %q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://provider.example/v1", "http://10.0.0.5/v1", "http://192.168.1.2/v1",
		"http://0.0.0.0/v1", "http://[::]/v1", "http://[fe80::1]/v1",
		"http://127.0.0.1.attacker.example/v1", "http://localhost.attacker.example/v1",
		"http://localhost./v1", "http://2130706433/v1", "http://127.1/v1",
		"ftp://provider.example/v1", "/v1", "https:provider.example",
		"https://user:secret@provider.example/v1", "https://provider.example/v1#fragment",
		"https://provider.example:0/v1", "https://provider.example:65536/v1",
	} {
		if err := validateOutboundURL(raw); err == nil {
			t.Errorf("unsafe or ambiguous destination %q accepted", raw)
		}
	}
}

type outboundBodyCloseProbe struct {
	io.Reader
	closed bool
}

func (p *outboundBodyCloseProbe) Close() error { p.closed = true; return nil }

func TestOutboundPlaintextRejectsBeforeAnyDialAndClosesBody(t *testing.T) {
	transport := newSecureOutboundTransport()
	defer transport.CloseIdleConnections()
	var dialed atomic.Int32
	transport.base.DialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed.Add(1)
		return nil, fmt.Errorf("fixture dial must not be reached")
	}
	body := &outboundBodyCloseProbe{Reader: strings.NewReader("private prompt fixture")}
	r, err := http.NewRequest(http.MethodPost, "http://198.51.100.1/v1", body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer private-fixture-key")
	response, err := transport.RoundTrip(r)
	if response != nil || err == nil || !strings.Contains(err.Error(), "HTTPS") || dialed.Load() != 0 || !body.closed {
		t.Fatalf("plaintext request was not rejected before network I/O: response=%v err=%v dialed=%d closed=%v", response != nil, err, dialed.Load(), body.closed)
	}
}

func TestCompileAuditReaderAndProbeRejectRemotePlaintext(t *testing.T) {
	profile := Profile{Name: "fixture", APIKey: "fixture-key", BaseURL: "http://198.51.100.1/v1", Model: "fixture"}
	transport := newSecureOutboundTransport()
	defer transport.CloseIdleConnections()
	var dialed atomic.Int32
	transport.base.DialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed.Add(1)
		return nil, fmt.Errorf("fixture dial must not be reached")
	}
	previous := sharedOutboundTransport
	sharedOutboundTransport = transport
	t.Cleanup(func() { sharedOutboundTransport = previous })
	if _, err := callLLM(profile, "compiler policy", "private fixture source", 0); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("compiler accepted plaintext destination: %v", err)
	}
	const source = "记录处理结果。"
	if result := runValidation(profile, source, source, nil); result.Status != "error" {
		t.Fatalf("audit accepted insecure transport: status=%s", result.Status)
	}
	if result := runBlindValidationContext(context.Background(), profile, profile, source, source, nil); result.Status != "error" || len(result.Stages) != 3 || result.Stages[1].Status != "error" || result.Stages[2].Status != "not_run" {
		t.Fatalf("reader transport failure advanced to comparison: %+v", result)
	}
	if result := probeProfile(profile); result.OK || !strings.Contains(result.Detail, "HTTPS") {
		t.Fatalf("probe accepted plaintext destination: %+v", result)
	}
	if dialed.Load() != 0 {
		t.Fatal("a model operation dialed an insecure remote destination")
	}
}

func TestProxyRetainsOriginalWhenCompilerTransportIsRejected(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			isolateDomainIntegration(t)
			isolateProxyState(t)
			const original = "Please summarize this document for the engineering team, preserving its conditions, exceptions and operational requirements."
			var forwarded atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded.Add(1)
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got["messages"].([]any)[0].(map[string]any)["content"] != original {
					t.Error("transport rejection changed the original prompt")
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()
			if err := writeConfigFile(Config{ActiveProfile: "fixture", Profiles: map[string]Profile{
				"fixture": {APIKey: "compiler-key", BaseURL: "http://198.51.100.1/v1", Model: "fixture", UpstreamURL: upstream.URL},
			}}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"model": "consumer", "messages": []any{map[string]any{"role": "user", "content": original}}})
			request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(raw))
			request.Header.Set("Authorization", "Bearer consumer-fixture")
			response := httptest.NewRecorder()
			newServerMux().ServeHTTP(response, request)
			if response.Code != http.StatusOK || forwarded.Load() != 1 {
				t.Fatalf("compiler transport rejection blocked the valid final destination: status=%d forwarded=%d", response.Code, forwarded.Load())
			}
		})
	}
}

func TestOutboundLocalhostUsesLoopbackDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "LOCALHOST:") {
			t.Error("dialing rewrote the request authority")
		}
		_, _ = w.Write([]byte("local fixture"))
	}))
	defer server.Close()
	transport := newSecureOutboundTransport()
	defer transport.CloseIdleConnections()
	endpoint := strings.Replace(server.URL, "127.0.0.1", "LOCALHOST", 1)
	request, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	if proxy, err := transport.base.Proxy(request); err != nil || proxy != nil {
		t.Fatal("localhost was handed to an environment proxy")
	}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "local fixture" {
		t.Fatalf("fixed localhost connection failed: %q %v", body, err)
	}
}

func TestOutboundEnvironmentProxyPolicy(t *testing.T) {
	// net/http caches proxy environment values, so exercise the actual production
	// selector in a fresh process instead of relying on other tests' call order.
	if os.Getenv("BABEL_PROXY_ENV_FIXTURE") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOutboundEnvironmentProxyPolicy$")
		for _, setting := range os.Environ() {
			name, _, _ := strings.Cut(setting, "=")
			switch strings.ToUpper(name) {
			case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "REQUEST_METHOD", "BABEL_PROXY_ENV_FIXTURE":
				continue
			}
			command.Env = append(command.Env, setting)
		}
		command.Env = append(command.Env, "BABEL_PROXY_ENV_FIXTURE=1", "HTTPS_PROXY=http://127.0.0.1:31280", "HTTP_PROXY=http://127.0.0.1:31280", "NO_PROXY=")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("proxy environment fixture failed: %v\n%s", err, output)
		}
		return
	}
	transport := newSecureOutboundTransport()
	defer transport.CloseIdleConnections()
	for _, endpoint := range []string{"http://localhost:11434", "http://127.0.0.2:8080", "http://[::1]:8080", "https://127.0.0.1:8443"} {
		r, _ := http.NewRequest(http.MethodGet, endpoint, nil)
		if proxy, err := transport.base.Proxy(r); err != nil || proxy != nil {
			t.Fatalf("loopback escaped through an environment proxy: %s", endpoint)
		}
	}
	r, _ := http.NewRequest(http.MethodGet, "https://provider.example/v1", nil)
	proxy, err := transport.base.Proxy(r)
	if err != nil || proxy == nil || proxy.String() != "http://127.0.0.1:31280" {
		t.Fatal("normal HTTPS proxy configuration was silently disabled")
	}
}
