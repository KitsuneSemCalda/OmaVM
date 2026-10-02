package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

type fakeEphemeralBackend struct {
	*fakeBackend
	ephemeral map[string]bool
}

func (f *fakeEphemeralBackend) StartEphemeral(ctx context.Context, env core.Environment) error {
	f.running[env.Name] = true
	f.ephemeral[env.Name] = true
	return nil
}

func TestStartEphemeral(t *testing.T) {
	box := newFakeBackend("fake-box")
	machine := &fakeEphemeralBackend{fakeBackend: newFakeBackend("fake-machine"), ephemeral: map[string]bool{}}
	svc := core.NewService(&memStore{}, box, machine)
	ctx := context.Background()
	if _, err := svc.Create(ctx, core.Environment{Name: "vm", Kind: core.Machine, Image: testISO(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, core.Environment{Name: "dev", Kind: core.Box, Image: "fedora"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartEphemeral(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	if !machine.ephemeral["vm"] {
		t.Fatal("the backend was not asked for an ephemeral session")
	}
	if err := svc.StartEphemeral(ctx, "dev"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("a Box can't start without keeping changes, got %v", err)
	}
	if box.running["dev"] {
		t.Fatal("the Box was started anyway")
	}
	if err := svc.StartEphemeral(ctx, "missing"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}
