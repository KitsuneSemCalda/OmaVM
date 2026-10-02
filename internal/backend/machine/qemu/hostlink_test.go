package qemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestLinkCreatesAndUpdatesSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	// Link only needs a file at diskPath to point the symlink at, not a
	// real qcow2 image — set that up directly instead of going through
	// Create(), which shells out to qemu-img and would make this test
	// depend on QEMU being installed for no reason (Testing Strategy:
	// domain/adapter logic that doesn't need a real tool shouldn't
	// require one).
	if err := os.MkdirAll(b.dir(env.Name), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(b.diskPath(env.Name), nil, 0o644); err != nil {
		t.Fatalf("write disk placeholder: %v", err)
	}

	link, err := b.Link(context.Background(), env, "blue")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected a symlink at %s: %v", link, err)
	}
	if target != b.diskPath(env.Name) {
		t.Fatalf("expected link to point at %s, got %s", b.diskPath(env.Name), target)
	}

	// Re-linking (e.g. a color change) must replace the stale symlink,
	// not fail or duplicate it.
	if _, err := b.Link(context.Background(), env, "red"); err != nil {
		t.Fatalf("re-Link: %v", err)
	}

	if err := b.Unlink(context.Background(), env); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("expected link to be removed, stat err: %v", err)
	}
	// Unlink must be idempotent.
	if err := b.Unlink(context.Background(), env); err != nil {
		t.Fatalf("second Unlink: %v", err)
	}
}

func TestLinkPathIsUnderHomeOmaVM(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	if err := os.MkdirAll(b.dir(env.Name), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(b.diskPath(env.Name), nil, 0o644); err != nil {
		t.Fatalf("write disk placeholder: %v", err)
	}
	link, err := b.Link(context.Background(), env, "")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	want := filepath.Join(home, "OmaVM", "guest")
	if link != want {
		t.Fatalf("expected link at %s, got %s", want, link)
	}
}

// ~/OmaVM is in the user's home: a file there that OmaVM did not create
// (notes, a copied disk) must survive a color change and a Remove.
func TestLinkNeverDeletesTheUsersOwnFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	if err := os.MkdirAll(b.dir(env.Name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.diskPath(env.Name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(home, "OmaVM", "guest")
	if err := os.MkdirAll(filepath.Dir(userFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userFile, []byte("my notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Link(context.Background(), env, "blue"); err == nil {
		t.Fatal("Link over the user's own file must fail, not replace it")
	}
	if err := b.Unlink(context.Background(), env); err != nil {
		t.Fatalf("Unlink must leave a foreign file alone without failing Remove: %v", err)
	}
	data, err := os.ReadFile(userFile)
	if err != nil || string(data) != "my notes" {
		t.Fatalf("user's file was changed or deleted: %q, %v", data, err)
	}

	// A symlink somewhere else is also not ours.
	other := filepath.Join(home, "elsewhere")
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(userFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, userFile); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlink(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(userFile); err != nil || target != other {
		t.Fatalf("user's own symlink was removed or changed: %q, %v", target, err)
	}
}
