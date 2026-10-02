// Command omavm is the CLI front end for the OmaVM Core. It contains no
// environment-management logic of its own: every operation is a thin
// call into core.Service, the same Core a future GUI will call.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/KitsuneForgering/OmaVM/internal/applog"
	"github.com/KitsuneForgering/OmaVM/internal/backend/box"
	"github.com/KitsuneForgering/OmaVM/internal/backend/box/container"
	"github.com/KitsuneForgering/OmaVM/internal/backend/box/distrobox"
	"github.com/KitsuneForgering/OmaVM/internal/backend/machine/qemu"
	"github.com/KitsuneForgering/OmaVM/internal/core"
	"github.com/KitsuneForgering/OmaVM/internal/desktop"
)

func main() {
	// Safe to install as the global slog default here: the CLI owns no
	// GUI toolkit whose internal messages could pollute this log.
	if logger, closeLog, err := applog.Open("omavm"); err != nil {
		fmt.Fprintln(os.Stderr, "omavm: warning: logging disabled:", err)
	} else {
		slog.SetDefault(logger)
		defer closeLog()
	}

	args := os.Args[1:]
	if err := run(args); err != nil {
		slog.Error("command failed", "error", err)
		fmt.Fprintln(os.Stderr, "omavm:", err)
		cmd := ""
		if len(args) > 0 {
			cmd = args[0]
		}
		if wantsJSON(args) {
			writeJSONError(os.Stdout, err)
		}
		if os.Getenv("OMAVM_NOTIFY_ERRORS") == "1" {
			notifyError(err)
		}
		os.Exit(exitCodeFor(cmd, err))
	}
}

// notifyError shows a failure as a desktop notification, for runs nobody
// sees the terminal of: a launcher entry opening a Machine would otherwise
// fail without a word. Best-effort; without notify-send, only the log has it.
func notifyError(err error) {
	path, lookErr := exec.LookPath("notify-send")
	if lookErr != nil {
		return
	}
	_ = exec.Command(path, "--app-name=OmaVM", "--icon=dev.omavm.app", "OmaVM", err.Error()).Run()
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return usagef("no command given")
	}

	qemuBackend, err := qemu.New()
	if err != nil {
		return err
	}
	store, err := core.NewFileStore()
	if err != nil {
		return err
	}
	// Only for a person at a terminal: the GUI and agents read stderr as
	// the error text of a failed command, and this line would pollute it.
	if info, err := os.Stderr.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		store.OnWait = func() {
			fmt.Fprintln(os.Stderr, "omavm: waiting for another OmaVM operation to finish…")
		}
	}
	boxBackend := box.New(distrobox.New(), container.New())
	svc := core.NewService(store, boxBackend, qemuBackend)
	if launcher, err := desktop.NewLauncher(); err == nil {
		svc.SetLauncher(launcher)
	} else {
		slog.Warn("launcher entries disabled", "error", err)
	}

	ctx := context.Background()
	cmd, rest := args[0], args[1:]
	// Reads (the GUI and the Omarchy bar poll list/status every few
	// seconds) are debug-level so the log keeps the actions that changed
	// something.
	level := slog.LevelInfo
	if readOnly(cmd, rest) {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, "command", "cmd", cmd, "args", rest)

	switch cmd {
	case "create":
		return cmdCreate(ctx, svc, rest)
	case "start":
		rest, ephemeral := extractBoolFlag(rest, "ephemeral")
		if ephemeral {
			return cmdSimple(ctx, rest, "start", svc.StartEphemeral)
		}
		return cmdSimple(ctx, rest, "start", svc.Start)
	case "open":
		return cmdSimple(ctx, rest, "open", svc.Open)
	case "stop":
		return cmdSimple(ctx, rest, "stop", svc.Stop)
	case "restart":
		return cmdSimple(ctx, rest, "restart", svc.Restart)
	case "pause":
		return cmdSimple(ctx, rest, "pause", svc.Pause)
	case "resume":
		return cmdSimple(ctx, rest, "resume", svc.Resume)
	case "force-stop":
		return cmdSimple(ctx, rest, "force-stop", svc.ForceStop)
	case "status":
		return cmdStatus(ctx, svc, rest)
	case "host":
		return cmdHost(ctx, svc, rest)
	case "integration":
		return cmdIntegration(ctx, svc, rest)
	case "settings", "configure":
		return cmdSettings(ctx, svc, rest)
	case "preview":
		return cmdPreview(ctx, svc, rest)
	case "snapshot":
		return cmdSnapshot(ctx, svc, rest)
	case "apps":
		return cmdApps(ctx, svc, rest)
	case "exec":
		return cmdExec(ctx, svc, rest)
	case "ssh":
		return cmdSSH(ctx, svc, rest)
	case "clone":
		return cmdClone(ctx, svc, rest)
	case "run":
		return cmdRun(ctx, svc, rest)
	case "update", "upgrade":
		rest, progress := extractBoolFlag(rest, "progress")
		return cmdSimple(withProgress(ctx, progress), rest, "update", svc.Update)
	case "rm", "remove":
		return cmdSimple(ctx, rest, "remove", svc.Remove)
	case "list", "ls":
		return cmdList(ctx, svc, rest)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return usagef("unknown command %q", cmd)
	}
}

