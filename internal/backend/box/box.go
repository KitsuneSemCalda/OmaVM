// Package box routes new Development Boxes to Distrobox while preserving
// lifecycle access to environments created by the former direct engine.
package box

import (
	"context"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

type Backend struct {
	primary core.Backend
	legacy  core.Backend
}

func New(primary, legacy core.Backend) *Backend { return &Backend{primary: primary, legacy: legacy} }
func (b *Backend) Name() string                 { return b.primary.Name() }

func (b *Backend) forEnv(env core.Environment) core.Backend {
	if env.Backend == "podman" || env.Backend == "docker" {
		return b.legacy
	}
	return b.primary
}

func (b *Backend) Create(ctx context.Context, env core.Environment) error {
	return b.primary.Create(ctx, env)
}
func (b *Backend) Start(ctx context.Context, env core.Environment) error {
	return b.forEnv(env).Start(ctx, env)
}
func (b *Backend) Open(ctx context.Context, env core.Environment) error {
	return b.forEnv(env).Open(ctx, env)
}
func (b *Backend) Stop(ctx context.Context, env core.Environment) error {
	return b.forEnv(env).Stop(ctx, env)
}
func (b *Backend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	return b.forEnv(env).Status(ctx, env)
}
func (b *Backend) Exec(ctx context.Context, env core.Environment, args []string) error {
	return b.forEnv(env).Exec(ctx, env, args)
}
func (b *Backend) Remove(ctx context.Context, env core.Environment) error {
	return b.forEnv(env).Remove(ctx, env)
}

// Statuses asks the Distrobox engine once for every Box it owns; Boxes of
// the legacy direct engine are asked one by one.
func (b *Backend) Statuses(ctx context.Context, envs []core.Environment) (map[string]core.Status, error) {
	statuses := make(map[string]core.Status, len(envs))
	var primary []core.Environment
	for _, env := range envs {
		if b.forEnv(env) == b.primary {
			primary = append(primary, env)
			continue
		}
		status, err := b.legacy.Status(ctx, env)
		if err != nil {
			status = core.Status{State: core.StateUnknown, Detail: err.Error()}
		}
		statuses[env.ID] = status
	}
	if len(primary) == 0 {
		return statuses, nil
	}
	lister, ok := b.primary.(core.StatusLister)
	if !ok {
		for _, env := range primary {
			status, err := b.primary.Status(ctx, env)
			if err != nil {
				status = core.Status{State: core.StateUnknown, Detail: err.Error()}
			}
			statuses[env.ID] = status
		}
		return statuses, nil
	}
	batch, err := lister.Statuses(ctx, primary)
	for _, env := range primary {
		status, found := batch[env.ID]
		switch {
		case err != nil:
			status = core.Status{State: core.StateUnknown, Detail: err.Error()}
		case !found:
			status = core.Status{State: core.StateUnknown}
		}
		statuses[env.ID] = status
	}
	return statuses, nil
}

// appExporter resolves env's backend as an AppExporter, or ErrUnsupported
// when it's routed to the legacy engine — distrobox-export (the Blend
// Mode base) is a Distrobox-native feature the legacy container adapter
// never had.
func (b *Backend) appExporter(env core.Environment) (core.AppExporter, error) {
	exporter, ok := b.forEnv(env).(core.AppExporter)
	if !ok {
		return nil, core.Unsupportedf("legacy container Boxes don't support application export")
	}
	return exporter, nil
}

func (b *Backend) ListApps(ctx context.Context, env core.Environment) ([]core.App, error) {
	exporter, err := b.appExporter(env)
	if err != nil {
		return nil, err
	}
	return exporter.ListApps(ctx, env)
}

func (b *Backend) ExportApp(ctx context.Context, env core.Environment, id string) error {
	exporter, err := b.appExporter(env)
	if err != nil {
		return err
	}
	return exporter.ExportApp(ctx, env, id)
}

func (b *Backend) UnexportApp(ctx context.Context, env core.Environment, id string) error {
	exporter, err := b.appExporter(env)
	if err != nil {
		return err
	}
	return exporter.UnexportApp(ctx, env, id)
}

// Clone copies a Box through Distrobox's own --clone. Boxes on the legacy
// direct-container engine can't: it never had a clone.
func (b *Backend) Clone(ctx context.Context, source, clone core.Environment) error {
	cloner, ok := b.forEnv(source).(core.Cloner)
	if !ok {
		return core.Unsupportedf("legacy container Boxes can't be cloned")
	}
	return cloner.Clone(ctx, source, clone)
}

// Upgrade updates a Box's packages through distrobox upgrade.
func (b *Backend) Upgrade(ctx context.Context, env core.Environment) error {
	upgrader, ok := b.forEnv(env).(core.Upgrader)
	if !ok {
		return core.Unsupportedf("legacy container Boxes can't be updated from OmaVM; update them from their terminal")
	}
	return upgrader.Upgrade(ctx, env)
}
