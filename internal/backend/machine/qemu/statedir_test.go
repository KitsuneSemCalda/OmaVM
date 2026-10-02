package qemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func legacyMachine(t *testing.T, b *Backend, env core.Environment) {
	t.Helper()
	if err := os.MkdirAll(b.dir(env.Name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.diskPath(env.Name), []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Machines created before directories were named by ID move to their ID
// the first time they are seen stopped, and their ~/OmaVM link follows.
func TestStoppedLegacyMachineMovesToItsID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no setfattr/gio
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{ID: "0123456789abcdef", Name: "Área de testes", Kind: core.Machine}
	legacyMachine(t, b, env)
	link, err := b.Link(context.Background(), core.Environment{Name: env.Name}, "")
	if err != nil {
		t.Fatal(err)
	}

	if got := b.key(env); got != env.ID {
		t.Fatalf("key = %q, want the ID", got)
	}
	if data, err := os.ReadFile(b.diskPath(env.ID)); err != nil || string(data) != "disk" {
		t.Fatalf("disk not moved: %q, %v", data, err)
	}
	if _, err := os.Stat(b.dir(env.Name)); !os.IsNotExist(err) {
		t.Fatalf("old directory still there: %v", err)
	}
	if target, _ := os.Readlink(link); target != b.diskPath(env.ID) {
		t.Fatalf("link points at %q, want the moved disk", target)
	}
	if err := b.Unlink(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("Unlink left the moved Machine's link behind")
	}
}

// A running QEMU is only recognized by the pidfile path it was given, so
// its directory can't move under it.
func TestRunningLegacyMachineStaysUntilStopped(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{ID: "0123456789abcdef", Name: "vm", Kind: core.Machine}
	legacyMachine(t, b, env)
	qemu := startFakeQEMU(t, b, env.Name)

	if got := b.key(env); got != env.Name {
		t.Fatalf("key = %q while running, want the old directory", got)
	}
	if running, _ := b.isRunning(b.key(env)); !running {
		t.Fatal("the running Machine is no longer recognized")
	}

	_ = qemu.Process.Kill()
	_ = qemu.Wait()

	if got := b.key(env); got != env.ID {
		t.Fatalf("key = %q once stopped, want the ID", got)
	}
	if _, err := os.Stat(filepath.Join(b.dir(env.ID), "disk.qcow2")); err != nil {
		t.Fatal(err)
	}
}

// New Machines never depend on their name for paths: a name that would
// have been too long for the socket paths is fine now.
func TestNewMachineDirectoryIsItsID(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{ID: "0123456789abcdef", Name: "a very long machine name that would not fit in a unix socket path at all", Kind: core.Machine}
	if got := b.key(env); got != env.ID {
		t.Fatalf("key = %q, want the ID", got)
	}
	if err := b.checkNameLength(b.key(env)); err != nil {
		t.Fatalf("long name refused: %v", err)
	}
}
