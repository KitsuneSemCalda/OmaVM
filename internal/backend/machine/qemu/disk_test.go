package qemu

import (
	"context"
	"os/exec"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func virtualSize(t *testing.T, disk string) int64 {
	t.Helper()
	size, err := diskVirtualSize(context.Background(), disk)
	if err != nil {
		t.Fatal(err)
	}
	return size
}

// Machines created with the old 20 GiB disk, or sent back to a snapshot
// taken before the disk grew (a qcow2 snapshot keeps its own size), grow
// to 1 TiB the next time they start. It is a sparse file, so growing
// costs no host space; a disk is never shrunk.
func TestStartGrowsTheDiskToOneTebibyte(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	ctx := context.Background()
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{ID: "0123456789abcdef", Name: "old", Kind: core.Machine}
	disk := b.diskPath(b.key(env))
	if err := b.Create(ctx, env); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "resize", "--shrink", disk, "20G").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := exec.Command("qemu-img", "snapshot", "-c", "before", disk).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}

	b.growDisk(ctx, env)
	if got := virtualSize(t, disk); got != diskSize {
		t.Fatalf("virtual size %d, want %d", got, diskSize)
	}
	// Going back to the snapshot brings its 20 GiB back; the next start
	// grows it again.
	if out, err := exec.Command("qemu-img", "snapshot", "-a", "before", disk).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b.growDisk(ctx, env)
	if got := virtualSize(t, disk); got != diskSize {
		t.Fatalf("after going to an older snapshot: %d, want %d", got, diskSize)
	}

	if out, err := exec.Command("qemu-img", "resize", disk, "2T").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b.growDisk(ctx, env)
	if got := virtualSize(t, disk); got != 2*diskSize {
		t.Fatalf("a larger disk was shrunk to %d", got)
	}
}
