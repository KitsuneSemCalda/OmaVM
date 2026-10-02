package distrobox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func fakeDistroboxUpgrade(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "distrobox"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// The package manager's lines become the stages a person follows, without
// the color codes distrobox prints.
func TestRunReportingStreamsCleanLines(t *testing.T) {
	fakeDistroboxUpgrade(t, `printf '\033[1;31m Upgrading omavm-dev...\n\033[0m'
echo "Upgrading 3 packages"
echo ""
echo "(1/3) upgrading curl" >&2
`)
	var stages []string
	ctx := core.WithProgress(context.Background(), func(s string) { stages = append(stages, s) })
	if err := (&Backend{}).runReporting(ctx, "upgrade", "omavm-dev"); err != nil {
		t.Fatal(err)
	}
	want := "Upgrading omavm-dev...|Upgrading 3 packages|(1/3) upgrading curl"
	if strings.Join(stages, "|") != want {
		t.Fatalf("stages = %q", stages)
	}
}

func TestRunReportingFailureCarriesTheLastLines(t *testing.T) {
	fakeDistroboxUpgrade(t, `echo "error: failed retrieving file 'core.db' from mirror"
exit 1
`)
	err := (&Backend{}).runReporting(context.Background(), "upgrade", "omavm-dev")
	if err == nil || !strings.Contains(err.Error(), "failed retrieving file") {
		t.Fatalf("expected the package manager's error, got %v", err)
	}
}
