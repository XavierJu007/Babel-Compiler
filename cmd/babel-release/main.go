// babel-release creates the six downloadable, self-contained Babel packages.
// Run from the repository root with a clean checkout and an explicit commit SHA.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	archiveTime    = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
)

type target struct{ goos, label, arch string }

var targets = []target{
	{"windows", "windows", "amd64"}, {"windows", "windows", "arm64"},
	{"darwin", "macos", "amd64"}, {"darwin", "macos", "arm64"},
	{"linux", "linux", "amd64"}, {"linux", "linux", "arm64"},
}

const startHere = `Babel-Compiler — quick start / 快速开始

Windows: Extract this package, double-click babel.exe, and keep its window open.
Windows：解压后双击 babel.exe，使用期间保持程序窗口打开。

macOS / Linux: Open a terminal in this folder and run ./babel.
macOS / Linux：在此目录打开终端，运行 ./babel。

Babel selects an available local port (127.0.0.1:8080–8099) and tries to open
your browser. If it does not open, use the address printed in the terminal.
程序自动选择本机可用端口并尝试打开浏览器；未自动打开时访问终端显示的地址。

In model settings, save your API request URL, API key and model name, then
paste a prompt to compile. You supply the model service; none is bundled.
在「模型配置中心」填写你自己的请求 URL、API Key 和模型名，然后粘贴文本编译。

The UI, artwork and token counter are built in. No Go, Node or source files
are needed. Configuration is stored in your user directory by default.
页面、海报和 Token 计数器已内嵌，无需 Go、Node 或源码；配置默认保存在用户目录。

Version / 查看版本: ./babel -version (Windows: .\babel.exe -version)
Stop / 停止服务: Ctrl+C, or close the program window / 或关闭程序窗口。

Documentation / 使用说明: https://github.com/XavierJu007/Babel-Compiler
`

func main() {
	version := flag.String("version", "", "release version (defaults to VERSION)")
	commit := flag.String("commit", "", "required full source commit SHA")
	out := flag.String("out", "dist", "new or empty output directory")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(1)
	}
	if err := buildRelease(*version, *commit, *out); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func buildRelease(version, commit, out string) error {
	if version == "" {
		data, err := os.ReadFile("VERSION")
		if err != nil {
			return fmt.Errorf("read VERSION from the repository root: %w", err)
		}
		version = strings.TrimSpace(string(data))
	}
	if !versionPattern.MatchString(version) || !commitPattern.MatchString(commit) {
		return fmt.Errorf("provide version vX.Y.Z[-suffix] and -commit with the full 40-character source SHA")
	}
	commit = strings.ToLower(commit)
	for _, name := range []string{"go.mod", "LICENSE", "docs/THIRD_PARTY_NOTICES.md", "babel-go/main.go"} {
		if info, err := os.Stat(name); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("missing release input %s; run from the repository root", name)
		}
	}
	if err := emptyOutputDir(out); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "babel-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var archives []string
	for _, target := range targets {
		binaryName, extension := "babel", ".tar.gz"
		if target.goos == "windows" {
			binaryName, extension = "babel.exe", ".zip"
		}
		binaryPath := filepath.Join(tmp, binaryName)
		cmd := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags",
			"-s -w -X main.version="+version+" -X main.commit="+commit,
			"-o", binaryPath, "./babel-go")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.goos, "GOARCH="+target.arch)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		fmt.Printf("Building %s/%s\n", target.goos, target.arch)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", target.goos, target.arch, err)
		}
		name := fmt.Sprintf("babel_%s_%s_%s%s", version, target.label, target.arch, extension)
		if err := writeArchive(filepath.Join(out, name), packageFiles(binaryPath, binaryName), target.goos == "windows"); err != nil {
			return err
		}
		archives = append(archives, name)
	}
	if err := writeChecksums(out, archives); err != nil {
		return err
	}
	fmt.Printf("Created %d packages and SHA256SUMS in %s\n", len(archives), out)
	return nil
}

func emptyOutputDir(path string) error {
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		return fmt.Errorf("output must be a directory, not a file or symlink: %s", path)
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("output directory must be empty; refusing to overwrite %s", path)
	}
	return nil
}

type archiveFile struct {
	name, path string
	text       string
	mode       int64
}

// Explicit inputs avoid accidentally shipping a local API key, cache or dictionary.
func packageFiles(binaryPath, binaryName string) []archiveFile {
	return []archiveFile{
		{name: binaryName, path: binaryPath, mode: 0755},
		{name: "LICENSE", path: "LICENSE", mode: 0644},
		{name: "THIRD_PARTY_NOTICES.md", path: "docs/THIRD_PARTY_NOTICES.md", mode: 0644},
		{name: "START_HERE.txt", text: startHere, mode: 0644},
	}
}

func writeArchive(path string, files []archiveFile, zipFormat bool) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	var appendFile func(archiveFile, []byte) error
	var closeArchive func() error
	if zipFormat {
		writer := zip.NewWriter(f)
		appendFile = func(file archiveFile, data []byte) error {
			header := &zip.FileHeader{Name: file.name, Method: zip.Deflate, Modified: archiveTime}
			header.SetMode(os.FileMode(file.mode))
			entry, err := writer.CreateHeader(header)
			if err != nil {
				return err
			}
			_, err = entry.Write(data)
			return err
		}
		closeArchive = writer.Close
	} else {
		compressed := gzip.NewWriter(f)
		writer := tar.NewWriter(compressed)
		appendFile = func(file archiveFile, data []byte) error {
			if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(data)), ModTime: archiveTime}); err != nil {
				return err
			}
			_, err := writer.Write(data)
			return err
		}
		closeArchive = func() error {
			if err := writer.Close(); err != nil {
				_ = compressed.Close()
				return err
			}
			return compressed.Close()
		}
	}
	for _, file := range files {
		data := []byte(file.text)
		if file.path != "" {
			data, err = os.ReadFile(file.path)
			if err != nil {
				_ = closeArchive()
				return err
			}
		}
		if err = appendFile(file, data); err != nil {
			_ = closeArchive()
			return err
		}
	}
	return closeArchive()
}

func writeChecksums(dir string, names []string) error {
	sort.Strings(names)
	var sums strings.Builder
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintf(&sums, "%x  %s\n", hash.Sum(nil), name)
	}
	f, err := os.OpenFile(filepath.Join(dir, "SHA256SUMS"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, sums.String())
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
