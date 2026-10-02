package qemu

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestCloneCopiesTheDiskOfAStoppedMachine(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	source := core.Environment{ID: "src", Name: "vm", Kind: core.Machine}
	clone := core.Environment{ID: "dst", Name: "vm copy", Kind: core.Machine}
	if err := os.MkdirAll(b.dir("src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.diskPath("src"), []byte("qcow2 bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Clone(context.Background(), source, clone); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(b.diskPath("dst")); err != nil || string(data) != "qcow2 bytes" {
		t.Fatalf("copy: %q, %v", data, err)
	}
	if data, _ := os.ReadFile(b.diskPath("src")); string(data) != "qcow2 bytes" {
		t.Fatal("the source disk changed")
	}
}

func TestCloneRefusesARunningMachine(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	source := core.Environment{Name: "vm", Kind: core.Machine}
	startFakeQEMU(t, b, source.Name)
	err := b.Clone(context.Background(), source, core.Environment{ID: "dst", Name: "copy", Kind: core.Machine})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("expected shut-down-first, got %v", err)
	}
	if _, statErr := os.Stat(b.dir("dst")); statErr == nil {
		t.Fatal("a refused clone left a directory behind")
	}
}
