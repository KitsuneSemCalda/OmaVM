// Package qemu adapts QEMU/KVM to the core.Backend interface for
// Machine environments: environments with an independent kernel. It
// shells out to qemu-img/qemu-system-x86_64 directly rather than through
// libvirt, keeping the dependency surface small for Phase 1.
//
// Every Machine gets its own state directory holding its disk image,
// pidfile, and QMP/VNC unix sockets — no infrastructure detail leaks
// past this package.
package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
	"github.com/KitsuneForgering/OmaVM/internal/power"
)

// diskSize is the virtual size of every Machine's disk: 1 TiB. The qcow2
// is sparse: it starts at a few hundred KiB and takes host space only as
// the guest writes, so the size costs nothing up front and no installer
// runs out of room. The guest can believe in more space than the host
// has; when the host fills up, QEMU pauses the Machine (io-error) and
// Status says why.
const diskSize int64 = 1 << 40

var gracefulShutdownTimeout = 10 * time.Second

// Backend implements core.Backend for Machine environments via
// qemu-img and qemu-system-x86_64. It assumes KVM is available
// (/dev/kvm); Machines are always hardware-accelerated VMs, never a
// software-emulated fallback silently substituted for it.
type Backend struct {
	stateDir string
}

// New roots the backend under OmaVM's state directory.
func New() (*Backend, error) {
	base, err := core.StateDir()
	if err != nil {
		return nil, err
	}
	return &Backend{stateDir: filepath.Join(base, "machines")}, nil
}

func (b *Backend) Name() string { return "qemu" }

// maxSocketPath is the usable length of a Unix socket path (sun_path is
// 108 bytes including the terminating NUL). The Machine's directory is
// part of every socket path, so a long one would otherwise pass Create and
// only fail at Start with QEMU's "UNIX socket path is too long".
const maxSocketPath = 107

func (b *Backend) checkNameLength(key string) error {
	longest := b.virtiofsPath(key)
	if len(longest) <= maxSocketPath {
		return nil
	}
	limit := len(key) - (len(longest) - maxSocketPath)
	if limit < 16 {
		return core.Invalidf("the OmaVM state directory %s is too long for a Machine's sockets; set XDG_STATE_HOME to a shorter path", b.stateDir)
	}
	// Only a Machine created before directories were named by ID.
	return core.Invalidf("machine name is too long; use at most %d bytes (accented letters count as two)", limit)
}

// key names the Machine's state directory: its ID, which never changes,
// so the directory doesn't depend on the name (socket paths don't grow
// with it, and a name is never a path component). Machines created before
// live under their name and move to their ID the first time they are
// seen stopped: a running QEMU was given paths in the old directory, and
// is only recognized by them.
func (b *Backend) key(env core.Environment) string {
	if env.ID == "" {
		return env.Name
	}
	if _, err := os.Stat(b.dir(env.ID)); err == nil {
		return env.ID
	}
	legacy := env.Name
	if _, err := os.Stat(b.dir(legacy)); err != nil {
		return env.ID
	}
	if running, _ := b.isRunning(legacy); running {
		return legacy
	}
	if err := os.Rename(b.dir(legacy), b.dir(env.ID)); err != nil {
		// Another OmaVM process may have just moved it.
		if _, statErr := os.Stat(b.dir(env.ID)); statErr == nil {
			return env.ID
		}
		slog.Warn("machine state directory not moved to its id", "machine", env.Name, "error", err)
		return legacy
	}
	slog.Info("machine state directory moved to its id", "machine", env.Name, "id", env.ID)
	// ~/OmaVM/<name> pointed into the old directory.
	if link, err := b.hostLinkPath(env.Name); err == nil {
		if target, err := os.Readlink(link); err == nil && target == b.diskPath(legacy) {
			if _, err := b.Link(context.Background(), env, env.Settings.Color); err != nil {
				slog.Warn("host link not updated", "machine", env.Name, "error", err)
			}
		}
	}
	return env.ID
}

func (b *Backend) dir(name string) string      { return filepath.Join(b.stateDir, name) }
func (b *Backend) diskPath(name string) string { return filepath.Join(b.dir(name), "disk.qcow2") }
func (b *Backend) pidPath(name string) string  { return filepath.Join(b.dir(name), "qemu.pid") }
func (b *Backend) qmpPath(name string) string  { return filepath.Join(b.dir(name), "qmp.sock") }
func (b *Backend) qgaPath(name string) string  { return filepath.Join(b.dir(name), "qga.sock") }
func (b *Backend) virtiofsPath(name string) string {
	return filepath.Join(b.dir(name), "virtiofs.sock")
}
func (b *Backend) virtiofsPIDPath(name string) string {
	return filepath.Join(b.dir(name), "virtiofs.pid")
}
func (b *Backend) previewPath(name string) string { return filepath.Join(b.dir(name), "preview.ppm") }

func (b *Backend) Create(ctx context.Context, env core.Environment) error {
	if err := b.checkNameLength(b.key(env)); err != nil {
		return err
	}
	if err := os.MkdirAll(b.dir(b.key(env)), 0o755); err != nil {
		return fmt.Errorf("create machine state dir: %w", err)
	}
	disk := b.diskPath(b.key(env))
	if _, err := os.Stat(disk); err == nil {
		return nil // idempotent: disk already provisioned
	}
	out, err := runQEMU(ctx, "qemu-img", "create", "-f", "qcow2", disk, strconv.FormatInt(diskSize, 10))
	if err != nil {
		return qemuErr("qemu-img create", err, out)
	}
	return nil
}

