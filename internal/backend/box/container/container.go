// Package container is OmaVM's legacy Box engine: a minimal adapter built
// directly on Podman (preferred) or Docker. Per CLAUDE.md's Backend Rules, it stays
// deliberately small — create/start/stop/exec/remove plus a home
// directory mount and host networking — and never reimplements the
// container engine itself (image storage, runc/OCI): that stays Podman
// or Docker's job.
package container

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// containerPrefix namespaces containers this engine manages so it never
// collides with or accidentally touches unrelated containers on the
// host.
const containerPrefix = "omavm-box-"

// Backend implements core.Backend for Box environments via a
// container engine CLI (podman or docker — their CLIs are
// compatible for the subset of commands used here).
type Backend struct {
	// detect guards the lazy runtime probe: `podman info` costs a few
	// hundred milliseconds, and every omavm invocation constructs this
	// backend even when it only touches Machines (the GUI polls
	// list/status every few seconds).
	detect  sync.Once
	runtime string
}

// New returns a backend whose engine is chosen on first use, not here.
func New() *Backend { return &Backend{} }

// detectRuntime prefers a working Podman, then falls back to a working
// Docker. Finding a binary is insufficient: a stale rootless Podman setup
// or an unreachable daemon must not prevent an otherwise healthy Docker
// fallback.
func detectRuntime() string {
	for _, runtime := range []string{"podman", "docker"} {
		if runtimeReady(runtime) {
			return runtime
		}
	}
	// Preserve a useful operation error when neither engine is healthy.
	// Prefer an installed binary so its own diagnostic reaches the user.
	for _, runtime := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(runtime); err == nil {
			return runtime
		}
	}
	return "podman"
}

func (b *Backend) defaultRuntime() string {
	b.detect.Do(func() {
		if b.runtime == "" {
			b.runtime = detectRuntime()
		}
	})
	return b.runtime
}

func runtimeReady(runtime string) bool {
	if _, err := exec.LookPath(runtime); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, runtime, "info").Run() == nil
}

func (b *Backend) Name() string { return b.defaultRuntime() }

func (b *Backend) runtimeFor(env core.Environment) string {
	if env.Backend == "podman" || env.Backend == "docker" {
		return env.Backend
	}
	return b.defaultRuntime()
}

func containerName(env core.Environment) string {
	return containerPrefix + env.Name
}

func (b *Backend) Create(ctx context.Context, env core.Environment) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	args := []string{
		"create",
		"--name", containerName(env),
		"--hostname", env.Name,
		"--volume", home + ":" + home,
		"--network", "host",
		"--entrypoint", "sleep",
		env.Image,
		"infinity",
	}
	return b.run(ctx, b.runtimeFor(env), args...)
}

func (b *Backend) Start(ctx context.Context, env core.Environment) error {
	return b.run(ctx, b.runtimeFor(env), "start", containerName(env))
}

// Open attaches an interactive shell, starting the container first if
// needed.
func (b *Backend) Open(ctx context.Context, env core.Environment) error {
	if err := b.Start(ctx, env); err != nil {
		return err
	}
	return b.runInteractive(ctx, b.runtimeFor(env), "exec", "-it", containerName(env), shellFor(env))
}

func (b *Backend) Stop(ctx context.Context, env core.Environment) error {
	return b.run(ctx, b.runtimeFor(env), "stop", containerName(env))
}

func (b *Backend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	runtime := b.runtimeFor(env)
	cmd := exec.CommandContext(ctx, runtime, "inspect", "--format", "{{.State.Status}}", containerName(env))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return core.Status{}, fmt.Errorf("%w: %s has no container named %s: %s", core.ErrNotFound, runtime, env.Name, strings.TrimSpace(string(out)))
	}
	rawStatus := strings.TrimSpace(string(out))
	state := core.StateStopped
	if rawStatus == "running" {
		state = core.StateRunning
	}
	return core.Status{State: state, Detail: rawStatus}, nil
}

func (b *Backend) Exec(ctx context.Context, env core.Environment, args []string) error {
	full := append([]string{"exec", containerName(env)}, args...)
	return b.runInteractive(ctx, b.runtimeFor(env), full...)
}

func (b *Backend) Remove(ctx context.Context, env core.Environment) error {
	return b.run(ctx, b.runtimeFor(env), "rm", "--force", containerName(env))
}

func shellFor(env core.Environment) string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func (b *Backend) run(ctx context.Context, runtime string, args ...string) error {
	cmd := exec.CommandContext(ctx, runtime, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", runtime, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (b *Backend) runInteractive(ctx context.Context, runtime string, args ...string) error {
	cmd := exec.CommandContext(ctx, runtime, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", runtime, strings.Join(args, " "), err)
	}
	return nil
}
