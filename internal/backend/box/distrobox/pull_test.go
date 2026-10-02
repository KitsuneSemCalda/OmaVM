package distrobox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestPullStagesFollowLayersWithoutPercentages(t *testing.T) {
	podman := []string{
		"Trying to pull registry.fedoraproject.org/fedora-toolbox:42...",
		"Getting image source signatures",
		"Copying blob sha256:aaa",
		"Copying blob sha256:bbb",
		"Copying config sha256:ccc",
		"Writing manifest to image destination",
		"ccc",
	}
	p := pullStages{image: "fedora"}
	var got []string
	for _, line := range podman {
		if stage, ok := p.next(line); ok {
			got = append(got, stage)
		}
	}
	want := []string{"Downloading fedora: layer 1", "Downloading fedora: layer 2", "Downloading fedora: finishing"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("podman stages = %q, want %q", got, want)
	}

	p = pullStages{image: "ubuntu"}
	stage, ok := p.next("a1b2c3: Pull complete")
	if !ok || stage != "Downloading ubuntu: 1 layers done" {
		t.Fatalf("docker stage = %q, %t", stage, ok)
	}
	if _, ok := p.next("a1b2c3: Downloading [====>   ] 12MB/40MB"); ok {
		t.Fatal("a progress bar line became a stage")
	}
}

// fakeEngine puts a podman on PATH that has no image yet and prints what
// a real pull prints.
func fakeEngine(t *testing.T, pullExit int) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
case "$1 $2" in
"image inspect") exit 1 ;;
esac
if [ "$1" = pull ]; then
  echo "Trying to pull $2..." >&2
  echo "Copying blob sha256:aaa" >&2
  echo "Writing manifest to image destination" >&2
  exit ` + string(rune('0'+pullExit)) + `
fi
`
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DBX_CONTAINER_MANAGER", "")
	return log
}

func TestPullImageReportsStages(t *testing.T) {
	log := fakeEngine(t, 0)
	var stages []string
	ctx := core.WithProgress(context.Background(), func(s string) { stages = append(stages, s) })
	(&Backend{}).pullImage(ctx, "fedora")
	want := []string{"Downloading fedora", "Downloading fedora: layer 1", "Downloading fedora: finishing", "Creating the Box from fedora"}
	if strings.Join(stages, "|") != strings.Join(want, "|") {
		t.Fatalf("stages = %q, want %q", stages, want)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "pull fedora") {
		t.Fatalf("no pull: %s", calls)
	}
}

func TestFailedPullLeavesItToDistrobox(t *testing.T) {
	fakeEngine(t, 1)
	var stages []string
	ctx := core.WithProgress(context.Background(), func(s string) { stages = append(stages, s) })
	(&Backend{}).pullImage(ctx, "fedora")
	if last := stages[len(stages)-1]; last != "Preparing fedora" {
		t.Fatalf("after a failed pull the stage is %q", last)
	}
}

func TestIsCloneImage(t *testing.T) {
	for image, want := range map[string]bool{
		"localhost/omavm-dev-1a2b3c4d:2026-10-01":      true,
		"omavm-dev-1a2b3c4d:2026-10-01":                true,
		"registry.fedoraproject.org/fedora-toolbox:42": false,
		"fedora:latest":                 false,
		"quay.io/omavm-org/omavm-box:1": false,
		"localhost/someone/omavm-x:1":   false,
	} {
		if got := isCloneImage(image); got != want {
			t.Errorf("isCloneImage(%q) = %t, want %t", image, got, want)
		}
	}
}
