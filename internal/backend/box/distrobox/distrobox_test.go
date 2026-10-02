package distrobox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestCreateUsesStableUniqueDistroboxName(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "args")
	writeDistrobox(t, bin, `printf '%s\n' "$@" > "$OMAVM_TEST_LOG"`)
	t.Setenv("PATH", bin)
	t.Setenv("OMAVM_TEST_LOG", log)

	env := core.Environment{ID: "12345678-abcd", Name: "Fast Box!", Image: "fedora:latest"}
	if err := New().Create(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "create\n--yes\n--name\nomavm-fast-box-12345678\n--image\nfedora:latest\n"
	if string(got) != want {
		t.Fatalf("unexpected arguments:\n%s\nwant:\n%s", got, want)
	}
}

func TestStatusComesFromTheContainerEngine(t *testing.T) {
	bin := t.TempDir()
	writeEngine(t, bin, "omavm-fast-12345678\trunning\tUp 2 minutes\n")
	t.Setenv("PATH", bin)

	status, err := New().Status(context.Background(), core.Environment{ID: "12345678", Name: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != core.StateRunning || status.Detail != "Up 2 minutes" {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestMissingDistroboxReturnsInstallHint(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := New().Create(context.Background(), core.Environment{Name: "fast", Image: "fedora"})
	if err == nil || !strings.Contains(err.Error(), "sudo pacman -S distrobox") {
		t.Fatalf("expected install hint, got %v", err)
	}
}

// writeEngine fakes Podman answering `podman ps` with lines of name, state
// and status, as listContainers asks for them.
func writeEngine(t *testing.T, dir, ps string) {
	t.Helper()
	t.Setenv("DBX_CONTAINER_MANAGER", "podman")
	script := "#!/bin/sh\n[ \"$1\" = ps ] || exit 1\nprintf '%s' '" + ps + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeDistrobox(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, "distrobox")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Podman only accepts container names matching [a-zA-Z0-9][a-zA-Z0-9_.-]*
// (checked against podman on 2026-09-28), while environment names may use
// accents and any script. Every derived name must be valid, and existing
// ASCII names must map exactly as before so current Boxes keep working.
func TestBoxNameIsAValidContainerName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	const id = "abcd1234ef"
	tests := map[string]string{
		"radic":           "omavm-radic-abcd1234",
		"My Box":          "omavm-my-box-abcd1234",
		"Café":            "omavm-cafe-abcd1234",
		"Projeto Ação":    "omavm-projeto-acao-abcd1234",
		"Pão de Queijo ü": "omavm-pao-de-queijo-u-abcd1234",
		"日本":              "omavm-abcd1234",
		"----":            "omavm-abcd1234",
	}
	for name, want := range tests {
		got := boxName(core.Environment{Name: name, ID: id})
		if !valid.MatchString(got) {
			t.Errorf("boxName(%q) = %q is not a valid container name", name, got)
		}
		if got != want {
			t.Errorf("boxName(%q) = %q, want %q", name, got, want)
		}
	}
}

// When a Box's container is removed outside OmaVM (podman rm, a podman
// system reset), `distrobox enter` offers to create it "out of image" its
// own default — and without a terminal it answers yes by itself. Start
// would silently turn an Ubuntu Box into a fresh Fedora toolbox. OmaVM
// must refuse instead, and say what happened.
func TestMissingContainerIsNeverRecreatedByEnter(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	writeDistrobox(t, bin, `echo "$1" >> "$OMAVM_TEST_LOG"`)
	writeEngine(t, bin, "omavm-other-99999999\trunning\tUp\n")
	t.Setenv("PATH", bin)
	t.Setenv("OMAVM_TEST_LOG", log)
	env := core.Environment{ID: "12345678", Name: "ubuntu", Image: "ubuntu:latest"}
	b := New()

	for name, op := range map[string]func() error{
		"start": func() error { return b.Start(context.Background(), env) },
		"exec":  func() error { return b.Exec(context.Background(), env, []string{"true"}) },
		"open":  func() error { return b.Open(context.Background(), env) },
	} {
		err := op()
		if !errors.Is(err, core.ErrNotFound) || !strings.Contains(err.Error(), "create it again") {
			t.Errorf("%s on a missing container = %v, want ErrNotFound saying to create it again", name, err)
		}
	}
	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "enter") {
		t.Fatalf("distrobox enter was called on a missing container:\n%s", calls)
	}

	status, err := b.Status(context.Background(), env)
	if err != nil || status.State != core.StateError || !strings.Contains(status.Detail, "no longer exists") {
		t.Fatalf("Status of a missing container = %#v, %v; want an error state explaining it", status, err)
	}
	if err := b.Stop(context.Background(), env); err != nil {
		t.Fatalf("Stop of a missing container must be a no-op: %v", err)
	}
}

func TestParseContainersReadsPodmanAndDockerStates(t *testing.T) {
	got := parseContainers("omavm-a-1\trunning\tUp 3 minutes\nomavm-b-2,alias\texited\tExited (0) 2 hours ago\nomavm-c-3\tpaused\tUp (Paused)\n\n")
	want := map[string]core.State{"omavm-a-1": core.StateRunning, "omavm-b-2": core.StateStopped, "alias": core.StateStopped, "omavm-c-3": core.StatePaused}
	for name, state := range want {
		if got[name].State != state {
			t.Errorf("%s: %q, want %q", name, got[name].State, state)
		}
	}
	if got["omavm-a-1"].Detail != "Up 3 minutes" {
		t.Errorf("detail = %q", got["omavm-a-1"].Detail)
	}
}

func TestStatusesAsksTheEngineOnce(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("DBX_CONTAINER_MANAGER", "podman")
	script := "#!/bin/sh\necho call >> \"$OMAVM_TEST_LOG\"\nprintf 'omavm-a-11111111\\trunning\\tUp\\nomavm-b-22222222\\texited\\tExited\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("OMAVM_TEST_LOG", log)

	envs := []core.Environment{{ID: "11111111", Name: "a"}, {ID: "22222222", Name: "b"}, {ID: "33333333", Name: "c"}}
	got, err := New().Statuses(context.Background(), envs)
	if err != nil {
		t.Fatal(err)
	}
	if got["11111111"].State != core.StateRunning || got["22222222"].State != core.StateStopped || got["33333333"].State != core.StateError {
		t.Fatalf("unexpected statuses: %#v", got)
	}
	calls, _ := os.ReadFile(log)
	if n := strings.Count(string(calls), "call"); n != 1 {
		t.Fatalf("podman called %d times for three Boxes, want once", n)
	}
}

// Regression: every status poll also ran `docker ps`, which starts the
// Docker daemon on hosts where only docker.socket is enabled, although
// Distrobox only uses Docker without Podman or when configured to. Docker
// is asked only for Boxes Podman doesn't have.
func TestDockerIsOnlyAskedForBoxesPodmanDoesntHave(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("DBX_CONTAINER_MANAGER", "")
	t.Setenv("PATH", bin)
	t.Setenv("OMAVM_TEST_LOG", log)
	for engine, ps := range map[string]string{
		"podman": `omavm-a-11111111\trunning\tUp\n`,
		"docker": `omavm-b-22222222\texited\tExited\n`,
	} {
		script := "#!/bin/sh\necho " + engine + " >> \"$OMAVM_TEST_LOG\"\nprintf '" + ps + "'\n"
		if err := os.WriteFile(filepath.Join(bin, engine), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := func() string { data, _ := os.ReadFile(log); _ = os.Remove(log); return string(data) }

	got, err := New().Statuses(context.Background(), []core.Environment{{ID: "11111111", Name: "a"}})
	if err != nil || got["11111111"].State != core.StateRunning {
		t.Fatalf("got %v, %v", got, err)
	}
	if c := calls(); strings.Contains(c, "docker") {
		t.Fatalf("docker asked although Podman has every Box:\n%s", c)
	}

	got, err = New().Statuses(context.Background(), []core.Environment{{ID: "11111111", Name: "a"}, {ID: "22222222", Name: "b"}})
	if err != nil || got["22222222"].State != core.StateStopped {
		t.Fatalf("a Box in Docker was not found: %v, %v", got, err)
	}
	if c := calls(); !strings.Contains(c, "docker") {
		t.Fatalf("docker not asked for the Box Podman doesn't have:\n%s", c)
	}
}
