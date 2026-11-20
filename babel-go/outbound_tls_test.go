package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These fixtures trust their private CA only on a fresh transport owned by the
// test. They never change the shared client, system trust store or TLS policy.
type outboundTestCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newOutboundTestCA(t *testing.T) outboundTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Babel local transport test CA"},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return outboundTestCA{cert: cert, key: key, pool: pool}
}

func (ca outboundTestCA) leaf(t *testing.T, host string, before, after time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    before,
		NotAfter:     after,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}
}

func newOutboundTLSServer(t *testing.T, cert tls.Certificate, min, max uint16, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: min, MaxVersion: max}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func outboundTLSClient(t *testing.T, roots *x509.CertPool) (*http.Client, *outboundTransport) {
	t.Helper()
	transport := newSecureOutboundTransport()
	transport.base.TLSClientConfig = transport.base.TLSClientConfig.Clone()
	transport.base.TLSClientConfig.RootCAs = roots
	t.Cleanup(transport.CloseIdleConnections)
	client := newOutboundHTTPClient(5 * time.Second)
	client.Transport = transport
	return client, transport
}

func outboundPrivateRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"prompt":"private-babel-transport-fixture-prompt"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer private-babel-transport-fixture-key")
	req.Header.Set("x-api-key", "private-babel-transport-fixture-key")
	req.Close = true
	return req
}

