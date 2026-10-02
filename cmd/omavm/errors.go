package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// Exit codes are a contract with scripts and agents: they say what the
// caller should do next without parsing the message. Documented in the
// README ("Exit codes"); never renumber one.
const (
	exitFailure     = 1 // the system or a backend failed; retrying may help
	exitUsage       = 2 // invalid request: fix the arguments
	exitNotFound    = 3 // no environment by that name
	exitExists      = 4 // the name is taken
	exitUnsupported = 5 // not available for this kind of environment or host
	exitBusy        = 6 // another operation on the environment is running
)

// usageError marks a malformed command line, as opposed to a request the
// Core rejected.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// errorClass maps an error to its exit code and a stable snake_case code
// for --json output.
func errorClass(err error) (int, string) {
	var usage usageError
	switch {
	case errors.As(err, &usage), errors.Is(err, core.ErrInvalidInput):
		return exitUsage, "invalid_input"
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrSnapshotGone):
		return exitNotFound, "not_found"
	case errors.Is(err, core.ErrAlreadyExists):
		return exitExists, "already_exists"
	case errors.Is(err, core.ErrUnsupported):
		return exitUnsupported, "unsupported"
	case errors.Is(err, core.ErrBusy):
		return exitBusy, "busy"
	}
	var kind *core.InvalidKindError
	if errors.As(err, &kind) {
		return exitUsage, "invalid_input"
	}
	return exitFailure, "failed"
}

// exitCodeFor is the process exit code for a failed command. exec and ssh
// pass the remote command's own code through, like ssh and docker exec
// do: `omavm exec box -- go test ./...` must fail the way go test failed.
func exitCodeFor(cmd string, err error) int {
	var exitErr *exec.ExitError
	if (cmd == "exec" || cmd == "ssh") && errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	code, _ := errorClass(err)
	return code
}

// writeJSONError reports a failure on stdout for a caller that asked for
// --json: {"error": {"code": "not_found", "message": "..."}}.
func writeJSONError(w io.Writer, err error) {
	_, code := errorClass(err)
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body.Error.Code = code
	body.Error.Message = err.Error()
	_ = json.NewEncoder(w).Encode(body)
}

// wantsJSON reports whether a command line asked for --json output. What
// follows "--" belongs to the command run by exec/ssh, not to omavm.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "--json=true" {
			return true
		}
	}
	return false
}
