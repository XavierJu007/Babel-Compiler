package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func awaitReloadLock(t *testing.T, mutex *sync.Mutex, completed <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !mutex.TryLock() {
			return
		}
		mutex.Unlock()
		select {
		case <-completed:
			t.Fatal("old snapshot finished before ordering could be observed")
		default:
		}
		runtime.Gosched()
	}
	t.Fatal("reload did not enter its serialized snapshot")
}

func TestDomainNewerReloadCannotBeOverwrittenByOlderScan(t *testing.T) {
	isolateCompilerLibraries(t)
	oldDir, newDir, emptyDir := t.TempDir(), t.TempDir(), t.TempDir()
	old := symbolTestPack("a-lifecycle", nil)
	newer := cloneDomainPack(old)
	newer.Version = 2
	writeDomainFixture(t, oldDir, old)
	writeDomainFixture(t, newDir, newer)
	// Slow only the older snapshot using valid, unrelated packs. This reproduces
	// the original late-publication bug without changing scanner behavior.
	for i := 0; i < 64; i++ {
		padding := symbolTestPack(fmt.Sprintf("z-padding-%03d", i), nil)
		padding.Entries[0].UseWhen = []string{strings.Repeat("a", 600)}
		for j := 1; j < 128; j++ {
			entry := padding.Entries[0]
			entry.ID = fmt.Sprint(j)
			padding.Entries = append(padding.Entries, entry)
		}
		writeDomainFixture(t, oldDir, padding)
	}
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		if _, err := reloadDomainsFromDirs(oldDir, emptyDir); err != nil {
			t.Error(err)
		}
	}()
	awaitReloadLock(t, &domainsLifecycleMutex, completed)
	if _, err := reloadDomainsFromDirs(newDir, emptyDir); err != nil {
		t.Fatal(err)
	}
	<-completed
	domainsMutex.RLock()
	version := loadedDomains[old.Name].Version
	domainsMutex.RUnlock()
	if version != 2 {
		t.Fatalf("old scan replaced newer published version: %d", version)
	}
}

func TestDomainSaveDeleteRefreshDespiteUnrelatedInvalidFile(t *testing.T) {
	isolateCompilerLibraries(t)
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir("domains", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("domains", "unrelated-broken.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	pack := symbolTestPack("review-lifecycle-fixture", nil)
	if err := saveDomain(pack, "workspace"); err != nil {
		t.Fatalf("unrelated bad file made successful save look failed: %v", err)
	}
	domainsMutex.RLock()
	loaded := loadedDomains[pack.Name]
	warning := domainFileErrors["unrelated-broken"]
	domainsMutex.RUnlock()
	if loaded.Version != 1 || warning == nil {
		t.Fatalf("save did not publish new pack and preserve warning: version=%d warning=%v", loaded.Version, warning)
	}
	pack.Version = 2
	if err := saveDomain(pack, "workspace"); err != nil {
		t.Fatal(err)
	}
	domainsMutex.RLock()
	version := loadedDomains[pack.Name].Version
	domainsMutex.RUnlock()
	if version != 2 {
		t.Fatalf("save returned before new version was active: %d", version)
	}
	if err := deleteDomain(pack.Name, "workspace"); err != nil {
		t.Fatalf("unrelated bad file made successful delete look failed: %v", err)
	}
	domainsMutex.RLock()
	_, exists := loadedDomains[pack.Name]
	domainsMutex.RUnlock()
	if exists {
		t.Fatal("delete returned with deleted pack still active")
	}
}