func (b *Backend) Start(ctx context.Context, env core.Environment) error {
	return b.start(ctx, env, false)
}

// StartEphemeral starts a session whose disk writes go to a temporary
// overlay QEMU throws away when it exits (-snapshot), so the Machine
// shuts down exactly as it was. The overlay lives in the Machine's state
// directory rather than /var/tmp: it grows on the same disk as the qcow2,
// where a full disk is already explained (ioErrorStatus).
func (b *Backend) StartEphemeral(ctx context.Context, env core.Environment) error {
	return b.start(ctx, env, true)
}

// isEphemeral reports whether the running QEMU was started by
// StartEphemeral, read from its command line like the vsock CID.
func (b *Backend) isEphemeral(name string) bool {
	pid, err := b.readPID(name)
	return err == nil && processHasArg(pid, "-snapshot")
}

func (b *Backend) start(ctx context.Context, env core.Environment, ephemeral bool) error {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return err
	}
	if running {
		if ephemeral && !b.isEphemeral(b.key(env)) {
			return core.Invalidf("%s is already running and keeping its changes; shut it down first to start without keeping them", env.Name)
		}
		return nil
	}
	if err := b.checkNameLength(b.key(env)); err != nil {
		return err
	}
	if ended := b.unexpectedEnd(b.key(env)); ended != "" {
		// Kept in the log: the next Start overwrites the evidence.
		slog.Warn("previous session ended unexpectedly", "machine", env.Name, "detail", ended)
	}

	for _, socket := range []string{b.qgaPath(b.key(env)), b.qmpPath(b.key(env))} {
		if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}
	b.growDisk(ctx, env)
	settings := env.EffectiveSettings()
	if !settings.TravelModeDisabled && env.Settings.CPUs == 0 && power.OnBattery() {
		// Travel Mode: the host is unplugged and the user hasn't pinned
		// CPUs explicitly, so trim this session's allocation — never the
		// persisted setting — instead of running full tilt on battery.
		// Real host automation, not a manual toggle (UX Principles #4/#7).
		if reduced := settings.CPUs / 2; reduced >= 1 {
			settings.CPUs = reduced
		}
		slog.Info("travel mode: reduced CPU allocation while on battery", "machine", env.Name, "cpus", settings.CPUs)
	}
	if total, ok := hostMemoryMiB(); ok && settings.MemoryMiB > total {
		return core.Invalidf("%s is set to %d MiB of memory, more than this computer has (%d MiB); lower it in Settings", env.Name, settings.MemoryMiB, total)
	}
	virtiofsRunning := false
	if settings.SharedPath != "" {
		if err := b.startVirtiofs(ctx, b.key(env), settings); err != nil {
			return err
		}
		virtiofsRunning = true
	}

	args := []string{
		"-name", env.Name,
		"-m", strconv.Itoa(settings.MemoryMiB),
		"-smp", strconv.Itoa(settings.CPUs),
		"-enable-kvm",
		"-drive", fmt.Sprintf("file=%s,if=virtio,format=qcow2", b.diskPath(b.key(env))),
		// Absolute pointer: the viewer sends guest coordinates, never
		// relative motion that drifts from the host cursor.
		"-device", "qemu-xhci",
		"-device", "usb-tablet",
		"-device", "virtio-serial-pci",
		"-chardev", "qemu-vdagent,id=clipboard,clipboard=on,mouse=off",
		"-device", "virtserialport,chardev=clipboard,name=com.redhat.spice.0",
		"-chardev", "socket,path=" + b.qgaPath(b.key(env)) + ",server=on,wait=off,id=qga0",
		"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
		"-audiodev", "pipewire,id=audio0",
		"-device", "virtio-sound-pci,audiodev=audio0",
		"-qmp", "unix:" + b.qmpPath(b.key(env)) + ",server,nowait",
		"-pidfile", b.pidPath(b.key(env)),
		"-daemonize",
	}
	graphics := detectGraphics(ctx)
	vulkan := graphics.vulkan && !settings.VulkanDisabled
	slog.Info("graphics", "machine", env.Name, "opengl", graphics.openGL, "vulkan", vulkan, "detail", graphics.vulkanDetail)
	args = append(args, displayArgs(graphics.openGL, vulkan, settings.MemoryMiB, virtiofsRunning)...)
	if virtiofsRunning {
		args = append(args,
			"-chardev", "socket,id=virtiofs,path="+b.virtiofsPath(b.key(env)),
			"-device", "vhost-user-fs-pci,chardev=virtiofs,tag=omavm-share")
	}
	var qemuEnv []string
	if ephemeral {
		args = append(args, "-snapshot")
		qemuEnv = []string{"TMPDIR=" + b.dir(b.key(env))}
	}
	if env.Image != "" && !settings.DisconnectISO {
		// The disk boots first anyway: an ISO deleted, moved or on a USB
		// stick that isn't plugged in must not keep an installed Machine
		// from starting (QEMU refuses a -cdrom it can't open).
		// Opened, not just stat'ed: a file this user can't read makes
		// QEMU refuse to start at all ("Permission denied").
		if f, err := os.Open(env.Image); err == nil {
			f.Close()
			args = append(args, "-cdrom", env.Image, "-boot", "order=cd,menu=on")
		} else {
			slog.Warn("installation media not found; starting from the disk", "machine", env.Name, "image", env.Image, "error", err)
		}
	}

	vsock := !settings.SSHDisabled && canOpenRW(vhostVsockPath)
	var cid uint32
	if vsock {
		if cid, err = b.guestCID(b.key(env), false); err != nil {
			return err
		}
	}
	out, err := runQEMUEnv(ctx, qemuEnv, "qemu-system-x86_64", withVsock(args, vsock, cid)...)
	if err != nil && vsock && cidTaken(out) {
		// Another VM on this host holds the CID: draw a new one. The
		// guest's SSH host key is remembered per Machine, not per CID.
		if cid, err = b.guestCID(b.key(env), true); err != nil {
			return err
		}
		out, err = runQEMUEnv(ctx, qemuEnv, "qemu-system-x86_64", withVsock(args, vsock, cid)...)
	}
	if err != nil {
		// Two concurrent Start calls can both observe "not running" above
		// before either qemu-system-x86_64 process exists (docs/TODO.md
		// P0, reproduced 2026-09-27: two `omavm start` fired in parallel
		// against the same stopped Machine). qemu-system-x86_64's own
		// -pidfile uses a real flock, so the loser here reliably fails
		// fast with "cannot create PID file" instead of actually running
		// a second instance against the same disk — but surfacing that
		// raw error to whichever caller lost the race would look like a
		// genuine startup failure. If the Machine is actually running
		// now (the other Start won), this call's goal was met: treat it
		// as the idempotent success it already is a few lines up when
		// isRunning() finds it running before even trying.
		if running, runningErr := b.isRunning(b.key(env)); runningErr == nil && running {
			return nil
		}
		if virtiofsRunning {
			_ = b.stopVirtiofs(b.key(env))
		}
		if known := startFailure(env, out); known != nil {
			return known
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && !canOpenRW(filepath.Join(devRoot, "kvm")) {
			// QEMU's own words ("Could not access KVM kernel module")
			// don't say what to do about it. Only when QEMU actually ran:
			// a missing qemu-system-x86_64 already has its own message.
			return core.Unsupportedf("this computer can't run Desktops yet: hardware virtualization (/dev/kvm) is missing or not accessible to your user. Enable virtualization in the firmware and add your user to the kvm group, then log in again (QEMU said: %s)", out)
		}
		return qemuErr("qemu-system-x86_64", err, out)
	}
	return nil
}

