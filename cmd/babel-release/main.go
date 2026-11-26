// babel-release creates the downloadable, self-contained Babel packages.
// Run from the repository root with a clean checkout and an explicit commit SHA.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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

type releaseOptions struct {
	version       string
	commit        string
	out           string
	targetsSpec   string
	macosSignID   string
	buildDMG      bool
	skipChecksums bool
}

const startHere = `Babel-Compiler — quick start / 快速开始

Windows: Extract this package, double-click babel.exe, and keep its window open.
Windows：解压后双击 babel.exe，使用期间保持程序窗口打开。

macOS Desktop (.dmg): Drag Babel.app into Applications and open it.
macOS CLI / Linux (.tar.gz): Open a terminal in this folder and run ./babel.
macOS 桌面安装 (.dmg)：将 Babel.app 拖入「应用程序」双击启动；命令行版在此目录运行 ./babel。

Babel selects an available local port (127.0.0.1:8080–8099) and tries to open
your browser. If it does not open, use the address printed in the terminal.
程序自动选择本机可用端口并尝试打开浏览器；未自动打开时访问终端显示的地址。

In model settings, save your API request URL, API key and model name, then
paste a prompt to compile. You supply the model service; none is bundled.
在「模型配置」填写你自己的请求 URL、API Key 和模型名，然后粘贴文本编译。

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
	targetsSpec := flag.String("targets", "all", "comma-separated target families: all, windows, linux, macos")
	macosSignID := flag.String("macos-sign-id", "", "macOS ad-hoc codesign identifier (e.g. 6wyndoIin)")
	buildDMG := flag.Bool("dmg", false, "package macOS .app inside .dmg installer (requires macOS)")
	skipChecksums := flag.Bool("skip-checksums", false, "skip writing SHA256SUMS (for multi-runner CI merge)")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(1)
	}
	err := buildReleaseWithOptions(releaseOptions{
		version:       *version,
		commit:        *commit,
		out:           *out,
		targetsSpec:   *targetsSpec,
		macosSignID:   *macosSignID,
		buildDMG:      *buildDMG,
		skipChecksums: *skipChecksums,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func buildRelease(version, commit, out string) error {
	return buildReleaseWithOptions(releaseOptions{
		version:     version,
		commit:      commit,
		out:         out,
		targetsSpec: "all",
	})
}

func selectTargets(spec string) ([]target, error) {
	spec = strings.TrimSpace(strings.ToLower(spec))
	if spec == "" || spec == "all" {
		out := make([]target, len(targets))
		copy(out, targets)
		return out, nil
	}
	want := make(map[string]bool)
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		switch name {
		case "windows", "linux", "macos", "darwin":
			if name == "darwin" {
				name = "macos"
			}
			want[name] = true
		default:
			return nil, fmt.Errorf("unknown target selector %q; use all, windows, linux, or macos", raw)
		}
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("no valid targets selected from %q", spec)
	}
	var selected []target
	for _, t := range targets {
		if want[t.label] {
			selected = append(selected, t)
		}
	}
	return selected, nil
}

func xmlEscape(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}

func macosAppBundleVersion(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if idx := strings.IndexByte(v, '-'); idx >= 0 {
		v = v[:idx]
	}
	if v == "" {
		return "0.0.0"
	}
	return v
}

func renderMacOSInfoPlist(version, signID string) string {
	shortVer := strings.TrimPrefix(strings.TrimSpace(version), "v")
	bundleVer := macosAppBundleVersion(version)
	bundleID := strings.TrimSpace(signID)
	if bundleID == "" {
		bundleID = "com.babel-compiler.app"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key>
	<string>en</string>
	<key>CFBundleDisplayName</key>
	<string>Babel Compiler</string>
	<key>CFBundleExecutable</key>
	<string>babel-launcher</string>
	<key>CFBundleIconFile</key>
	<string>Babel.icns</string>
	<key>CFBundleIdentifier</key>
	<string>%s</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleName</key>
	<string>Babel</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>%s</string>
	<key>CFBundleVersion</key>
	<string>%s</string>
	<key>LSMinimumSystemVersion</key>
	<string>12.0</string>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
`, xmlEscape(bundleID), xmlEscape(shortVer), xmlEscape(bundleVer))
}

