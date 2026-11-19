package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func invalidUTF8DomainJSON(t *testing.T, pack DomainPack) []byte {
	t.Helper()
	pack.Label = "invalid-utf8-marker"
	raw, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Replace(raw, []byte("invalid-utf8-marker"), []byte{0xff}, 1)
}

func TestDomainJSONRejectsInvalidUTF8(t *testing.T) {
	pack := symbolTestPack("review-encoding", nil)
	if parsed, err := parseDomainJSON(invalidUTF8DomainJSON(t, pack)); err == nil {
		t.Fatalf("invalid UTF-8 was silently rewritten and loaded: label=%q", parsed.Label)
	}
}

func TestDomainAPIBadEncodingCannotReplaceSavedDictionary(t *testing.T) {
	isolateDomainIntegration(t)
	pack := symbolTestPack("review-encoding", nil)
	if err := saveDomain(pack, "workspace"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspaceDomainsDir(), pack.Name+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("{\"action\":\"save\",\"scope\":\"workspace\",\"domain\":"), invalidUTF8DomainJSON(t, pack)...)
	payload = append(payload, '}')
	response := httptest.NewRecorder()
	domainAPIHandler(response, httptest.NewRequest(http.MethodPost, "/api/domains", bytes.NewReader(payload)))
	if response.Code != http.StatusBadRequest {
		t.Errorf("bad encoding returned %d, want 400", response.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("invalid request replaced the saved dictionary: %s; %v", after, err)
	}
	packs, err := resolveDomainPacks("mixed", []string{pack.Name})
	if err != nil {
		t.Fatal(err)
	}
	for _, loaded := range packs {
		if loaded.Name == pack.Name && loaded.Label != pack.Label {
			t.Fatalf("invalid request changed the active dictionary: %q", loaded.Label)
		}
	}
}

func TestDomainReloadBlocksInvalidUTF8OverrideUntilRepaired(t *testing.T) {
	globalDir, workspaceDir := isolateCompilerLibraries(t)
	global := symbolTestPack("review-encoding", nil)
	global.Label = "global definition"
	writeDomainFixture(t, globalDir, global)
	path := filepath.Join(workspaceDir, global.Name+".json")
	if err := os.WriteFile(path, invalidUTF8DomainJSON(t, global), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid override did not report an encoding warning: %v", err)
	}
	if _, err := resolveDomainPacks("mixed", []string{global.Name}); err == nil {
		t.Fatal("selected invalid override silently loaded or fell back to the global definition")
	}
	workspace := cloneDomainPack(global)
	workspace.Label = "修复后的工作区词典；允许显式字符 �"
	workspace.Version = 2
	writeDomainFixture(t, workspaceDir, workspace)
	if _, err := reloadDomainsFromDirs(globalDir, workspaceDir); err != nil {
		t.Fatal(err)
	}
	packs, err := resolveDomainPacks("mixed", []string{workspace.Name})
	if err != nil {
		t.Fatal(err)
	}
	for _, loaded := range packs {
		if loaded.Name == workspace.Name && (loaded.Label != workspace.Label || loaded.Version != 2) {
			t.Fatalf("repaired UTF-8 content was not loaded exactly: %+v", loaded)
		}
	}
}
