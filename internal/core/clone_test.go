package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

type fakeCloneBackend struct {
	*fakeSnapshotBackend
	cloned  map[string]string // clone -> source
	failErr error
}

func (f *fakeCloneBackend) Clone(ctx context.Context, source, clone core.Environment) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.cloned[clone.Name] = source.Name
	return nil
}

func TestClone(t *testing.T) {
	machine := &fakeCloneBackend{fakeSnapshotBackend: newFakeSnapshotBackend("fake-machine"), cloned: map[string]string{}}
	svc := core.NewService(&memStore{}, newFakeBackend("fake-box"), machine)
	ctx := context.Background()
	color := "green"
	if _, err := svc.Create(ctx, core.Environment{Name: "vm", Kind: core.Machine, Image: testISO(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Configure(ctx, "vm", core.SettingsPatch{Color: &color}); err != nil {
		t.Fatal(err)
	}
	snap, _ := svc.CreateSnapshot(ctx, "vm", "clean")

	clone, err := svc.Clone(ctx, "vm", " vm copy ")
	if err != nil {
		t.Fatal(err)
	}
	if clone.Name != "vm copy" || clone.Kind != core.Machine || clone.Settings.Color != "green" || clone.ID == "" {
		t.Fatalf("clone: %+v", clone)
	}
	if machine.cloned["vm copy"] != "vm" {
		t.Fatalf("backend not asked: %v", machine.cloned)
	}
	if snaps, _ := svc.ListSnapshots(ctx, "vm copy"); len(snaps) != 1 || snaps[0].ID != snap.ID {
		t.Fatalf("the copied disk carries the snapshots, so the list must too: %+v", snaps)
	}

	if _, err := svc.Clone(ctx, "vm", "vm copy"); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("taken name: %v", err)
	}
	if _, err := svc.Clone(ctx, "nope", "x"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	if _, err := svc.Clone(ctx, "vm", "a/b"); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("bad name: %v", err)
	}
	machine.failErr = core.Invalidf("shut down vm first")
	if _, err := svc.Clone(ctx, "vm", "second"); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("backend refusal: %v", err)
	}
	if _, err := svc.Status(ctx, "second"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("a failed clone must leave nothing registered: %v", err)
	}
}

func TestCloneUnsupportedBackend(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, core.Environment{Name: "dev", Kind: core.Box, Image: "fedora"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Clone(ctx, "dev", "dev2"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected unsupported, got %v", err)
	}
}

func TestUpdateIsForBoxes(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, core.Environment{Name: "vm", Kind: core.Machine, Image: testISO(t)}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Update(ctx, "vm"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("a Desktop updates from inside, got %v", err)
	}
}
