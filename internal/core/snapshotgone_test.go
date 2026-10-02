package core_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// A snapshot the registry lists but the disk lost (deleted by hand) must
// not get stuck in the list: removing it, or discarding it as the oldest,
// just forgets it. Before, the oldest such snapshot made every new
// snapshot report a failed discard.
func TestSnapshotGoneFromTheDiskIsForgotten(t *testing.T) {
	box := newFakeBackend("fake-box")
	machine := newFakeSnapshotBackend("fake-machine")
	svc := core.NewService(&memStore{}, box, machine)
	ctx := context.Background()
	if _, err := svc.Create(ctx, core.Environment{Name: "vm", Kind: core.Machine, Image: testISO(t)}); err != nil {
		t.Fatal(err)
	}
	limit := 2
	if _, err := svc.Configure(ctx, "vm", core.SettingsPatch{SnapshotLimit: &limit}); err != nil {
		t.Fatal(err)
	}
	a, _ := svc.CreateSnapshot(ctx, "vm", "a")
	b, _ := svc.CreateSnapshot(ctx, "vm", "b")

	machine.removeErr = fmt.Errorf("%w (qemu-img: snapshot not found)", core.ErrSnapshotGone)
	if _, err := svc.CreateSnapshot(ctx, "vm", "c"); err != nil {
		t.Fatalf("discarding an oldest snapshot that is already gone must not fail: %v", err)
	}
	snaps, _ := svc.ListSnapshots(ctx, "vm")
	if len(snaps) != 2 || snaps[0].ID != b.ID {
		t.Fatalf("expected b and c, got %+v (a=%s)", snaps, a.ID)
	}
	if err := svc.RemoveSnapshot(ctx, "vm", b.ID); err != nil {
		t.Fatalf("removing a snapshot already gone from the disk: %v", err)
	}
	if snaps, _ = svc.ListSnapshots(ctx, "vm"); len(snaps) != 1 {
		t.Fatalf("expected only c, got %+v", snaps)
	}
}
