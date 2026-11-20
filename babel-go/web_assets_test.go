package main

import (
	"bytes"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPosterAssetEmbeddedAndServed(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	handler, err := newServerHandler(server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			request, err := http.NewRequest(method, server.URL+"/assets/babel-poster.png", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Sec-Fetch-Site", "same-origin")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/png" || response.ContentLength != int64(len(posterPNG)) {
				t.Fatalf("unexpected asset response: status=%d headers=%v length=%d", response.StatusCode, response.Header, response.ContentLength)
			}
			if response.Header.Get("Content-Security-Policy") != localDataCSP || response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("X-Frame-Options") != "DENY" || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("asset bypassed local response protections: %v", response.Header)
			}
			if method == http.MethodHead {
				if len(body) != 0 {
					t.Fatalf("HEAD returned %d body bytes", len(body))
				}
				return
			}
			config, err := png.DecodeConfig(bytes.NewReader(body))
			if err != nil || config.Width != 1774 || config.Height != 887 {
				t.Fatalf("embedded artwork is invalid or unexpected: config=%+v err=%v", config, err)
			}
		})
	}
}

func TestPosterAssetDoesNotExposeOtherFiles(t *testing.T) {
	handler := guardedLocalHandler(t)
	for _, path := range []string{"/assets/", "/assets/index.html", "/assets/babel-poster.png/extra", "/assets/babel-poster-prompt.txt", "/babel-go/assets/babel-poster.png", "/docs/assets/babel-poster.png"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+path, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("unexpected file exposed: %s status=%d", path, response.Code)
			}
		})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/assets/babel-poster.png", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("asset accepted unsupported method: status=%d headers=%v", response.Code, response.Header())
	}
}

func TestPosterAssetKeepsLocalRequestBoundary(t *testing.T) {
	handler := guardedLocalHandler(t)
	for _, tc := range []struct {
		name, host, origin, fetchSite string
	}{
		{"cross-origin", "127.0.0.1:8080", "https://outside.example", "cross-site"},
		{"cross-site", "127.0.0.1:8080", "", "cross-site"},
		{"foreign-host", "outside.example:8080", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/assets/babel-poster.png", nil)
			request.Host = tc.host
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("asset bypassed request boundary: status=%d headers=%v", response.Code, response.Header())
			}
		})
	}
}
