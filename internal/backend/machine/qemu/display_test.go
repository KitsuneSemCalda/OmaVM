package qemu

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// fakeQMP accepts one session, answers the handshake and records each
// command with any fd passed alongside it. addClientErr, when set, is
// returned as add_client's QMP error description.
func fakeQMP(t *testing.T, path, addClientErr string) <-chan map[string]any {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	commands := make(chan map[string]any, 8)
	go func() {
		defer close(commands)
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		enc := json.NewEncoder(conn)
		enc.Encode(map[string]any{"QMP": map[string]any{}})
		for {
			buf, oob := make([]byte, 4096), make([]byte, 64)
			n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
			if err != nil {
				return
			}
			scanner := bufio.NewScanner(strings.NewReader(string(buf[:n])))
			for scanner.Scan() {
				var req map[string]any
				if json.Unmarshal(scanner.Bytes(), &req) != nil {
					continue
				}
				if oobn > 0 {
					msgs, _ := syscall.ParseSocketControlMessage(oob[:oobn])
					if len(msgs) == 1 {
						if fds, err := syscall.ParseUnixRights(&msgs[0]); err == nil && len(fds) == 1 {
							req["fd"] = fds[0]
						}
					}
				}
				commands <- req
				if req["execute"] == "add_client" && addClientErr != "" {
					enc.Encode(map[string]any{"error": map[string]any{"class": "GenericError", "desc": addClientErr}})
					continue
				}
				enc.Encode(map[string]any{"return": map[string]any{}})
			}
		}
	}()
	return commands
}

func TestAttachDisplayPassesConnectionToQEMU(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	if err := os.MkdirAll(b.dir("vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	commands := fakeQMP(t, b.qmpPath("vm"), "")

	viewerEnd, err := b.attachDisplay("vm")
	if err != nil {
		t.Fatal(err)
	}
	defer viewerEnd.Close()

	var getfd, addClient map[string]any
	for req := range commands {
		switch req["execute"] {
		case "getfd":
			getfd = req
		case "add_client":
			addClient = req
		}
		if addClient != nil {
			break
		}
	}
	if getfd == nil || getfd["fd"] == nil {
		t.Fatalf("getfd was not sent with an fd: %v", getfd)
	}
	fdname := getfd["arguments"].(map[string]any)["fdname"]
	args := addClient["arguments"].(map[string]any)
	if args["protocol"] != "@dbus-display" || args["fdname"] != fdname {
		t.Fatalf("add_client = %v, want protocol @dbus-display with fdname %v", args, fdname)
	}

	// The fd QEMU received must be the other end of the viewer's socket.
	qemuEnd := os.NewFile(uintptr(getfd["fd"].(int)), "qemu")
	defer qemuEnd.Close()
	if _, err := qemuEnd.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := viewerEnd.Read(got); err != nil || string(got) != "ping" {
		t.Fatalf("viewer end read %q, %v; want the pair of QEMU's fd", got, err)
	}
}

func TestAttachDisplayExplainsMachinesStartedWithoutDBusDisplay(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	if err := os.MkdirAll(b.dir("vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeQMP(t, b.qmpPath("vm"), "D-Bus display is not in use") // QEMU 11.1's reply for a VNC-only Machine

	_, err := b.attachDisplay("vm")
	if !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), "restart it") {
		t.Fatalf("attachDisplay on an old Machine = %v, want a restart hint", err)
	}
}
