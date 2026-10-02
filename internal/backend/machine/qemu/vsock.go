package qemu

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// SSH into a Machine goes over AF_VSOCK, a host↔guest socket that needs no
// guest network: systemd 256+ in the guest listens for sshd on it by
// itself (systemd-ssh-generator), and systemd-ssh-proxy on the host turns
// `vsock/<cid>` into a connection. OmaVM only adds the device and runs ssh.
//
// On by default (EnvironmentSettings.SSHDisabled opts out). The CID is
// global on the host, not per user or per network namespace: any process
// here, a Box included, can reach the guest's sshd, and only the guest's
// login stands in the way. CLI help, Settings and the README say so.

// Overridable in tests.
var (
	vhostVsockPath = "/dev/vhost-vsock"
	sshProxyPaths  = []string{"/usr/lib/systemd/systemd-ssh-proxy", "/usr/libexec/systemd/systemd-ssh-proxy"}
)

func (b *Backend) cidPath(name string) string { return filepath.Join(b.dir(name), "vsock-cid") }

// guestCID returns the Machine's CID, drawing a new one the first time or
// when fresh is set (the old one was taken by another VM on this host).
// It stays the same across restarts, so the guest's SSH host key keeps
// matching. Random above 65535, clear of libvirt's small sequential ones.
func (b *Backend) guestCID(name string, fresh bool) (uint32, error) {
	if !fresh {
		if data, err := os.ReadFile(b.cidPath(name)); err == nil {
			if cid, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32); err == nil && cid > 0xffff {
				return uint32(cid), nil
			}
		}
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, fmt.Errorf("choose SSH channel address: %w", err)
	}
	cid := 0x10000 + binary.LittleEndian.Uint32(buf[:])%(0xfffffff0-0x10000)
	if err := os.MkdirAll(b.dir(name), 0o755); err != nil {
		return 0, fmt.Errorf("record SSH channel address: %w", err)
	}
	if err := os.WriteFile(b.cidPath(name), []byte(strconv.FormatUint(uint64(cid), 10)), 0o644); err != nil {
		return 0, fmt.Errorf("record SSH channel address: %w", err)
	}
	return cid, nil
}

func withVsock(args []string, enabled bool, cid uint32) []string {
	if !enabled {
		return args
	}
	return append(args[:len(args):len(args)], "-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", cid))
}

// cidTaken recognizes QEMU refusing a CID another VM already holds.
func cidTaken(out string) bool {
	return strings.Contains(out, "unable to set guest cid")
}

// runningCID reads the CID from the running QEMU's own command line, so it
// is right even if SSH was turned on or off after the Machine started.
func (b *Backend) runningCID(name string) (uint32, bool) {
	pid, err := b.readPID(name)
	if err != nil {
		return 0, false
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return 0, false
	}
	return parseCID(cmdline)
}

func parseCID(cmdline []byte) (uint32, bool) {
	for _, arg := range bytes.Split(cmdline, []byte{0}) {
		rest, ok := bytes.CutPrefix(arg, []byte("vhost-vsock-pci,guest-cid="))
		if !ok {
			continue
		}
		cid, err := strconv.ParseUint(string(rest), 10, 32)
		return uint32(cid), err == nil
	}
	return 0, false
}

func sshProxyPath() string {
	for _, path := range sshProxyPaths {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

// sshArgs builds the ssh command line. Options given here win over
// systemd's ssh_config.d snippet for vsock/*, which logs in as root and
// turns host-key checking off because it expects throwaway VMs; a Machine
// lasts, so its key is remembered under a per-Machine alias (the CID can
// change) and a changed key stops the connection.
func sshArgs(proxy string, env core.Environment, cid uint32, login string, batch bool, command []string) []string {
	args := []string{
		"-o", "ProxyCommand=" + proxy + " %h %p",
		"-o", "ProxyUseFdpass=yes",
		"-o", "CheckHostIP=no",
		"-o", "HostKeyAlias=omavm-" + env.ID,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=~/.ssh/known_hosts",
		"-l", login,
	}
	if batch {
		// Agents and scripts: fail instead of waiting at a password prompt.
		args = append(args, "-o", "BatchMode=yes")
	}
	args = append(args, fmt.Sprintf("vsock/%d", cid))
	if len(command) > 0 {
		// ssh hands the remote shell one string; quoting each argument
		// keeps `omavm exec m -- echo "a b"` meaning the same in the guest.
		quoted := make([]string, len(command))
		for i, arg := range command {
			quoted[i] = shellQuote(arg)
		}
		args = append(args, "--", strings.Join(quoted, " "))
	}
	return args
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@%+,", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SSH opens a shell in the Machine, or runs command there, over the vsock
// channel. login defaults to the host user's name.
func (b *Backend) SSH(ctx context.Context, env core.Environment, login string, command []string) error {
	return b.ssh(ctx, env, login, command, false)
}

// Exec runs a command in the Machine over SSH, non-interactively.
func (b *Backend) Exec(ctx context.Context, env core.Environment, args []string) error {
	return b.ssh(ctx, env, "", args, true)
}

func (b *Backend) ssh(ctx context.Context, env core.Environment, login string, command []string, batch bool) error {
	running, err := b.isRunning(b.key(env))
	if err != nil {
		return err
	}
	if !running {
		return core.Invalidf("%s is not running; start it first", env.Name)
	}
	cid, ok := b.runningCID(b.key(env))
	if !ok {
		if env.Settings.SSHDisabled {
			return core.Unsupportedf("SSH is turned off for %s (omavm settings %s --ssh=true, then restart it)", env.Name, env.Name)
		}
		return core.Unsupportedf("%s was started without the SSH channel; restart it to connect", env.Name)
	}
	proxy := sshProxyPath()
	if proxy == "" {
		return core.Unsupportedf("SSH into Machines needs systemd 256 or newer on this computer (systemd-ssh-proxy not found)")
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return core.Unsupportedf("ssh not found (install it with: sudo pacman -S openssh)")
	}
	if login == "" {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("resolve user name: %w", err)
		}
		login = u.Username
	}
	cmd := exec.CommandContext(ctx, sshPath, sshArgs(proxy, env, cid, login, batch, command)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
		// ssh's own message is already on stderr; add what the guest needs.
		return fmt.Errorf("could not connect to %s over SSH: the guest needs systemd 256 or newer with sshd installed, and must have finished booting", env.Name)
	}
	if err != nil {
		return fmt.Errorf("ssh: %w", err)
	}
	return nil
}

func sshCapability() core.HostCapability {
	c := core.HostCapability{ID: "ssh", Label: "SSH into Machines"}
	switch {
	case !canOpenRW(vhostVsockPath):
		c.Detail = vhostVsockPath + " is missing or not accessible to this user"
		c.Hint = "Load the vhost_vsock kernel module"
	case sshProxyPath() == "":
		c.Detail = "systemd-ssh-proxy not found"
		c.Hint = "Needs systemd 256 or newer on this computer"
	default:
		c.Available = true
		c.Detail = "over a local channel that any program on this computer can reach, Boxes included; the guest's login protects it"
	}
	return c
}
