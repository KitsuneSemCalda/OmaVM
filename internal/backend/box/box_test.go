package box

import (
	"context"
	"errors"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

type fakeBackend struct {
	name  string
	calls []string
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) record(call string) error {
	f.calls = append(f.calls, call)
	return nil
}
func (f *fakeBackend) Create(context.Context, core.Environment) error { return f.record("create") }
func (f *fakeBackend) Start(context.Context, core.Environment) error  { return f.record("start") }
func (f *fakeBackend) Open(context.Context, core.Environment) error   { return f.record("open") }
func (f *fakeBackend) Stop(context.Context, core.Environment) error   { return f.record("stop") }
func (f *fakeBackend) Status(context.Context, core.Environment) (core.Status, error) {
	f.calls = append(f.calls, "status")
	return core.Status{}, nil
}
func (f *fakeBackend) Exec(context.Context, core.Environment, []string) error {
	return f.record("exec")
}
func (f *fakeBackend) Remove(context.Context, core.Environment) error { return f.record("remove") }

func TestNewBoxesUseDistrobox(t *testing.T) {
	primary := &fakeBackend{name: "distrobox"}
	legacy := &fakeBackend{name: "podman"}
	b := New(primary, legacy)

	if err := b.Create(context.Background(), core.Environment{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if len(primary.calls) != 1 || primary.calls[0] != "create" || len(legacy.calls) != 0 {
		t.Fatalf("unexpected routing: primary=%v legacy=%v", primary.calls, legacy.calls)
	}
}

type fakeAppExporterBackend struct {
	*fakeBackend
	apps []core.App
}

func (f *fakeAppExporterBackend) ListApps(context.Context, core.Environment) ([]core.App, error) {
	f.calls = append(f.calls, "listApps")
	return f.apps, nil
}
func (f *fakeAppExporterBackend) ExportApp(context.Context, core.Environment, string) error {
	return f.record("exportApp")
}
func (f *fakeAppExporterBackend) UnexportApp(context.Context, core.Environment, string) error {
	return f.record("unexportApp")
}

func TestAppExporterRoutesToDistrobox(t *testing.T) {
	primary := &fakeAppExporterBackend{fakeBackend: &fakeBackend{name: "distrobox"}, apps: []core.App{{ID: "a", Name: "A"}}}
	legacy := &fakeBackend{name: "podman"}
	b := New(primary, legacy)

	apps, err := b.ListApps(context.Background(), core.Environment{Name: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].ID != "a" {
		t.Fatalf("unexpected apps: %+v", apps)
	}
	if err := b.ExportApp(context.Background(), core.Environment{Name: "new"}, "a"); err != nil {
		t.Fatal(err)
	}
	if err := b.UnexportApp(context.Background(), core.Environment{Name: "new"}, "a"); err != nil {
		t.Fatal(err)
	}
	want := []string{"listApps", "exportApp", "unexportApp"}
	if len(primary.calls) != len(want) {
		t.Fatalf("expected calls %v, got %v", want, primary.calls)
	}
	for i := range want {
		if primary.calls[i] != want[i] {
			t.Fatalf("expected calls %v, got %v", want, primary.calls)
		}
	}
}

func TestAppExporterUnsupportedOnLegacyBoxes(t *testing.T) {
	primary := &fakeAppExporterBackend{fakeBackend: &fakeBackend{name: "distrobox"}}
	legacy := &fakeBackend{name: "podman"} // does not implement core.AppExporter
	b := New(primary, legacy)

	env := core.Environment{Backend: "podman"}
	if _, err := b.ListApps(context.Background(), env); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if err := b.ExportApp(context.Background(), env, "a"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if err := b.UnexportApp(context.Background(), env, "a"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestLegacyBoxesKeepTheirContainerBackend(t *testing.T) {
	for _, backendName := range []string{"podman", "docker"} {
		t.Run(backendName, func(t *testing.T) {
			primary := &fakeBackend{name: "distrobox"}
			legacy := &fakeBackend{name: backendName}
			b := New(primary, legacy)

			if err := b.Start(context.Background(), core.Environment{Backend: backendName}); err != nil {
				t.Fatal(err)
			}
			if len(legacy.calls) != 1 || legacy.calls[0] != "start" || len(primary.calls) != 0 {
				t.Fatalf("unexpected routing: primary=%v legacy=%v", primary.calls, legacy.calls)
			}
		})
	}
}
