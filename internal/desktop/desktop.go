// Package desktop integrates environments with the host desktop through
// freedesktop.org files: one launcher entry per environment, so it shows
// up in the Omarchy launcher (or any other) like a normal application,
// and the per-color icons the file manager shows for a Machine's link.
package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// dataDirs lists the XDG data directories in lookup order: the user's own
// first, then the system ones.
func dataDirs() []string {
	home := os.Getenv("XDG_DATA_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".local", "share")
		}
	}
	system := os.Getenv("XDG_DATA_DIRS")
	if system == "" {
		system = "/usr/local/share:/usr/share"
	}
	return append([]string{home}, filepath.SplitList(system)...)
}

// ColorIcon returns the installed icon for a palette color (make install
// puts them in <data>/omavm/icons), or "" when there is none. A run from
// the source tree finds the repository's own copies next to bin/.
func ColorIcon(color string) string {
	if color == "" {
		return ""
	}
	dirs := []string{}
	for _, d := range dataDirs() {
		if d != "" {
			dirs = append(dirs, filepath.Join(d, "omavm", "icons"))
		}
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "..", "data", "icons", "colors"))
	}
	for _, d := range dirs {
		path := filepath.Join(d, color+".svg")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(path); err == nil {
				return abs
			}
		}
	}
	return ""
}

// appIcon is OmaVM's own icon for an entry: the themed name when OmaVM is
// installed into the icon theme (make install, the package), or the SVG
// that ships next to the binaries in the release tarball OmaStore
// installs under $HOME, where no icon theme has it.
func appIcon(cli string) string {
	svg := filepath.Join(filepath.Dir(cli), "..", "data", "icons", "dev.omavm.app.svg")
	if info, err := os.Stat(svg); err == nil && !info.IsDir() {
		if abs, err := filepath.Abs(svg); err == nil {
			return abs
		}
	}
	return "dev.omavm.app"
}

// Launcher writes one .desktop entry per environment. It is on by default
// (opt-out per environment, EnvironmentSettings.LauncherDisabled).
type Launcher struct {
	// Dir is where entries are written, normally
	// $XDG_DATA_HOME/applications.
	Dir string
	// CLI and GUI are the absolute paths the entries run; GUI may be ""
	// when omavm-gui isn't installed.
	CLI, GUI string
}

// NewLauncher locates the user's applications directory and the OmaVM
// binaries next to the running one (or on PATH).
func NewLauncher() (*Launcher, error) {
	dirs := dataDirs()
	if dirs[0] == "" {
		return nil, errors.New("resolve applications directory: no home directory")
	}
	cli, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve omavm path: %w", err)
	}
	return &Launcher{
		Dir: filepath.Join(dirs[0], "applications"),
		CLI: cli,
		GUI: sibling(cli, "omavm-gui"),
	}, nil
}

func sibling(exe, name string) string {
	path := filepath.Join(filepath.Dir(exe), name)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return ""
}

// path names the entry by the environment's ID, which never changes and
// needs no escaping, unlike its name.
func (l *Launcher) path(env core.Environment) string {
	return filepath.Join(l.Dir, "dev.omavm.env."+env.ID+".desktop")
}

// Publish creates or updates the environment's entry. Writing only when
// the content changed keeps it cheap to call on every Start/Open.
func (l *Launcher) Publish(env core.Environment) error {
	content := l.entry(env)
	path := l.path(env)
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, content) {
		return nil
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", l.Dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("write launcher entry: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write launcher entry: %w", err)
	}
	return nil
}

// Withdraw removes the entry; a missing one is not an error.
func (l *Launcher) Withdraw(env core.Environment) error {
	if err := os.Remove(l.path(env)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove launcher entry: %w", err)
	}
	return nil
}

func (l *Launcher) entry(env core.Environment) []byte {
	var exec []string
	terminal := false
	kindLabel := "Virtual machine"
	switch {
	case env.Kind == core.Machine:
		// Open starts a stopped Machine and then shows its display. Nobody
		// sees this run's terminal, so a failure becomes a notification.
		exec = []string{"env", "OMAVM_NOTIFY_ERRORS=1", l.CLI, "open", env.Name}
	case l.GUI != "":
		kindLabel = "Development Box"
		exec = []string{l.GUI, "--terminal", env.Name, "--title", env.Name + " — OmaVM"}
		if env.Settings.ClipboardDisabled {
			exec = append(exec, "--share-clipboard", "false")
		}
		if env.Settings.EmptyWorkspaceDisabled {
			exec = append(exec, "--empty-workspace", "false")
		}
		if env.Settings.Color != "" {
			exec = append(exec, "--color", env.Settings.Color)
		}
	default:
		// Without omavm-gui, a Box's shell still opens in the user's own
		// terminal: `omavm open` on a Box is interactive.
		kindLabel = "Development Box"
		exec = []string{l.CLI, "open", env.Name}
		terminal = true
	}
	icon := ColorIcon(env.Settings.Color)
	if icon == "" {
		icon = appIcon(l.CLI)
	}
	comment := env.Settings.Description
	if comment == "" {
		comment = kindLabel + " in OmaVM"
	}

	var b strings.Builder
	b.WriteString("[Desktop Entry]\n")
	b.WriteString("Type=Application\n")
	fmt.Fprintf(&b, "Name=%s\n", escapeValue(env.Name))
	fmt.Fprintf(&b, "GenericName=%s\n", kindLabel)
	fmt.Fprintf(&b, "Comment=%s\n", escapeValue(comment))
	fmt.Fprintf(&b, "Exec=%s\n", escapeValue(execLine(exec)))
	fmt.Fprintf(&b, "Icon=%s\n", escapeValue(icon))
	fmt.Fprintf(&b, "Terminal=%t\n", terminal)
	b.WriteString("Categories=System;Emulator;\n")
	b.WriteString("Keywords=OmaVM;\n")
	b.WriteString("X-OmaVM-Environment=" + env.ID + "\n")
	return []byte(b.String())
}

// execLine quotes each argument by the Exec key rules: arguments are
// double-quoted, with ", `, $ and \ backslash-escaped inside, and % is
// doubled because it starts a field code.
func execLine(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range arg {
			switch r {
			case '"', '`', '$', '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case '%':
				b.WriteString("%%")
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
		quoted[i] = b.String()
	}
	return strings.Join(quoted, " ")
}

// escapeValue applies the string escapes every value goes through before
// the Exec rules are read, so a backslash from execLine survives.
func escapeValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return r.Replace(s)
}