// growDisk brings a stopped Machine's disk up to diskSize: Machines
// created when disks were 20 GiB, and any Machine sent back to a snapshot
// taken before its disk grew (a qcow2 snapshot keeps its own size). Only
// the virtual size changes, so it costs no host space; the guest sees the
// new space as unpartitioned until its own partition is extended. Never
// shrinks a disk, and best-effort: a Machine still starts with the disk
// it has.
func (b *Backend) growDisk(ctx context.Context, env core.Environment) {
	disk := b.diskPath(b.key(env))
	size, err := diskVirtualSize(ctx, disk)
	if err != nil || size >= diskSize {
		if err != nil {
			slog.Warn("disk size not checked", "machine", env.Name, "error", err)
		}
		return
	}
	if out, err := runQEMU(ctx, "qemu-img", "resize", disk, strconv.FormatInt(diskSize, 10)); err != nil {
		slog.Warn("disk not grown", "machine", env.Name, "error", qemuErr("qemu-img resize", err, out))
		return
	}
	slog.Info("disk grown", "machine", env.Name, "from", size, "to", diskSize)
}

func diskVirtualSize(ctx context.Context, disk string) (int64, error) {
	out, err := runQEMU(ctx, "qemu-img", "info", "--output=json", disk)
	if err != nil {
		return 0, qemuErr("qemu-img info", err, out)
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return 0, fmt.Errorf("qemu-img info: %w", err)
	}
	return info.VirtualSize, nil
}

// virtiofsdPath is overridable in tests, like the host paths in hostcaps.go.
var virtiofsdPath = func() (string, error) {
	if path, err := exec.LookPath("virtiofsd"); err == nil {
		return path, nil
	}
	if info, err := os.Stat("/usr/lib/virtiofsd"); err == nil && info.Mode().Perm()&0o111 != 0 {
		return "/usr/lib/virtiofsd", nil
	}
	return "", fmt.Errorf("virtiofsd not found (install the virtiofsd package)")
}

// qemuBinaryPath resolves name (qemu-img or qemu-system-x86_64) via PATH,
// giving the same kind of actionable, install-hint error virtiofsdPath
// already provides instead of letting a missing binary surface as Go's
// raw "executable file not found in $PATH".
func qemuBinaryPath(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("%s not found (install it with: sudo pacman -S qemu-desktop)", name)
}

