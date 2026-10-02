// Package distrobox adapts Distrobox as OmaVM's integrated Development Box backend.
package distrobox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

type Backend struct{}

func New() *Backend           { return &Backend{} }
func (*Backend) Name() string { return "distrobox" }

var accentFold = func() map[rune]rune {
	fold := map[rune]rune{}
	for base, accented := range map[rune]string{
		'a': "áàâãäå", 'c': "ç", 'e': "éèêë", 'i': "íìîï",
		'o': "óòôõö", 'u': "úùûü", 'n': "ñ", 'y': "ýÿ",
	} {
		for _, r := range accented {
			fold[r] = base
		}
	}
	return fold
}()

func boxName(env core.Environment) string {
	var name strings.Builder
	name.WriteString("omavm-")
	for _, r := range strings.ToLower(env.Name) {
		// Podman only accepts [a-zA-Z0-9][a-zA-Z0-9_.-]*: accented Latin
		// letters keep their base letter so the name stays readable, and
		// anything else non-ASCII becomes a separator. The ID suffix below
		// is what keeps names unique.
		if base, ok := accentFold[r]; ok {
			r = base
		}
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-') {
			name.WriteRune(r)
		} else if name.Len() == 0 || !strings.HasSuffix(name.String(), "-") {
			name.WriteByte('-')
		}
	}
	clean := strings.TrimRight(name.String(), "-")
	id := strings.ReplaceAll(env.ID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		return clean
	}
	return clean + "-" + id
}

func (b *Backend) Create(ctx context.Context, env core.Environment) error {
	b.pullImage(ctx, env.Image)
	return b.run(ctx, "create", "--yes", "--name", boxName(env), "--image", env.Image)
}

func (b *Backend) Start(ctx context.Context, env core.Environment) error {
	if err := b.requireContainer(ctx, env); err != nil {
		return err
	}
	b.applyTravelMode(ctx, env)
	return b.run(ctx, "enter", "--no-tty", "--name", boxName(env), "--", "true")
}

func (b *Backend) Open(ctx context.Context, env core.Environment) error {
	if err := b.requireContainer(ctx, env); err != nil {
		return err
	}
	b.applyTravelMode(ctx, env)
	return b.runInteractive(ctx, "enter", "--name", boxName(env))
}

func (b *Backend) Stop(ctx context.Context, env core.Environment) error {
	if _, found, err := b.find(ctx, env); err == nil && !found {
		return nil // nothing left to stop
	}
	return b.run(ctx, "stop", "--yes", boxName(env))
}

// find looks the Box's container up, returning its status.
func (b *Backend) find(ctx context.Context, env core.Environment) (status core.Status, found bool, err error) {
	name := boxName(env)
	all, err := listContainers(ctx, []string{name})
	if err != nil {
		return core.Status{}, false, err
	}
	status, found = all[name]
	return status, found, nil
}

// containerEngines are the engines Distrobox can put a Box's container in,
// in its own order of preference. DBX_CONTAINER_MANAGER, Distrobox's own
// override, narrows it to one.
func containerEngines() []string {
	if engine := os.Getenv("DBX_CONTAINER_MANAGER"); engine != "" {
		return []string{engine}
	}
	return []string{"podman", "docker"}
}

// listContainers asks the container engines directly for the Distrobox
// containers (they carry the label manager=distrobox) and their states,
// one query per engine. `distrobox list` would do the same underneath, but
// prints a table meant for people: the engine's own state field
// ("running", "exited", ...) is stable and machine-readable.
//
// Engines are asked in Distrobox's order, and the next one only for the
// names the previous ones didn't have: the GUI asks every few seconds,
// and on a host where only docker.socket is enabled a `docker ps` starts
// the Docker daemon. An engine that is missing or fails (a stopped Docker
// daemon) is skipped as long as another one answered.
func listContainers(ctx context.Context, names []string) (map[string]core.Status, error) {
	all := map[string]core.Status{}
	answered := false
	var lastErr error
	for _, engine := range containerEngines() {
		missing := false
		for _, name := range names {
			if _, ok := all[name]; !ok {
				missing = true
				break
			}
		}
		if answered && !missing {
			break
		}
		path, err := exec.LookPath(engine)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, path, "ps", "--all",
			"--filter", "label=manager=distrobox",
			"--format", `{{.Names}}{{"\t"}}{{.State}}{{"\t"}}{{.Status}}`).Output()
		if err != nil {
			lastErr = fmt.Errorf("%s ps: %w", engine, err)
			continue
		}
		answered = true
		for name, status := range parseContainers(string(out)) {
			if _, ok := all[name]; !ok {
				all[name] = status
			}
		}
	}
	if !answered {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("Development Boxes need Podman or Docker (install one with: sudo pacman -S podman)")
	}
	return all, nil
}

