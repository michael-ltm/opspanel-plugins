package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-ltm/opspanel-plugins/internal/cli"
	"github.com/michael-ltm/opspanel-plugins/internal/plugintest"
)

func TestMain(m *testing.M) {
	plugintest.RunFakeCLIIfRequested()
	os.Exit(m.Run())
}

const snapshotsJSON = `[
 {"time":"2026-11-19T02:00:00.123+08:00","paths":["/etc"],"hostname":"box","tags":null,"id":"aaaa1111aaaa1111","short_id":"aaaa1111"},
 {"time":"2026-11-20T02:00:00Z","paths":["/home/a","/srv"],"hostname":"box","tags":["nightly"],"id":"bbbb2222bbbb2222","short_id":"bbbb2222"}
]`

func configured(t *testing.T) {
	t.Setenv("RESTIC_REPOSITORY", "/backups/repo")
	t.Setenv("RESTIC_PASSWORD", "pw")
}

func found(p string) func() (string, error) { return func() (string, error) { return p, nil } }

func TestAnnotations(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{find: found("")}))
	plugintest.WantKinds(t, cs, map[string]string{"snapshots": "read", "stats": "read", "check": "read", "backup": "write", "restore": "destructive"})
}

func TestSnapshotsNewestFirst(t *testing.T) {
	configured(t)
	bin, dir := plugintest.FakeCLI(t, "restic", plugintest.Response{Match: "snapshots --json", Stdout: snapshotsJSON})
	cs := plugintest.Connect(t, newServer(&app{find: found(bin)}))
	out := plugintest.MustOK(t, cs, "snapshots", map[string]any{"tag": "nightly", "limit": 1})
	snaps := out["snapshots"].([]any)
	first := snaps[0].(map[string]any)
	if out["count"] != float64(2) || len(snaps) != 1 || first["id"] != "bbbb2222" || first["paths"] != "/home/a, /srv" || first["tags"] != "nightly" || first["time"] != float64(1795140000000) {
		t.Fatalf("out = %v", out)
	}
	if out["latest"] != float64(1795140000000) {
		t.Fatalf("latest = %v", out["latest"])
	}
	if c := strings.Join(plugintest.Calls(t, dir)[0], " "); c != "snapshots --json --tag nightly" {
		t.Fatalf("call = %q", c)
	}
}

func TestStatsBackupRestore(t *testing.T) {
	configured(t)
	src := t.TempDir()
	dst := t.TempDir()
	bin, dir := plugintest.FakeCLI(t, "restic",
		plugintest.Response{Match: "stats --json", Stdout: `{"total_size":3221225472,"total_file_count":1200,"snapshots_count":2}`},
		plugintest.Response{Match: "backup --json", Stdout: `{"message_type":"status","percent_done":0.5}
{"message_type":"summary","files_new":3,"files_changed":1,"files_unmodified":10,"data_added":2048,"total_files_processed":14,"total_bytes_processed":4096,"total_duration":1.5,"snapshot_id":"cccc3333cccc3333"}
`},
		plugintest.Response{Match: "restore", Stdout: "restoring snapshot bbbb2222 to " + dst + "\nSummary: Restored 14 files\n"},
	)
	cs := plugintest.Connect(t, newServer(&app{find: found(bin)}))
	st := plugintest.MustOK(t, cs, "stats", nil)
	if st["totalSize"] != float64(3221225472) || st["totalSizeHuman"] != "3.0 GiB" || st["files"] != float64(1200) {
		t.Fatalf("stats = %v", st)
	}
	b := plugintest.MustOK(t, cs, "backup", map[string]any{"paths": []string{src}, "tags": []string{"manual"}})
	if b["snapshotId"] != "cccc3333" || b["filesNew"] != float64(3) || b["dataAdded"] != float64(2048) {
		t.Fatalf("backup = %v", b)
	}
	r := plugintest.MustOK(t, cs, "restore", map[string]any{"snapshot_id": "latest", "target": dst})
	if !strings.Contains(r["output"].(string), "Restored 14 files") {
		t.Fatalf("restore = %v", r)
	}
	calls := plugintest.Calls(t, dir)
	if c := strings.Join(calls[1], " "); c != "backup --json --tag manual -- "+src {
		t.Fatalf("backup call = %q", c)
	}
	if c := strings.Join(calls[2], " "); c != "restore latest --target "+dst {
		t.Fatalf("restore call = %q", c)
	}
}

func TestRejectsBadPathsAndIDs(t *testing.T) {
	configured(t)
	bin, dir := plugintest.FakeCLI(t, "restic", plugintest.Response{Match: ""})
	cs := plugintest.Connect(t, newServer(&app{find: found(bin)}))
	missing := filepath.Join(t.TempDir(), "nope")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"backup", map[string]any{"paths": []string{"relative/dir"}}},
		{"backup", map[string]any{"paths": []string{missing}}},
		{"backup", map[string]any{"paths": []string{}}},
		{"restore", map[string]any{"snapshot_id": "--help", "target": t.TempDir()}},
		{"restore", map[string]any{"snapshot_id": "latest", "target": "rel"}},
		{"snapshots", map[string]any{"tag": "-x"}},
	} {
		if r := plugintest.Call(t, cs, c.tool, c.args); !r.IsError {
			t.Errorf("%s %v accepted", c.tool, c.args)
		}
	}
	if calls := plugintest.Calls(t, dir); len(calls) != 0 {
		t.Fatalf("restic ran: %v", calls)
	}
}

func TestExplainsMissingSetup(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{find: func() (string, error) {
		return "", &cli.NotFoundError{Tool: "restic", Checked: []string{"PATH"}, Hint: installHint}
	}}))
	if r := plugintest.Call(t, cs, "snapshots", nil); !r.IsError || !strings.Contains(r.Text, "brew install restic") {
		t.Fatalf("missing restic: %+v", r)
	}

	t.Setenv("RESTIC_REPOSITORY", "")
	cs = plugintest.Connect(t, newServer(&app{find: found("/bin/restic")}))
	if r := plugintest.Call(t, cs, "snapshots", nil); !r.IsError || !strings.Contains(r.Text, "RESTIC_REPOSITORY") {
		t.Fatalf("missing repo: %+v", r)
	}

	configured(t)
	bin, _ := plugintest.FakeCLI(t, "restic", plugintest.Response{Match: "snapshots", Stderr: `{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file\nIs there a repository at the following location?\n/backups/repo"}`, Exit: 10})
	cs = plugintest.Connect(t, newServer(&app{find: found(bin)}))
	if r := plugintest.Call(t, cs, "snapshots", nil); !r.IsError || !strings.Contains(r.Text, "restic init") || strings.Contains(r.Text, "message_type") {
		t.Fatalf("uninitialised repo: %+v", r)
	}
}

func TestManifestMatchesTools(t *testing.T) {
	plugintest.CheckManifest(t, plugintest.Connect(t, newServer(&app{find: found("")})), "../../plugins/restic/server.json")
}