// displayArgs returns the display device and backend and, when something
// needs to share guest RAM with another process, a memfd memory backend.
// virtiofsd maps guest RAM directly; Venus (Vulkan) blob resources are
// exported to the host GPU as dma-bufs through udmabuf, which also needs
// memfd-backed RAM.
//
// The display is QEMU's D-Bus display in peer-to-peer mode: nothing is
// listening until Open hands the viewer a connection over QMP. With a
// usable host GPU, frames reach the viewer as dma-bufs (no copies through
// the CPU); without one, the guest gets a plain virtio-vga and frames are
// shared through memory mappings.
func displayArgs(openGL, vulkan bool, memoryMiB int, virtiofs bool) []string {
	vulkan = vulkan && openGL
	device, display := "virtio-vga", "dbus,p2p=yes"
	if openGL {
		device, display = "virtio-vga-gl", "dbus,p2p=yes,gl=on"
		if vulkan {
			// hostmem sizes the PCI BAR host-visible Vulkan memory is
			// mapped through: it reserves guest address space, not host RAM.
			device += ",blob=on,hostmem=4G,venus=on"
		}
	}
	args := []string{"-device", device, "-display", display}
	if vulkan || virtiofs {
		args = append(args,
			"-object", fmt.Sprintf("memory-backend-memfd,id=mem,size=%dM,share=on", memoryMiB),
			"-machine", "memory-backend=mem")
	}
	return args
}

func runQEMU(ctx context.Context, name string, args ...string) (string, error) {
	return runQEMUEnv(ctx, nil, name, args...)
}

// runQEMUEnv is runQEMU with extra environment variables on top of
// OmaVM's own.
func runQEMUEnv(ctx context.Context, env []string, name string, args ...string) (string, error) {
	path, err := qemuBinaryPath(name)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// qemuErr wraps a failed qemu-img/qemu-system-x86_64 invocation with the
// action that failed. out is empty when the binary itself was missing
// (qemuBinaryPath already produced a complete message), so it is only
// appended when there is command output to show.
func qemuErr(action string, err error, out string) error {
	if out == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, out)
}

