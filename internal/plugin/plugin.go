// Package plugin holds what every catalog plugin's MCP server shares: stdio
// serving and the tool annotations OpsPanel uses to decide confirmations.
package plugin

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// New returns a server named after the plugin.
func New(name, version string) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)
}

// Serve runs s over stdio until the client disconnects. "-version" prints the
// version, which release builds use as a smoke test.
func Serve(s *mcp.Server, name, version string) {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(name, version)
		return
	}
	log.SetFlags(0)
	log.SetPrefix(name + ": ")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := s.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

// ReadOnly marks a tool that only reads; status cards may bind to it.
func ReadOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true}
}

// Mutating marks a tool that changes something but destroys nothing.
func Mutating() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(false)}
}

// Destructive marks a tool that can delete or overwrite; OpsPanel always asks
// before running it.
func Destructive() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(true)}
}

// Millis converts t to unix milliseconds, the unit OpsPanel's "time" columns
// expect; the zero time becomes nil.
func Millis(t time.Time) *int64 {
	if t.IsZero() || t.Year() < 1971 {
		return nil
	}
	return ptr(t.UnixMilli())
}
