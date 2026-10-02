package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// scriptedQMP answers each connection's greeting and handshake, then hands
// every command to reply, which writes whatever QEMU would (or nothing).
func scriptedQMP(t *testing.T, reply func(command string, enc *json.Encoder, conn net.Conn)) string {
	t.Helper()
	return scriptedQMPAt(t, filepath.Join(t.TempDir(), "qmp.sock"), reply)
}

func scriptedQMPAt(t *testing.T, socket string, reply func(command string, enc *json.Encoder, conn net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
				enc.Encode(map[string]any{"QMP": map[string]any{}})
				for {
					var req struct {
						Execute string `json:"execute"`
					}
					if dec.Decode(&req) != nil {
						return
					}
					if req.Execute == "qmp_capabilities" {
						enc.Encode(map[string]any{"return": map[string]any{}})
						continue
					}
					reply(req.Execute, enc, conn)
				}
			}()
		}
	}()
	return socket
}

func TestQMPErrorReplyIsStructured(t *testing.T) {
	socket := scriptedQMP(t, func(_ string, enc *json.Encoder, _ net.Conn) {
		enc.Encode(map[string]any{"error": map[string]any{"class": "DeviceNotActive", "desc": "D-Bus display is not in use"}})
	})
	_, err := qmpExecute(socket, "add_client", nil)
	var qerr *qmpError
	if !errors.As(err, &qerr) || qerr.Class != "DeviceNotActive" || !strings.Contains(err.Error(), "D-Bus display is not in use") {
		t.Fatalf("got %v", err)
	}
	if errors.Is(err, errQMPNoReply) {
		t.Fatal("a refusal is a known result, not a missing reply")
	}
}

func TestQMPEventBeforeTheReplyIsSkipped(t *testing.T) {
	socket := scriptedQMP(t, func(_ string, enc *json.Encoder, _ net.Conn) {
		enc.Encode(map[string]any{"event": "RESUME", "timestamp": map[string]any{}})
		enc.Encode(map[string]any{"return": map[string]any{"status": "running"}})
	})
	if status, err := qmpStatus(socket); err != nil || status != "running" {
		t.Fatalf("status %q, %v", status, err)
	}
}

func TestQMPMissingReplyIsUnknownNotFailure(t *testing.T) {
	dropped := scriptedQMP(t, func(_ string, _ *json.Encoder, conn net.Conn) { conn.Close() })
	if _, err := qmpExecute(dropped, "stop", nil); !errors.Is(err, errQMPNoReply) {
		t.Fatalf("dropped connection: %v", err)
	}
	silent := scriptedQMP(t, func(string, *json.Encoder, net.Conn) {})
	start := time.Now()
	if _, err := qmpExecuteTimeout(silent, "stop", nil, 200*time.Millisecond); !errors.Is(err, errQMPNoReply) {
		t.Fatalf("no reply: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the wait wasn't bounded")
	}
}

// A snapshot whose reply never came is looked up before being reported:
// one that exists on the disk must reach the registry, not be lost.
func TestSnapshotWithoutReplyIsReconciled(t *testing.T) {
	snapshots := map[string]bool{}
	reply := func(command string, enc *json.Encoder, conn net.Conn) {
		switch command {
		case "blockdev-snapshot-internal-sync":
			snapshots["before-upgrade"] = true
			conn.Close() // done, but the reply is lost
		case "blockdev-snapshot-delete-internal-sync":
			delete(snapshots, "before-upgrade")
			conn.Close()
		case "query-block":
			var list []map[string]any
			for name := range snapshots {
				list = append(list, map[string]any{"name": name})
			}
			enc.Encode(map[string]any{"return": []any{map[string]any{"device": bootDisk, "inserted": map[string]any{"image": map[string]any{"snapshots": list}}}}})
		}
	}
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	scriptedQMPAt(t, b.qmpPath(env.Name), reply)
	if _, err := b.CreateSnapshot(context.Background(), env, "before-upgrade"); err != nil {
		t.Fatalf("the snapshot exists, so creating it succeeded: %v", err)
	}
	if err := b.RemoveSnapshot(context.Background(), env, "before-upgrade"); err != nil {
		t.Fatalf("the snapshot is gone, so deleting it succeeded: %v", err)
	}
}