// parseContainers reads `<engine> ps` lines of name, state and the
// engine's human status ("Up 2 minutes"), which is kept as detail.
func parseContainers(out string) map[string]core.Status {
	all := map[string]core.Status{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 2 {
			continue
		}
		status := core.Status{State: containerState(fields[1])}
		if len(fields) == 3 {
			status.Detail = strings.TrimSpace(fields[2])
		}
		// Docker joins several names of one container with commas.
		for _, name := range strings.Split(fields[0], ",") {
			all[strings.TrimSpace(name)] = status
		}
	}
	return all
}

// containerState maps Podman's and Docker's container states.
func containerState(state string) core.State {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return core.StateRunning
	case "paused":
		return core.StatePaused
	case "restarting":
		return core.StateStarting
	case "stopping", "removing":
		return core.StateStopping
	case "created", "configured", "exited", "stopped":
		return core.StateStopped
	case "dead":
		return core.StateError
	default:
		return core.StateUnknown
	}
}

func missingContainer(env core.Environment) string {
	return fmt.Sprintf("the container of Box %s no longer exists (it was removed outside OmaVM); delete this Box and create it again", env.Name)
}

// requireContainer guards every `distrobox enter`: for a missing container,
// enter offers to create one out of Distrobox's own default image and,
// without a terminal, accepts by itself — silently replacing the Box's
// image (an Ubuntu Box would come back as a Fedora toolbox).
func (b *Backend) requireContainer(ctx context.Context, env core.Environment) error {
	_, found, err := b.find(ctx, env)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", core.ErrNotFound, missingContainer(env))
	}
	return nil
}

func (b *Backend) Status(ctx context.Context, env core.Environment) (core.Status, error) {
	status, found, err := b.find(ctx, env)
	if err != nil {
		return core.Status{}, err
	}
	if !found {
		return core.Status{State: core.StateError, Detail: missingContainer(env)}, nil
	}
	return status, nil
}

// Statuses answers for many Boxes with one query per container engine
// instead of one per Box (core.StatusLister).
func (b *Backend) Statuses(ctx context.Context, envs []core.Environment) (map[string]core.Status, error) {
	names := make([]string, len(envs))
	for i, env := range envs {
		names[i] = boxName(env)
	}
	all, err := listContainers(ctx, names)
	if err != nil {
		return nil, err
	}
	statuses := make(map[string]core.Status, len(envs))
	for _, env := range envs {
		status, found := all[boxName(env)]
		if !found {
			status = core.Status{State: core.StateError, Detail: missingContainer(env)}
		}
		statuses[env.ID] = status
	}
	return statuses, nil
}

func (b *Backend) Exec(ctx context.Context, env core.Environment, args []string) error {
	if err := b.requireContainer(ctx, env); err != nil {
		return err
	}
	full := append([]string{"enter", "--no-tty", "--name", boxName(env), "--"}, args...)
	return b.runInteractive(ctx, full...)
}

