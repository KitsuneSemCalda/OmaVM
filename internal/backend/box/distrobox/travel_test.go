package distrobox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
	"github.com/KitsuneForgering/OmaVM/internal/power"
)

// travelHost fakes a host with 8 CPUs, the given power state and a podman
// whose container currently has limit (in CPUs, 0 = none). It returns the
// file podman's update calls are logged to.
func travelHost(t *testing.T, onBattery bool, limit string) string {
	t.Helper()
	orig := hostCPUs
	t.Cleanup(func() { hostCPUs = orig })
	hostCPUs = func() int { return 8 }

	origDir := power.SupplyDir
	t.Cleanup(func() { power.SupplyDir = origDir })
	power.SupplyDir = t.TempDir()
	online := "1"
	if onBattery {
		online = "0"
	}
	ac := filepath.Join(power.SupplyDir, "AC")
	if err := os.MkdirAll(ac, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, value := range map[string]string{"type": "Mains", "online": online} {
		if err := os.WriteFile(filepath.Join(ac, file), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "updates")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = container ]; then echo " + limit + "000000000; exit 0; fi\n" +
		"echo \"$@\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return log
}

func updates(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

var travelBox = core.Environment{ID: "12345678", Name: "dev"}

func TestTravelModeHalvesCPUsOnBattery(t *testing.T) {
	log := travelHost(t, true, "0")
	New().applyTravelMode(context.Background(), travelBox)
	if got := updates(t, log); got != "update --cpus 4 omavm-dev-12345678" {
		t.Fatalf("unexpected update: %q", got)
	}
}

// Plugged in again: the limit it set is lifted by giving every CPU back,
// since the engine ignores --cpus 0.
func TestTravelModeRestoresCPUsOnAC(t *testing.T) {
	log := travelHost(t, false, "4")
	New().applyTravelMode(context.Background(), travelBox)
	if got := updates(t, log); got != "update --cpus 8 omavm-dev-12345678" {
		t.Fatalf("unexpected update: %q", got)
	}
}

func TestTravelModeLeavesUnlimitedBoxAloneOnAC(t *testing.T) {
	log := travelHost(t, false, "0")
	New().applyTravelMode(context.Background(), travelBox)
	if got := updates(t, log); got != "" {
		t.Fatalf("expected no update, got %q", got)
	}
}

func TestTravelModeSkipsWhenAlreadyReduced(t *testing.T) {
	log := travelHost(t, true, "4")
	New().applyTravelMode(context.Background(), travelBox)
	if got := updates(t, log); got != "" {
		t.Fatalf("expected no update, got %q", got)
	}
}

func TestTravelModeOptOutRestoresOnBattery(t *testing.T) {
	log := travelHost(t, true, "4")
	env := travelBox
	env.Settings.TravelModeDisabled = true
	New().applyTravelMode(context.Background(), env)
	if got := updates(t, log); got != "update --cpus 8 omavm-dev-12345678" {
		t.Fatalf("unexpected update: %q", got)
	}
}

// Without a container engine on PATH nothing happens and nothing fails.
func TestTravelModeWithoutEngineIsSilent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	New().applyTravelMode(context.Background(), travelBox)
}
