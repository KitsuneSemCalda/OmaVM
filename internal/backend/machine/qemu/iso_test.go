package qemu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// Regression: an installed Machine whose ISO was deleted, moved or was on
// a USB stick no longer plugged in never started again — QEMU refuses a
// -cdrom it can't open — although the disk boots first anyway.
func TestStartWithoutTheInstallationMediaBootsTheDisk(t *testing.T) {
	dir, log := fakeQEMU(t, false)
	useVsock(t, false)
	b := &Backend{stateDir: dir}
	env := core.Environment{Name: "guest", Kind: core.Machine, Image: filepath.Join(t.TempDir(), "gone.iso")}
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatalf("Start with the ISO gone: %v", err)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "-cdrom") {
		t.Fatalf("passed QEMU an ISO that no longer exists: %s", data)
	}
}

func TestStartAttachesTheInstallationMedia(t *testing.T) {
	dir, log := fakeQEMU(t, false)
	useVsock(t, false)
	iso := filepath.Join(t.TempDir(), "fedora.iso")
	if err := os.WriteFile(iso, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Backend{stateDir: dir}
	env := core.Environment{Name: "guest", Kind: core.Machine, Image: iso}
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "-cdrom "+iso) {
		t.Fatalf("ISO not attached: %s", data)
	}
}

// Regression: a Machine opened from its launcher entry or `omavm open`
// never went to an empty workspace nor fullscreen: only the Experience
// Center placed the window. The viewer now does it, told by Open.
func TestViewerIsToldWhereAndHowToOpen(t *testing.T) {
	args := strings.Join(viewerArgs(core.Environment{Name: "vm", Kind: core.Machine}), " ")
	for _, want := range []string{"--empty-workspace true", "--fullscreen true", "--share-clipboard true", "--title vm — OmaVM", "--environment vm"} {
		if !strings.Contains(args, want) {
			t.Errorf("default viewer args %q lack %q", args, want)
		}
	}
	off := core.Environment{Name: "vm", Kind: core.Machine, Settings: core.EnvironmentSettings{EmptyWorkspaceDisabled: true, FullscreenDisabled: true}}
	args = strings.Join(viewerArgs(off), " ")
	for _, want := range []string{"--empty-workspace false", "--fullscreen false"} {
		if !strings.Contains(args, want) {
			t.Errorf("opted-out viewer args %q lack %q", args, want)
		}
	}
}

// With a 1 TB sparse disk the guest can outgrow the host's disk; QEMU
// then pauses the Machine (io-error). It must read as paused, which the
// card can Resume, with the reason, not as an unexplained error.
func TestIOErrorReadsAsPausedWithTheReason(t *testing.T) {
	status := ioErrorStatus(t.TempDir())
	if status.State != core.StatePaused || !strings.Contains(status.Detail, "Resume") {
		t.Fatalf("got %#v", status)
	}
	if diskSize != 1<<40 {
		t.Fatalf("Machines get a %d-byte disk, want 1 TiB", diskSize)
	}
}

// The viewer enforces a one-way clipboard; it learns it from its arguments.
func TestViewerArgsCarryTheClipboardDirection(t *testing.T) {
	for setting, want := range map[core.EnvironmentSettings]string{
		{}:                               "--share-clipboard true",
		{ClipboardDisabled: true}:        "--share-clipboard false",
		{ClipboardDirection: "to-host"}:  "--share-clipboard to-host",
		{ClipboardDirection: "to-guest"}: "--share-clipboard to-guest",
	} {
		args := strings.Join(viewerArgs(core.Environment{Name: "vm", Kind: core.Machine, Settings: setting}), " ")
		if !strings.Contains(args, want) {
			t.Errorf("%+v: args %q lack %q", setting, args, want)
		}
	}
}
