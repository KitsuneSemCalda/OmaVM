package qemu

import (
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestGuestCapabilities(t *testing.T) {
	yes, no := true, false
	mounted, unmounted := "/mnt/omavm-share", ""
	shared := core.EnvironmentSettings{SharedPath: "/home/me/work"}
	tests := []struct {
		name                    string
		settings                core.EnvironmentSettings
		checks                  *guestChecks
		clipboard, sharedFolder string
	}{
		{"stopped is never ready", shared, nil, core.GuestNotVerified, core.GuestNotVerified},
		{"turned off", core.EnvironmentSettings{ClipboardDisabled: true}, &guestChecks{agent: true, clipboardOpen: &yes}, core.GuestOff, core.GuestOff},
		{"vdagent running, folder mounted", shared, &guestChecks{agent: true, clipboardOpen: &yes, virtiofsMount: &mounted, sessionHasShared: true}, core.GuestReady, core.GuestReady},
		{"no vdagent, folder not mounted", shared, &guestChecks{agent: true, clipboardOpen: &no, virtiofsMount: &unmounted, sessionHasShared: true}, core.GuestNeedsComponent, core.GuestNeedsComponent},
		{"folder shared after start", shared, &guestChecks{agent: true, clipboardOpen: &yes, sessionHasShared: false}, core.GuestReady, core.GuestNeedsRestart},
		{"no guest agent to check the mount", shared, &guestChecks{clipboardOpen: &yes, sessionHasShared: true}, core.GuestReady, core.GuestNotVerified},
		{"channel unreadable", shared, &guestChecks{agent: true, sessionHasShared: true, virtiofsMount: &mounted}, core.GuestNotVerified, core.GuestReady},
	}
	for _, tt := range tests {
		caps := guestCapabilities(core.Environment{Name: "vm", Kind: core.Machine, Settings: tt.settings}, tt.checks)
		if caps[0].State != tt.clipboard || caps[1].State != tt.sharedFolder {
			t.Errorf("%s: clipboard=%s shared=%s, want %s %s", tt.name, caps[0].State, caps[1].State, tt.clipboard, tt.sharedFolder)
		}
		for _, c := range caps {
			if c.Hint == "" {
				t.Errorf("%s: %s has no next step", tt.name, c.ID)
			}
		}
	}
}
