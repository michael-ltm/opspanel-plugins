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

const statusJSON = `{
  "Version": "1.90.0",
  "BackendState": "Running",
  "MagicDNSSuffix": "tail1234.ts.net",
  "CurrentTailnet": {"Name": "example.com", "MagicDNSSuffix": "tail1234.ts.net"},
  "Health": ["some warning"],
  "Self": {"HostName": "box", "DNSName": "box.tail1234.ts.net.", "OS": "linux", "TailscaleIPs": ["100.64.0.1", "fd7a::1"], "Online": true, "LastSeen": "0001-01-01T00:00:00Z"},
  "Peer": {
    "nodekey:a": {"HostName": "Elons-Mac-mini", "DNSName": "mac-mini.tail1234.ts.net.", "OS": "macOS", "TailscaleIPs": ["100.64.0.2"], "Online": false, "LastSeen": "2026-11-20T10:00:00Z", "Relay": "lax"},
    "nodekey:b": {"HostName": "win", "DNSName": "win-vm.tail1234.ts.net.", "OS": "windows", "TailscaleIPs": ["100.64.0.3"], "Online": true, "LastSeen": "0001-01-01T00:00:00Z", "CurAddr": "192.168.1.5:41641", "ExitNode": true, "RxBytes": 10, "TxBytes": 20}
  }
}`

func TestToolsAreReadOnly(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{find: func() (string, error) { return "", nil }}))
	plugintest.WantKinds(t, cs, map[string]string{"status": "read", "peers": "read"})
}

func TestStatusAndPeers(t *testing.T) {
	bin, dir := plugintest.FakeCLI(t, "tailscale", plugintest.Response{Match: "status --json", Stdout: statusJSON})
	cs := plugintest.Connect(t, newServer(&app{find: func() (string, error) { return bin, nil }}))

	st := plugintest.MustOK(t, cs, "status", nil)
	if st["backendState"] != "Running" || st["tailnet"] != "example.com" || st["online"] != float64(1) || st["total"] != float64(2) {
		t.Fatalf("status = %v", st)
	}
	self := st["self"].(map[string]any)
	if self["name"] != "box" || self["ip"] != "100.64.0.1" {
		t.Fatalf("self = %v", self)
	}

	peers := plugintest.MustOK(t, cs, "peers", nil)["peers"].([]any)
	if len(peers) != 2 {
		t.Fatalf("peers = %v", peers)
	}
	first, second := peers[0].(map[string]any), peers[1].(map[string]any)
	// Online peers first; the machine name comes from the MagicDNS name.
	if first["name"] != "win-vm" || first["online"] != true || first["lastSeen"] != nil || first["connection"] != "direct" || first["exitNode"] != true {
		t.Fatalf("first = %v", first)
	}
	if second["name"] != "mac-mini" || second["lastSeen"] == nil || second["connection"] != "relay:lax" {
		t.Fatalf("second = %v", second)
	}

	only := plugintest.MustOK(t, cs, "peers", map[string]any{"online_only": true})["peers"].([]any)
	if len(only) != 1 {
		t.Fatalf("online_only = %v", only)
	}
	if calls := plugintest.Calls(t, dir); len(calls) != 3 || strings.Join(calls[0], " ") != "status --json" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestReportsMissingCLIAndStoppedDaemon(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{find: func() (string, error) {
		return "", &cli.NotFoundError{Tool: "tailscale", Checked: []string{"PATH", "/usr/sbin/tailscale"}, Hint: "install"}
	}}))
	r := plugintest.Call(t, cs, "status", nil)
	if !r.IsError || !strings.Contains(r.Text, "/usr/sbin/tailscale") {
		t.Fatalf("missing cli: %+v", r)
	}

	bin, _ := plugintest.FakeCLI(t, "tailscale", plugintest.Response{Match: "status", Stderr: "failed to connect to local tailscaled; it doesn't appear to be running", Exit: 1})
	cs = plugintest.Connect(t, newServer(&app{find: func() (string, error) { return bin, nil }}))
	r = plugintest.Call(t, cs, "peers", nil)
	if !r.IsError || !strings.Contains(r.Text, "tailscaled") {
		t.Fatalf("stopped daemon: %+v", r)
	}
}

func TestSpecCoversEveryPlatform(t *testing.T) {
	for _, goos := range []string{"darwin", "windows", "linux"} {
		if len(spec.Known[goos]) == 0 {
			t.Errorf("no known locations for %s", goos)
		}
	}
}

func TestManifestMatchesTools(t *testing.T) {
	plugintest.CheckManifest(t, plugintest.Connect(t, newServer(&app{find: func() (string, error) { return "", nil }})), "../../plugins/tailscale/server.json")
}
