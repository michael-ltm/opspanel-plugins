// Package cli finds and runs the external command-line tools plugins wrap,
// without trusting the caller's PATH: a panel started as a service often has
// a minimal one.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Spec says where a tool may live.
type Spec struct {
	// Name is the command name looked up on PATH (".exe" is implied on Windows).
	Name string
	// Known lists absolute paths to try per GOOS when PATH has no match.
	Known map[string][]string
	// Hint tells the user how to get the tool, shown when it isn't found.
	Hint string
}

// NotFoundError says which tool is missing and every place that was checked.
type NotFoundError struct {
	Tool    string
	Checked []string
	Hint    string
}

func (e *NotFoundError) Error() string {
	msg := fmt.Sprintf("%s not found (checked: %s)", e.Tool, strings.Join(e.Checked, ", "))
	if e.Hint != "" {
		msg += ". " + e.Hint
	}
	return msg
}

// lookPath and isExec are replaced in tests.
var (
	lookPath = exec.LookPath
	isExec   = func(path string) bool {
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() {
			return false
		}
		return runtime.GOOS == "windows" || fi.Mode()&0o111 != 0
	}
)

// Find resolves the tool for the running OS.
func (s Spec) Find() (string, error) { return s.find(runtime.GOOS) }

func (s Spec) find(goos string) (string, error) {
	checked := []string{"PATH"}
	if p, err := lookPath(s.Name); err == nil {
		return p, nil
	}
	for _, p := range s.Known[goos] {
		checked = append(checked, p)
		if isExec(p) {
			return p, nil
		}
	}
	return "", &NotFoundError{Tool: s.Name, Checked: checked, Hint: s.Hint}
}

// ExitError is a tool that ran and failed; Output is the tail of what it
// printed, which is usually the real reason.
type ExitError struct {
	Tool   string
	Output string
	Err    error
}

func (e *ExitError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("%s failed: %v", e.Tool, e.Err)
	}
	return fmt.Sprintf("%s failed: %s", e.Tool, e.Output)
}

func (e *ExitError) Unwrap() error { return e.Err }

// Run runs path with args and returns stdout. On failure the error carries
// stderr (or stdout when stderr is empty). env entries are added to the
// process environment.
func Run(ctx context.Context, path string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	out := stderr.String()
	if strings.TrimSpace(out) == "" {
		out = stdout.String()
	}
	return stdout.Bytes(), &ExitError{Tool: name(path), Output: Tail(out, 800), Err: err}
}

// Combined runs path and returns stdout and stderr interleaved, for logs.
func Combined(ctx context.Context, path string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return out, &ExitError{Tool: name(path), Output: Tail(string(out), 800), Err: err}
	}
	return out, nil
}

func name(path string) string {
	base := path[strings.LastIndexAny(path, `/\`)+1:]
	return strings.TrimSuffix(strings.ToLower(base), ".exe")
}

// Tail keeps the last n bytes of s, trimmed, starting at a line boundary
// when one is close.
func Tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < n/2 {
		s = s[i+1:]
	}
	return "…" + s
}

// TailLines keeps the last n lines of s.
func TailLines(s string, n int) string {
	s = strings.TrimRight(s, "\r\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// IsNotFound reports whether err is a NotFoundError.
func IsNotFound(err error) bool {
	var nf *NotFoundError
	return errors.As(err, &nf)
}
