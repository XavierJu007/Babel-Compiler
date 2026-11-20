package main

import (
	_ "embed"
	"net/http"
	"strconv"
)

// Keep the artwork in the binary so the UI needs no third-party image host or
// working-directory files. Only this exact asset is exposed by the HTTP mux.
//
//go:embed assets/babel-poster.png
var posterPNG []byte

func servePosterAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(posterPNG)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(posterPNG)
}
