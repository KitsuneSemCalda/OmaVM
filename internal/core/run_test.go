package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// runBackend is a Machine whose guest powers off after a few status
// checks, recording what was asked of it.
type runBackend struct {
	*fakeEphemeralBackend
	checksUntilOff int
	forceStopped   []string
	opened         []string
}

func (r *runBackend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	if r.running[env.Name] {
		if r.checksUntilOff == 0 {
			r.running[env.Name] = false
		} else {
			r.checksUntilOff--
		}
	}
	return r.fakeEphemeralBackend.Status(ctx, env)
}
func (r *runBackend) Open(ctx context.Context, env core.Environment) error {
	r.opened = append(r.opened, env.Name)
	return nil
}
func (r *runBackend) Restart(ctx context.Context, env core.Environment) error { return nil }
func (r *runBackend) Pause(ctx context.Context, env core.Environment) error   { return nil }
func (r *runBackend) Resume(ctx context.Context, env core.Environment) error  { return nil }
func (r *runBackend) ForceStop(ctx context.Context, env core.Environment) error {
	r.forceStopped = append(r.forceStopped, env.Name)
	r.running[env.Name] = false
	return nil
}

func newRunService(checks int) (*core.Service, *runBackend) {
	machine := &runBackend{fakeEphemeralBackend: &fakeEphemeralBackend{fakeBackend: newFakeBackend("fake-machine"), ephemeral: map[string]bool{}}, checksUntilOff: checks}
	return core.NewService(&memStore{}, newFakeBackend("fake-box"), machine), machine
}

func TestRunEphemeralDeletesTheMachineWhenItShutsDown(t *testing.T) {
	svc, machine := newRunService(2)
	created, err := svc.RunEphemeral(context.Background(), core.Environment{Kind: core.Machine, Image: testISO(t)}, true, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !machine.ephemeral[created.Name] || len(machine.opened) != 1 {
		t.Fatalf("not started ephemeral and opened: %+v", machine)
	}
	if !created.Settings.LauncherDisabled {
		t.Fatal("an ephemeral Machine must not appear in the launcher")
	}
	if _, err := svc.Status(context.Background(), created.Name); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("still registered after shutting down: %v", err)
	}
	if len(machine.forceStopped) != 0 {
		t.Fatal("a guest that powered off must not be force-stopped")
	}
}

func TestRunEphemeralCancelledEndsAndDeletesIt(t *testing.T) {
	svc, machine := newRunService(1 << 30) // never powers off on its own
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	created, err := svc.RunEphemeral(ctx, core.Environment{Name: "try", Kind: core.Machine, Image: testISO(t)}, false, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the cancellation, got %v", err)
	}
	if len(machine.forceStopped) != 1 || machine.forceStopped[0] != "try" {
		t.Fatalf("not ended: %v", machine.forceStopped)
	}
	if _, err := svc.Status(context.Background(), created.Name); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("still registered: %v", err)
	}
}

func TestRunEphemeralIsForMachines(t *testing.T) {
	svc, _ := newRunService(0)
	if _, err := svc.RunEphemeral(context.Background(), core.Environment{Kind: core.Box, Image: "fedora"}, false, time.Millisecond); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected unsupported, got %v", err)
	}
}
