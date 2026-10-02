package qemu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// fakeVirtiofsd points virtiofsdPath at a shell script and returns a file
// the script touches when it runs.
func fakeVirtiofsd(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	path := filepath.Join(dir, "virtiofsd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n: > "+ran+"\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := virtiofsdPath
	virtiofsdPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { virtiofsdPath = old })
	return ran
}

// A shared folder that was deleted or moved after it was configured must
// say which folder and where to fix it, not "virtiofsd did not create its
// socket" after a two-second wait.
func TestMissingSharedFolderIsExplained(t *testing.T) {
	ran := fakeVirtiofsd(t, "exit 0")
	b := &Backend{stateDir: t.TempDir()}
	if err := os.MkdirAll(b.dir("vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "Projetos")
	err := b.startVirtiofs(context.Background(), "vm", core.EnvironmentSettings{SharedPath: missing})
	if !errors.Is(err, core.ErrInvalidInput) || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "Settings") {
		t.Fatalf("startVirtiofs with a missing folder = %v, want an ErrInvalidInput naming the folder and Settings", err)
	}
	if _, statErr := os.Stat(ran); statErr == nil {
		t.Fatal("virtiofsd must not be started for a folder that does not exist")
	}
}

// When virtiofsd itself fails, its own reason reaches the user, and as soon
// as it exits rather than after the socket timeout.
func TestVirtiofsdFailureIsReportedQuickly(t *testing.T) {
	fakeVirtiofsd(t, `echo "boom: cannot sandbox" >&2; exit 1`)
	b := &Backend{stateDir: t.TempDir()}
	if err := os.MkdirAll(b.dir("vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := b.startVirtiofs(context.Background(), "vm", core.EnvironmentSettings{SharedPath: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "boom: cannot sandbox") {
		t.Fatalf("startVirtiofs = %v, want virtiofsd's own error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("failure took %v to report; virtiofsd had already exited", elapsed)
	}
}
