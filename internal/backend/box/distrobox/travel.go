package distrobox

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/KitsuneForgering/OmaVM/internal/core"
	"github.com/KitsuneForgering/OmaVM/internal/power"
)

// hostCPUs is overridable in tests.
var hostCPUs = runtime.NumCPU

// applyTravelMode is Travel Mode for Boxes: on battery, the Box's container
// runs with half the host's CPUs, and gets them all back once the host is
// plugged in again. It goes under Distrobox straight to the container
// engine, because `<engine> update` changes a container in place: tested
// against Podman 6.1 and Distrobox 1.8 (2026-09-28), the limit applies to a
// stopped or running container and survives `distrobox enter` and a
// stop/start. `--cpus 0` does not remove a limit there, so "no limit" is
// a limit equal to the host's CPU count.
//
// Best-effort, like the Machine's: a missing engine or a host without
// delegated cgroup controllers must never keep the Box from starting.
func (b *Backend) applyTravelMode(ctx context.Context, env core.Environment) {
	engine, current, ok := containerCPUs(ctx, boxName(env))
	if !ok {
		return
	}
	all := hostCPUs()
	want := all
	if !env.Settings.TravelModeDisabled && power.OnBattery() {
		want = max(1, all/2)
	}
	if current == 0 && want == all {
		return // never limited, nothing to restore
	}
	if current == float64(want) {
		return
	}
	out, err := exec.CommandContext(ctx, engine, "update", "--cpus", strconv.Itoa(want), boxName(env)).CombinedOutput()
	if err != nil {
		slog.Warn("travel mode: could not change the Box's CPUs", "box", env.Name, "error", err, "output", strings.TrimSpace(string(out)))
		return
	}
	if want < all {
		slog.Info("travel mode: reduced CPUs while on battery", "box", env.Name, "cpus", want)
	} else {
		slog.Info("travel mode: restored all CPUs", "box", env.Name, "cpus", want)
	}
}

// containerCPUs finds which engine holds the Box's container (Distrobox
// prefers Podman) and its current CPU limit, 0 meaning none.
func containerCPUs(ctx context.Context, name string) (engine string, cpus float64, ok bool) {
	for _, candidate := range []string{"podman", "docker"} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, path, "container", "inspect", "--format", "{{.HostConfig.NanoCpus}}", name).Output()
		if err != nil {
			continue
		}
		nano, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return "", 0, false
		}
		return path, float64(nano) / 1e9, true
	}
	return "", 0, false
}
