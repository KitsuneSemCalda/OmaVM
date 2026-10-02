package core_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestConcurrentCreatesPreserveRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environments.json")
	results := make(chan error, 12)
	for i := 0; i < cap(results); i++ {
		go func(i int) {
			svc := core.NewService(&core.FileStore{Path: path}, newFakeBackend("box"), nil)
			_, err := svc.Create(context.Background(), core.Environment{Name: fmt.Sprintf("box-%d", i), Image: "fedora:latest", Kind: core.Box})
			results <- err
		}(i)
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	envs, err := (&core.FileStore{Path: path}).Load()
	if err != nil || len(envs) != cap(results) {
		t.Fatalf("registry: %d environments, %v", len(envs), err)
	}
}

func TestRegistryLockCancellationAndRelease(t *testing.T) {
	store := &core.FileStore{Path: filepath.Join(t.TempDir(), "environments.json")}
	unlock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = (&core.FileStore{Path: store.Path}).Lock(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock: %v", err)
	}
	unlock()
	release, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestDisconnectISOSettings(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	for _, kind := range []core.EnvironmentKind{core.Box, core.Machine} {
		name := kind.String()
		env := core.Environment{Name: name, Image: "fedora:latest", Kind: kind}
		if kind == core.Machine {
			env.Image = testISO(t)
		}
		if _, err := svc.Create(ctx, env); err != nil {
			t.Fatal(err)
		}
		for _, value := range []bool{true, false} {
			got, err := svc.Configure(ctx, name, core.SettingsPatch{DisconnectISO: &value})
			if kind == core.Box {
				if !errors.Is(err, core.ErrUnsupported) {
					t.Fatalf("Box media: %v", err)
				}
			} else if err != nil || got.DisconnectISO != value {
				t.Fatalf("Machine media: %+v, %v", got, err)
			}
		}
	}
}

// A save keeps the previous registry as <path>.bak, so a registry damaged
// by a crash or power loss can be recovered instead of every command
// failing with "parse state file" and no way back.
func TestSaveKeepsPreviousRegistryAsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environments.json")
	store := &core.FileStore{Path: path}
	first := []core.Environment{{Name: "one", Kind: core.Box}}
	second := []core.Environment{{Name: "one", Kind: core.Box}, {Name: "two", Kind: core.Box}}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	backup := &core.FileStore{Path: path + ".bak"}
	got, err := backup.Load()
	if err != nil || len(got) != 1 || got[0].Name != "one" {
		t.Fatalf("backup = %+v, %v; want the registry before the last save", got, err)
	}
	if now, err := store.Load(); err != nil || len(now) != 2 {
		t.Fatalf("registry = %+v, %v; want the last save", now, err)
	}
}

func TestDamagedRegistryPointsToTheBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environments.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (&core.FileStore{Path: path}).Load()
	if err == nil || !strings.Contains(err.Error(), path+".bak") {
		t.Fatalf("Load of an empty registry = %v, want a pointer to %s.bak", err, path)
	}
}

// Another command holding the registry (a Box pulling its image) used to
// leave this one waiting in silence; OnWait lets the CLI say why.
func TestLockReportsWaitingOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "environments.json")
	holder := &core.FileStore{Path: path}
	release, err := holder.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waits := 0
	waiter := &core.FileStore{Path: path, OnWait: func() { waits++ }}
	acquired := make(chan func())
	go func() {
		unlock, err := waiter.Lock(context.Background())
		if err != nil {
			t.Error(err)
		}
		acquired <- unlock
	}()
	time.Sleep(200 * time.Millisecond)
	release()
	unlock := <-acquired
	unlock()
	if waits != 1 {
		t.Fatalf("OnWait called %d times, want once", waits)
	}

	// Uncontended: no notice.
	quiet := 0
	free := &core.FileStore{Path: path, OnWait: func() { quiet++ }}
	unlock, err = free.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if quiet != 0 {
		t.Fatalf("OnWait called %d times without contention", quiet)
	}
}

// The registry is still written as the bare list an older OmaVM reads,
// but a newer format is refused rather than rewritten without what it
// added.
func TestRegistryFormats(t *testing.T) {
	store := &core.FileStore{Path: filepath.Join(t.TempDir(), "environments.json")}
	if err := store.Save([]core.Environment{{ID: "1", Name: "dev", Image: "fedora", Kind: core.Box}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(store.Path)
	if !strings.HasPrefix(string(data), "[") {
		t.Fatalf("saved in a format an older OmaVM can't read:\n%s", data)
	}

	envelope := `{"version": 1, "environments": [{"id":"1","name":"dev","image":"fedora","kind":"box"}]}`
	if err := os.WriteFile(store.Path, []byte(envelope), 0o644); err != nil {
		t.Fatal(err)
	}
	if envs, err := store.Load(); err != nil || len(envs) != 1 || envs[0].Name != "dev" {
		t.Fatalf("versioned format: %v, %v", envs, err)
	}

	newer := `{"version": 2, "environments": []}`
	if err := os.WriteFile(store.Path, []byte(newer), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "newer OmaVM") {
		t.Fatalf("a newer format must be refused, got %v", err)
	}
}
