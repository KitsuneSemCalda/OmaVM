package qemu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestParseApplied(t *testing.T) {
	a := parseApplied([]string{"qemu-system-x86_64", "-m", "4096", "-smp", "3", "-cdrom", "/x.iso",
		"-device", "vhost-user-fs-pci,chardev=virtiofs,tag=omavm-share", "-device", "vhost-vsock-pci,guest-cid=70000"})
	if a != (applied{cpus: 3, memoryMiB: 4096, vsock: true, virtiofs: true, cdrom: true}) {
		t.Fatalf("parsed %+v", a)
	}
}

func TestSessionAdjustments(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "x.iso")
	if err := os.WriteFile(iso, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// What a default Machine with SSH gets at Start on AC power.
	base := applied{cpus: 2, memoryMiB: 2048, vsock: true, cdrom: true}
	env := core.Environment{Name: "vm", Kind: core.Machine, Image: iso}
	tests := []struct {
		name            string
		change          func(*core.Environment, *applied)
		restart, travel bool
	}{
		{"as started", func(*core.Environment, *applied) {}, false, false},
		{"travel mode on battery", func(_ *core.Environment, a *applied) { a.cpus = 1 }, false, true},
		{"pinned CPUs changed", func(e *core.Environment, _ *applied) { e.Settings.CPUs = 4 }, true, false},
		{"pinned CPUs are never travel mode", func(e *core.Environment, a *applied) { e.Settings.CPUs = 4; a.cpus = 2 }, true, false},
		{"memory changed", func(e *core.Environment, _ *applied) { e.Settings.MemoryMiB = 4096 }, true, false},
		{"ssh turned off", func(e *core.Environment, _ *applied) { e.Settings.SSHDisabled = true }, true, false},
		{"folder shared", func(e *core.Environment, _ *applied) { e.Settings.SharedPath = "/home/x" }, true, false},
		{"iso to disconnect", func(e *core.Environment, _ *applied) { e.Settings.DisconnectISO = true }, true, false},
		{"iso already gone", func(e *core.Environment, a *applied) { e.Settings.DisconnectISO = true; a.cdrom = false }, false, false},
	}
	for _, tt := range tests {
		e, a := env, base
		tt.change(&e, &a)
		restart, travel := sessionAdjustments(e, a, true)
		if restart != tt.restart || travel != tt.travel {
			t.Errorf("%s: restart=%t travel=%t, want %t %t", tt.name, restart, travel, tt.restart, tt.travel)
		}
	}
	// Without vhost-vsock on the host, no SSH channel is expected.
	a := base
	a.vsock = false
	if restart, _ := sessionAdjustments(env, a, false); restart {
		t.Error("a host without vsock asked for a restart")
	}
}
