// Package applog opens the structured logger shared by the omavm CLI
// and other OmaVM processes.
//
// /var/log is root-owned (0755 root:root) on a stock Linux install, and
// CLAUDE.md's Security Model forbids silently elevating privileges to
// work around that. So Open prefers /var/log/omavm when it already
// exists and is writable — the standard location an admin or installer
// can provision once (see README's "Logs" section for the one-time
// `install -d` step) — and falls back to the user's own state directory
// otherwise. Either way the location is logged on the first line of each
// new log file so it's never a mystery which one is in effect.
package applog

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// SystemLogDir is the standard location Open prefers when writable.
const SystemLogDir = "/var/log/omavm"

// maxLogSize caps a component's log: past it, the log moves to <name>.1
// (replacing the previous one) and starts over, so at most about twice
// this is kept. The GUI and the Omarchy bar run omavm every few seconds.
// A variable so tests can use a small limit.
var maxLogSize int64 = 5 << 20

// Open returns a JSON structured logger for component (e.g. "omavm",
// "omavm-gui") plus a close function to flush/release the underlying
// file.
//
// It deliberately does NOT call slog.SetDefault; each process decides
// whether this component logger should also become its global logger.
func Open(component string) (*slog.Logger, func() error, error) {
	dir, err := logDir()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}

	path := filepath.Join(dir, component+".log")
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		// Best-effort: a concurrent process may rotate first, and losing
		// that race only costs an older log.
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(f, nil))
	// Once per file, not per invocation: enough to say where logs live.
	if info, err := f.Stat(); err == nil && info.Size() == 0 {
		logger.Info("log opened", "component", component, "path", path)
	}
	return logger, f.Close, nil
}

func logDir() (string, error) {
	if writable(SystemLogDir) {
		return SystemLogDir, nil
	}
	base, err := core.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "logs"), nil
}

func writable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := filepath.Join(dir, ".omavm-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}
