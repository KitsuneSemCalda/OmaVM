package qemu

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestQGAPing(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		var request map[string]string
		if json.NewDecoder(server).Decode(&request) == nil && request["execute"] == "guest-ping" {
			_ = json.NewEncoder(server).Encode(map[string]any{"return": map[string]any{}})
		}
	}()
	if err := qgaPingConn(client); err != nil {
		t.Fatal(err)
	}
}

// fakeGuestAgent answers guest-sync with its id, and records every other
// command into events, failing the ones in refuse.
func fakeGuestAgent(t *testing.T, socket string, events chan<- string, refuse map[string]bool) {
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
			go func(conn net.Conn) {
				defer conn.Close()
				dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
				for {
					var req struct {
						Execute   string         `json:"execute"`
						Arguments map[string]any `json:"arguments"`
					}
					if dec.Decode(&req) != nil {
						return
					}
					switch {
					case req.Execute == "guest-sync":
						_ = enc.Encode(map[string]any{"return": req.Arguments["id"]})
					case refuse[req.Execute]:
						events <- req.Execute
						_ = enc.Encode(map[string]any{"error": map[string]any{"desc": "no"}})
					default:
						events <- req.Execute
						_ = enc.Encode(map[string]any{"return": 1})
					}
				}
			}(conn)
		}
	}()
}

// With the guest agent, a running Machine's snapshot is taken with its
// filesystems frozen, and they are always thawed afterwards.
func TestSnapshotOfRunningMachineFreezesTheGuest(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	events := make(chan string, 10)
	fakeGuestAgent(t, b.qgaPath(env.Name), events, nil)
	calls := recordQMP(t, b.qmpPath(env.Name))

	crash, err := b.CreateSnapshot(context.Background(), env, "before-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if crash {
		t.Fatal("a snapshot taken with the guest frozen is not crash-consistent")
	}
	if got := []string{<-events, <-events}; got[0] != "guest-fsfreeze-freeze" || got[1] != "guest-fsfreeze-thaw" {
		t.Fatalf("guest agent calls = %v", got)
	}
	if got := calls(); len(got) != 1 || got[0].Execute != "blockdev-snapshot-internal-sync" {
		t.Fatalf("QMP calls = %+v", got)
	}
}

// An agent that refuses to freeze (no fsfreeze support, a Windows guest
// without VSS) must not block the snapshot, and anything it froze is
// thawed.
func TestSnapshotGoesOnWhenTheGuestCantFreeze(t *testing.T) {
	b := &Backend{stateDir: t.TempDir()}
	env := core.Environment{Name: "guest", Kind: core.Machine}
	startFakeQEMU(t, b, env.Name)
	events := make(chan string, 10)
	fakeGuestAgent(t, b.qgaPath(env.Name), events, map[string]bool{"guest-fsfreeze-freeze": true})
	recordQMP(t, b.qmpPath(env.Name))

	crash, err := b.CreateSnapshot(context.Background(), env, "x")
	if err != nil || !crash {
		t.Fatalf("CreateSnapshot = %t, %v; want a crash-consistent snapshot", crash, err)
	}
	if got := []string{<-events, <-events}; got[1] != "guest-fsfreeze-thaw" {
		t.Fatalf("guest agent calls = %v; want a thaw after the failed freeze", got)
	}
}

// Regression: guest-sync is there to skip replies an earlier client left
// unread (a GUI ping that timed out), but a stale reply that wasn't a
// number failed the sync instead, and the snapshot silently lost its
// filesystem freeze.
func TestGuestSyncSkipsStaleReplies(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		enc, dec := json.NewEncoder(server), json.NewDecoder(server)
		var req struct {
			Arguments map[string]any `json:"arguments"`
		}
		if dec.Decode(&req) != nil {
			return
		}
		_ = enc.Encode(map[string]any{"return": map[string]any{}}) // a stale guest-ping reply
		_ = enc.Encode(map[string]any{"return": 7})                // a stale reply to another sync
		_ = enc.Encode(map[string]any{"return": req.Arguments["id"]})
	}()
	if _, err := qgaSync(client); err != nil {
		t.Fatalf("sync failed on a stale reply: %v", err)
	}
}
