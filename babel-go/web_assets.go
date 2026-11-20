package main

import (
	_ "embed"
	"net/http"
	"sort"
	"strconv"
)

// Keep the artwork in the binary so the UI needs no third-party image host or
// working-directory files. Only the exact paths below are exposed by the HTTP mux.
//
// babel-poster.png 与 babel-poster-6x1.png 是同一份 6:1 作品，别名复用同一份内嵌
// 字节，避免把 1.2 MB 的图重复打进二进制。
//
//go:embed assets/babel-poster.png
var posterPNG []byte

//go:embed assets/babel-poster-4x1.png
var posterWidePNG []byte

// posterAssets 是唯一的公开资源白名单：精确路径 -> 内嵌字节。
var posterAssets = map[string][]byte{
	"/assets/babel-poster.png":     posterPNG,
	"/assets/babel-poster-6x1.png": posterPNG,
	"/assets/babel-poster-4x1.png": posterWidePNG,
}

// posterAssetPaths 按字典序返回注册路径，保证 mux 注册顺序稳定。
func posterAssetPaths() []string {
	paths := make([]string, 0, len(posterAssets))
	for path := range posterAssets {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func servePosterAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	data, ok := posterAssets[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}
