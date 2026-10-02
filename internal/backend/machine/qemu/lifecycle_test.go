package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
	"github.com/KitsuneForgering/OmaVM/internal/power"
)

func TestSlowShutdownNeverSendsQuit(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	listener, err := net.Listen("unix", b.qmpPath(env.Name))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan string, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
			enc.Encode(map[string]any{"QMP": map[string]any{}})
			var req struct {
				Execute string `json:"execute"`
			}
			if dec.Decode(&req) == nil {
				enc.Encode(map[string]any{"return": map[string]any{}})
				if dec.Decode(&req) == nil {
					commands <- req.Execute
					enc.Encode(map[string]any{"return": map[string]any{"status": "running"}})
				}
			}
			conn.Close()
		}
	}()
	err = b.Stop(context.Background(), env)
	listener.Close()
	<-done
	close(commands)
	if err == nil || !strings.Contains(err.Error(), "left running") {
		t.Fatalf("slow shutdown: %v", err)
	}
	for command := range commands {
		if command != "query-status" && command != "system_powerdown" {
			t.Errorf("unsafe shutdown command: %s", command)
		}
	}
}

func TestShutdownFailurePreservesGuest(t *testing.T) {
	for _, action := range []string{"stop", "restart", "remove"} {
		t.Run(action, func(t *testing.T) {
			b := &Backend{stateDir: t.TempDir()}
			env := core.Environment{Name: "guest", Kind: core.Machine}
			child := startFakeQEMU(t, b, env.Name)
			var err error
			switch action {
			case "stop":
				err = b.Stop(context.Background(), env)
			case "restart":
				err = b.Restart(context.Background(), env)
			case "remove":
				err = b.Remove(context.Background(), env)
			}
			if err == nil {
				t.Fatal("expected shutdown failure")
			}
			if err := child.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("guest was killed: %v", err)
			}
			if _, err := os.Stat(b.dir(env.Name)); err != nil {
				t.Fatalf("guest data removed: %v", err)
			}
		})
	}
}

// recordQMP answers each connection's one command after qmp_capabilities
// and records it with its arguments.
type qmpCall struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments"`
}

func recordQMP(t *testing.T, socket string) (calls func() []qmpCall) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []qmpCall
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
			enc.Encode(map[string]any{"QMP": map[string]any{}})
			var req qmpCall
			if dec.Decode(&req) == nil { // qmp_capabilities
				enc.Encode(map[string]any{"return": map[string]any{}})
				req = qmpCall{}
				if dec.Decode(&req) == nil {
					recorded = append(recorded, req)
					enc.Encode(map[string]any{"return": map[string]any{}})
				}
			}
			conn.Close()
		}
	}()
	return func() []qmpCall {
		listener.Close()
		<-done
		return recorded
	}
}

