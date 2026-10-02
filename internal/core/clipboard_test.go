package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

func TestClipboardDirection(t *testing.T) {
	svc, _, _ := newTestService()
	ctx := context.Background()
	if _, err := svc.Create(ctx, core.Environment{Name: "vm", Kind: core.Machine, Image: testISO(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, core.Environment{Name: "dev", Kind: core.Box, Image: "fedora"}); err != nil {
		t.Fatal(err)
	}
	dir := func(s string) core.SettingsPatch { return core.SettingsPatch{ClipboardDirection: &s} }

	settings, err := svc.Configure(ctx, "vm", dir(core.ClipboardToGuest))
	if err != nil {
		t.Fatal(err)
	}
	if settings.ClipboardToHost() || !settings.ClipboardToGuest() || settings.ClipboardMode() != "to-guest" {
		t.Fatalf("to-guest: %+v", settings)
	}
	if settings, _ = svc.Configure(ctx, "vm", dir(core.ClipboardBoth)); settings.ClipboardDirection != "" || settings.ClipboardMode() != "true" {
		t.Fatalf("both must be the stored default: %+v", settings)
	}
	off := false
	if settings, _ = svc.Configure(ctx, "vm", core.SettingsPatch{ShareClipboard: &off}); settings.ClipboardToHost() || settings.ClipboardToGuest() || settings.ClipboardMode() != "false" {
		t.Fatalf("off must turn both ways off: %+v", settings)
	}

	if _, err := svc.Configure(ctx, "dev", dir(core.ClipboardToGuest)); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("a Box has no way to the guest, got %v", err)
	}
	if _, err := svc.Configure(ctx, "dev", dir(core.ClipboardToHost)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Configure(ctx, "vm", dir("sideways")); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("expected invalid input, got %v", err)
	}
}