func (b *Backend) startVirtiofs(ctx context.Context, name string, settings core.EnvironmentSettings) error {
	// Checked here, not only when configured: the folder may have been
	// moved or deleted since, and the Machine should say so plainly.
	if info, err := os.Stat(settings.SharedPath); err != nil || !info.IsDir() {
		return core.Invalidf("shared folder %s no longer exists or is not a folder; choose another one in Settings or remove it", settings.SharedPath)
	}
	path, err := virtiofsdPath()
	if err != nil {
		return fmt.Errorf("shared folder: %w", err)
	}
	if err := os.Remove(b.virtiofsPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale virtiofs socket: %w", err)
	}
	args := []string{"--shared-dir", settings.SharedPath, "--socket-path", b.virtiofsPath(name), "--sandbox", "namespace", "--tag", "omavm-share"}
	if settings.SharedReadOnly {
		args = append(args, "--readonly")
	}
	// virtiofsd outlives this command, so its stderr goes to a file: a pipe
	// read by this short-lived process would break (SIGPIPE) once it exits.
	logPath := filepath.Join(b.dir(name), "virtiofsd.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("shared folder: %w", err)
	}
	defer logFile.Close()
	cmd := exec.CommandContext(context.Background(), path, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start virtiofsd: %w", err)
	}
	if err := os.WriteFile(b.virtiofsPIDPath(name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("record virtiofsd pid: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(b.virtiofsPath(name)); err == nil {
			return nil
		}
		select {
		case waitErr := <-exited:
			_ = os.Remove(b.virtiofsPIDPath(name))
			return fmt.Errorf("shared folder %s could not be shared: %s", settings.SharedPath, virtiofsdFailure(logPath, waitErr))
		case <-ctx.Done():
			_ = b.stopVirtiofs(name)
			return ctx.Err()
		case <-deadline:
			_ = b.stopVirtiofs(name)
			return fmt.Errorf("shared folder %s could not be shared: virtiofsd did not start in time", settings.SharedPath)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// virtiofsdFailure is the end of virtiofsd's own output, which names the
// actual problem (permissions, sandboxing), or its exit status.
func virtiofsdFailure(logPath string, waitErr error) string {
	data, _ := os.ReadFile(logPath)
	text := strings.TrimSpace(string(data))
	if len(text) > 500 {
		text = text[len(text)-500:]
	}
	if text == "" && waitErr != nil {
		return "virtiofsd " + waitErr.Error()
	}
	return text
}

func (b *Backend) stopVirtiofs(name string) error {
	data, err := os.ReadFile(b.virtiofsPIDPath(name))
	if err == nil {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if parseErr == nil && processHasArg(pid, b.virtiofsPath(name)) {
			if proc, findErr := os.FindProcess(pid); findErr == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
		}
	}
	_ = os.Remove(b.virtiofsPIDPath(name))
	_ = os.Remove(b.virtiofsPath(name))
	return nil
}

// Open ensures the Machine is running, then launches omavm-gui in its
// native VNC viewer mode so the graphical userspace is actually visible —
// not left as a socket nobody's looking at. Requires omavm-gui to be
// installed; without it, this fails loudly with what to install, instead
// of silently no-op'ing (Security/UX principle: never hide a capability
// gap).
func (b *Backend) Open(ctx context.Context, env core.Environment) error {
	if err := b.Start(ctx, env); err != nil {
		return err
	}
	viewer, err := guiBinaryPath()
	if err != nil {
		return core.Unsupportedf("omavm-gui not found (install OmaVM's GUI to view a Machine's display)")
	}
	display, err := b.attachDisplay(b.key(env))
	if err != nil {
		return err
	}
	defer display.Close()

	// Detached on purpose: the viewer is a GUI the user drives for a
	// while, not something that should die when this call's context
	// ends (same reasoning as the GUI's own openInTerminal). It inherits
	// its end of the display connection as fd 3.
	cmd := exec.CommandContext(context.Background(), viewer, viewerArgs(env)...)
	cmd.ExtraFiles = []*os.File{display}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch display viewer: %w", err)
	}
	return nil
}

// viewerArgs are omavm-gui's arguments for a Machine's display. The
// viewer itself picks the workspace and goes fullscreen, so it happens
// however the Machine was opened: Experience Center, launcher entry or
// `omavm open`.
func viewerArgs(env core.Environment) []string {
	settings := env.EffectiveSettings()
	return []string{
		"--display-fd", "3",
		"--title", env.Name + " — OmaVM",
		// Lets the viewer reconnect after losing the display (omavm open).
		"--environment", env.Name,
		"--share-clipboard", settings.ClipboardMode(),
		"--empty-workspace", strconv.FormatBool(!settings.EmptyWorkspaceDisabled),
		"--fullscreen", strconv.FormatBool(!settings.FullscreenDisabled),
	}
}

// attachDisplay opens a new peer-to-peer connection to the Machine's D-Bus
// display: one end of a socket pair goes to QEMU over QMP, the other is
// returned for the viewer.
func (b *Backend) attachDisplay(name string) (*os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create display connection: %w", err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "display"), os.NewFile(uintptr(fds[1]), "display-qemu")
	defer theirs.Close()
	if err := qmpAddDisplayClient(b.qmpPath(name), theirs); err != nil {
		ours.Close()
		if strings.Contains(err.Error(), "D-Bus display is not in use") || strings.Contains(err.Error(), "not accepted in bus mode") {
			return nil, core.Unsupportedf("this Machine was started by an older OmaVM; restart it to open its display")
		}
		return nil, fmt.Errorf("connect to the Machine's display: %w", err)
	}
	return ours, nil
}

// guiBinaryPath locates omavm-gui the same way the GUI itself locates the
// omavm CLI (gui/backend.cpp's cliPath): next to this process's own binary
// first, falling back to PATH.
func guiBinaryPath() (string, error) {
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "omavm-gui")
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	return exec.LookPath("omavm-gui")
}

// Preview captures a screenshot of a running Machine's graphical
// userspace via QMP screendump — no SPICE client implementation
// needed, since QEMU writes the framebuffer straight to a local file.
// Returns a PPM image path; gdk-pixbuf loads PNM natively, so the GUI
// can hand this straight to a Picture/Image widget.
func (b *Backend) Preview(ctx context.Context, env core.Environment) (string, error) {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return "", err
	}
	if !running {
		return "", core.Unsupportedf("machine is not running")
	}

	dst := b.previewPath(b.key(env))
	if err := qmpScreendump(b.qmpPath(b.key(env)), dst); err != nil {
		return "", fmt.Errorf("screendump: %w", err)
	}
	return dst, nil
}

// bootDisk is the QMP device name QEMU gives the Machine's disk, the
// first -drive if=virtio.
const bootDisk = "virtio0"

// Snapshots are internal qcow2 snapshots of the disk. A stopped Machine's
// disk is changed directly with qemu-img. A running one gets a disk-only
// snapshot through QMP, as if the power had been cut at that moment: a
// full savevm (memory included) is refused by devices every Machine has
// (tested on QEMU 11.1: "State blocked by non-migratable device
// virtio-sound", and "virgl is not yet migratable" with 3D acceleration),
// so snapshots of a running Machine never worked before this.
func (b *Backend) CreateSnapshot(ctx context.Context, env core.Environment, tag string) (bool, error) {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return false, err
	}
	if !running {
		out, err := runQEMU(ctx, "qemu-img", "snapshot", "-c", tag, b.diskPath(b.key(env)))
		if err != nil {
			return false, qemuErr("qemu-img snapshot -c", err, out)
		}
		return false, nil
	}
	if b.isEphemeral(b.key(env)) {
		return false, errEphemeralSnapshot(env)
	}
	// With the guest agent, the guest flushes and freezes its
	// filesystems for the instant of the snapshot.
	thaw := freezeGuest(ctx, b.qgaPath(b.key(env)), env.Name)
	if thaw != nil {
		defer thaw()
	}
	if _, err := qmpExecuteTimeout(b.qmpPath(b.key(env)), "blockdev-snapshot-internal-sync", map[string]any{"device": bootDisk, "name": tag}, qmpSlowTimeout); err != nil {
		// No reply: look before deciding, rather than report a failure
		// for a snapshot that exists (it would sit on the disk, unknown).
		if errors.Is(err, errQMPNoReply) {
			if names, qerr := qmpDiskSnapshots(b.qmpPath(b.key(env))); qerr == nil && names[tag] {
				return thaw == nil, nil
			}
		}
		return false, fmt.Errorf("snapshot the disk: %w", err)
	}
	return thaw == nil, nil
}

// errEphemeralSnapshot: during a session started without keeping changes,
// QEMU's active disk is the temporary overlay, so a snapshot taken through
// QMP would vanish at shutdown and a deletion would miss the real disk.
func errEphemeralSnapshot(env core.Environment) error {
	return core.Unsupportedf("%s is running without keeping changes, and a snapshot taken now would be discarded with them; shut it down to manage snapshots", env.Name)
}

