package qemu

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestSpaceWarning(t *testing.T) {
	for _, tt := range []struct {
		free uint64
		want string
	}{
		{8 << 30, ""},
		{lowSpace, ""},
		{3<<30 + 1<<29, "only 3.5 GiB free on this computer: the Machine pauses if it runs out"},
		{700 << 20, "only 700 MiB free on this computer: the Machine pauses if it runs out"},
	} {
		if got := spaceWarning(tt.free); got != tt.want {
			t.Errorf("spaceWarning(%d) = %q, want %q", tt.free, got, tt.want)
		}
	}
}

// A Machine whose directory doesn't exist yet is measured on the disk
// that will hold it, and a stopped Machine still reports its state.
func TestFreeSpaceOfAMissingDirectory(t *testing.T) {
	if _, ok := freeSpace(filepath.Join(t.TempDir(), "machines", "x")); !ok {
		t.Fatal("free space not measured on the parent directory")
	}
	b := &Backend{stateDir: t.TempDir()}
	st, err := b.Status(context.Background(), core.Environment{Name: "guest", Kind: core.Machine})
	if err != nil || st.State != core.StateStopped {
		t.Fatalf("status = %+v, %v", st, err)
	}
}
