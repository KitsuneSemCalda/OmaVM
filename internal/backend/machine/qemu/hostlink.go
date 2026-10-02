package qemu

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/KitsuneForgering/OmaVM/internal/core"
	"github.com/KitsuneForgering/OmaVM/internal/desktop"
)

// hostLinkDir is ~/OmaVM, the directory OmaVM exposes Machine disks
// under so an Environment has a real, browsable presence on the host
// filesystem — the same role a .pvm bundle plays for Parallels, and a
// prerequisite for a color tag to have anything to attach to.
func hostLinkDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "OmaVM"), nil
}

func (b *Backend) hostLinkPath(name string) (string, error) {
	dir, err := hostLinkDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// ownLink reports whether something exists at link and, if so, whether it
// is the link OmaVM made: a symlink to this Machine's disk, where it is
// now or where it was before Machine directories were named by ID.
// ~/OmaVM is in the user's home, so anything else there (a note, a copied
// disk, their own symlink) is theirs and must never be removed or replaced.
func (b *Backend) ownLink(link string, env core.Environment) (exists, ours bool) {
	info, err := os.Lstat(link)
	if err != nil {
		return false, false
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true, false
	}
	target, err := os.Readlink(link)
	return true, err == nil && (target == b.diskPath(b.key(env)) || target == b.diskPath(env.Name))
}

// Link ensures ~/OmaVM/<name> exists as a symlink to the Machine's disk
// image, then applies color as a best-effort user.xdg.tags xattr on it.
// Best-effort because whether any given file manager actually reads that
// xattr (Dolphin/Baloo do; many others, including what Omarchy ships by
// default, may not) is outside OmaVM's control — never promise a
// guarantee that isn't real (Security Model). A missing setfattr binary
// or a failed xattr write does not fail Link: the visible link itself is
// the real, guaranteed part.
func (b *Backend) Link(ctx context.Context, env core.Environment, color string) (string, error) {
	dir, err := hostLinkDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	link, err := b.hostLinkPath(env.Name)
	if err != nil {
		return "", err
	}
	if exists, ours := b.ownLink(link, env); exists && !ours {
		return "", core.Invalidf("%s already exists and was not created by OmaVM; move or rename it to link this Machine there", link)
	} else if exists {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove stale link: %w", err)
		}
	}
	if err := os.Symlink(b.diskPath(b.key(env)), link); err != nil {
		return "", fmt.Errorf("link %s: %w", link, err)
	}
	if color != "" {
		if path, lookErr := exec.LookPath("setfattr"); lookErr == nil {
			_ = exec.CommandContext(ctx, path, "-n", "user.xdg.tags", "-v", color, link).Run()
		}
	}
	setFileManagerIcon(ctx, link, color)
	return link, nil
}

func (b *Backend) Unlink(ctx context.Context, env core.Environment) error {
	link, err := b.hostLinkPath(env.Name)
	if err != nil {
		return err
	}
	if _, ours := b.ownLink(link, env); !ours {
		return nil
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", link, err)
	}
	return nil
}

// setFileManagerIcon shows the color in Nautilus, Omarchy's file manager,
// which ignores user.xdg.tags: it reads custom icons from GVFS metadata.
// The metadata is set on the link itself (--nofollow-symlinks), not on the
// disk it points to, and goes away with the link. Best-effort like the
// xattr: without gio, the metadata daemon or an installed color icon, the
// color still holds inside OmaVM.
func setFileManagerIcon(ctx context.Context, link, color string) {
	gio, err := exec.LookPath("gio")
	if err != nil {
		return
	}
	icon := desktop.ColorIcon(color)
	if icon == "" {
		_ = exec.CommandContext(ctx, gio, "set", "--nofollow-symlinks", "--delete", link, "metadata::custom-icon").Run()
		return
	}
	uri := (&url.URL{Scheme: "file", Path: icon}).String()
	_ = exec.CommandContext(ctx, gio, "set", "--nofollow-symlinks", "--type=string", link, "metadata::custom-icon", uri).Run()
}