// GoToSnapshot restores the disk to a snapshot. Only a stopped Machine:
// QEMU refuses to revert a disk-only snapshot under a running system
// ("Revert to it offline using qemu-img"), and every snapshot here is
// disk-only.
func (b *Backend) GoToSnapshot(ctx context.Context, env core.Environment, tag string) error {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return err
	}
	if running {
		return core.Invalidf("shut down %s first: going to a snapshot replaces its disk, which can't change under a running system", env.Name)
	}
	out, err := runQEMU(ctx, "qemu-img", "snapshot", "-a", tag, b.diskPath(b.key(env)))
	if err != nil {
		if strings.Contains(out, "Failed to load snapshot: No such file or directory") {
			return fmt.Errorf("%w: delete it from the list (%s)", core.ErrSnapshotGone, out)
		}
		return qemuErr("qemu-img snapshot -a", err, out)
	}
	return nil
}

func (b *Backend) RemoveSnapshot(ctx context.Context, env core.Environment, tag string) error {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return err
	}
	if !running {
		out, err := runQEMU(ctx, "qemu-img", "snapshot", "-d", tag, b.diskPath(b.key(env)))
		if err != nil {
			if strings.Contains(out, "snapshot not found") {
				return fmt.Errorf("%w (%s)", core.ErrSnapshotGone, out)
			}
			return qemuErr("qemu-img snapshot -d", err, out)
		}
		return nil
	}
	if b.isEphemeral(b.key(env)) {
		return errEphemeralSnapshot(env)
	}
	if _, err := qmpExecuteTimeout(b.qmpPath(b.key(env)), "blockdev-snapshot-delete-internal-sync", map[string]any{"device": bootDisk, "name": tag}, qmpSlowTimeout); err != nil {
		if errors.Is(err, errQMPNoReply) {
			if names, qerr := qmpDiskSnapshots(b.qmpPath(b.key(env))); qerr == nil && !names[tag] {
				return nil // it is gone: the deletion went through
			}
		}
		var qerr *qmpError
		if errors.As(err, &qerr) && strings.Contains(qerr.Desc, "does not exist") {
			return fmt.Errorf("%w (%s)", core.ErrSnapshotGone, qerr.Desc)
		}
		return fmt.Errorf("delete the disk snapshot: %w", err)
	}
	return nil
}

func (b *Backend) Stop(ctx context.Context, env core.Environment) error {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return err
	}
	if !running {
		_ = b.stopVirtiofs(b.key(env))
		return nil
	}
	wasPaused := false
	if status, err := qmpStatus(b.qmpPath(b.key(env))); err == nil && (status == "paused" || status == "suspended") {
		if err := qmpCommand(b.qmpPath(b.key(env)), "cont"); err != nil {
			return fmt.Errorf("resume machine before shutdown: %w", err)
		}
		wasPaused = true
	}

	// A slow guest must never turn an ordinary shutdown into a power cut.
	if err := qmpCommand(b.qmpPath(b.key(env)), "system_powerdown"); err != nil {
		return fmt.Errorf("request shutdown: %w (use Force Stop only if necessary; unsaved work may be lost)", err)
	} else {
		deadline := time.Now().Add(gracefulShutdownTimeout)
		for time.Now().Before(deadline) {
			running, err := b.isRunning(b.key(env))
			if err != nil {
				return err
			}
			if !running {
				_ = b.stopVirtiofs(b.key(env))
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if wasPaused {
		// It was resumed only so it could shut down: put it back the
		// way it was found rather than leave it running.
		if err := qmpCommand(b.qmpPath(b.key(env)), "stop"); err == nil {
			return fmt.Errorf("shutdown is taking longer than expected; the machine was paused again, as it was, to protect your work. Resume it and shut it down from inside, or use Force Stop if necessary")
		}
	}
	return fmt.Errorf("shutdown is taking longer than expected; the machine was left running to protect your work. Wait or use Force Stop if necessary")
}

func (b *Backend) Restart(ctx context.Context, env core.Environment) error {
	if running, err := b.isRunning(b.key(env)); err != nil {
		return err
	} else if !running {
		return b.Start(ctx, env)
	}
	// A session started without keeping changes restarts the same way:
	// restarting must not quietly begin keeping them.
	ephemeral := b.isEphemeral(b.key(env))
	if err := b.Stop(ctx, env); err != nil {
		return err
	}
	return b.start(ctx, env, ephemeral)
}

func (b *Backend) Pause(ctx context.Context, env core.Environment) error {
	if running, err := b.isRunning(b.key(env)); err != nil {
		return err
	} else if !running {
		return core.Unsupportedf("machine is not running")
	}
	return qmpCommand(b.qmpPath(b.key(env)), "stop")
}

func (b *Backend) Resume(ctx context.Context, env core.Environment) error {
	if running, err := b.isRunning(b.key(env)); err != nil {
		return err
	} else if !running {
		return core.Unsupportedf("machine is not running")
	}
	if err := qmpCommand(b.qmpPath(b.key(env)), "cont"); err != nil {
		return err
	}
	// A Machine paused because its disk couldn't be written retries the
	// write on cont and pauses again at once if the host's disk is still
	// full: say so instead of reporting a Resume that didn't hold.
	for i := 0; i < resumeChecks; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(resumeCheckInterval):
		}
		if status, err := qmpStatus(b.qmpPath(b.key(env))); err == nil && status == "io-error" {
			return core.Unsupportedf("%s paused again: %s", env.Name, ioErrorStatus(b.dir(b.key(env))).Detail)
		}
	}
	return nil
}

