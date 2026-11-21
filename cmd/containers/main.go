// Command containers is an OpsPanel plugin that lists and controls Docker or
// Podman containers through their CLI. It needs no configuration: it uses
// whichever engine is installed and reachable, preferring Docker.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-ltm/opspanel-plugins/internal/cli"
	"github.com/michael-ltm/opspanel-plugins/internal/plugin"
)

var version = "dev"

func dockerSpec() cli.Spec {
	known := map[string][]string{
		"linux":   {"/usr/bin/docker", "/usr/local/bin/docker"},
		"darwin":  {"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker"},
		"windows": {`C:\Program Files\Docker\Docker\resources\bin\docker.exe`},
	}
	if home, err := os.UserHomeDir(); err == nil {
		// OrbStack and Rancher Desktop put their docker CLI under the home dir.
		known["darwin"] = append(known["darwin"], filepath.Join(home, ".orbstack", "bin", "docker"), filepath.Join(home, ".rd", "bin", "docker"))
	}
	return cli.Spec{Name: "docker", Known: known, Hint: "Install Docker (Docker Desktop on macOS / Windows, Docker Engine on Linux) or Podman"}
}

var podmanSpec = cli.Spec{
	Name: "podman",
	Known: map[string][]string{
		"linux":   {"/usr/bin/podman", "/usr/local/bin/podman"},
		"darwin":  {"/opt/homebrew/bin/podman", "/usr/local/bin/podman", "/opt/podman/bin/podman"},
		"windows": {`C:\Program Files\RedHat\Podman\podman.exe`},
	},
}

func main() {
	a := &app{docker: dockerSpec().Find, podman: podmanSpec.Find}
	plugin.Serve(newServer(a), "containers", version)
}

type engine struct{ name, path string }

type app struct {
	docker, podman func() (string, error)

	mu     sync.Mutex
	cached *engine
}

// Container is one container as the panel shows it.
type Container struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	State     string `json:"state" jsonschema:"running, exited, paused, created, …"`
	Status    string `json:"status"`
	Ports     string `json:"ports"`
	CreatedAt *int64 `json:"createdAt" jsonschema:"unix ms"`
}