func codesignAdHoc(targetPath, signID string, deep bool) error {
	args := []string{"--force"}
	if deep {
		args = append(args, "--deep")
	}
	args = append(args, "--sign", "-")
	if id := strings.TrimSpace(signID); id != "" {
		args = append(args, "--identifier", id)
	}
	args = append(args, targetPath)
	cmd := exec.Command("codesign", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codesign %s: %w", targetPath, err)
	}
	verifyArgs := []string{"--verify"}
	if deep {
		verifyArgs = append(verifyArgs, "--deep", "--strict")
	}
	verifyArgs = append(verifyArgs, "--verbose=2", targetPath)
	vcmd := exec.Command("codesign", verifyArgs...)
	vcmd.Stdout, vcmd.Stderr = os.Stdout, os.Stderr
	if err := vcmd.Run(); err != nil {
		return fmt.Errorf("verify codesign %s: %w", targetPath, err)
	}
	return nil
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

func createMacOSDMG(tmpRoot, version, arch, signID, binaryPath, icnsPath, outDMGPath string) error {
	stageDir := filepath.Join(tmpRoot, "dmg-stage-"+arch)
	if err := os.RemoveAll(stageDir); err != nil {
		return err
	}
	appDir := filepath.Join(stageDir, "Babel.app")
	macosDir := filepath.Join(appDir, "Contents", "MacOS")
	resDir := filepath.Join(appDir, "Contents", "Resources")
	if err := os.MkdirAll(macosDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(resDir, 0755); err != nil {
		return err
	}
	appBinPath := filepath.Join(macosDir, "babel")
	if err := copyRegularFile(binaryPath, appBinPath, 0755); err != nil {
		return err
	}
	launcher := "#!/bin/sh\nexec \"$(dirname \"$0\")/babel\" -app\n"
	if err := os.WriteFile(filepath.Join(macosDir, "babel-launcher"), []byte(launcher), 0755); err != nil {
		return err
	}
	if err := copyRegularFile(icnsPath, filepath.Join(resDir, "Babel.icns"), 0644); err != nil {
		return err
	}
	plist := renderMacOSInfoPlist(version, signID)
	if err := os.WriteFile(filepath.Join(appDir, "Contents", "Info.plist"), []byte(plist), 0644); err != nil {
		return err
	}
	if err := codesignAdHoc(appBinPath, signID, false); err != nil {
		return err
	}
	if err := codesignAdHoc(appDir, signID, true); err != nil {
		return err
	}
	if err := os.Symlink("/Applications", filepath.Join(stageDir, "Applications")); err != nil {
		return err
	}
	cmd := exec.Command("hdiutil", "create", "-volname", "Babel Compiler", "-srcfolder", stageDir, "-ov", "-format", "UDZO", outDMGPath)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("create dmg %s: %w", outDMGPath, err)
	}
	return nil
}

func buildReleaseWithOptions(opts releaseOptions) error {
	version := opts.version
	if version == "" {
		data, err := os.ReadFile("VERSION")
		if err != nil {
			return fmt.Errorf("read VERSION from the repository root: %w", err)
		}
		version = strings.TrimSpace(string(data))
	}
	if !versionPattern.MatchString(version) || !commitPattern.MatchString(opts.commit) {
		return fmt.Errorf("provide version vX.Y.Z[-suffix] and -commit with the full 40-character source SHA")
	}
	selectedTargets, err := selectTargets(opts.targetsSpec)
	if err != nil {
		return err
	}
	hasMacTarget := false
	for _, t := range selectedTargets {
		if t.goos == "darwin" {
			hasMacTarget = true
			break
		}
	}
	if hasMacTarget && (opts.macosSignID != "" || opts.buildDMG) && runtime.GOOS != "darwin" {
		return fmt.Errorf("macOS signing / .dmg packaging requires running on macOS (got %s)", runtime.GOOS)
	}
	commit := strings.ToLower(opts.commit)
	requiredFiles := []string{"go.mod", "LICENSE", "docs/THIRD_PARTY_NOTICES.md", "babel-go/main.go"}
	icnsPath := filepath.Join("babel-go", "assets", "babel.icns")
	if opts.buildDMG {
		requiredFiles = append(requiredFiles, icnsPath)
	}
	for _, name := range requiredFiles {
		if info, err := os.Stat(name); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("missing release input %s; run from the repository root", name)
		}
	}
	if err := emptyOutputDir(opts.out); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "babel-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var archives []string
	for _, target := range selectedTargets {
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
		if target.goos == "darwin" && runtime.GOOS == "darwin" && (opts.macosSignID != "" || opts.buildDMG) {
			fmt.Printf("Self-signing macOS %s/%s binary (id=%q)\n", target.goos, target.arch, opts.macosSignID)
			if err := codesignAdHoc(binaryPath, opts.macosSignID, false); err != nil {
				return err
			}
		}
		name := fmt.Sprintf("babel_%s_%s_%s%s", version, target.label, target.arch, extension)
		if err := writeArchive(filepath.Join(opts.out, name), packageFiles(binaryPath, binaryName), target.goos == "windows"); err != nil {
			return err
		}
		archives = append(archives, name)
		if target.goos == "darwin" && opts.buildDMG {
			dmgName := fmt.Sprintf("babel_%s_%s_%s.dmg", version, target.label, target.arch)
			dmgPath := filepath.Join(opts.out, dmgName)
			fmt.Printf("Packaging macOS installer %s\n", dmgName)
			if err := createMacOSDMG(tmp, version, target.arch, opts.macosSignID, binaryPath, icnsPath, dmgPath); err != nil {
				return err
			}
			archives = append(archives, dmgName)
		}
	}
	if !opts.skipChecksums {
		if err := writeChecksums(opts.out, archives); err != nil {
			return err
		}
		fmt.Printf("Created %d packages and SHA256SUMS in %s\n", len(archives), opts.out)
	} else {
		fmt.Printf("Created %d packages in %s (checksums skipped)\n", len(archives), opts.out)
	}
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
