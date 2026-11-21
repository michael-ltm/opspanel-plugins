package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestFindPrefersPATHThenKnownLocations(t *testing.T) {
	defer func(l func(string) (string, error), e func(string) bool) { lookPath, isExec = l, e }(lookPath, isExec)
	spec := Spec{Name: "tool", Known: map[string][]string{"darwin": {"/opt/a/tool", "/opt/b/tool"}}, Hint: "brew install tool"}

	lookPath = func(string) (string, error) { return "/usr/bin/tool", nil }
	if p, err := spec.find("darwin"); err != nil || p != "/usr/bin/tool" {
		t.Fatalf("PATH hit: %q %v", p, err)
	}

	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	isExec = func(p string) bool { return p == "/opt/b/tool" }
	if p, err := spec.find("darwin"); err != nil || p != "/opt/b/tool" {
		t.Fatalf("known hit: %q %v", p, err)
	}

	isExec = func(string) bool { return false }
	_, err := spec.find("darwin")
	var nf *NotFoundError
	if !errors.As(err, &nf) || !IsNotFound(err) {
		t.Fatalf("want NotFoundError, got %v", err)
	}
	for _, want := range []string{"tool not found", "PATH", "/opt/a/tool", "/opt/b/tool", "brew install tool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %q", want, err)
		}
	}
	// Another OS's locations are not checked.
	if _, err := spec.find("linux"); strings.Contains(err.Error(), "/opt/a") {
		t.Errorf("linux checked darwin paths: %v", err)
	}
}

func TestRunReportsStderrOnFailure(t *testing.T) {
	if os.Getenv("CLI_HELPER") == "1" {
		os.Stdout.WriteString("partial")
		os.Stderr.WriteString("daemon is not running\n")
		os.Exit(3)
	}
	out, err := Run(context.Background(), os.Args[0], []string{"CLI_HELPER=1"}, "-test.run=TestRunReportsStderrOnFailure")
	var ee *ExitError
	if !errors.As(err, &ee) || !strings.Contains(err.Error(), "daemon is not running") {
		t.Fatalf("err = %v", err)
	}
	if string(out) != "partial" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestTail(t *testing.T) {
	if got := Tail("  short  ", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	got := Tail("line one\nline two\nline three", 14)
	if got != "…line three" {
		t.Fatalf("got %q", got)
	}
	if got := TailLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Fatalf("got %q", got)
	}
}
