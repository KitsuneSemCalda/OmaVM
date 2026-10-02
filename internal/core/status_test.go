package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// batchBackend answers every status in one Statuses call and fails any
// single Status call, so a test notices if the Core falls back to asking
// one by one.
type batchBackend struct {
	*fakeBackend
	calls    int
	statuses map[string]core.Status
	err      error
}

func (b *batchBackend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	return core.Status{}, errors.New("Status must not be called on a StatusLister")
}

func (b *batchBackend) Statuses(ctx context.Context, envs []core.Environment) (map[string]core.Status, error) {
	b.calls++
	return b.statuses, b.err
}

type integrationBackend struct{ *fakeBackend }

func (integrationBackend) Integration(ctx context.Context, env core.Environment) (core.IntegrationReport, error) {
	return core.IntegrationReport{GuestAgent: "connected"}, nil
}

func TestListWithStatusBatchesAndFallsBack(t *testing.T) {
	store := &memStore{envs: []core.Environment{
		{ID: "b1", Name: "one", Kind: core.Box},
		{ID: "b2", Name: "two", Kind: core.Box},
		{ID: "b3", Name: "gone", Kind: core.Box},
		{ID: "m1", Name: "vm", Kind: core.Machine},
	}}
	box := &batchBackend{fakeBackend: newFakeBackend("box"), statuses: map[string]core.Status{
		"b1": {State: core.StateRunning},
		"b2": {State: core.StateStopped},
	}}
	machine := integrationBackend{newFakeBackend("machine")}
	machine.running["vm"] = true
	svc := core.NewService(store, box, machine)

	states, err := svc.ListWithStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if box.calls != 1 {
		t.Fatalf("Statuses called %d times, want once for all Boxes", box.calls)
	}
	want := map[string]core.State{"one": core.StateRunning, "two": core.StateStopped, "gone": core.StateUnknown, "vm": core.StateRunning}
	for _, s := range states {
		if s.Status.State != want[s.Name] {
			t.Errorf("%s: state %q, want %q", s.Name, s.Status.State, want[s.Name])
		}
		if (s.Integration != nil) != (s.Kind == core.Machine) {
			t.Errorf("%s: integration %v, want it only for the Machine", s.Name, s.Integration)
		}
	}
	if states[0].Name != "one" || states[3].Name != "vm" {
		t.Fatalf("order changed: %v", states)
	}
}

func TestListWithStatusReportsEngineFailureAsStatus(t *testing.T) {
	store := &memStore{envs: []core.Environment{{ID: "b1", Name: "one", Kind: core.Box}}}
	box := &batchBackend{fakeBackend: newFakeBackend("box"), err: errors.New("podman ps: exit status 125")}
	svc := core.NewService(store, box, nil)

	states, err := svc.ListWithStatus(context.Background())
	if err != nil {
		t.Fatalf("an engine failure must not fail the list: %v", err)
	}
	if states[0].Status.State != core.StateUnknown || states[0].Status.Detail == "" {
		t.Fatalf("got %#v, want unknown with the engine's error", states[0].Status)
	}
}