// Regression: a running Machine's snapshot used savevm, which every
// Machine's devices refuse (virtio-sound, and virgl with 3D), so it
// always failed. It is now a disk-only snapshot through QMP.
func TestSnapshotOfRunningMachineIsDiskOnly(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	calls := recordQMP(t, b.qmpPath(env.Name))
	crash, err := b.CreateSnapshot(context.Background(), env, "before-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if !crash {
		t.Fatal("without a guest agent the snapshot is crash-consistent and must say so")
	}
	if err := b.RemoveSnapshot(context.Background(), env, "before-upgrade"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	want := []string{"blockdev-snapshot-internal-sync", "blockdev-snapshot-delete-internal-sync"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %+v", want, got)
	}
	for i, call := range got {
		if call.Execute != want[i] || call.Arguments["device"] != bootDisk || call.Arguments["name"] != "before-upgrade" {
			t.Fatalf("call %d: %+v", i, call)
		}
	}
}

// Going to a snapshot replaces the disk, which QEMU refuses under a
// running system; say so instead of pausing and failing halfway.
func TestGoToSnapshotNeedsAStoppedMachine(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	calls := recordQMP(t, b.qmpPath(env.Name))
	err := b.GoToSnapshot(context.Background(), env, "before-upgrade")
	if !errors.Is(err, core.ErrInvalidInput) || !strings.Contains(err.Error(), "shut down guest first") {
		t.Fatalf("expected a shut-down-first error, got %v", err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("a running Machine was touched: %+v", got)
	}
}

func TestSnapshotLifecycleOnStoppedMachine(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available")
	}
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	if err := b.Create(context.Background(), env); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx := context.Background()
	if crash, err := b.CreateSnapshot(ctx, env, "clean-install"); err != nil || crash {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	out, err := runOutput(ctx, "qemu-img", "snapshot", "-l", b.diskPath(env.Name))
	if err != nil {
		t.Fatalf("qemu-img snapshot -l: %v: %s", err, out)
	}
	if !strings.Contains(out, "clean-install") {
		t.Fatalf("expected snapshot to be listed, got: %s", out)
	}

	if err := b.GoToSnapshot(ctx, env, "clean-install"); err != nil {
		t.Fatalf("GoToSnapshot: %v", err)
	}
	if err := b.RemoveSnapshot(ctx, env, "clean-install"); err != nil {
		t.Fatalf("RemoveSnapshot: %v", err)
	}
	out, err = runOutput(ctx, "qemu-img", "snapshot", "-l", b.diskPath(env.Name))
	if err != nil {
		t.Fatalf("qemu-img snapshot -l: %v: %s", err, out)
	}
	if strings.Contains(out, "clean-install") {
		t.Fatalf("expected snapshot to be removed, got: %s", out)
	}
}

func TestTravelModeReducesCPUsOnBattery(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	t.Setenv("OMAVM_TEST_ARGS", capture)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAVM_TEST_ARGS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	orig := power.SupplyDir
	defer func() { power.SupplyDir = orig }()
	power.SupplyDir = filepath.Join(dir, "power_supply")
	writePowerSupply(t, power.SupplyDir, "AC", "Mains", "0")

	b := &Backend{stateDir: dir}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "-smp\n1\n") {
		t.Fatalf("expected halved default CPUs (1) on battery, got: %s", data)
	}

	// A user-pinned CPU count must never be overridden by Travel Mode.
	if err := os.Remove(b.pidPath(env.Name)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	env.Settings.CPUs = 4
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "-smp\n4\n") {
		t.Fatalf("expected pinned CPU count to survive Travel Mode, got: %s", data)
	}

	// Opt-out: TravelModeDisabled must stop the reduction even on
	// battery with default CPUs.
	if err := os.Remove(b.pidPath(env.Name)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	env.Settings.CPUs = 0
	env.Settings.TravelModeDisabled = true
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "-smp\n2\n") {
		t.Fatalf("expected Travel Mode opt-out to keep default CPUs (2), got: %s", data)
	}
}

func TestBootMediaArguments(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	t.Setenv("OMAVM_TEST_ARGS", capture)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAVM_TEST_ARGS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	b := &Backend{stateDir: dir}
	iso := filepath.Join(t.TempDir(), "installer.iso")
	if err := os.WriteFile(iso, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env := core.Environment{Name: "guest", Kind: core.Machine, Image: iso}
	for _, disconnect := range []bool{false, true} {
		env.Settings.DisconnectISO = disconnect
		if err := b.Start(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		args := string(data)
		if strings.Contains(args, "-cdrom\n") == disconnect {
			t.Fatalf("unexpected media arguments: %s", args)
		}
		if !disconnect && !strings.Contains(args, "order=cd,menu=on") {
			t.Fatalf("disk must precede installer: %s", args)
		}
	}
}

// TestMissingBinaryDiagnostic verifies that a missing qemu-img/
// qemu-system-x86_64 surfaces an actionable install hint (docs/TODO.md
// P1 "Tornar pré-requisitos e recursos compreensíveis") instead of Go's
// raw "executable file not found in $PATH".
func TestMissingBinaryDiagnostic(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty: neither binary resolves
	// Like CI: no KVM either, which must not hide the missing binary.
	saved := devRoot
	devRoot = t.TempDir()
	t.Cleanup(func() { devRoot = saved })

	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}

	err := b.Create(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "qemu-img not found") || !strings.Contains(err.Error(), "qemu-desktop") {
		t.Fatalf("expected actionable qemu-img missing-binary error, got: %v", err)
	}

	// Create's disk provisioning is required for Start to reach the
	// qemu-system-x86_64 lookup instead of failing on the missing disk.
	if err := os.MkdirAll(b.dir(env.Name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.diskPath(env.Name), []byte("not a real qcow2"), 0600); err != nil {
		t.Fatal(err)
	}
	err = b.Start(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "qemu-system-x86_64 not found") || !strings.Contains(err.Error(), "qemu-desktop") {
		t.Fatalf("expected actionable qemu-system-x86_64 missing-binary error, got: %v", err)
	}
}

// startFakeQEMU runs a stand-in for this Machine's QEMU: a long-lived
// process with -pidfile <its pidfile> on its command line, recorded in
// that pidfile, like the real one.
func startFakeQEMU(t *testing.T, b *Backend, name string, extra ...string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(b.dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	// "; :" keeps sh from exec'ing sleep, which would drop the arguments.
	child := exec.Command("sh", append([]string{"-c", "sleep 60; :", "qemu-system-x86_64", "-pidfile", b.pidPath(name)}, extra...)...)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	// /proc/<pid>/cmdline reads empty for a moment after the exec; the
	// real QEMU writes its pidfile only once it is up.
	for deadline := time.Now().Add(2 * time.Second); !processHasArg(child.Process.Pid, b.pidPath(name)); {
		if time.Now().After(deadline) {
			t.Fatal("fake QEMU never showed its command line")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(b.pidPath(name), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	return child
}

// Regression: a pidfile left by a crash or a reboot, whose pid now belongs
// to another process, made the Machine look running: Start did nothing and
// Force Stop sent SIGTERM to that process.
func TestStalePidfileOfAnotherProcessIsNotRunning(t *testing.T) {
	stranger := exec.Command("sleep", "60")
	if err := stranger.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stranger.Process.Kill(); stranger.Wait() }()
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	if err := os.MkdirAll(b.dir(env.Name), 0700); err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(stranger.Process.Pid)
	if err := os.WriteFile(b.pidPath(env.Name), []byte(pid), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.virtiofsPIDPath(env.Name), []byte(pid), 0600); err != nil {
		t.Fatal(err)
	}
	if running, err := b.isRunning(env.Name); err != nil || running {
		t.Fatalf("isRunning = %t, %v; want false", running, err)
	}
	if err := b.ForceStop(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := stranger.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("Force Stop killed an unrelated process: %v", err)
	}
}

// Without access to /dev/kvm, QEMU's error doesn't say what to do; the
// failure says it, and is classified as unsupported on this computer.
func TestStartWithoutKVMSaysHowToFixIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	script := "#!/bin/sh\necho 'Could not access KVM kernel module: No such file or directory' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	saved := devRoot
	devRoot = t.TempDir() // no kvm node
	t.Cleanup(func() { devRoot = saved })
	b := &Backend{stateDir: dir}
	err := b.Start(context.Background(), core.Environment{Name: "guest", Kind: core.Machine})
	if !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), "kvm group") {
		t.Fatalf("expected an actionable KVM error, got %v", err)
	}
}

// QEMU deletes its pidfile on any normal exit; one left behind means the
// process was killed or crashed, and the stopped Machine says so instead
// of looking like it was shut down.
func TestSessionThatEndedWithoutShuttingDownIsReported(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	st, err := b.Status(context.Background(), env)
	if err != nil || strings.Contains(st.Warning, "without shutting down") {
		t.Fatalf("a Machine that was never started: %+v, %v", st, err)
	}
	if err := os.MkdirAll(b.dir(env.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	// A pid no process has: the session's QEMU is gone.
	if err := os.WriteFile(b.pidPath(env.Name), []byte("2147483646"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = b.Status(context.Background(), env)
	if err != nil || st.State != core.StateStopped || !strings.Contains(st.Warning, "ended without shutting down") || !strings.Contains(st.Warning, "coredumpctl") {
		t.Fatalf("unexpected end not reported: %+v, %v", st, err)
	}
}

func TestKnownStartFailuresSayWhatToDo(t *testing.T) {
	env := core.Environment{Name: "guest", Kind: core.Machine}
	locked := "qemu-system-x86_64: -drive file=/x/disk.qcow2,if=virtio,format=qcow2: Failed to get \"write\" lock\nIs another process using the image [/x/disk.qcow2]?"
	if err := startFailure(env, locked); !errors.Is(err, core.ErrBusy) || !strings.Contains(err.Error(), "in use by another program") {
		t.Fatalf("locked disk: %v", err)
	}
	memory := "qemu-system-x86_64: cannot set up guest memory 'pc.ram': Cannot allocate memory"
	if err := startFailure(env, memory); !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), "lower its memory") {
		t.Fatalf("memory: %v", err)
	}
	if err := startFailure(env, "qemu-system-x86_64: something else"); err != nil {
		t.Fatalf("unknown output must stay as QEMU said it: %v", err)
	}
}

// More memory than the host has is refused before QEMU runs, and an
// installation image this user can't read doesn't stop the Machine from
// starting from its disk.
func TestStartChecksMemoryAndReadableMedia(t *testing.T) {
	if _, ok := hostMemoryMiB(); !ok {
		t.Skip("no /proc/meminfo")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	t.Setenv("OMAVM_TEST_ARGS", capture)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAVM_TEST_ARGS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Backend{stateDir: dir}
	env := core.Environment{Name: "guest", Kind: core.Machine, Settings: core.EnvironmentSettings{MemoryMiB: 1 << 30}}
	if err := b.Start(context.Background(), env); !errors.Is(err, core.ErrInvalidInput) || !strings.Contains(err.Error(), "more than this computer has") {
		t.Fatalf("expected a memory error, got %v", err)
	}
	if _, err := os.Stat(capture); err == nil {
		t.Fatal("QEMU ran anyway")
	}

	if os.Geteuid() == 0 {
		return // root reads anything
	}
	iso := filepath.Join(t.TempDir(), "x.iso")
	if err := os.WriteFile(iso, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	env = core.Environment{Name: "guest", Kind: core.Machine, Image: iso}
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(capture); strings.Contains(string(data), "-cdrom") {
		t.Fatalf("an unreadable ISO was passed to QEMU: %s", data)
	}
}
