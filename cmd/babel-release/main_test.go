package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPackagesContainOnlyDistributionFiles(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Mkdir("docs", 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"program": "test executable", "LICENSE": "license", "docs/THIRD_PARTY_NOTICES.md": "notices",
		"babel.config.json": "SECRET API KEY", "babel.cache.json": "PRIVATE PROMPT", "docs/private.md": "NOT FOR RELEASE",
	} {
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, zipFormat := range []bool{false, true} {
		t.Run(fmt.Sprintf("zip=%v", zipFormat), func(t *testing.T) {
			name := "babel"
			if zipFormat {
				name += ".exe"
			}
			files := packageFiles("program", name)
			first, second := filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "two")
			for _, path := range []string{first, second} {
				if err := writeArchive(path, files, zipFormat); err != nil {
					t.Fatal(err)
				}
			}
			one, _ := os.ReadFile(first)
			two, _ := os.ReadFile(second)
			if !bytes.Equal(one, two) {
				t.Fatal("archive output is not deterministic")
			}
			got := map[string]string{}
			check := func(entry string, mode os.FileMode, reader io.Reader) {
				t.Helper()
				wantMode := os.FileMode(0644)
				if entry == name {
					wantMode = 0755
				}
				if mode.Perm() != wantMode {
					t.Errorf("%s mode=%o, want %o", entry, mode.Perm(), wantMode)
				}
				data, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				got[entry] = string(data)
			}
			if zipFormat {
				reader, err := zip.NewReader(bytes.NewReader(one), int64(len(one)))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range reader.File {
					stream, err := entry.Open()
					if err != nil {
						t.Fatal(err)
					}
					check(entry.Name, entry.Mode(), stream)
					_ = stream.Close()
				}
			} else {
				compressed, err := gzip.NewReader(bytes.NewReader(one))
				if err != nil {
					t.Fatal(err)
				}
				defer compressed.Close()
				reader := tar.NewReader(compressed)
				for {
					entry, err := reader.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					check(entry.Name, entry.FileInfo().Mode(), reader)
				}
			}
			want := map[string]string{name: "test executable", "LICENSE": "license", "THIRD_PARTY_NOTICES.md": "notices", "START_HERE.txt": startHere}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("archive contains unexpected or missing distribution files: %v", got)
			}
			if err := writeArchive(first, files, zipFormat); err == nil {
				t.Fatal("overwrote an existing package")
			}
			after, _ := os.ReadFile(first)
			if !bytes.Equal(after, one) {
				t.Fatal("rejected overwrite changed the existing package")
			}
		})
	}
}

func TestChecksumsAreSortedAndCorrect(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z.zip", "a.tar.gz"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeChecksums(dir, []string{"z.zip", "a.tar.gz"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x  a.tar.gz\n%x  z.zip\n", sha256.Sum256([]byte("a.tar.gz")), sha256.Sum256([]byte("z.zip")))
	if string(got) != want {
		t.Fatalf("unexpected checksum file: %s", got)
	}
}

func TestRejectsNonEmptyOutputAndUnsafeMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := emptyOutputDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := emptyOutputDir(dir); err == nil {
		t.Fatal("accepted nonempty directory")
	}
	if err := emptyOutputDir(filepath.Join(dir, "keep")); err == nil {
		t.Fatal("accepted a regular file as output directory")
	}
	for _, metadata := range [][2]string{{"../../v1.0.0", strings.Repeat("a", 40)}, {"v1.0.0 -X injected", strings.Repeat("a", 40)}, {"v1.0.0", "short"}, {"v1.0.0", strings.Repeat("a", 40) + " -X injected"}} {
		if err := buildRelease(metadata[0], metadata[1], filepath.Join(dir, "new")); err == nil {
			t.Fatal("accepted unsafe metadata")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Fatal("invalid metadata created an output directory")
	}
}

func TestSelectTargetsAndMacOSBundleMetadata(t *testing.T) {
	all, err := selectTargets("all")
	if err != nil || len(all) != 6 {
		t.Fatalf("expected all 6 targets, got %d (%v)", len(all), err)
	}
	wl, err := selectTargets("windows, linux")
	if err != nil || len(wl) != 4 {
		t.Fatalf("expected 4 windows+linux targets, got %d (%v)", len(wl), err)
	}
	mac, err := selectTargets("macos")
	if err != nil || len(mac) != 2 || mac[0].goos != "darwin" || mac[1].goos != "darwin" {
		t.Fatalf("expected 2 macos targets, got %+v (%v)", mac, err)
	}
	if _, err := selectTargets("plan9"); err == nil {
		t.Fatal("expected unknown target error")
	}

	if got := macosAppBundleVersion("v0.2.0-rc.1"); got != "0.2.0" {
		t.Fatalf("macosAppBundleVersion mismatch: %q", got)
	}
	plist := renderMacOSInfoPlist("v0.2.0", "6wyndoIin")
	for _, want := range []string{
		"<string>6wyndoIin</string>",
		"<string>0.2.0</string>",
		"<string>babel-launcher</string>",
		"<string>Babel.icns</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("Info.plist missing %q:\n%s", want, plist)
		}
	}
}