// How long Resume watches for a Machine pausing again on a disk error.
var (
	resumeChecks        = 3
	resumeCheckInterval = 100 * time.Millisecond
)

func (b *Backend) ForceStop(ctx context.Context, env core.Environment) error {
	if running, err := b.isRunning(b.key(env)); err != nil {
		return err
	} else if !running {
		_ = b.stopVirtiofs(b.key(env))
		return nil
	}
	if err := qmpCommand(b.qmpPath(b.key(env)), "quit"); err != nil {
		pid, err := b.readPID(b.key(env))
		if err != nil {
			return err
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("find machine process: %w", err)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("stop machine process: %w", err)
		}
	}
	_ = b.stopVirtiofs(b.key(env))
	// QEMU takes a moment to exit after quit. Returning before that let a
	// Delete right after Force Stop find it still running and try a
	// normal shutdown through the QMP socket QEMU had already closed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if running, err := b.isRunning(b.key(env)); err != nil || !running {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("the machine is still shutting down; try again in a moment")
}

func (b *Backend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return core.Status{}, err
	}
	if !running {
		warnings := []string{}
		if ended := b.unexpectedEnd(b.key(env)); ended != "" {
			warnings = append(warnings, ended)
		}
		if low := lowSpaceWarning(b.dir(b.key(env))); low != "" {
			warnings = append(warnings, low)
		}
		return core.Status{State: core.StateStopped, Warning: strings.Join(warnings, "; ")}, nil
	}
	status, err := qmpStatus(b.qmpPath(b.key(env)))
	if err != nil {
		return core.Status{State: core.StateUnknown, Detail: err.Error()}, nil
	}
	var st core.Status
	if status == "io-error" {
		st = ioErrorStatus(b.dir(b.key(env)))
	} else {
		st = statusFromQMP(status)
		st.Warning = lowSpaceWarning(b.dir(b.key(env)))
	}
	st.Ephemeral = b.isEphemeral(b.key(env))
	if a, ok := b.appliedConfig(b.key(env)); ok {
		st.RestartNeeded, st.TravelMode = sessionAdjustments(env, a, canOpenRW(vhostVsockPath))
	}
	return st, nil
}

// startFailure turns QEMU's start errors that have a known cause into one
// that says what to do; nil when the output isn't one of them.
func startFailure(env core.Environment, out string) error {
	switch {
	case strings.Contains(out, `Failed to get "write" lock`):
		return core.Busyf("%s's disk is in use by another program (another QEMU, or qemu-img); close it and try again (QEMU said: %s)", env.Name, out)
	case strings.Contains(out, "cannot set up guest memory"):
		return core.Unsupportedf("this computer doesn't have %d MiB of memory free for %s right now; close some programs or lower its memory in Settings (QEMU said: %s)", env.EffectiveSettings().MemoryMiB, env.Name, out)
	}
	return nil
}

// hostMemoryMiB is the host's total memory, from /proc/meminfo.
func hostMemoryMiB() (int, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			kib, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(rest), " kB"))
			if err != nil {
				return 0, false
			}
			return kib / 1024, true
		}
	}
	return 0, false
}

// unexpectedEnd explains a session that ended without shutting down. QEMU
// deletes its pidfile when it exits, whether the guest powered off or
// Force Stop quit it (checked on QEMU 11.1); a pidfile left behind with no
// QEMU on it means the process was killed or crashed (the host ran out of
// memory, a QEMU bug). Its stderr is gone with -daemonize, so the system's
// own records are where the cause is.
func (b *Backend) unexpectedEnd(name string) string {
	info, err := os.Stat(b.pidPath(name))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("the session started %s ended without shutting down (the process was killed or crashed); if it happens again, `coredumpctl list qemu-system-x86_64` or `journalctl -b -k` may show why",
		info.ModTime().Local().Format("2006-01-02 15:04"))
}

// lowSpace is the free space under which a Machine is warned about: its
// sparse disk claims host space as the guest writes, and an update or an
// installer can take a few GiB at once.
const lowSpace = 4 << 30

// lowSpaceWarning warns before the host's disk fills up under a Machine,
// when there is still time to free space. It can't promise anything: the
// guest can write more than what is free now at any moment.
func lowSpaceWarning(dir string) string {
	free, ok := freeSpace(dir)
	if !ok {
		return ""
	}
	return spaceWarning(free)
}

func spaceWarning(free uint64) string {
	if free >= lowSpace {
		return ""
	}
	return fmt.Sprintf("only %s free on this computer: the Machine pauses if it runs out", formatSize(free))
}

func freeSpace(dir string) (uint64, bool) {
	// A Machine's directory may not exist yet; its disk lands in the
	// state directory above it.
	for ; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(dir, &fs); err == nil {
			return fs.Bavail * uint64(fs.Bsize), true
		}
	}
	return 0, false
}

func formatSize(bytes uint64) string {
	if bytes >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
	}
	if bytes >= 1<<20 {
		return fmt.Sprintf("%d MiB", bytes>>20)
	}
	return fmt.Sprintf("%d KiB", bytes>>10)
}

// ioErrorStatus explains a Machine QEMU paused because its disk couldn't
// be written. The usual cause is the host's disk filling up under the
// sparse qcow2; the Machine is paused, not lost, and Resume continues it
// once there is room.
func ioErrorStatus(dir string) core.Status {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err == nil {
		free := fs.Bavail * uint64(fs.Bsize)
		if free < 1<<30 {
			return core.Status{State: core.StatePaused, Detail: fmt.Sprintf("paused: this computer's disk is full (%d MiB free); free up space, then Resume", free>>20)}
		}
	}
	return core.Status{State: core.StatePaused, Detail: "paused: the Machine's disk could not be written; check the disk holding OmaVM's state, then Resume"}
}

