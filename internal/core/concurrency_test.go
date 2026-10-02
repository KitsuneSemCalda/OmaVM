package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// slowBackend blocks Create of the environment named "slow" until
// release is closed, like a Box pulling its image.
type slowBackend struct {
	*fakeBackend
	started chan struct{}
	release chan struct{}
}

func (b *slowBackend) Create(ctx context.Context, env core.Environment) error {
	if env.Name == "slow" {
		close(b.started)
		<-b.release
	}
	return nil
}

func (b *slowBackend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	return core.Status{State: core.StateStopped}, nil
}

func (b *slowBackend) Start(ctx context.Context, env core.Environment) error  { return nil }
func (b *slowBackend) Remove(ctx context.Context, env core.Environment) error { return nil }
func (b *slowBackend) Stop(ctx context.Context, env core.Environment) error   { return nil }
func (b *slowBackend) Open(ctx context.Context, env core.Environment) error   { return nil }

// A slow creation used to hold the whole registry: nothing else could be
// created, configured or removed until the image finished pulling.
func TestSlowCreateDoesNotHoldOtherEnvironments(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "environments.json")
	backend := &slowBackend{fakeBackend: newFakeBackend("box"), started: make(chan struct{}), release: make(chan struct{})}
	svc := core.NewService(&core.FileStore{Path: path}, backend, nil)

	done := make(chan error, 1)
	go func() {
		_, err := svc.Create(ctx, core.Environment{Name: "slow", Image: "fedora", Kind: core.Box})
		done <- err
	}()
	<-backend.started

	quick, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := svc.Create(quick, core.Environment{Name: "other", Image: "fedora", Kind: core.Box}); err != nil {
		t.Fatalf("creating another Box while one is slow: %v", err)
	}
	desc := "note"
	if _, err := svc.Configure(quick, "other", core.SettingsPatch{Description: &desc}); err != nil {
		t.Fatalf("configuring another Box while one is slow: %v", err)
	}
	if _, err := svc.Create(quick, core.Environment{Name: "slow", Image: "fedora", Kind: core.Box}); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("the name being created must be taken, got %v", err)
	}

	status, err := svc.Status(ctx, "slow")
	if err != nil || status.State != core.StateCreating {
		t.Fatalf("status while creating = %v, %v; want creating", status, err)
	}
	if err := svc.Start(quick, "slow"); err == nil {
		t.Fatal("Start of an environment still being created must wait or fail, not run")
	}
	if _, err := svc.Settings(ctx, "slow"); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Settings while creating = %v, want ErrBusy", err)
	}

	close(backend.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if status, err := svc.Status(ctx, "slow"); err != nil || status.State != core.StateStopped {
		t.Fatalf("status after creating = %v, %v", status, err)
	}
}

// A creation whose process died (killed, crashed, power cut) must not
// look like one still running forever: its lock died with it.
func TestInterruptedCreationCanBeRemoved(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "environments.json")
	registry := `{"version":1,"environments":[{"id":"abc","name":"half","image":"fedora","backend":"box","kind":"box","operation":"creating"}]}`
	if err := os.WriteFile(path, []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := newFakeBackend("box")
	svc := core.NewService(&core.FileStore{Path: path}, backend, nil)

	status, err := svc.Status(ctx, "half")
	if err != nil || status.State != core.StateError {
		t.Fatalf("status = %v, %v; want an error state", status, err)
	}
	if err := svc.Start(ctx, "half"); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("Start = %v, want it to say the creation was interrupted", err)
	}
	if err := svc.Remove(ctx, "half"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if envs, _ := svc.List(ctx); len(envs) != 0 {
		t.Fatalf("still registered: %+v", envs)
	}
}

// Operations on one environment wait for each other instead of racing
// in the engine.
func TestOperationsOnOneEnvironmentWaitForEachOther(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "environments.json")
	store := &core.FileStore{Path: path}
	svc := core.NewService(store, newFakeBackend("box"), nil)
	env, err := svc.Create(ctx, core.Environment{Name: "dev", Image: "fedora", Kind: core.Box})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.LockEnvironment(ctx, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := svc.Stop(short, "dev"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop while another operation holds the Box = %v, want it to wait", err)
	}
	unlock()
	if err := svc.Stop(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
}
