package qemu

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// applied is what a running QEMU was started with, read back from its
// command line: the settings that only change on the next Start.
type applied struct {
	cpus, memoryMiB        int
	vsock, virtiofs, cdrom bool
}

func parseApplied(args []string) applied {
	var a applied
	for i, arg := range args {
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch arg {
		case "-smp":
			a.cpus, _ = strconv.Atoi(next)
		case "-m":
			a.memoryMiB, _ = strconv.Atoi(next)
		case "-cdrom":
			a.cdrom = true
		case "-device":
			a.vsock = a.vsock || strings.HasPrefix(next, "vhost-vsock-pci")
			a.virtiofs = a.virtiofs || strings.HasPrefix(next, "vhost-user-fs-pci")
		}
	}
	return a
}

func (b *Backend) appliedConfig(name string) (applied, bool) {
	pid, err := b.readPID(name)
	if err != nil {
		return applied{}, false
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return applied{}, false
	}
	return parseApplied(strings.Split(string(cmdline), "\x00")), true
}

// sessionAdjustments compares a running session with the saved settings.
// travel: Travel Mode started it with fewer CPUs because the host was on
// battery. restart: a saved setting differs from what the session got and
// only applies on the next start.
func sessionAdjustments(env core.Environment, a applied, vsockAvailable bool) (restart, travel bool) {
	want := env.EffectiveSettings()
	switch {
	case a.cpus == want.CPUs:
	case a.cpus < want.CPUs && env.Settings.CPUs == 0 && !want.TravelModeDisabled:
		travel = true
	default:
		restart = true
	}
	if a.memoryMiB != want.MemoryMiB {
		restart = true
	}
	if (!want.SSHDisabled && vsockAvailable) != a.vsock {
		restart = true
	}
	if (want.SharedPath != "") != a.virtiofs {
		restart = true
	}
	if want.DisconnectISO && a.cdrom {
		restart = true
	}
	if !want.DisconnectISO && !a.cdrom && env.Image != "" {
		if _, err := os.Stat(env.Image); err == nil {
			restart = true
		}
	}
	return restart, travel
}
