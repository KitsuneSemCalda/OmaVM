package qemu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestStatusFromQMP(t *testing.T) {
	tests := map[string]core.State{
		"running":        core.StateRunning,
		"paused":         core.StatePaused,
		"suspended":      core.StatePaused,
		"inmigrate":      core.StateStarting,
		"shutdown":       core.StateStopping,
		"guest-panicked": core.StateError,
		"colo":           core.StateUnknown,
	}
	for input, want := range tests {
		if got := statusFromQMP(input).State; got != want {
			t.Errorf("statusFromQMP(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCreateRejectsNameTooLongForSocketPath(t *testing.T) {
	b := &Backend{stateDir: "/home/user/.local/state/omavm/machines"}
	// "<stateDir>/<name>/virtiofs.sock": one more "/" than virtiofsPath("").
	fits := strings.Repeat("a", maxSocketPath-len(b.virtiofsPath(""))-1)
	if err := b.checkNameLength(fits); err != nil {
		t.Fatalf("name of %d bytes should fit: %v", len(fits), err)
	}
	err := b.Create(context.Background(), core.Environment{Name: fits + "a"})
	if !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("Create with a too-long name = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("at most %d bytes", len(fits))) {
		t.Fatalf("error should state the limit of %d bytes: %v", len(fits), err)
	}
}

func TestDisplayArgs(t *testing.T) {
	const memfd = " -object memory-backend-memfd,id=mem,size=2048M,share=on -machine memory-backend=mem"
	tests := map[string]struct {
		openGL, vulkan, virtiofs bool
		want                     string
	}{
		"no host gpu":   {false, false, false, "-device virtio-vga -display dbus,p2p=yes"},
		"no gpu, fs":    {false, false, true, "-device virtio-vga -display dbus,p2p=yes" + memfd},
		"vulkan w/o gl": {false, true, false, "-device virtio-vga -display dbus,p2p=yes"},
		"opengl":        {true, false, false, "-device virtio-vga-gl -display dbus,p2p=yes,gl=on"},
		"opengl, fs":    {true, false, true, "-device virtio-vga-gl -display dbus,p2p=yes,gl=on" + memfd},
		"vulkan":        {true, true, false, "-device virtio-vga-gl,blob=on,hostmem=4G,venus=on -display dbus,p2p=yes,gl=on" + memfd},
	}
	for name, tt := range tests {
		if got := strings.Join(displayArgs(tt.openGL, tt.vulkan, 2048, tt.virtiofs), " "); got != tt.want {
			t.Errorf("%s: got %q, want %q", name, got, tt.want)
		}
	}
}