func TestSecureOutboundRejectsInvalidCertificatesBeforeSendingSecrets(t *testing.T) {
	now := time.Now()
	for _, scenario := range []struct {
		name     string
		host     string
		trusted  bool
		before   time.Time
		after    time.Time
		checkErr func(error) bool
	}{
		{
			name: "unknown CA", host: "127.0.0.1", before: now.Add(-time.Hour), after: now.Add(time.Hour),
			checkErr: func(err error) bool { var target x509.UnknownAuthorityError; return errors.As(err, &target) },
		},
		{
			name: "trusted CA wrong hostname", host: "other.babel.test", trusted: true,
			before: now.Add(-time.Hour), after: now.Add(time.Hour),
			checkErr: func(err error) bool { var target x509.HostnameError; return errors.As(err, &target) },
		},
		{
			name: "trusted CA expired", host: "127.0.0.1", trusted: true,
			before: now.Add(-2 * time.Hour), after: now.Add(-time.Hour),
			checkErr: func(err error) bool {
				var target x509.CertificateInvalidError
				return errors.As(err, &target) && target.Reason == x509.Expired
			},
		},
		{
			name: "trusted CA not yet valid", host: "127.0.0.1", trusted: true,
			before: now.Add(time.Hour), after: now.Add(2 * time.Hour),
			checkErr: func(err error) bool {
				var target x509.CertificateInvalidError
				return errors.As(err, &target) && target.Reason == x509.Expired
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ca := newOutboundTestCA(t)
			cert := ca.leaf(t, scenario.host, scenario.before, scenario.after)
			var received atomic.Int32
			server := newOutboundTLSServer(t, cert, tls.VersionTLS12, tls.VersionTLS13, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			var roots *x509.CertPool
			if scenario.trusted {
				roots = ca.pool
			}
			client, _ := outboundTLSClient(t, roots)
			resp, err := client.Do(outboundPrivateRequest(t, server.URL+"/v1/chat/completions"))
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil || !scenario.checkErr(err) {
				t.Fatalf("expected certificate validation failure, got %v", err)
			}
			if received.Load() != 0 {
				t.Fatal("endpoint received an HTTP request before its certificate was verified")
			}
		})
	}
}

func TestSecureOutboundNegotiatesSupportedTLS(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(version), func(t *testing.T) {
			ca := newOutboundTestCA(t)
			now := time.Now()
			cert := ca.leaf(t, "127.0.0.1", now.Add(-time.Hour), now.Add(time.Hour))
			received := make(chan string, 1)
			server := newOutboundTLSServer(t, cert, version, version, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				received <- r.Header.Get("Authorization") + "\n" + r.Header.Get("x-api-key") + "\n" + string(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			client, _ := outboundTLSClient(t, ca.pool)
			resp, err := client.Do(outboundPrivateRequest(t, server.URL+"/v1/messages"))
			if err != nil {
				t.Fatalf("valid TLS endpoint failed: %v", err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(body) != `{"ok":true}` || resp.TLS == nil || resp.TLS.Version != version {
				t.Fatalf("response did not retain negotiated TLS and body: body=%q, TLS=%+v, err=%v", body, resp.TLS, err)
			}
			want := "Bearer private-babel-transport-fixture-key\nprivate-babel-transport-fixture-key\n" + `{"prompt":"private-babel-transport-fixture-prompt"}`
			if got := <-received; got != want {
				t.Fatalf("trusted endpoint received a changed request: %q", got)
			}
		})
	}
}

func TestSecureOutboundRejectsTLS11BeforeSendingSecrets(t *testing.T) {
	ca := newOutboundTestCA(t)
	now := time.Now()
	cert := ca.leaf(t, "127.0.0.1", now.Add(-time.Hour), now.Add(time.Hour))
	var received atomic.Int32
	server := newOutboundTLSServer(t, cert, tls.VersionTLS11, tls.VersionTLS11, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	client, _ := outboundTLSClient(t, ca.pool)
	resp, err := client.Do(outboundPrivateRequest(t, server.URL+"/v1/chat/completions"))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("expected a TLS version rejection, got %v", err)
	}
	if received.Load() != 0 {
		t.Fatal("legacy TLS endpoint received an HTTP request")
	}
}

func TestSecureOutboundDoesNotFollowHTTPSRedirectToHTTP(t *testing.T) {
	var redirected atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()
	ca := newOutboundTestCA(t)
	now := time.Now()
	cert := ca.leaf(t, "127.0.0.1", now.Add(-time.Hour), now.Add(time.Hour))
	server := newOutboundTLSServer(t, cert, tls.VersionTLS12, tls.VersionTLS13, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	client, _ := outboundTLSClient(t, ca.pool)
	resp, err := client.Do(outboundPrivateRequest(t, server.URL+"/v1/messages"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("HTTPS redirect was followed: status=%d, sink calls=%d", resp.StatusCode, redirected.Load())
	}
}

type outboundProxyCapture struct {
	mu    sync.Mutex
	bytes bytes.Buffer
}

func (c *outboundProxyCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes.Write(p)
}

func (c *outboundProxyCapture) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.bytes.Bytes()...)
}

// The CONNECT proxy records the actual bytes it can observe. Its only dial
// target is the caller's local TLS fixture, never the requested public hostname.
func newOutboundCONNECTFixture(t *testing.T, authority, localTarget string) (*httptest.Server, *outboundProxyCapture, *atomic.Int32) {
	t.Helper()
	capture := new(outboundProxyCapture)
	calls := new(atomic.Int32)
	var connMu sync.Mutex
	var connections []net.Conn
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodConnect || r.Host != authority {
			t.Errorf("unexpected proxy request: %s %q", r.Method, r.Host)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		dump, err := httputil.DumpRequest(r, false)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = capture.Write(dump)
		upstream, err := net.DialTimeout("tcp", localTarget, time.Second)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		connMu.Lock()
		connections = append(connections, upstream, client)
		connMu.Unlock()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := rw.Flush(); err != nil {
			t.Error(err)
			return
		}
		go func() {
			_, _ = io.Copy(client, upstream)
			client.Close()
			upstream.Close()
		}()
		_, _ = io.Copy(io.MultiWriter(capture, upstream), rw)
	}))
	t.Cleanup(func() {
		connMu.Lock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		connMu.Unlock()
		server.Close()
	})
	return server, capture, calls
}

func TestSecureOutboundCONNECTPreservesEndToEndTLS(t *testing.T) {
	for _, forged := range []bool{false, true} {
		name := "valid tunnel"
		if forged {
			name = "forged endpoint rejected"
		}
		t.Run(name, func(t *testing.T) {
			trustedCA := newOutboundTestCA(t)
			endpointCA := trustedCA
			if forged {
				endpointCA = newOutboundTestCA(t)
			}
			now := time.Now()
			cert := endpointCA.leaf(t, "provider.babel.test", now.Add(-time.Hour), now.Add(time.Hour))
			var received atomic.Int32
			server := newOutboundTLSServer(t, cert, tls.VersionTLS12, tls.VersionTLS13, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Contains(body, []byte("private-babel-transport-fixture-prompt")) || r.Header.Get("Authorization") != "Bearer private-babel-transport-fixture-key" {
					t.Errorf("trusted endpoint received wrong body or credentials: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			authority := net.JoinHostPort("provider.babel.test", port)
			proxy, capture, connects := newOutboundCONNECTFixture(t, authority, server.Listener.Addr().String())
			proxyURL, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			client, transport := outboundTLSClient(t, trustedCA.pool)
			transport.base.Proxy = http.ProxyURL(proxyURL)
			resp, requestErr := client.Do(outboundPrivateRequest(t, "https://"+authority+"/v1/messages"))
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			if forged {
				var certificateErr x509.UnknownAuthorityError
				if requestErr == nil || !errors.As(requestErr, &certificateErr) || received.Load() != 0 {
					t.Fatalf("forged endpoint escaped TLS validation: err=%v, HTTP calls=%d", requestErr, received.Load())
				}
			} else if requestErr != nil || received.Load() != 1 {
				t.Fatalf("valid CONNECT tunnel failed: err=%v, HTTP calls=%d", requestErr, received.Load())
			}
			if connects.Load() != 1 {
				t.Fatalf("expected one CONNECT request, got %d", connects.Load())
			}
			observed := capture.snapshot()
			if !bytes.Contains(observed, []byte("CONNECT "+authority)) {
				t.Fatal("fixture did not capture the real CONNECT request")
			}
			for _, secret := range []string{"private-babel-transport-fixture-key", "private-babel-transport-fixture-prompt"} {
				if bytes.Contains(observed, []byte(secret)) {
					t.Fatalf("CONNECT proxy observed plaintext secret %q", secret)
				}
			}
		})
	}
}
