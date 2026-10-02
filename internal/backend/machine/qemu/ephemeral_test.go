package qemu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestEphemeralStartDiscardsWritesNextToTheDisk(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	t.Setenv("OMAVM_TEST_ARGS", capture)
	t.Setenv("PATH", dir)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAVM_TEST_ARGS\"\necho \"TMPDIR=$TMPDIR\" >> \"$OMAVM_TEST_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Backend{stateDir: dir}
	env := core.Environment{Name: "guest", Kind: core.Machine}

	if err := b.Start(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(capture); strings.Contains(string(data), "-snapshot\n") {
		t.Fatalf("a normal start must keep changes: %s", data)
	}
	if err := b.StartEphemeral(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(capture)
	if !strings.Contains(string(data), "-snapshot\n") {
		t.Fatalf("ephemeral start without -snapshot: %s", data)
	}
	if !strings.Contains(string(data), "TMPDIR="+b.dir(b.key(env))) {
		t.Fatalf("the overlay must live next to the Machine's disk: %s", data)
	}
}

func TestEphemeralSessionIsReportedAndBlocksSnapshots(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name, "-snapshot")
	calls := recordQMP(t, b.qmpPath(env.Name))

	st, err := b.Status(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ephemeral {
		t.Fatalf("status does not say changes will be discarded: %+v", st)
	}
	if _, err := b.CreateSnapshot(context.Background(), env, "x"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("snapshot in an ephemeral session: %v", err)
	}
	if err := b.RemoveSnapshot(context.Background(), env, "x"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("snapshot removal in an ephemeral session: %v", err)
	}
	for _, c := range calls() {
		if strings.HasPrefix(c.Execute, "blockdev-snapshot") {
			t.Fatalf("the overlay was snapshotted: %+v", c)
		}
	}
	// Already running without keeping changes: nothing to do.
	if err := b.StartEphemeral(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

// A Machine already keeping its changes must not be reported as started
// without keeping them.
func TestEphemeralStartRefusesARunningNormalSession(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	err := b.StartEphemeral(context.Background(), env)
	if !errors.Is(err, core.ErrInvalidInput) || !strings.Contains(err.Error(), "shut it down first") {
		t.Fatalf("expected a shut-down-first error, got %v", err)
	}
	if b.isEphemeral(env.Name) {
		t.Fatal("normal session reported as ephemeral")
	}
}
