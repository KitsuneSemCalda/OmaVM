package container

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestNewFallsBackToDockerWhenPodmanIsUnhealthy(t *testing.T) {
	bin := t.TempDir()
	writeRuntime(t, bin, "podman", "exit 1")
	writeRuntime(t, bin, "docker", "exit 0")
	t.Setenv("PATH", bin)

	if got := New().Name(); got != "docker" {
		t.Fatalf("expected docker fallback, got %q", got)
	}
}

func TestPersistedRuntimeOverridesCurrentDefault(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "runtime")
	writeRuntime(t, bin, "podman", `printf podman > "$OMAVM_TEST_LOG"`)
	writeRuntime(t, bin, "docker", `printf docker > "$OMAVM_TEST_LOG"`)
	t.Setenv("PATH", bin)
	t.Setenv("OMAVM_TEST_LOG", log)

	b := &Backend{runtime: "podman"}
	if err := b.Start(context.Background(), core.Environment{Name: "old", Backend: "docker"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "docker" {
		t.Fatalf("expected persisted docker runtime, got %q", got)
	}
}

func TestNewPrefersHealthyPodman(t *testing.T) {
	bin := t.TempDir()
	writeRuntime(t, bin, "podman", "exit 0")
	writeRuntime(t, bin, "docker", "exit 0")
	t.Setenv("PATH", bin)

	if got := New().Name(); got != "podman" {
		t.Fatalf("expected podman, got %q", got)
	}
}

func writeRuntime(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
