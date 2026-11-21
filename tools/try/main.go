// Command try starts a plugin binary over stdio and calls one tool, for
// smoke-testing builds on a real machine:
//
//	go run ./tools/try ./dist/tailscale-linux-amd64 peers '{"online_only":true}'
//	go run ./tools/try ./dist/tailscale-linux-amd64          # lists the tools
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: try <plugin-binary> [tool [json-args]]")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "try", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(os.Args[1])}, nil)
	if err != nil {
		fail(err)
	}
	defer cs.Close()
	if len(os.Args) == 2 {
		for t, err := range cs.Tools(ctx, nil) {
			if err != nil {
				fail(err)
			}
			a, _ := json.Marshal(t.Annotations)
			fmt.Printf("%-20s %s\n", t.Name, a)
		}
		return
	}
	args := map[string]any{}
	if len(os.Args) > 3 {
		if err := json.Unmarshal([]byte(os.Args[3]), &args); err != nil {
			fail(err)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: os.Args[2], Arguments: args})
	if err != nil {
		fail(err)
	}
	var out any = res.StructuredContent
	if out == nil || res.IsError {
		out = res.Content
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if res.IsError {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "try:", err)
	os.Exit(1)
}
