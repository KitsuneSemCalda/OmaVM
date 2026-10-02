package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// fakeBackend is an in-memory Backend used to test Service without
// starting real containers or QEMU machines.
type fakeBackend struct {
	name    string
	created map[string]bool
	running map[string]bool
	removed map[string]bool
	execErr error
}

func newFakeBackend(name string) *fakeBackend {
	return &fakeBackend{
		name:    name,
		created: map[string]bool{},
		running: map[string]bool{},
		removed: map[string]bool{},
	}
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Create(ctx context.Context, env core.Environment) error {
	f.created[env.Name] = true
	return nil
}

func (f *fakeBackend) Start(ctx context.Context, env core.Environment) error {
	f.running[env.Name] = true
	return nil
}

func (f *fakeBackend) Open(ctx context.Context, env core.Environment) error {
	f.running[env.Name] = true
	return nil
}

func (f *fakeBackend) Stop(ctx context.Context, env core.Environment) error {
	f.running[env.Name] = false
	return nil
}

func (f *fakeBackend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	if f.running[env.Name] {
		return core.Status{State: core.StateRunning}, nil
	}
	return core.Status{State: core.StateStopped}, nil
}

func (f *fakeBackend) Exec(ctx context.Context, env core.Environment, args []string) error {
	return f.execErr
}

func (f *fakeBackend) Remove(ctx context.Context, env core.Environment) error {
	f.removed[env.Name] = true
	delete(f.created, env.Name)
	return nil
}

// memStore is an in-memory Store for tests.
type memStore struct {
	envs    []core.Environment
	saveErr error
	// savesBeforeErr lets that many saves succeed before saveErr applies.
	savesBeforeErr int
}

func (m *memStore) Lock(context.Context) (func(), error) { return func() {}, nil }

func (m *memStore) LockEnvironment(context.Context, string) (func(), error) {
	return func() {}, nil
}

func (m *memStore) TryLockEnvironment(string) (func(), bool, error) {
	return func() {}, true, nil
}

func (m *memStore) Load() ([]core.Environment, error) {
	out := make([]core.Environment, len(m.envs))
	copy(out, m.envs)
	return out, nil
}

func (m *memStore) Save(envs []core.Environment) error {
	if m.saveErr != nil && m.savesBeforeErr > 0 {
		m.savesBeforeErr--
	} else if m.saveErr != nil {
		return m.saveErr
	}
	m.envs = envs
	return nil
}

// fakeSnapshotBackend adds SnapshotManager on top of fakeBackend so tests
// can exercise a backend that supports snapshots (e.g. Machine/QEMU)
// separately from one that doesn't (e.g. Box today).
type fakeSnapshotBackend struct {
	*fakeBackend
	snapshots       map[string]bool
	createErr       error
	removeErr       error
	crashConsistent bool
}

func newFakeSnapshotBackend(name string) *fakeSnapshotBackend {
	return &fakeSnapshotBackend{fakeBackend: newFakeBackend(name), snapshots: map[string]bool{}}
}

func (f *fakeSnapshotBackend) CreateSnapshot(ctx context.Context, env core.Environment, tag string) (bool, error) {
	if f.createErr != nil {
		return false, f.createErr
	}
	f.snapshots[tag] = true
	return f.crashConsistent, nil
}

func (f *fakeSnapshotBackend) GoToSnapshot(ctx context.Context, env core.Environment, tag string) error {
	if !f.snapshots[tag] {
		return errors.New("unknown snapshot")
	}
	return nil
}

func (f *fakeSnapshotBackend) RemoveSnapshot(ctx context.Context, env core.Environment, tag string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	if !f.snapshots[tag] {
		return errors.New("unknown snapshot")
	}
	delete(f.snapshots, tag)
	return nil
}

func newTestService() (*core.Service, *fakeBackend, *fakeBackend) {
	box := newFakeBackend("fake-box")
	machine := newFakeBackend("fake-machine")
	svc := core.NewService(&memStore{}, box, machine)
	return svc, box, machine
}

// testISO returns a stand-in installation medium: Machines can't be created
// without one.
func testISO(t *testing.T) string {
	t.Helper()
	iso := filepath.Join(t.TempDir(), "system.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	return iso
}

// A Machine without installation media has an empty disk and nothing to
// boot, and there's no command to attach an ISO afterwards.
func TestCreateMachineRequiresInstallationMedia(t *testing.T) {
	svc, _, machine := newTestService()
	_, err := svc.Create(context.Background(), core.Environment{Name: "desktop", Kind: core.Machine})
	if !errors.Is(err, core.ErrInvalidInput) || !strings.Contains(err.Error(), "ISO") {
		t.Fatalf("Create without an ISO = %v, want ErrInvalidInput asking for one", err)
	}
	if machine.created["desktop"] {
		t.Fatal("backend must not create a Machine that cannot boot")
	}
}

func TestCreateStartStopStatus(t *testing.T) {
	ctx := context.Background()
	svc, box, _ := newTestService()

	env, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora", Kind: core.Box})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !box.created["radic"] {
		t.Fatal("expected backend Create to be called")
	}
	if env.Backend != "fake-box" {
		t.Fatalf("expected Backend to be set from backend.Name(), got %q", env.Backend)
	}
	if env.ID == "" {
		t.Fatal("expected a generated ID")
	}

	status, err := svc.Status(ctx, "radic")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != core.StateStopped {
		t.Fatalf("expected stopped before Start, got %s", status.State)
	}

	if err := svc.Start(ctx, "radic"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	status, err = svc.Status(ctx, "radic")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != core.StateRunning {
		t.Fatalf("expected running after Start, got %s", status.State)
	}

	if err := svc.Stop(ctx, "radic"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	status, err = svc.Status(ctx, "radic")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != core.StateStopped {
		t.Fatalf("expected stopped after Stop, got %s", status.State)
	}
}

func TestConfigureMachineSettings(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	iso := filepath.Join(t.TempDir(), "system.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: iso, Kind: core.Machine}); err != nil {
		t.Fatal(err)
	}
	initial, err := svc.Settings(ctx, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if initial.CPUs != 2 || initial.MemoryMiB != 2048 {
		t.Fatalf("unexpected defaults: %+v", initial)
	}
	description, cpus, memory := "Work desktop", 4, 4096
	sharedPath := t.TempDir()
	sharedReadOnly := true
	updated, err := svc.Configure(ctx, "desktop", core.SettingsPatch{
		Description:    &description,
		CPUs:           &cpus,
		MemoryMiB:      &memory,
		SharedPath:     &sharedPath,
		SharedReadOnly: &sharedReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != description || updated.CPUs != cpus || updated.MemoryMiB != memory || updated.SharedPath != sharedPath || !updated.SharedReadOnly {
		t.Fatalf("settings were not persisted: %+v", updated)
	}
}

func TestClipboardAndTravelModeDefaultOnAndMachineOnly(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatal(err)
	}
	initial, err := svc.Settings(ctx, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if initial.ClipboardDisabled || initial.TravelModeDisabled {
		t.Fatalf("expected clipboard sharing and travel mode on by default, got %+v", initial)
	}

	no := false
	updated, err := svc.Configure(ctx, "desktop", core.SettingsPatch{ShareClipboard: &no, TravelMode: &no})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ClipboardDisabled || !updated.TravelModeDisabled {
		t.Fatalf("expected opt-out to persist, got %+v", updated)
	}

	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatal(err)
	}
	// On a Box the same opt-out covers programs copying from its
	// terminal (OSC 52).
	boxSettings, err := svc.Configure(ctx, "radic", core.SettingsPatch{ShareClipboard: &no})
	if err != nil {
		t.Fatalf("clipboard setting on a Box: %v", err)
	}
	if !boxSettings.ClipboardDisabled {
		t.Fatalf("expected the Box's clipboard opt-out to persist, got %+v", boxSettings)
	}
	yes := true
	if boxSettings, err = svc.Configure(ctx, "radic", core.SettingsPatch{TravelMode: &no}); err != nil || !boxSettings.TravelModeDisabled {
		t.Fatalf("travel mode opt-out on a Box: %+v, %v", boxSettings, err)
	}
	_ = yes
}

func TestVulkanDefaultOnAndMachineOnly(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatal(err)
	}
	no, yes := false, true
	updated, err := svc.Configure(ctx, "desktop", core.SettingsPatch{Vulkan: &no})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.VulkanDisabled {
		t.Fatalf("expected Vulkan opt-out to persist, got %+v", updated)
	}
	if updated, err = svc.Configure(ctx, "desktop", core.SettingsPatch{Vulkan: &yes}); err != nil || updated.VulkanDisabled {
		t.Fatalf("expected Vulkan back on, got %+v, %v", updated, err)
	}
	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Configure(ctx, "radic", core.SettingsPatch{Vulkan: &yes}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for Vulkan on a Box, got %v", err)
	}
}

func TestOpenInEmptyWorkspaceAppliesToBothKinds(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatal(err)
	}
	initial, err := svc.Settings(ctx, "radic")
	if err != nil {
		t.Fatal(err)
	}
	if initial.EmptyWorkspaceDisabled {
		t.Fatalf("expected on by default, got %+v", initial)
	}

	no := false
	for _, name := range []string{"desktop", "radic"} {
		updated, err := svc.Configure(ctx, name, core.SettingsPatch{OpenInEmptyWorkspace: &no})
		if err != nil {
			t.Fatalf("Configure(%s): %v", name, err)
		}
		if !updated.EmptyWorkspaceDisabled {
			t.Fatalf("expected the opt-out to persist for %s, got %+v", name, updated)
		}
	}
}

func TestSSHIsOnByDefaultAndMachineOnly(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatal(err)
	}
	settings, err := svc.Settings(ctx, "desktop")
	if err != nil || settings.SSHDisabled {
		t.Fatalf("expected SSH on by default: %+v, %v", settings, err)
	}
	off := false
	if settings, err = svc.Configure(ctx, "desktop", core.SettingsPatch{SSH: &off}); err != nil || !settings.SSHDisabled {
		t.Fatalf("SSH opt-out: %+v, %v", settings, err)
	}
	if _, err := svc.Configure(ctx, "radic", core.SettingsPatch{SSH: &off}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for SSH on a Box, got %v", err)
	}
	// The fake backends don't implement RemoteShell.
	if err := svc.SSH(ctx, "radic", "", nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

// Regression: a Box without an image reached `distrobox create --image ""`,
// which never returns, so `omavm create --name x` hung forever.
func TestCreateBoxRequiresImage(t *testing.T) {
	svc, box, _ := newTestService()
	for _, image := range []string{"", "   "} {
		_, err := svc.Create(context.Background(), core.Environment{Name: "radic", Image: image, Kind: core.Box})
		if !errors.Is(err, core.ErrInvalidInput) {
			t.Fatalf("image %q: expected ErrInvalidInput, got %v", image, err)
		}
	}
	if len(box.created) != 0 {
		t.Fatalf("the backend was asked to create a Box without an image: %+v", box.created)
	}
}

// Regression: a Machine named "--json" was created, but `omavm status
// --json` read its name as the option and could not reach it.
func TestCreateRejectsNamesThatLookLikeOptions(t *testing.T) {
	svc, box, _ := newTestService()
	for _, name := range []string{"-x", "--json", " --json"} {
		_, err := svc.Create(context.Background(), core.Environment{Name: name, Image: "fedora:latest", Kind: core.Box})
		if !errors.Is(err, core.ErrInvalidInput) {
			t.Fatalf("name %q: expected ErrInvalidInput, got %v", name, err)
		}
	}
	if len(box.created) != 0 {
		t.Fatalf("backend asked to create %+v", box.created)
	}
	if _, err := svc.Create(context.Background(), core.Environment{Name: "a-b", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("a dash inside the name is fine: %v", err)
	}
}

func TestCreateDuplicateNameFails(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()

	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box})
	if !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

// TestCreateRollsBackBackendWhenRegistrySaveFails guards against leaking
// an orphaned backend resource (a container, a VM disk) that exists but
// is invisible to the CLI/GUI because the registry never learned about
// it — the same "never leave a half-succeeded operation invisible" rule
// as the snapshot retention fix, applied to Create's own registry write.
func TestCreateRollsBackBackendWhenRegistrySaveFails(t *testing.T) {
	ctx := context.Background()
	box := newFakeBackend("fake-box")
	store := &memStore{}
	svc := core.NewService(store, box, newFakeBackend("fake-machine"))

	// The reservation is saved; recording the finished creation fails.
	store.saveErr = errors.New("disk full")
	store.savesBeforeErr = 1
	_, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box})
	if err == nil {
		t.Fatal("expected the registry save failure to surface as an error")
	}
	// fakeBackend.Remove clears its own "created" bookkeeping, so
	// removed=true means Create created the resource and then undid it.
	if !box.removed["radic"] {
		t.Fatal("expected Create to roll back the backend resource when the registry save fails")
	}

	// With the disk still full, the reservation can't be dropped either:
	// it stays, marked as an unfinished creation, never as a usable one.
	store.saveErr = nil
	envs, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, env := range envs {
		if env.Operation != core.OperationCreating {
			t.Fatalf("a rolled-back create left a usable environment: %+v", env)
		}
	}
}

func TestCreateTouchesNoBackendWhenTheNameCantBeReserved(t *testing.T) {
	box := newFakeBackend("fake-box")
	store := &memStore{saveErr: errors.New("disk full")}
	svc := core.NewService(store, box, nil)
	if _, err := svc.Create(context.Background(), core.Environment{Name: "radic", Image: "fedora", Kind: core.Box}); err == nil {
		t.Fatal("expected an error")
	}
	if box.created["radic"] {
		t.Fatal("the backend created a resource the registry never reserved")
	}
}

func TestCreateHardwareSettingsRequireMachine(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()

	_, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box, Settings: core.EnvironmentSettings{CPUs: 4}})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for a Box with CPUs set, got %v", err)
	}

	_, err = svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine, Settings: core.EnvironmentSettings{CPUs: 128}})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected an out-of-range CPUs error, got %v", err)
	}

	env, err := svc.Create(ctx, core.Environment{Name: "desktop2", Image: testISO(t), Kind: core.Machine, Settings: core.EnvironmentSettings{CPUs: 4, MemoryMiB: 4096}})
	if err != nil {
		t.Fatalf("Create with valid hardware settings: %v", err)
	}
	if env.Settings.CPUs != 4 || env.Settings.MemoryMiB != 4096 {
		t.Fatalf("hardware settings were not persisted: %+v", env.Settings)
	}
}

