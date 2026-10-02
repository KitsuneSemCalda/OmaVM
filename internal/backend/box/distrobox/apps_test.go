package distrobox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestListAppsParsesOutputAndDetectsExported(t *testing.T) {
	bin := t.TempDir()
	writeDistrobox(t, bin, `printf '/usr/share/applications/foo.desktop\tFoo App\n/usr/share/applications/bar.desktop\tBar App\n'`)
	writeEngine(t, bin, "omavm-dev-12345678\trunning\tUp\n")
	t.Setenv("PATH", bin)

	home := t.TempDir()
	t.Setenv("HOME", home)
	env := core.Environment{ID: "12345678", Name: "dev"}
	exportDir := filepath.Join(home, ".local", "share", "applications")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// boxName(env) with no special characters in "dev" and an 8-char ID
	// collapses to "omavm-dev-12345678".
	exported := filepath.Join(exportDir, boxName(env)+"-foo.desktop")
	if err := os.WriteFile(exported, []byte("[Desktop Entry]"), 0o644); err != nil {
		t.Fatal(err)
	}

	apps, err := New().ListApps(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("expected 2 apps, got %+v", apps)
	}
	if apps[0].ID != "/usr/share/applications/foo.desktop" || apps[0].Name != "Foo App" || !apps[0].Exported {
		t.Fatalf("unexpected foo app: %+v", apps[0])
	}
	if apps[1].ID != "/usr/share/applications/bar.desktop" || apps[1].Name != "Bar App" || apps[1].Exported {
		t.Fatalf("unexpected bar app: %+v", apps[1])
	}
}

func TestExportAppPassesAbsolutePathToDistroboxExport(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "args")
	writeDistrobox(t, bin, `printf '%s\n' "$@" > "$OMAVM_TEST_LOG"`)
	writeEngine(t, bin, "omavm-dev-12345678\trunning\tUp\n")
	t.Setenv("PATH", bin)
	t.Setenv("OMAVM_TEST_LOG", log)

	env := core.Environment{ID: "12345678", Name: "dev"}
	if err := New().ExportApp(context.Background(), env, "/usr/share/applications/foo.desktop"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "enter\n--no-tty\n--name\n" + boxName(env) + "\n--\ndistrobox-export\n--app\n/usr/share/applications/foo.desktop\n"
	if string(got) != want {
		t.Fatalf("unexpected arguments:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnexportAppAddsDeleteFlag(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "args")
	writeDistrobox(t, bin, `printf '%s\n' "$@" > "$OMAVM_TEST_LOG"`)
	writeEngine(t, bin, "omavm-dev-12345678\trunning\tUp\n")
	t.Setenv("PATH", bin)
	t.Setenv("OMAVM_TEST_LOG", log)

	env := core.Environment{ID: "12345678", Name: "dev"}
	if err := New().UnexportApp(context.Background(), env, "/usr/share/applications/foo.desktop"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "enter\n--no-tty\n--name\n" + boxName(env) + "\n--\ndistrobox-export\n--app\n/usr/share/applications/foo.desktop\n--delete\n"
	if string(got) != want {
		t.Fatalf("unexpected arguments:\n%s\nwant:\n%s", got, want)
	}
}
