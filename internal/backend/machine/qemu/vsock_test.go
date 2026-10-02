package qemu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestParseCID(t *testing.T) {
	cmdline := []byte("qemu-system-x86_64\x00-device\x00vhost-vsock-pci,guest-cid=123456\x00-daemonize\x00")
	if cid, ok := parseCID(cmdline); !ok || cid != 123456 {
		t.Fatalf("parseCID = %d, %v", cid, ok)
	}
	if _, ok := parseCID([]byte("qemu-system-x86_64\x00-m\x002048\x00")); ok {
		t.Fatal("found a CID in a command line without the device")
	}
}

func TestGuestCIDIsStableUntilRedrawn(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	first, err := b.guestCID("m", false)
	if err != nil {
		t.Fatal(err)
	}
	if first <= 0xffff {
		t.Fatalf("CID %d is in the range libvirt hands out", first)
	}
	again, err := b.guestCID("m", false)
	if err != nil || again != first {
		t.Fatalf("CID changed across starts: %d then %d (%v)", first, again, err)
	}
	fresh, err := b.guestCID("m", true)
	if err != nil || fresh == first {
		t.Fatalf("expected a new CID, got %d (%v)", fresh, err)
	}
}

// The guest's shell must see the same arguments the caller passed.
func TestSSHArgsQuoteTheCommand(t *testing.T) {
	env := core.Environment{ID: "abc", Name: "m"}
	args := sshArgs("/proxy", env, 70000, "ana", true, []string{"echo", "a b", "it's", "$HOME"})
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		"ProxyCommand=/proxy %h %p",
		"HostKeyAlias=omavm-abc",
		"StrictHostKeyChecking=accept-new",
		"-l\nana",
		"BatchMode=yes",
		"vsock/70000\n--\necho 'a b' 'it'\\''s' '$HOME'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if interactive := strings.Join(sshArgs("/proxy", env, 70000, "ana", false, nil), " "); strings.Contains(interactive, "BatchMode") || !strings.HasSuffix(interactive, "vsock/70000") {
		t.Errorf("interactive shell args: %s", interactive)
	}
}

// fakeQEMU records each run's arguments on its own line and fails the
// first run with QEMU's "CID in use" error when failFirst is set.
func fakeQEMU(t *testing.T, failFirst bool) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n"
	if failFirst {
		script += "case \"$*\" in *guest-cid=*) ;; *) exit 0;; esac\n"
		script += "if [ ! -e " + dir + "/failed ]; then : > " + dir + "/failed; echo 'vhost-vsock: unable to set guest cid: Address already in use' >&2; exit 1; fi\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return dir, log
}

func useVsock(t *testing.T, available bool) {
	t.Helper()
	orig := vhostVsockPath
	t.Cleanup(func() { vhostVsockPath = orig })
	vhostVsockPath = filepath.Join(t.TempDir(), "vhost-vsock")
	if available {
		if err := os.WriteFile(vhostVsockPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartAddsSSHChannelByDefault(t *testing.T) {
	dir, log := fakeQEMU(t, false)
	useVsock(t, true)
	b := &Backend{stateDir: dir}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "vhost-vsock-pci,guest-cid=") {
		t.Fatalf("expected the vsock device, got: %s", data)
	}
}

func TestStartWithoutSSH(t *testing.T) {
	for name, tc := range map[string]struct {
		available, disabled bool
	}{
		"turned off":         {available: true, disabled: true},
		"host without vsock": {available: false},
	} {
		t.Run(name, func(t *testing.T) {
			dir, log := fakeQEMU(t, false)
			useVsock(t, tc.available)
			b := &Backend{stateDir: dir}
			env := core.Environment{Name: "guest", Kind: core.Machine, Settings: core.EnvironmentSettings{SSHDisabled: tc.disabled}}
			if err := b.Start(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(log)
			if strings.Contains(string(data), "vhost-vsock") {
				t.Fatalf("unexpected vsock device: %s", data)
			}
		})
	}
}

func TestStartRetriesWithANewCIDWhenTaken(t *testing.T) {
	dir, log := fakeQEMU(t, true)
	useVsock(t, true)
	b := &Backend{stateDir: dir}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	if err := b.Start(context.Background(), env); err != nil {
		t.Fatalf("Start should retry with another CID: %v", err)
	}
	data, _ := os.ReadFile(log)
	// detectGraphics also runs the fake binary to list devices.
	var runs []string
	for _, run := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(run, "guest-cid=") {
			runs = append(runs, run)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d:\n%s", len(runs), data)
	}
	cid := func(run string) string {
		i := strings.Index(run, "guest-cid=")
		return strings.Fields(run[i:])[0]
	}
	if cid(runs[0]) == cid(runs[1]) {
		t.Fatalf("retried with the same CID: %s", cid(runs[1]))
	}
}