func (b *Backend) Remove(ctx context.Context, env core.Environment) error {
	engine, image, known := containerImage(ctx, boxName(env))
	if err := b.run(ctx, "rm", "--force", boxName(env)); err != nil {
		return err
	}
	// A clone's container was made from a commit of its source
	// (distrobox --clone tags it <source container>:<date>); nobody else
	// knows that image, so it goes with the clone. Refused, harmlessly,
	// while another clone still uses it. Pulled images are never touched.
	if known && isCloneImage(image) {
		if out, err := exec.CommandContext(ctx, engine, "image", "rm", image).CombinedOutput(); err != nil {
			slog.Info("clone image kept", "box", env.Name, "image", image, "reason", strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// containerImage finds which engine holds a container and the image it
// was created from.
func containerImage(ctx context.Context, name string) (engine, image string, ok bool) {
	for _, candidate := range containerEngines() {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, path, "container", "inspect", "--format", "{{.Config.Image}}", name).Output()
		if err == nil {
			return path, strings.TrimSpace(string(out)), true
		}
	}
	return "", "", false
}

// isCloneImage recognizes the image distrobox --clone commits for a Box
// OmaVM named: "omavm-…:<date>", local, with no registry in front.
func isCloneImage(image string) bool {
	repo := strings.TrimPrefix(image, "localhost/")
	return strings.HasPrefix(repo, "omavm-") && !strings.Contains(repo, "/")
}

func (b *Backend) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	path, err := exec.LookPath("distrobox")
	if err != nil {
		return nil, fmt.Errorf("distrobox is required for Development Boxes (install it with: sudo pacman -S distrobox): %w", err)
	}
	return exec.CommandContext(ctx, path, args...), nil
}

func (b *Backend) output(ctx context.Context, args ...string) (string, error) {
	cmd, err := b.command(ctx, args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("distrobox %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (b *Backend) run(ctx context.Context, args ...string) error {
	_, err := b.output(ctx, args...)
	return err
}

func (b *Backend) runInteractive(ctx context.Context, args ...string) error {
	cmd, err := b.command(ctx, args...)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("distrobox %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Clone makes clone a copy of source with Distrobox's --clone, which
// commits the source container to an image and creates the new one from
// it. Distrobox refuses a running source ("Cannot clone a running
// container"); this says so first, in OmaVM's words.
func (b *Backend) Clone(ctx context.Context, source, clone core.Environment) error {
	status, found, err := b.find(ctx, source)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", core.ErrNotFound, missingContainer(source))
	}
	if status.State != core.StateStopped {
		return core.Invalidf("stop %s first: a running Box can't be copied (omavm stop %s)", source.Name, source.Name)
	}
	core.ReportProgress(ctx, "Copying %s", source.Name)
	return b.run(ctx, "create", "--yes", "--clone", boxName(source), "--name", boxName(clone))
}

// Upgrade runs distrobox upgrade, which starts the Box if needed and has
// its package manager update everything (dnf, apt, pacman, ...). Each line
// it prints is reported as a stage: the package manager's own progress.
func (b *Backend) Upgrade(ctx context.Context, env core.Environment) error {
	if err := b.requireContainer(ctx, env); err != nil {
		return err
	}
	core.ReportProgress(ctx, "Updating %s", env.Name)
	return b.runReporting(ctx, "upgrade", boxName(env))
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// runReporting runs a distrobox command, reporting each line of its output
// as a progress stage; on failure, the error carries the last lines.
func (b *Backend) runReporting(ctx context.Context, args ...string) error {
	cmd, err := b.command(ctx, args...)
	if err != nil {
		return err
	}
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("distrobox %s: %w", strings.Join(args, " "), err)
	}
	done := make(chan struct{})
	var tail []string
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(ansiEscape.ReplaceAllString(scanner.Text(), ""))
			if line == "" {
				continue
			}
			tail = append(tail, line)
			if len(tail) > 8 {
				tail = tail[1:]
			}
			if len(line) > 120 {
				line = line[:120] + "…"
			}
			core.ReportProgress(ctx, "%s", line)
		}
	}()
	err = cmd.Wait()
	writer.Close()
	<-done
	if err != nil {
		return fmt.Errorf("distrobox %s: %w: %s", strings.Join(args, " "), err, strings.Join(tail, "\n"))
	}
	return nil
}
