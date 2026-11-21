package plugintest

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Response is what the fake CLI prints when its arguments start with Match.
type Response struct {
	Match  string `json:"match"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

const envDir = "PLUGINTEST_FAKE_CLI_DIR"

// RunFakeCLIIfRequested turns the test binary into the fake CLI when a test
// started it through FakeCLI. Call it first in TestMain.
func RunFakeCLIIfRequested() {
	dir := os.Getenv(envDir)
	if dir == "" {
		return
	}
	args := os.Args[1:]
	if f, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(args)
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	var rs []Response
	b, _ := os.ReadFile(filepath.Join(dir, "responses.json"))
	_ = json.Unmarshal(b, &rs)
	joined := strings.Join(args, " ")
	for _, r := range rs {
		if strings.HasPrefix(joined, r.Match) {
			_, _ = io.WriteString(os.Stdout, r.Stdout)
			_, _ = io.WriteString(os.Stderr, r.Stderr)
			os.Exit(r.Exit)
		}
	}
	_, _ = io.WriteString(os.Stderr, "fake cli: no response for: "+joined+"\n")
	os.Exit(99)
}

// FakeCLI places an executable named name that answers with responses, and
// returns its path and the directory holding its call log.
func FakeCLI(t *testing.T, name string, responses ...Response) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	b, _ := json.Marshal(responses)
	if err := os.WriteFile(filepath.Join(dir, "responses.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path = filepath.Join(dir, name)
	if err := os.Link(self, path); err != nil {
		copyFile(t, self, path)
	}
	t.Setenv(envDir, dir)
	return path, dir
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// Calls returns the argument lists the fake CLI received.
func Calls(t *testing.T, dir string) [][]string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	var out [][]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatal(err)
		}
		out = append(out, args)
	}
	return out
}

// Alias adds another name for the fake CLI at path; it shares the responses.
func Alias(t *testing.T, path, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	alias := filepath.Join(filepath.Dir(path), name)
	if err := os.Link(path, alias); err != nil {
		copyFile(t, path, alias)
	}
	return alias
}