func cmdCreate(ctx context.Context, svc *core.Service, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	name := fs.String("name", "", "environment name (required)")
	kindStr := fs.String("kind", "box", `environment kind: "box" or "machine"`)
	image := fs.String("image", "", "distro image (Box) or boot ISO path (Machine)")
	cpus := fs.Int("cpus", 0, "virtual CPUs (Machines only, defaults to 2)")
	memory := fs.Int("memory-mib", 0, "memory in MiB (Machines only, defaults to 2048)")
	progress := fs.Bool("progress", false, `print each stage of a long creation on stdout as "progress: STAGE"`)
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	// Stages of a long creation (a Box's image download).
	ctx = withProgress(ctx, *progress)
	if *name == "" {
		return usagef("create: --name is required")
	}
	kind, err := core.ParseEnvironmentKind(*kindStr)
	if err != nil {
		return err
	}

	env, err := svc.Create(ctx, core.Environment{
		Name:  *name,
		Image: *image,
		Kind:  kind,
		Settings: core.EnvironmentSettings{
			CPUs:      *cpus,
			MemoryMiB: *memory,
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("created %s (%s, backend=%s)\n", env.Name, env.Kind, env.Backend)
	return nil
}

// cmdRun: omavm run --ephemeral --image ISO [--name NAME] [--cpus N]
// [--memory-mib N] [--no-open]. Waits for the Machine to shut down, then
// deletes it; Ctrl+C ends it at once.
func cmdRun(ctx context.Context, svc *core.Service, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	ephemeral := fs.Bool("ephemeral", false, "delete the environment when it shuts down (required)")
	image := fs.String("image", "", "installation ISO to boot (Desktops)")
	name := fs.String("name", "", "name while it runs (default: ephemeral-XXXXXX)")
	cpus := fs.Int("cpus", 0, "virtual CPUs (defaults to 2)")
	memory := fs.Int("memory-mib", 0, "memory in MiB (defaults to 2048)")
	noOpen := fs.Bool("no-open", false, "don't open its display (for scripts and agents)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if !*ephemeral {
		return usagef("run: only --ephemeral is supported: omavm run --ephemeral --image path/to/system.iso")
	}
	if fs.NArg() != 0 {
		return usagef("run: unexpected arguments %q", fs.Args())
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := core.Environment{Name: *name, Kind: core.Machine, Image: *image,
		Settings: core.EnvironmentSettings{CPUs: *cpus, MemoryMiB: *memory}}
	fmt.Fprintln(os.Stderr, "omavm: running without keeping anything; it is deleted when it shuts down (Ctrl+C ends it now)")
	created, err := svc.RunEphemeral(ctx, env, !*noOpen, time.Second)
	if errors.Is(err, context.Canceled) {
		fmt.Printf("ended and deleted %s\n", created.Name)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s shut down and was deleted\n", created.Name)
	return nil
}

// cmdClone: omavm clone NAME NEW_NAME [--progress]
func cmdClone(ctx context.Context, svc *core.Service, args []string) error {
	args, progress := extractBoolFlag(args, "progress")
	if len(args) != 2 {
		return usagef("clone: usage: omavm clone <name> <new-name>")
	}
	ctx = withProgress(ctx, progress)
	clone, err := svc.Clone(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Printf("cloned %s as %s\n", args[0], clone.Name)
	return nil
}

// withProgress reports the stages of a long operation: on stdout as
// "progress: STAGE" for a program that asked (the GUI), on a terminal for
// a person. Never on a piped stderr, which callers read as the error text.
func withProgress(ctx context.Context, toStdout bool) context.Context {
	if toStdout {
		return core.WithProgress(ctx, func(stage string) { fmt.Println("progress:", stage) })
	}
	if info, err := os.Stderr.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		return core.WithProgress(ctx, func(stage string) { fmt.Fprintf(os.Stderr, "omavm: %s…\n", stage) })
	}
	return ctx
}

func cmdSimple(ctx context.Context, args []string, verb string, fn func(context.Context, string) error) error {
	if len(args) != 1 {
		return usagef("%s: expected exactly one environment name", verb)
	}
	return fn(ctx, args[0])
}

func cmdStatus(ctx context.Context, svc *core.Service, args []string) error {
	// Not flag.FlagSet: CLAUDE.md's own CLI examples put --json after the
	// positional name (`omavm status radic --json`), and the stdlib flag
	// package stops parsing flags at the first non-flag argument, so it
	// would silently swallow a trailing --json instead of honoring it.
	args, jsonOut := extractBoolFlag(args, "json")
	if len(args) != 1 {
		return usagef("status: expected exactly one environment name")
	}

	status, err := svc.Status(ctx, args[0])
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(status)
	}
	if status.Detail != "" {
		fmt.Printf("%s (%s)\n", status.State, status.Detail)
	} else {
		fmt.Println(status.State)
	}
	if status.Ephemeral {
		fmt.Println("changes in this session are discarded when it shuts down")
	}
	if status.Warning != "" {
		fmt.Println("warning:", status.Warning)
	}
	if status.TravelMode {
		fmt.Println("travel mode: started with fewer CPUs because this computer was on battery")
	}
	if status.RestartNeeded {
		fmt.Println("restart to apply the saved settings")
	}
	return nil
}

func cmdPreview(ctx context.Context, svc *core.Service, args []string) error {
	if len(args) != 1 {
		return usagef("preview: expected exactly one environment name")
	}
	path, err := svc.Preview(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func cmdSnapshot(ctx context.Context, svc *core.Service, args []string) error {
	if len(args) == 0 {
		return usagef("snapshot: expected a subcommand (create, list, go-to, remove)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		// Not flag.FlagSet: as with cmdStatus's --json, the CLI examples
		// put --label after the positional name, which stdlib flag can't
		// parse (it stops at the first non-flag argument).
		rest, label, err := extractValueFlag(rest, "label")
		if err != nil {
			return fmt.Errorf("snapshot create: %w", err)
		}
		if len(rest) != 1 {
			return usagef("snapshot create: expected exactly one environment name")
		}
		snap, err := svc.CreateSnapshot(ctx, rest[0], label)
		if err != nil {
			return err
		}
		fmt.Printf("created snapshot %q (%s)\n", snap.Label, snap.ID)
		return nil
	case "list":
		rest, jsonOut := extractBoolFlag(rest, "json")
		if len(rest) != 1 {
			return usagef("snapshot list: expected exactly one environment name")
		}
		snapshots, err := svc.ListSnapshots(ctx, rest[0])
		if err != nil {
			return err
		}
		if jsonOut {
			if snapshots == nil {
				snapshots = []core.Snapshot{}
			}
			return json.NewEncoder(os.Stdout).Encode(snapshots)
		}
		if len(snapshots) == 0 {
			fmt.Println("no snapshots yet")
			return nil
		}
		for _, snap := range snapshots {
			fmt.Printf("%s\t%s\t%s\n", snap.Label, snap.CreatedAt.Local().Format("2006-01-02 15:04"), snap.ID)
		}
		return nil
	case "go-to":
		if len(rest) != 2 {
			return usagef("snapshot go-to: usage: omavm snapshot go-to <name> <id>")
		}
		return svc.GoToSnapshot(ctx, rest[0], rest[1])
	case "remove", "rm":
		if len(rest) != 2 {
			return usagef("snapshot remove: usage: omavm snapshot remove <name> <id>")
		}
		return svc.RemoveSnapshot(ctx, rest[0], rest[1])
	default:
		return usagef("snapshot: unknown subcommand %q", sub)
	}
}

func cmdApps(ctx context.Context, svc *core.Service, args []string) error {
	args, jsonOut := extractBoolFlag(args, "json")
	args, exportID, err := extractValueFlag(args, "export")
	if err != nil {
		return fmt.Errorf("apps: %w", err)
	}
	args, unexportID, err := extractValueFlag(args, "unexport")
	if err != nil {
		return fmt.Errorf("apps: %w", err)
	}
	if len(args) != 1 {
		return usagef("apps: expected exactly one environment name")
	}
	name := args[0]

	if exportID != "" && unexportID != "" {
		return usagef("apps: choose only one of --export or --unexport")
	}
	if exportID != "" {
		return svc.ExportApp(ctx, name, exportID)
	}
	if unexportID != "" {
		return svc.UnexportApp(ctx, name, unexportID)
	}

	apps, err := svc.ListApps(ctx, name)
	if err != nil {
		return err
	}
	if jsonOut {
		if apps == nil {
			apps = []core.App{}
		}
		return json.NewEncoder(os.Stdout).Encode(apps)
	}
	if len(apps) == 0 {
		fmt.Println("no exportable applications found")
		return nil
	}
	for _, app := range apps {
		exported := ""
		if app.Exported {
			exported = " (exported)"
		}
		fmt.Printf("%s%s\t%s\n", app.Name, exported, app.ID)
	}
	return nil
}

func cmdIntegration(ctx context.Context, svc *core.Service, args []string) error {
	args, jsonOut := extractBoolFlag(args, "json")
	if len(args) != 1 {
		return usagef("integration: expected exactly one environment name")
	}
	report, err := svc.Integration(ctx, args[0])
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	fmt.Printf("guest agent: %s", report.GuestAgent)
	if report.Hint != "" {
		fmt.Printf(" (%s)", report.Hint)
	}
	fmt.Println()
	for _, c := range report.Capabilities {
		fmt.Printf("%s: %s\n", strings.ToLower(c.Label), strings.ReplaceAll(c.State, "_", " "))
		if c.Hint != "" {
			fmt.Printf("  %s\n", c.Hint)
		}
	}
	return nil
}

func cmdHost(ctx context.Context, svc *core.Service, args []string) error {
	args, jsonOut := extractBoolFlag(args, "json")
	if len(args) != 0 {
		return usagef("host: takes no arguments")
	}
	caps, err := svc.InspectHost(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(caps)
	}
	for _, c := range caps {
		mark := "no "
		if c.Available {
			mark = "yes"
		}
		fmt.Printf("%s  %s: %s\n", mark, c.Label, c.Detail)
		if c.Hint != "" {
			fmt.Printf("     %s\n", c.Hint)
		}
	}
	return nil
}

func cmdSettings(ctx context.Context, svc *core.Service, args []string) error {
	if len(args) == 0 {
		return usagef("settings: expected an environment name")
	}
	name := args[0]
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	description := fs.String("description", "", "human-readable description")
	cpus := fs.Int("cpus", 0, "virtual CPUs (Machines only)")
	memory := fs.Int("memory-mib", 0, "memory in MiB (Machines only)")
	sharedPath := fs.String("shared-path", "", "host directory to share (Machines only)")
	sharedReadOnly := fs.Bool("shared-read-only", false, "make the shared folder read-only")
	sharedWritable := fs.Bool("shared-writable", false, "make the shared folder writable")
	disconnectISO := fs.Bool("disconnect-iso", false, "disconnect installation media on next start (Machines only)")
	snapshotLimit := fs.Int("snapshot-limit", 0, "max snapshots to keep, oldest discarded first (1-100)")
	color := fs.String("color", "", "tag color: "+strings.Join(core.EnvironmentColors, ", "))
	shareClipboard := fs.Bool("share-clipboard", false, "share the text clipboard with a Machine's guest, or let programs in a Box's terminal copy to it (on by default)")
	clipboardDirection := fs.String("clipboard-direction", "", "limit the shared clipboard to one way: both, to-host or to-guest (Machines only)")
	travelMode := fs.Bool("travel-mode", false, "use half the CPUs automatically while the host is on battery (on by default)")
	vulkan := fs.Bool("vulkan", false, "Vulkan acceleration when the host supports it (Machines only, on by default)")
	ssh := fs.Bool("ssh", false, "reach the Machine with omavm ssh/exec over a local channel any program on this computer can use, Boxes included (Machines only, on by default)")
	launcher := fs.Bool("launcher", false, "list this environment in the application launcher (on by default)")
	openInEmptyWorkspace := fs.Bool("open-in-empty-workspace", false, "open this environment in an empty Hyprland workspace instead of the current one (on by default; requires Omarchy's Lua-based Hyprland)")
	fullscreen := fs.Bool("fullscreen", false, "open the Machine's display fullscreen (Machines only, on by default)")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 0 {
		return usagef("settings: unexpected positional arguments")
	}
	patch := core.SettingsPatch{}
	changed := false
	modeFlags := 0
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "disconnect-iso":
			patch.DisconnectISO = disconnectISO
			changed = true
		case "description":
			patch.Description = description
			changed = true
		case "cpus":
			patch.CPUs = cpus
			changed = true
		case "memory-mib":
			patch.MemoryMiB = memory
			changed = true
		case "snapshot-limit":
			patch.SnapshotLimit = snapshotLimit
			changed = true
		case "color":
			patch.Color = color
			changed = true
		case "share-clipboard":
			patch.ShareClipboard = shareClipboard
			changed = true
		case "clipboard-direction":
			patch.ClipboardDirection = clipboardDirection
			changed = true
		case "vulkan":
			patch.Vulkan = vulkan
			changed = true
		case "travel-mode":
			patch.TravelMode = travelMode
			changed = true
		case "ssh":
			patch.SSH = ssh
			changed = true
		case "launcher":
			patch.Launcher = launcher
			changed = true
		case "open-in-empty-workspace":
			patch.OpenInEmptyWorkspace = openInEmptyWorkspace
			changed = true
		case "fullscreen":
			patch.Fullscreen = fullscreen
			changed = true
		case "shared-path":
			patch.SharedPath = sharedPath
			changed = true
		case "shared-read-only":
			patch.SharedReadOnly = sharedReadOnly
			modeFlags++
			changed = true
		case "shared-writable":
			readOnly := !*sharedWritable
			patch.SharedReadOnly = &readOnly
			modeFlags++
			changed = true
		}
	})
	if modeFlags > 1 {
		return usagef("settings: choose only one of --shared-read-only or --shared-writable")
	}
	var settings core.EnvironmentSettings
	var err error
	if changed {
		settings, err = svc.Configure(ctx, name, patch)
	} else {
		settings, err = svc.Settings(ctx, name)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(settings)
	}
	if settings.Description != "" {
		fmt.Println("description:", settings.Description)
	}
	if settings.CPUs != 0 {
		fmt.Printf("hardware: %d CPUs, %d MiB memory\n", settings.CPUs, settings.MemoryMiB)
		fmt.Printf("installation media disconnected on next start: %t\n", settings.DisconnectISO)
		fmt.Printf("vulkan acceleration (when the host supports it): %t\n", !settings.VulkanDisabled)
		fmt.Printf("open the display fullscreen: %t\n", !settings.FullscreenDisabled)
		fmt.Printf("ssh (reachable by any program on this computer, Boxes included): %t\n", !settings.SSHDisabled)
	}
	switch {
	case settings.ClipboardDisabled:
		fmt.Println("share clipboard: false")
	case settings.ClipboardDirection == core.ClipboardToHost:
		fmt.Println("share clipboard: only from the environment to this computer")
	case settings.ClipboardDirection == core.ClipboardToGuest:
		fmt.Println("share clipboard: only from this computer to the environment")
	default:
		fmt.Println("share clipboard: true (both ways)")
	}
	fmt.Printf("travel mode (reduce CPUs on battery): %t\n", !settings.TravelModeDisabled)
	fmt.Printf("show in the app launcher: %t\n", !settings.LauncherDisabled)
	if settings.SharedPath != "" {
		mode := "writable"
		if settings.SharedReadOnly {
			mode = "read-only"
		}
		fmt.Printf("shared folder: %s (%s)\n", settings.SharedPath, mode)
	}
	if settings.Color != "" {
		fmt.Println("color:", settings.Color)
	}
	fmt.Printf("open in an empty workspace: %t\n", !settings.EmptyWorkspaceDisabled)
	return nil
}

func cmdExec(ctx context.Context, svc *core.Service, args []string) error {
	if len(args) < 2 {
		return usagef("exec: usage: omavm exec <name> -- <command> [args...]")
	}
	name := args[0]
	rest := args[1:]
	if rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return usagef("exec: no command given")
	}
	return svc.Exec(ctx, name, rest)
}

// cmdSSH: omavm ssh NAME [--user LOGIN] [-- COMMAND...]
func cmdSSH(ctx context.Context, svc *core.Service, args []string) error {
	var command []string
	for i, a := range args {
		if a == "--" {
			command = args[i+1:]
			args = args[:i]
			break
		}
	}
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	login := fs.String("user", "", "user to log in as in the guest (default: your user name)")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return usagef("ssh: usage: omavm ssh <name> [--user LOGIN] [-- command...]")
	}
	if name == "" {
		return usagef("ssh: expected a Machine name")
	}
	return svc.SSH(ctx, name, *login, command)
}

func cmdList(ctx context.Context, svc *core.Service, args []string) error {
	args, jsonOut := extractBoolFlag(args, "json")
	_, withStatus := extractBoolFlag(args, "status")

	if withStatus {
		states, err := svc.ListWithStatus(ctx)
		if err != nil {
			return err
		}
		if jsonOut {
			if states == nil {
				states = []core.EnvironmentState{}
			}
			return json.NewEncoder(os.Stdout).Encode(states)
		}
		if len(states) == 0 {
			fmt.Println("no environments yet — try: omavm create --name <name> --kind box --image <distro>")
			return nil
		}
		for _, s := range states {
			fmt.Printf("%s\t%s\t%s\t%s\n", s.Name, s.Kind, s.Status.State, s.Image)
		}
		return nil
	}

	envs, err := svc.List(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		if envs == nil {
			envs = []core.Environment{}
		}
		return json.NewEncoder(os.Stdout).Encode(envs)
	}
	if len(envs) == 0 {
		fmt.Println("no environments yet — try: omavm create --name <name> --kind box --image <distro>")
		return nil
	}
	for _, env := range envs {
		fmt.Printf("%s\t%s\t%s\t%s\n", env.Name, env.Kind, env.Image, env.Backend)
	}
	return nil
}

// extractBoolFlag pulls a --name boolean flag out of args regardless of
// its position, returning the remaining positional args and whether the
// flag was present. Go's flag.FlagSet can't do this: it stops parsing
// flags at the first non-flag argument, which breaks the
// "flag after the positional name" ordering CLAUDE.md's CLI examples use.
func extractBoolFlag(args []string, name string) ([]string, bool) {
	needle := "--" + name
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == needle {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// extractValueFlag pulls a --name VALUE (or --name=VALUE) flag out of
// args regardless of position, for the same reason extractBoolFlag
// exists: Go's flag package stops parsing at the first positional
// argument, which breaks "flag after the positional name" CLI ordering.
func extractValueFlag(args []string, name string) ([]string, string, error) {
	prefix := "--" + name
	out := make([]string, 0, len(args))
	value := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if eq, ok := strings.CutPrefix(a, prefix+"="); ok {
			value = eq
			continue
		}
		if a == prefix {
			if i+1 >= len(args) {
				return nil, "", usagef("%s requires a value", prefix)
			}
			value = args[i+1]
			i++
			continue
		}
		out = append(out, a)
	}
	return out, value, nil
}

// readOnly reports whether a command only reads state.
func readOnly(cmd string, args []string) bool {
	switch cmd {
	case "list", "ls", "status", "integration", "preview", "host", "help", "-h", "--help":
		return true
	case "snapshot":
		return len(args) > 0 && args[0] == "list"
	case "apps":
		for _, a := range args {
			if strings.HasPrefix(a, "--export") || strings.HasPrefix(a, "--unexport") {
				return false
			}
		}
		return true
	case "settings", "configure":
		// Just a name shows the settings; any flag changes them.
		return len(args) == 1
	}
	return false
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: omavm <command> [arguments]

commands:
  create --name NAME --kind box|machine --image IMAGE     create an environment (a Box needs a
    [--progress]                                           container image, a Machine an ISO);
                                                           --progress prints "progress: STAGE" lines
  start NAME [--ephemeral]                                start an environment; --ephemeral (Machines
                                                           only) discards this session's changes at shutdown
  open NAME                                                start (if needed) and attach
  stop NAME                                                stop an environment
  restart NAME                                             restart a Machine
  pause NAME                                               pause a Machine
  resume NAME                                              resume a Machine
  force-stop NAME                                          immediately stop a Machine
  status NAME [--json]                                     show environment status
  integration NAME [--json]                                check Machine guest tools
  host [--json]                                            show what this computer offers Machines
  settings NAME [--description TEXT] [--cpus N] [--color C] view or change settings
    [--share-clipboard=BOOL] [--travel-mode=BOOL]           (on by default)
    [--clipboard-direction both|to-host|to-guest]          one-way clipboard (Machines)
    [--vulkan=BOOL] [--ssh=BOOL] [--fullscreen=BOOL]       (Machines only, on by default)
    [--launcher=BOOL]                                      list in the app launcher (on by default)
    [--open-in-empty-workspace=BOOL]                       open in an empty workspace (on by default)
  preview NAME                                             capture a Machine screenshot
  snapshot create NAME --label TEXT                        capture the environment's current state
  snapshot list NAME [--json]                              list snapshots
  snapshot go-to NAME ID                                   restore a prior snapshot
  snapshot remove NAME ID                                  delete a snapshot
  apps NAME [--json]                                       list exportable applications (Boxes only)
  apps NAME --export APP_ID                                export an app as a host launcher
  apps NAME --unexport APP_ID                              remove a previously exported launcher
  exec NAME -- CMD [ARGS...]                               run a command inside a Box, or a Machine over SSH
  ssh NAME [--user LOGIN] [-- CMD...]                      open a shell in a Machine (guest needs systemd 256+
                                                           and sshd; any program here can reach it, Boxes too)
  run --ephemeral --image ISO [--name NAME] [--no-open]    run a new Desktop that is deleted when it shuts
    [--cpus N] [--memory-mib N]                            down (Ctrl+C ends and deletes it at once)
  clone NAME NEW_NAME [--progress]                         copy a stopped environment: its disk with its
                                                           snapshots, or its container (needs that much space)
  update NAME [--progress]                                 update a Box's installed software (distrobox upgrade)
  rm NAME                                                  remove an environment
  list [--status] [--json]                                 list known environments, with their state

exit codes: 0 ok, 1 failure, 2 invalid request, 3 not found, 4 name taken,
5 unsupported here, 6 busy; exec and ssh return their command's code.
With --json, failures also print {"error": {"code", "message"}} on stdout.`)
}