// Without a reply and without the snapshot on the disk, it fails, and the
// error says the outcome wasn't confirmed.
func TestSnapshotWithoutReplyThatDidNotHappenFails(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	scriptedQMPAt(t, b.qmpPath(env.Name), func(command string, enc *json.Encoder, conn net.Conn) {
		if command == "query-block" {
			enc.Encode(map[string]any{"return": []any{map[string]any{"device": bootDisk, "inserted": map[string]any{"image": map[string]any{}}}}})
			return
		}
		conn.Close()
	})
	_, err := b.CreateSnapshot(context.Background(), env, "before-upgrade")
	if !errors.Is(err, errQMPNoReply) || !strings.Contains(err.Error(), "may or may not have run") {
		t.Fatalf("expected an unconfirmed failure, got %v", err)
	}
}

// Resume after a disk-full pause: if QEMU pauses again at once (the host's
// disk is still full), Resume says so instead of reporting success.
func TestResumeThatPausesAgainOnDiskErrorFails(t *testing.T) {
	for _, tt := range []struct {
		status string
		fails  bool
	}{{"io-error", true}, {"running", false}} {
		b := &Backend{stateDir: t.TempDir()}
		env := core.Environment{Name: "guest", Kind: core.Machine}
		startFakeQEMU(t, b, env.Name)
		scriptedQMPAt(t, b.qmpPath(env.Name), func(command string, enc *json.Encoder, _ net.Conn) {
			if command == "query-status" {
				enc.Encode(map[string]any{"return": map[string]any{"status": tt.status}})
				return
			}
			enc.Encode(map[string]any{"return": map[string]any{}})
		})
		err := b.Resume(context.Background(), env)
		if tt.fails != (err != nil) {
			t.Fatalf("status %s after cont: err = %v", tt.status, err)
		}
		if tt.fails && !strings.Contains(err.Error(), "paused again") {
			t.Fatalf("unexplained: %v", err)
		}
	}
}

// A paused Machine is resumed so it can shut down; if the guest doesn't,
// it goes back to paused, as it was found, and is never quit.
func TestSlowShutdownOfPausedMachinePausesItAgain(t *testing.T) {
	saved := gracefulShutdownTimeout
	gracefulShutdownTimeout = 300 * time.Millisecond
	t.Cleanup(func() { gracefulShutdownTimeout = saved })
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	commands := make(chan string, 20)
	scriptedQMPAt(t, b.qmpPath(env.Name), func(command string, enc *json.Encoder, _ net.Conn) {
		commands <- command
		if command == "query-status" {
			enc.Encode(map[string]any{"return": map[string]any{"status": "paused"}})
			return
		}
		enc.Encode(map[string]any{"return": map[string]any{}})
	})
	err := b.Stop(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "paused again") {
		t.Fatalf("expected the Machine back in pause, got %v", err)
	}
	close(commands)
	var sent []string
	for c := range commands {
		if c == "quit" {
			t.Fatal("a slow shutdown must never become a power cut")
		}
		sent = append(sent, c)
	}
	if sent[len(sent)-1] != "stop" {
		t.Fatalf("last command %v, want stop", sent)
	}
}

// With the agent's port closed in the guest, Integration answers at once
// instead of waiting out a ping nobody will answer (it held every poll of
// the environment list for 700 ms).
func TestIntegrationSkipsThePingWhenTheAgentPortIsClosed(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	scriptedQMPAt(t, b.qmpPath(env.Name), func(command string, enc *json.Encoder, _ net.Conn) {
		enc.Encode(map[string]any{"return": []any{
			map[string]any{"label": "qga0", "frontend-open": false},
			map[string]any{"label": "clipboard", "frontend-open": false},
		}})
	})
	// The agent's socket accepts and never answers, as QEMU's does when
	// no agent runs in the guest.
	silent, err := net.Listen("unix", b.qgaPath(env.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	start := time.Now()
	report, err := b.Integration(context.Background(), env)
	if err != nil || report.GuestAgent != "unavailable" {
		t.Fatalf("report %+v, %v", report, err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("waited %v for an agent whose port is closed", elapsed)
	}
}