// TestConfigureUnrelatedFieldDoesNotPinHardwareDefaults guards against a
// real bug found in this codebase: Configure used to seed its working
// copy from EffectiveSettings() (which substitutes CPUs/MemoryMiB/
// SnapshotLimit defaults for display) and then unconditionally persisted
// that whole copy back — so saving only the description silently pinned
// CPUs/MemoryMiB to their resolved defaults, permanently disabling
// Travel Mode's automatic reduction on battery (docs/TODO.md P2). List
// (unlike Settings/Configure's return value) reflects the raw, persisted
// settings, so it's the only way to observe whether a field was really
// left unpinned.
func TestConfigureUnrelatedFieldDoesNotPinHardwareDefaults(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	description := "just a note"
	settings, err := svc.Configure(ctx, "desktop", core.SettingsPatch{Description: &description})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// The returned settings are still resolved for display.
	if settings.CPUs != 2 || settings.MemoryMiB != 2048 || settings.SnapshotLimit != 10 {
		t.Fatalf("expected Configure's return value to show resolved defaults, got %+v", settings)
	}

	envs, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	env, idx := findEnvironmentForTest(envs, "desktop")
	if idx == -1 {
		t.Fatal("desktop not found in List")
	}
	if env.Settings.CPUs != 0 || env.Settings.MemoryMiB != 0 || env.Settings.SnapshotLimit != 0 {
		t.Fatalf("expected raw settings to stay unpinned (zero) after an unrelated Configure, got %+v", env.Settings)
	}

	// An explicit pin must still persist and survive later unrelated saves.
	cpus := 4
	if _, err := svc.Configure(ctx, "desktop", core.SettingsPatch{CPUs: &cpus}); err != nil {
		t.Fatalf("Configure with explicit CPUs: %v", err)
	}
	secondDescription := "second edit"
	if _, err := svc.Configure(ctx, "desktop", core.SettingsPatch{Description: &secondDescription}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	envs, err = svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	env, idx = findEnvironmentForTest(envs, "desktop")
	if idx == -1 {
		t.Fatal("desktop not found in List")
	}
	if env.Settings.CPUs != 4 {
		t.Fatalf("expected an explicit pin to survive a later unrelated Configure, got %+v", env.Settings)
	}
}

func findEnvironmentForTest(envs []core.Environment, name string) (core.Environment, int) {
	for i, e := range envs {
		if e.Name == name {
			return e, i
		}
	}
	return core.Environment{}, -1
}

func TestOperationsOnUnknownNameFail(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()

	if err := svc.Start(ctx, "ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := svc.Status(ctx, "ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := svc.Remove(ctx, "ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestKindWithoutBackendIsUnsupported(t *testing.T) {
	ctx := context.Background()
	svc := core.NewService(&memStore{}, newFakeBackend("fake-box"), nil)

	_, err := svc.Create(ctx, core.Environment{Name: "vm1", Image: testISO(t), Kind: core.Machine})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestRemoveDropsFromStoreOnlyAfterBackendConfirms(t *testing.T) {
	ctx := context.Background()
	svc, box, _ := newTestService()

	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Remove(ctx, "radic"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !box.removed["radic"] {
		t.Fatal("expected backend Remove to be called")
	}
	envs, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("expected no environments after Remove, got %v", envs)
	}
}

func TestExecPropagatesBackendError(t *testing.T) {
	ctx := context.Background()
	svc, box, _ := newTestService()
	box.execErr = errors.New("boom")

	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Exec(ctx, "radic", []string{"go", "test", "./..."}); err == nil {
		t.Fatal("expected Exec error to propagate")
	}
}

// fakeLinkBackend adds HostLinker on top of fakeBackend to test that
// Configure calls it best-effort when a Color patch is applied.
type fakeLinkBackend struct {
	*fakeBackend
	linkedColor map[string]string
	unlinked    map[string]bool
}

func newFakeLinkBackend(name string) *fakeLinkBackend {
	return &fakeLinkBackend{fakeBackend: newFakeBackend(name), linkedColor: map[string]string{}, unlinked: map[string]bool{}}
}

func (f *fakeLinkBackend) Link(ctx context.Context, env core.Environment, color string) (string, error) {
	f.linkedColor[env.Name] = color
	return "/fake/" + env.Name, nil
}

func (f *fakeLinkBackend) Unlink(ctx context.Context, env core.Environment) error {
	f.unlinked[env.Name] = true
	return nil
}

func TestConfigureColorValidatesPaletteAndLinksHost(t *testing.T) {
	ctx := context.Background()
	machine := newFakeLinkBackend("fake-machine")
	svc := core.NewService(&memStore{}, newFakeBackend("fake-box"), machine)
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	bogus := "chartreuse"
	if _, err := svc.Configure(ctx, "desktop", core.SettingsPatch{Color: &bogus}); err == nil {
		t.Fatal("expected an invalid color to be rejected")
	}

	blue := "blue"
	settings, err := svc.Configure(ctx, "desktop", core.SettingsPatch{Color: &blue})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if settings.Color != "blue" {
		t.Fatalf("expected color to be persisted, got %+v", settings)
	}
	if machine.linkedColor["desktop"] != "blue" {
		t.Fatalf("expected HostLinker.Link to be called with the new color, got %+v", machine.linkedColor)
	}

	if err := svc.Remove(ctx, "desktop"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !machine.unlinked["desktop"] {
		t.Fatal("expected HostLinker.Unlink to be called on Remove")
	}
}

func TestConfigureColorOnBackendWithoutHostLinkerStillPersists(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService() // plain fakeBackend: no HostLinker
	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	green := "green"
	settings, err := svc.Configure(ctx, "radic", core.SettingsPatch{Color: &green})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if settings.Color != "green" {
		t.Fatalf("expected color to be persisted even without HostLinker, got %+v", settings)
	}
}

// fakeLauncher records which environments have a launcher entry.
type fakeLauncher struct {
	entries map[string]core.Environment
	err     error
}

func (f *fakeLauncher) Publish(env core.Environment) error {
	if f.err != nil {
		return f.err
	}
	f.entries[env.ID] = env
	return nil
}

func (f *fakeLauncher) Withdraw(env core.Environment) error {
	delete(f.entries, env.ID)
	return nil
}

func TestLauncherEntryIsOptOut(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	launcher := &fakeLauncher{entries: map[string]core.Environment{}}
	svc.SetLauncher(launcher)

	env, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := launcher.entries[env.ID]; !ok {
		t.Fatal("a new environment should be in the launcher by default")
	}

	desc := "Go toolchain"
	if _, err := svc.Configure(ctx, "radic", core.SettingsPatch{Description: &desc}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got := launcher.entries[env.ID].Settings.Description; got != desc {
		t.Fatalf("entry should follow settings, got description %q", got)
	}

	off := false
	settings, err := svc.Configure(ctx, "radic", core.SettingsPatch{Launcher: &off})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !settings.LauncherDisabled {
		t.Fatal("expected launcher to be disabled")
	}
	if _, ok := launcher.entries[env.ID]; ok {
		t.Fatal("disabling should withdraw the entry")
	}
	// Starting must not bring back an entry the user turned off.
	if err := svc.Start(ctx, "radic"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := launcher.entries[env.ID]; ok {
		t.Fatal("Start republished a disabled entry")
	}

	on := true
	if _, err := svc.Configure(ctx, "radic", core.SettingsPatch{Launcher: &on}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := svc.Remove(ctx, "radic"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(launcher.entries) != 0 {
		t.Fatal("Remove should withdraw the entry")
	}
}

// An environment created before launcher entries existed gets one the
// first time it is started.
func TestStartPublishesExistingEnvironment(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	env, err := svc.Create(ctx, core.Environment{Name: "old", Image: "fedora:latest", Kind: core.Box})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	launcher := &fakeLauncher{entries: map[string]core.Environment{}}
	svc.SetLauncher(launcher)
	if err := svc.Start(ctx, "old"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := launcher.entries[env.ID]; !ok {
		t.Fatal("Start should publish the entry")
	}
}

func TestLauncherFailureDoesNotFailCreate(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	svc.SetLauncher(&fakeLauncher{entries: map[string]core.Environment{}, err: errors.New("read-only home")})
	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("Create should succeed even when the launcher entry can't be written: %v", err)
	}
}

// fakeAppExporterBackend adds AppExporter on top of fakeBackend to test
// Service.ListApps/ExportApp/UnexportApp.
type fakeAppExporterBackend struct {
	*fakeBackend
	apps       []core.App
	exported   []string
	unexported []string
}

func newFakeAppExporterBackend(name string) *fakeAppExporterBackend {
	return &fakeAppExporterBackend{fakeBackend: newFakeBackend(name)}
}

func (f *fakeAppExporterBackend) ListApps(ctx context.Context, env core.Environment) ([]core.App, error) {
	return f.apps, nil
}
func (f *fakeAppExporterBackend) ExportApp(ctx context.Context, env core.Environment, id string) error {
	f.exported = append(f.exported, id)
	return nil
}
func (f *fakeAppExporterBackend) UnexportApp(ctx context.Context, env core.Environment, id string) error {
	f.unexported = append(f.unexported, id)
	return nil
}

func TestAppExportLifecycle(t *testing.T) {
	ctx := context.Background()
	box := newFakeAppExporterBackend("fake-distrobox")
	box.apps = []core.App{{ID: "/usr/share/applications/mpv.desktop", Name: "mpv"}}
	svc := core.NewService(&memStore{}, box, newFakeBackend("fake-machine"))
	if _, err := svc.Create(ctx, core.Environment{Name: "dev", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	apps, err := svc.ListApps(ctx, "dev")
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 1 || apps[0].Name != "mpv" {
		t.Fatalf("unexpected apps: %+v", apps)
	}

	if err := svc.ExportApp(ctx, "dev", apps[0].ID); err != nil {
		t.Fatalf("ExportApp: %v", err)
	}
	if len(box.exported) != 1 || box.exported[0] != apps[0].ID {
		t.Fatalf("expected backend ExportApp to be called with %s, got %v", apps[0].ID, box.exported)
	}

	if err := svc.UnexportApp(ctx, "dev", apps[0].ID); err != nil {
		t.Fatalf("UnexportApp: %v", err)
	}
	if len(box.unexported) != 1 || box.unexported[0] != apps[0].ID {
		t.Fatalf("expected backend UnexportApp to be called with %s, got %v", apps[0].ID, box.unexported)
	}
}

func TestAppExportOnUnsupportedBackendFails(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService() // plain fakeBackend: no AppExporter
	if _, err := svc.Create(ctx, core.Environment{Name: "dev", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.ListApps(ctx, "dev"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if err := svc.ExportApp(ctx, "dev", "id"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if err := svc.UnexportApp(ctx, "dev", "id"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestSnapshotOnUnsupportedBackendFails(t *testing.T) {
	ctx := context.Background()
	svc, box, _ := newTestService() // fakeBackend does not implement SnapshotManager
	_ = box
	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora:latest", Kind: core.Box}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.CreateSnapshot(ctx, "radic", "Before upgrade"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestSnapshotLifecycle(t *testing.T) {
	ctx := context.Background()
	machine := newFakeSnapshotBackend("fake-machine")
	svc := core.NewService(&memStore{}, newFakeBackend("fake-box"), machine)

	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	snap, err := svc.CreateSnapshot(ctx, "desktop", "Before upgrade")
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if snap.Label != "Before upgrade" || snap.ID == "" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if !machine.snapshots[snap.ID] {
		t.Fatal("expected backend to record the snapshot tag")
	}

	list, err := svc.ListSnapshots(ctx, "desktop")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list) != 1 || list[0].ID != snap.ID {
		t.Fatalf("unexpected snapshot list: %+v", list)
	}

	if err := svc.GoToSnapshot(ctx, "desktop", snap.ID); err != nil {
		t.Fatalf("GoToSnapshot: %v", err)
	}
	if err := svc.GoToSnapshot(ctx, "desktop", "does-not-exist"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown snapshot, got %v", err)
	}

	if err := svc.RemoveSnapshot(ctx, "desktop", snap.ID); err != nil {
		t.Fatalf("RemoveSnapshot: %v", err)
	}
	if machine.snapshots[snap.ID] {
		t.Fatal("expected backend RemoveSnapshot to be called")
	}
	list, err = svc.ListSnapshots(ctx, "desktop")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no snapshots after removal, got %+v", list)
	}
}

func TestSnapshotLimitDiscardsOldest(t *testing.T) {
	ctx := context.Background()
	machine := newFakeSnapshotBackend("fake-machine")
	svc := core.NewService(&memStore{}, newFakeBackend("fake-box"), machine)

	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	limit := 2
	if _, err := svc.Configure(ctx, "desktop", core.SettingsPatch{SnapshotLimit: &limit}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	var ids []string
	for _, label := range []string{"one", "two", "three"} {
		snap, err := svc.CreateSnapshot(ctx, "desktop", label)
		if err != nil {
			t.Fatalf("CreateSnapshot(%s): %v", label, err)
		}
		ids = append(ids, snap.ID)
	}

	list, err := svc.ListSnapshots(ctx, "desktop")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected snapshot limit to cap history at 2, got %d: %+v", len(list), list)
	}
	if list[0].Label != "two" || list[1].Label != "three" {
		t.Fatalf("expected oldest snapshot discarded, got %+v", list)
	}
	if machine.snapshots[ids[0]] {
		t.Fatal("expected backend to have discarded the oldest snapshot too")
	}
}

// TestSnapshotCreatedButRetentionDiscardFailsStillPersistsNewSnapshot
// guards against a real class of bug in this codebase (see the CPU/memory
// pinning bugs above): CreateSnapshot used to batch the new snapshot and
// every retention discard into a single store.Save at the very end, so if
// the backend already created the new snapshot but a later discard of an
// old one failed, the function returned an error *before ever saving* —
// silently losing track of a snapshot that genuinely exists in the
// backend (a "ghost" invisible to `snapshot list`, with no way back to it
// short of hand-editing the state file).
func TestSnapshotCreatedButRetentionDiscardFailsStillPersistsNewSnapshot(t *testing.T) {
	ctx := context.Background()
	machine := newFakeSnapshotBackend("fake-machine")
	svc := core.NewService(&memStore{}, newFakeBackend("fake-box"), machine)

	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	limit := 2
	if _, err := svc.Configure(ctx, "desktop", core.SettingsPatch{SnapshotLimit: &limit}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if _, err := svc.CreateSnapshot(ctx, "desktop", "one"); err != nil {
		t.Fatalf("CreateSnapshot(one): %v", err)
	}
	if _, err := svc.CreateSnapshot(ctx, "desktop", "two"); err != nil {
		t.Fatalf("CreateSnapshot(two): %v", err)
	}

	machine.removeErr = errors.New("disk full")
	snap, err := svc.CreateSnapshot(ctx, "desktop", "three")
	if err == nil {
		t.Fatal("expected the retention discard failure to surface as an error")
	}
	if snap.Label != "three" {
		t.Fatalf("expected the returned snapshot to reflect the one that really was created, got %+v", snap)
	}

	machine.removeErr = nil
	list, err := svc.ListSnapshots(ctx, "desktop")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	labels := make([]string, len(list))
	for i, s := range list {
		labels[i] = s.Label
	}
	if len(list) != 3 || labels[2] != "three" {
		t.Fatalf("expected the new snapshot to still be tracked despite the discard failure, got %+v", labels)
	}
}

// Regression: the 500-character description limit counted bytes, so a
// Portuguese description the Settings dialog accepted (it counts
// characters) was refused on save.
func TestDescriptionLimitCountsCharacters(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, core.Environment{Name: "dev", Image: "fedora", Kind: core.Box}); err != nil {
		t.Fatal(err)
	}
	accented := strings.Repeat("ã", 500) // 1000 bytes
	if _, err := svc.Configure(ctx, "dev", core.SettingsPatch{Description: &accented}); err != nil {
		t.Fatalf("500 characters refused: %v", err)
	}
	tooLong := accented + "a"
	if _, err := svc.Configure(ctx, "dev", core.SettingsPatch{Description: &tooLong}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("501 characters accepted: %v", err)
	}
}

func TestFullscreenIsOnByDefaultAndMachineOnly(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	iso := testISO(t)
	if _, err := svc.Create(ctx, core.Environment{Name: "vm", Image: iso, Kind: core.Machine}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, core.Environment{Name: "dev", Image: "fedora", Kind: core.Box}); err != nil {
		t.Fatal(err)
	}
	if s, _ := svc.Settings(ctx, "vm"); s.FullscreenDisabled {
		t.Fatal("fullscreen must be on by default")
	}
	off := false
	if s, err := svc.Configure(ctx, "vm", core.SettingsPatch{Fullscreen: &off}); err != nil || !s.FullscreenDisabled {
		t.Fatalf("turning fullscreen off: %v, %v", s, err)
	}
	if _, err := svc.Configure(ctx, "dev", core.SettingsPatch{Fullscreen: &off}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("fullscreen on a Box = %v, want ErrUnsupported", err)
	}
}
