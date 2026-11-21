package main

import (
	"os"
	"strings"
	"testing"

	"github.com/michael-ltm/opspanel-plugins/internal/cli"
	"github.com/michael-ltm/opspanel-plugins/internal/plugintest"
)

func TestMain(m *testing.M) {
	plugintest.RunFakeCLIIfRequested()
	os.Exit(m.Run())
}

const dockerPS = `{"CreatedAt":"2026-11-20 10:00:00 +0800 CST","ID":"0123456789ab","Image":"nginx:1.29","Names":"web","Ports":"0.0.0.0:8080->80/tcp","State":"running","Status":"Up 2 hours"}
{"CreatedAt":"2026-11-19 09:00:00 +0000 UTC","ID":"ba9876543210","Image":"redis","Names":"cache","Ports":"","State":"exited","Status":"Exited (0) 3 hours ago"}
`

const podmanPS = `[{"Id":"fedcba9876543210fedcba","Names":["db"],"Image":"docker.io/library/postgres:18","State":"running","Status":"Up 5 minutes","Created":1763600000,"Ports":[{"host_ip":"","container_port":5432,"host_port":15432,"range":1,"protocol":"tcp"}]}]`

func notFound(tool string) func() (string, error) {
	return func() (string, error) {
		return "", &cli.NotFoundError{Tool: tool, Checked: []string{"PATH", "/usr/bin/" + tool}}
	}
}

func found(path string) func() (string, error) { return func() (string, error) { return path, nil } }

func TestAnnotations(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{docker: notFound("docker"), podman: notFound("podman")}))
	plugintest.WantKinds(t, cs, map[string]string{
		"containers_list": "read", "container_logs": "read",
		"container_start": "write", "container_stop": "write", "container_restart": "write",
		"container_remove": "destructive",
	})
}

func TestListDocker(t *testing.T) {
	bin, dir := plugintest.FakeCLI(t, "docker", plugintest.Response{Match: "ps -a --format", Stdout: dockerPS}, plugintest.Response{Match: "ps --format", Stdout: ""})
	cs := plugintest.Connect(t, newServer(&app{docker: found(bin), podman: notFound("podman")}))
	out := plugintest.MustOK(t, cs, "containers_list", nil)
	if out["engine"] != "docker" || out["running"] != float64(1) || out["total"] != float64(2) {
		t.Fatalf("out = %v", out)
	}
	first := out["containers"].([]any)[0].(map[string]any)
	if first["name"] != "web" || first["id"] != "0123456789ab" || first["ports"] != "0.0.0.0:8080->80/tcp" || first["createdAt"] != float64(1795140000000) {
		t.Fatalf("first = %v", first)
	}
	plugintest.MustOK(t, cs, "containers_list", map[string]any{"running_only": true})
	calls := plugintest.Calls(t, dir)
	if strings.Join(calls[len(calls)-1], " ") != "ps --format {{json .}}" {
		t.Fatalf("running_only must drop -a: %v", calls)
	}
}

func TestFallsBackToPodmanWhenDockerDaemonIsDown(t *testing.T) {
	docker, _ := plugintest.FakeCLI(t, "docker",
		plugintest.Response{Match: "ps -a --format json", Stdout: podmanPS},
		plugintest.Response{Match: "ps", Stderr: "Cannot connect to the Docker daemon", Exit: 1},
	)
	podman := plugintest.Alias(t, docker, "podman")
	cs := plugintest.Connect(t, newServer(&app{docker: found(docker), podman: found(podman)}))
	out := plugintest.MustOK(t, cs, "containers_list", nil)
	c := out["containers"].([]any)[0].(map[string]any)
	if out["engine"] != "podman" || c["name"] != "db" || c["id"] != "fedcba987654" || c["ports"] != "15432->5432/tcp" || c["createdAt"] != float64(1763600000000) {
		t.Fatalf("out = %v", out)
	}
}

func TestExplainsWhyNoEngineWorks(t *testing.T) {
	docker, _ := plugintest.FakeCLI(t, "docker", plugintest.Response{Match: "ps", Stderr: "Cannot connect to the Docker daemon. Is the docker daemon running?", Exit: 1})
	cs := plugintest.Connect(t, newServer(&app{docker: found(docker), podman: notFound("podman")}))
	r := plugintest.Call(t, cs, "containers_list", nil)
	for _, want := range []string{"Cannot connect to the Docker daemon", "podman not found", "/usr/bin/podman"} {
		if !r.IsError || !strings.Contains(r.Text, want) {
			t.Errorf("%q missing from %+v", want, r)
		}
	}

	cs = plugintest.Connect(t, newServer(&app{docker: notFound("docker"), podman: notFound("podman")}))
	r = plugintest.Call(t, cs, "containers_list", nil)
	if !r.IsError || !strings.Contains(r.Text, "docker not found") || !strings.Contains(r.Text, "podman not found") {
		t.Fatalf("r = %+v", r)
	}
}

func TestActionsAndLogs(t *testing.T) {
	bin, dir := plugintest.FakeCLI(t, "docker",
		plugintest.Response{Match: "ps -q", Stdout: "0123456789ab\n"},
		plugintest.Response{Match: "logs", Stdout: "line1\nline2\n"},
		plugintest.Response{Match: "restart", Stdout: "web\n"},
		plugintest.Response{Match: "rm", Stdout: "web\n"},
	)
	cs := plugintest.Connect(t, newServer(&app{docker: found(bin), podman: notFound("podman")}))
	logs := plugintest.MustOK(t, cs, "container_logs", map[string]any{"id": "web", "tail": 50, "since": "10m"})
	if logs["logs"] != "line1\nline2" {
		t.Fatalf("logs = %v", logs)
	}
	plugintest.MustOK(t, cs, "container_restart", map[string]any{"id": "web"})
	plugintest.MustOK(t, cs, "container_remove", map[string]any{"id": "web", "force": true})
	var got []string
	for _, c := range plugintest.Calls(t, dir) {
		got = append(got, strings.Join(c, " "))
	}
	want := []string{"ps -q", "logs --tail 50 --since=10m web", "restart web", "rm -f web"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestRejectsOptionLikeIDs(t *testing.T) {
	bin, dir := plugintest.FakeCLI(t, "docker", plugintest.Response{Match: "", Stdout: ""})
	cs := plugintest.Connect(t, newServer(&app{docker: found(bin), podman: notFound("podman")}))
	for _, args := range []map[string]any{{"id": "--privileged"}, {"id": "a b"}, {"id": "web", "since": "--all"}} {
		if r := plugintest.Call(t, cs, "container_logs", args); !r.IsError {
			t.Errorf("%v accepted", args)
		}
	}
	if r := plugintest.Call(t, cs, "container_stop", map[string]any{"id": "-x"}); !r.IsError {
		t.Error("-x accepted")
	}
	if calls := plugintest.Calls(t, dir); len(calls) != 0 {
		t.Fatalf("CLI ran with bad input: %v", calls)
	}
}

func TestManifestMatchesTools(t *testing.T) {
	plugintest.CheckManifest(t, plugintest.Connect(t, newServer(&app{docker: notFound("docker"), podman: notFound("podman")})), "../../plugins/containers/server.json")
}
