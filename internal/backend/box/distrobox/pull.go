package distrobox

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// pullImage downloads a new Box's image before `distrobox create`, which
// would download it in silence for minutes. Pulling it here lets the
// caller follow the stages the engine prints (Podman doesn't report bytes:
// containers/podman#24887). distrobox create then finds the image local.
//
// Best-effort: distrobox knows its engine's quirks better (its own config
// file can pick the engine, Docker may need sudo), so when this pull
// fails, distrobox create still gets its own try and its own error.
func (b *Backend) pullImage(ctx context.Context, image string) {
	if image == "" {
		return
	}
	engine := ""
	for _, candidate := range containerEngines() {
		if path, err := exec.LookPath(candidate); err == nil {
			engine = path
			break
		}
	}
	if engine == "" {
		return
	}
	if exec.CommandContext(ctx, engine, "image", "inspect", image).Run() == nil {
		return // already here: nothing to download
	}
	core.ReportProgress(ctx, "Downloading %s", image)
	cmd := exec.CommandContext(ctx, engine, "pull", image)
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		slog.Warn("image download failed; leaving it to distrobox", "image", image, "error", err)
		return
	}
	done := make(chan struct{})
	var tail []string
	go func() {
		defer close(done)
		stages := pullStages{image: image}
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()
			tail = append(tail, line)
			if len(tail) > 5 {
				tail = tail[1:]
			}
			if stage, ok := stages.next(line); ok {
				core.ReportProgress(ctx, "%s", stage)
			}
		}
	}()
	err := cmd.Wait()
	writer.Close()
	<-done
	if err != nil {
		slog.Warn("image download failed; leaving it to distrobox", "image", image, "error", err, "output", strings.Join(tail, "\n"))
		core.ReportProgress(ctx, "Preparing %s", image)
		return
	}
	core.ReportProgress(ctx, "Creating the Box from %s", image)
}

// pullStages turns the lines of `podman pull` or `docker pull` into stages
// a person can follow: which layer is downloading, never a percentage.
type pullStages struct {
	image  string
	layers int
}

func (p *pullStages) next(line string) (string, bool) {
	line = strings.TrimSpace(line)
	switch {
	// Podman: one "Copying blob" line as each layer starts.
	case strings.HasPrefix(line, "Copying blob"):
		p.layers++
		return "Downloading " + p.image + ": layer " + strconv.Itoa(p.layers), true
	case strings.HasPrefix(line, "Writing manifest"):
		return "Downloading " + p.image + ": finishing", true
	// Docker: "<id>: Pull complete" as each layer lands.
	case strings.HasSuffix(line, ": Pull complete"):
		p.layers++
		return "Downloading " + p.image + ": " + strconv.Itoa(p.layers) + " layers done", true
	}
	return "", false
}
