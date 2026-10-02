package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// Error messages reach people through the CLI and the GUI's toasts: the
// sentinel is for callers to branch on with errors.Is, not text to show.
// "operation not supported by this backend" also names infrastructure the
// UI must not expose (CLAUDE.md, UX Principles #3/#4).
func TestErrorMessagesReadWithoutSentinelJargon(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	if _, err := svc.Create(ctx, core.Environment{Name: "radic", Image: "fedora", Kind: core.Box}); err != nil {
		t.Fatal(err)
	}
	yes := true

	tests := map[string]struct {
		err  error
		kind error
	}{
		"invalid input": {
			err: func() error {
				_, err := svc.Create(ctx, core.Environment{Name: "desktop", Kind: core.Machine})
				return err
			}(),
			kind: core.ErrInvalidInput,
		},
		"unsupported": {
			err: func() error {
				_, err := svc.Configure(ctx, "radic", core.SettingsPatch{Vulkan: &yes})
				return err
			}(),
			kind: core.ErrUnsupported,
		},
	}
	for name, tt := range tests {
		if !errors.Is(tt.err, tt.kind) {
			t.Errorf("%s: %v does not match %v", name, tt.err, tt.kind)
		}
		msg := tt.err.Error()
		for _, jargon := range []string{"invalid input", "not supported by this backend", "backend"} {
			if strings.Contains(msg, jargon) {
				t.Errorf("%s: message %q contains %q", name, msg, jargon)
			}
		}
	}
}

// Every rejected value is ErrInvalidInput, so callers (CLI, GUI, agents)
// can tell "fix your input" from "something broke".
func TestValidationErrorsAreInvalidInput(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService()
	if _, err := svc.Create(ctx, core.Environment{Name: "desktop", Image: testISO(t), Kind: core.Machine}); err != nil {
		t.Fatal(err)
	}
	tooMany, tooLittle, badLimit := 128, 16, 0
	long := strings.Repeat("x", 501)
	notADir := testISO(t)
	color := "pink"
	checks := map[string]error{
		"create cpus": func() error {
			_, err := svc.Create(ctx, core.Environment{Name: "d2", Image: testISO(t), Kind: core.Machine, Settings: core.EnvironmentSettings{CPUs: 128}})
			return err
		}(),
		"create memory": func() error {
			_, err := svc.Create(ctx, core.Environment{Name: "d3", Image: testISO(t), Kind: core.Machine, Settings: core.EnvironmentSettings{MemoryMiB: 16}})
			return err
		}(),
		"cpus":           configure(svc, core.SettingsPatch{CPUs: &tooMany}),
		"memory":         configure(svc, core.SettingsPatch{MemoryMiB: &tooLittle}),
		"description":    configure(svc, core.SettingsPatch{Description: &long}),
		"shared folder":  configure(svc, core.SettingsPatch{SharedPath: &notADir}),
		"snapshot limit": configure(svc, core.SettingsPatch{SnapshotLimit: &badLimit}),
		"color":          configure(svc, core.SettingsPatch{Color: &color}),
		"snapshot label": func() error { _, err := svc.CreateSnapshot(ctx, "desktop", "   "); return err }(),
	}
	for name, err := range checks {
		if !errors.Is(err, core.ErrInvalidInput) {
			t.Errorf("%s: %v is not ErrInvalidInput", name, err)
		}
	}
}

func configure(svc *core.Service, patch core.SettingsPatch) error {
	_, err := svc.Configure(context.Background(), "desktop", patch)
	return err
}