var (
	idRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	sinceRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z:.+-]{0,39}$`)
)

func checkID(id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid container id or name %q", id)
	}
	return nil
}

// candidate is an engine, or why it is unavailable.
type candidate struct {
	engine
	missing error
}

func (a *app) candidates() []candidate {
	var out []candidate
	for _, c := range []struct {
		name string
		find func() (string, error)
	}{{"docker", a.docker}, {"podman", a.podman}} {
		p, err := c.find()
		out = append(out, candidate{engine{c.name, p}, err})
	}
	return out
}

// pick runs try with the cached engine, or with each installed engine until
// one works. try must be safe to repeat (it is only used for reads).
func (a *app) pick(try func(engine) error) (engine, error) {
	a.mu.Lock()
	cached := a.cached
	a.mu.Unlock()
	if cached != nil {
		err := try(*cached)
		if err != nil {
			a.forget(*cached)
		}
		return *cached, err
	}
	var why []string
	for _, c := range a.candidates() {
		if c.missing != nil {
			why = append(why, c.missing.Error())
			continue
		}
		e := c.engine
		err := try(e)
		if err == nil {
			a.mu.Lock()
			a.cached = &e
			a.mu.Unlock()
			return e, nil
		}
		msg := fmt.Sprintf("%s at %s is not usable: %v", e.name, e.path, err)
		if strings.Contains(err.Error(), "permission denied") && strings.Contains(err.Error(), ".sock") {
			msg += " (the user running OpsPanel needs access to that socket, e.g. membership in the docker group)"
		}
		why = append(why, msg)
	}
	return engine{}, errors.New("no usable container engine: " + strings.Join(why, "; "))
}

func (a *app) forget(e engine) {
	a.mu.Lock()
	if a.cached != nil && *a.cached == e {
		a.cached = nil
	}
	a.mu.Unlock()
}

// engine returns the engine for an action: the one that last worked, or the
// first that answers `ps -q`.
func (a *app) engine(ctx context.Context) (engine, error) {
	a.mu.Lock()
	cached := a.cached
	a.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	return a.pick(func(e engine) error {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		_, err := cli.Run(ctx, e.path, nil, "ps", "-q")
		return err
	})
}

func list(ctx context.Context, e engine, all bool) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"ps"}
	if all {
		args = append(args, "-a")
	}
	if e.name == "podman" {
		out, err := cli.Run(ctx, e.path, nil, append(args, "--format", "json")...)
		if err != nil {
			return nil, err
		}
		return parsePodman(out)
	}
	out, err := cli.Run(ctx, e.path, nil, append(args, "--format", "{{json .}}")...)
	if err != nil {
		return nil, err
	}
	return parseDocker(out)
}

func parseDocker(out []byte) ([]Container, error) {
	cs := []Container{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r struct{ ID, Names, Image, State, Status, Ports, CreatedAt string }
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("unexpected docker ps output: %w", err)
		}
		c := Container{ID: short(r.ID), Name: r.Names, Image: r.Image, State: r.State, Status: r.Status, Ports: r.Ports}
		if t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", r.CreatedAt); err == nil {
			c.CreatedAt = plugin.Millis(t)
		}
		cs = append(cs, c)
	}
	return cs, sc.Err()
}

func parsePodman(out []byte) ([]Container, error) {
	var rows []struct {
		ID      string `json:"Id"`
		Names   []string
		Image   string
		State   any
		Status  string
		Created int64
		Ports   []struct {
			HostIP        string `json:"host_ip"`
			ContainerPort int    `json:"container_port"`
			HostPort      int    `json:"host_port"`
			Protocol      string `json:"protocol"`
		}
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("unexpected podman ps output: %w", err)
	}
	cs := make([]Container, 0, len(rows))
	for _, r := range rows {
		var ports []string
		for _, p := range r.Ports {
			s := strconv.Itoa(p.HostPort) + "->" + strconv.Itoa(p.ContainerPort) + "/" + p.Protocol
			if p.HostIP != "" {
				s = p.HostIP + ":" + s
			}
			ports = append(ports, s)
		}
		c := Container{ID: short(r.ID), Image: r.Image, State: fmt.Sprint(r.State), Status: r.Status, Ports: strings.Join(ports, ", ")}
		if len(r.Names) > 0 {
			c.Name = r.Names[0]
		}
		if r.Created > 0 {
			c.CreatedAt = plugin.Millis(time.Unix(r.Created, 0))
		}
		cs = append(cs, c)
	}
	return cs, nil
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ListIn filters the list.
type ListIn struct {
	RunningOnly bool `json:"running_only,omitempty" jsonschema:"only list running containers (default: all)"`
}

// ListOut is the container list.
type ListOut struct {
	Engine     string      `json:"engine" jsonschema:"docker or podman"`
	Running    int         `json:"running"`
	Total      int         `json:"total"`
	Containers []Container `json:"containers"`
}

// LogsIn picks a container's logs.
type LogsIn struct {
	ID    string `json:"id" jsonschema:"container id or name"`
	Tail  int    `json:"tail,omitempty" jsonschema:"lines from the end (default 200, at most 2000)"`
	Since string `json:"since,omitempty" jsonschema:"only logs since this time: a duration like 10m or an RFC 3339 timestamp"`
}

// LogsOut is a log excerpt.
type LogsOut struct {
	Engine string `json:"engine"`
	Logs   string `json:"logs"`
}

// IDIn names a container.
type IDIn struct {
	ID string `json:"id" jsonschema:"container id or name"`
}

// RemoveIn names a container to delete.
type RemoveIn struct {
	ID    string `json:"id" jsonschema:"container id or name"`
	Force bool   `json:"force,omitempty" jsonschema:"stop it first if it is running"`
}

// ActionOut reports what the engine printed.
type ActionOut struct {
	Engine string `json:"engine"`
	ID     string `json:"id"`
	Action string `json:"action"`
	Output string `json:"output"`
}

func (a *app) action(ctx context.Context, id, verb string, timeout time.Duration, extra ...string) (ActionOut, error) {
	if err := checkID(id); err != nil {
		return ActionOut{}, err
	}
	e, err := a.engine(ctx)
	if err != nil {
		return ActionOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(append([]string{verb}, extra...), id)
	out, err := cli.Run(ctx, e.path, nil, args...)
	if err != nil {
		a.forget(e)
		return ActionOut{}, err
	}
	return ActionOut{Engine: e.name, ID: id, Action: verb, Output: strings.TrimSpace(string(out))}, nil
}

func newServer(a *app) *mcp.Server {
	s := plugin.New("containers", version)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "containers_list",
		Description: "List Docker or Podman containers (all of them by default) with image, state, status and published ports.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, ListOut, error) {
		var cs []Container
		e, err := a.pick(func(e engine) error {
			var err error
			cs, err = list(ctx, e, !in.RunningOnly)
			return err
		})
		if err != nil {
			return nil, ListOut{}, err
		}
		out := ListOut{Engine: e.name, Total: len(cs), Containers: cs}
		for _, c := range cs {
			if c.State == "running" {
				out.Running++
			}
		}
		return nil, out, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "container_logs",
		Description: "The last lines of a container's logs (stdout and stderr).",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in LogsIn) (*mcp.CallToolResult, LogsOut, error) {
		if err := checkID(in.ID); err != nil {
			return nil, LogsOut{}, err
		}
		if in.Since != "" && !sinceRE.MatchString(in.Since) {
			return nil, LogsOut{}, fmt.Errorf("invalid since %q: use a duration like 10m or an RFC 3339 timestamp", in.Since)
		}
		tail := in.Tail
		if tail <= 0 {
			tail = 200
		}
		tail = min(tail, 2000)
		e, err := a.engine(ctx)
		if err != nil {
			return nil, LogsOut{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		args := []string{"logs", "--tail", strconv.Itoa(tail)}
		if in.Since != "" {
			args = append(args, "--since="+in.Since)
		}
		out, err := cli.Combined(ctx, e.path, nil, append(args, in.ID)...)
		if err != nil {
			a.forget(e)
			return nil, LogsOut{}, err
		}
		return nil, LogsOut{Engine: e.name, Logs: cli.Tail(string(out), 64<<10)}, nil
	})
	for _, v := range []struct{ verb, desc string }{
		{"start", "Start a stopped container."},
		{"stop", "Stop a running container (it can be started again)."},
		{"restart", "Restart a container."},
	} {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "container_" + v.verb,
			Description: v.desc,
			Annotations: plugin.Mutating(),
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in IDIn) (*mcp.CallToolResult, ActionOut, error) {
			out, err := a.action(ctx, in.ID, v.verb, 90*time.Second)
			return nil, out, err
		})
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "container_remove",
		Description: "Delete a container. Its writable layer is lost; named volumes are kept.",
		Annotations: plugin.Destructive(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RemoveIn) (*mcp.CallToolResult, ActionOut, error) {
		var extra []string
		if in.Force {
			extra = append(extra, "-f")
		}
		out, err := a.action(ctx, in.ID, "rm", 60*time.Second, extra...)
		return nil, out, err
	})
	return s
}