func (b *Backend) Integration(ctx context.Context, env core.Environment) (core.IntegrationReport, error) {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return core.IntegrationReport{}, err
	}
	if !running {
		return core.IntegrationReport{GuestAgent: "stopped", Hint: "Start the Machine to check guest tools", Capabilities: guestCapabilities(env, nil)}, nil
	}
	// The agent's port closed means no agent: no need to wait out a ping
	// that nobody will answer (it held every poll of the list for 700 ms
	// with a guest that has none). Open, the ping confirms it answers.
	channels, _ := guestChannels(b.qmpPath(b.key(env)))
	agent := false
	if open, known := channels["qga0"]; !known || open {
		agent = qgaPing(ctx, b.qgaPath(b.key(env))) == nil
	}
	caps := guestCapabilities(env, b.guestChecks(ctx, env, agent, channels))
	if !agent {
		return core.IntegrationReport{GuestAgent: "unavailable", Hint: "Install and start qemu-guest-agent inside the guest", Capabilities: caps}, nil
	}
	return core.IntegrationReport{GuestAgent: "connected", Capabilities: caps}, nil
}

func statusFromQMP(status string) core.Status {
	switch status {
	case "running":
		return core.Status{State: core.StateRunning, Detail: "display available"}
	case "paused", "suspended":
		return core.Status{State: core.StatePaused}
	case "prelaunch", "inmigrate", "restore-vm":
		return core.Status{State: core.StateStarting, Detail: status}
	case "shutdown":
		return core.Status{State: core.StateStopping}
	case "internal-error", "watchdog", "guest-panicked":
		return core.Status{State: core.StateError, Detail: status}
	default:
		return core.Status{State: core.StateUnknown, Detail: status}
	}
}

// Exec has no guest command channel yet (no omavm-guest agent, see
// CLAUDE.md): Machines don't get Exec in Phase 1, and this must fail
// loudly rather than pretend to run something inside the guest.
func (b *Backend) Remove(ctx context.Context, env core.Environment) error {
	if err := b.Stop(ctx, env); err != nil {
		return err
	}
	if err := os.RemoveAll(b.dir(b.key(env))); err != nil {
		return fmt.Errorf("remove machine state dir: %w", err)
	}
	return nil
}

func (b *Backend) readPID(name string) (int, error) {
	data, err := os.ReadFile(b.pidPath(name))
	if err != nil {
		return 0, fmt.Errorf("read pidfile: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile: %w", err)
	}
	return pid, nil
}

func (b *Backend) isRunning(name string) (bool, error) {
	pid, err := b.readPID(name)
	if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return processHasArg(pid, b.pidPath(name)), nil
}

// processHasArg reports whether pid is running with arg on its command
// line, which is how a pidfile is tied to the process OmaVM started (QEMU
// gets -pidfile <path>, virtiofsd --socket-path <path>). A pid alone is not
// enough: after a crash or a reboot the pidfile stays behind, and its
// number can belong to any other process by then. Trusting it showed the
// Machine as running, made Start a silent no-op, and sent SIGTERM to that
// stranger on Force Stop.
func processHasArg(pid int, arg string) bool {
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	for _, a := range strings.Split(string(cmdline), "\x00") {
		if a == arg {
			return true
		}
	}
	return false
}

func runOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// cloneMargin is the free space left over after a Machine's disk is
// copied, so the copy doesn't leave the host's disk full.
const cloneMargin = 1 << 30

// Clone copies a stopped Machine's disk, internal snapshots included, to
// a new Machine. cp keeps the file sparse and, on a filesystem that
// supports it (btrfs, xfs), shares the blocks until either side writes.
func (b *Backend) Clone(ctx context.Context, source, clone core.Environment) error {
	src, dst := b.key(source), b.key(clone)
	running, err := b.isRunning(src)
	if err != nil {
		return err
	}
	if running {
		return core.Invalidf("shut down %s first: its disk can't be copied while it runs", source.Name)
	}
	if err := b.checkNameLength(dst); err != nil {
		return err
	}
	disk := b.diskPath(src)
	info, err := os.Stat(disk)
	if err != nil {
		return fmt.Errorf("%s's disk: %w", source.Name, err)
	}
	used := uint64(0)
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		used = uint64(st.Blocks) * 512
	}
	if free, ok := freeSpace(b.stateDir); ok && free < used+cloneMargin {
		return core.Unsupportedf("not enough space to copy %s: its disk uses %s and this computer has %s free", source.Name, formatSize(used), formatSize(free))
	}
	if err := os.MkdirAll(b.dir(dst), 0o755); err != nil {
		return fmt.Errorf("create machine state dir: %w", err)
	}
	core.ReportProgress(ctx, "Copying the disk of %s (%s)", source.Name, formatSize(used))
	if out, err := runOutput(ctx, "cp", "--reflink=auto", "--sparse=always", disk, b.diskPath(dst)); err != nil {
		_ = os.RemoveAll(b.dir(dst))
		return fmt.Errorf("copy the disk of %s: %w: %s", source.Name, err, out)
	}
	return nil
}
