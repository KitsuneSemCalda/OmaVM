package distrobox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// listAppsScript enumerates .desktop launchers installed at the container
// level (/usr/local/share/applications, /usr/share/applications) — never
// ~/.local/share/applications, which is the host's own directory too
// (Distrobox shares $HOME), so anything already there is already visible
// on the host and needs no export. Verified against a real distrobox
// container: distrobox-export matches candidates by grepping Exec=/Name=
// for a caller-supplied substring, which is ambiguous (case-sensitive,
// breaks on spaces/regex metacharacters); passing the .desktop file's
// absolute path to --app instead sidesteps all of that and was confirmed
// to work for both export and --delete.
const listAppsScript = `for f in /usr/local/share/applications/*.desktop /usr/share/applications/*.desktop; do
  [ -f "$f" ] || continue
  grep -q '^NoDisplay=true' "$f" 2>/dev/null && continue
  name=$(sed -n 's/^Name=//p' "$f" | head -n1)
  [ -n "$name" ] || continue
  printf '%s\t%s\n' "$f" "$name"
done`

func (b *Backend) ListApps(ctx context.Context, env core.Environment) ([]core.App, error) {
	if err := b.requireContainer(ctx, env); err != nil {
		return nil, err
	}
	out, err := b.output(ctx, "enter", "--no-tty", "--name", boxName(env), "--", "sh", "-c", listAppsScript)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	exportDir := filepath.Join(home, ".local", "share", "applications")

	var apps []core.App
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		path, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(path), ".desktop")
		exportedPath := filepath.Join(exportDir, boxName(env)+"-"+base+".desktop")
		_, statErr := os.Stat(exportedPath)
		apps = append(apps, core.App{ID: path, Name: name, Exported: statErr == nil})
	}
	return apps, nil
}

// ExportApp/UnexportApp pass id (an absolute .desktop path, per ListApps)
// straight to distrobox-export --app, its own native mechanism — OmaVM
// only triggers it (Backend Rules: integrate, don't reimplement).
func (b *Backend) ExportApp(ctx context.Context, env core.Environment, id string) error {
	if err := b.requireContainer(ctx, env); err != nil {
		return err
	}
	return b.run(ctx, "enter", "--no-tty", "--name", boxName(env), "--", "distrobox-export", "--app", id)
}

func (b *Backend) UnexportApp(ctx context.Context, env core.Environment, id string) error {
	if err := b.requireContainer(ctx, env); err != nil {
		return err
	}
	return b.run(ctx, "enter", "--no-tty", "--name", boxName(env), "--", "distrobox-export", "--app", id, "--delete")
}
