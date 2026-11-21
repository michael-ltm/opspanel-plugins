// Package plugintest connects a plugin's MCP server to an in-memory client
// for tests.
package plugintest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connect starts s and returns a connected client session.
func Connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs
}

// Result is a decoded tool result.
type Result struct {
	IsError bool
	Text    string
	Data    map[string]any
}

// Call calls a tool and decodes its structured output.
func Call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) Result {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var r Result
	r.IsError = res.IsError
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	r.Text = strings.Join(texts, "\n")
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &r.Data)
	}
	return r
}

// MustOK calls a tool and fails the test when it returns an error.
func MustOK(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	r := Call(t, cs, name, args)
	if r.IsError {
		t.Fatalf("%s returned an error: %s", name, r.Text)
	}
	return r.Data
}

// Annotations lists each tool's annotations.
func Annotations(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.ToolAnnotations {
	t.Helper()
	out := map[string]*mcp.ToolAnnotations{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		out[tool.Name] = tool.Annotations
	}
	return out
}

// Kind classifies annotations the way OpsPanel does.
func Kind(a *mcp.ToolAnnotations) string {
	switch {
	case a == nil:
		return "unannotated"
	case a.ReadOnlyHint:
		return "read"
	case a.DestructiveHint != nil && *a.DestructiveHint:
		return "destructive"
	case a.DestructiveHint != nil:
		return "write"
	}
	return "unannotated"
}

// WantKinds checks every tool's annotation class, and that no other tools exist.
func WantKinds(t *testing.T, cs *mcp.ClientSession, want map[string]string) {
	t.Helper()
	got := Annotations(t, cs)
	if len(got) != len(want) {
		t.Errorf("tools = %v, want %v", keys(got), want)
	}
	for name, kind := range want {
		a, ok := got[name]
		if !ok {
			t.Errorf("tool %s missing", name)
			continue
		}
		if k := Kind(a); k != kind {
			t.Errorf("%s annotations = %s, want %s", name, k, kind)
		}
	}
}

func keys(m map[string]*mcp.ToolAnnotations) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
