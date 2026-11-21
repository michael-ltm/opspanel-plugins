package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/michael-ltm/opspanel-plugins/internal/plugintest"
)

const logText = "\ufeff2026-11-20T02:00:00.0000000Z ##[group]Run make test\n" +
	"2026-11-20T02:00:01.0000000Z ok   pkg/a\n" +
	"2026-11-20T02:00:02.0000000Z \x1b[31;1m--- FAIL: TestB\x1b[0m\n" +
	"2026-11-20T02:00:02.1000000Z ##[error]Process completed with exit code 1.\n" +
	"2026-11-20T02:00:03.0000000Z Post job cleanup.\n" +
	"2026-11-20T02:00:04.0000000Z Cleaning up orphan processes\n" +
	"2026-11-20T02:00:05.0000000Z more cleanup\n" +
	"2026-11-20T02:00:06.0000000Z even more cleanup\n"

type fakeGitHub struct {
	*httptest.Server
	mu        sync.Mutex
	calls     []string
	rawAuth   string
	repoCalls int
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{}
	run := func(id int, repo, status, conclusion, created string) string {
		return fmt.Sprintf(`{"id":%d,"name":"CI","display_title":"fix things","run_number":%d,"head_branch":"main","event":"push","status":%q,"conclusion":%q,"html_url":"https://github.com/%s/actions/runs/%d","created_at":%q,"updated_at":%q,"actor":{"login":"octo"}}`,
			id, id, status, conclusion, repo, id, created, created)
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/raw/") {
			f.mu.Lock()
			f.rawAuth = r.Header.Get("Authorization")
			f.mu.Unlock()
			_, _ = io.WriteString(w, logText)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /user/repos":
			f.mu.Lock()
			f.repoCalls++
			f.mu.Unlock()
			if r.URL.Query().Get("sort") != "pushed" {
				t.Errorf("repos query = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `[{"full_name":"me/a"},{"full_name":"me/b"}]`)
		case "GET /repos/me/a/actions/runs":
			_, _ = io.WriteString(w, `{"total_count":2,"workflow_runs":[`+run(7, "me/a", "completed", "failure", "2026-11-20T02:00:00Z")+`,`+run(5, "me/a", "completed", "success", "2026-11-18T02:00:00Z")+`]}`)
		case "GET /repos/me/b/actions/runs":
			_, _ = io.WriteString(w, `{"total_count":1,"workflow_runs":[`+run(9, "me/b", "in_progress", "", "2026-11-19T02:00:00Z")+`]}`)
		case "GET /repos/me/a/actions/runs/7/jobs":
			_, _ = io.WriteString(w, `{"total_count":2,"jobs":[{"id":70,"name":"test","conclusion":"failure","html_url":"https://github.com/me/a/actions/runs/7/job/70","steps":[{"name":"Checkout","conclusion":"success"},{"name":"Run make test","conclusion":"failure"}]},{"id":71,"name":"lint","conclusion":"success"}]}`)
		case "GET /repos/me/a/actions/jobs/70/logs":
			w.Header().Set("Location", f.URL+"/raw/70")
			w.WriteHeader(http.StatusFound)
		case "POST /repos/me/a/actions/runs/7/rerun-failed-jobs", "POST /repos/me/a/actions/runs/7/rerun":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case "GET /repos/me/a/releases":
			_, _ = io.WriteString(w, `[{"tag_name":"v1.0.0","name":"One","draft":false,"prerelease":false,"published_at":"2026-11-01T00:00:00Z","html_url":"https://github.com/me/a/releases/tag/v1.0.0","assets":[{"id":1},{"id":2}],"author":{"login":"octo"}}]`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func TestAnnotations(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{}))
	plugintest.WantKinds(t, cs, map[string]string{"runs_list": "read", "run_logs": "read", "releases_list": "read", "run_rerun": "write"})
}

func TestRunsAcrossRecentReposWhenNoneConfigured(t *testing.T) {
	f := newFake(t)
	cs := plugintest.Connect(t, newServer(&app{token: "tok", baseURL: f.URL + "/"}))
	out := plugintest.MustOK(t, cs, "runs_list", nil)
	runs := out["runs"].([]any)
	if len(runs) != 3 || fmt.Sprint(out["repos"]) != "[me/a me/b]" {
		t.Fatalf("out = %v", out)
	}
	first, second := runs[0].(map[string]any), runs[1].(map[string]any)
	if first["repo"] != "me/a" || first["id"] != float64(7) || first["state"] != "failure" || first["actor"] != "octo" || first["createdAt"] != float64(1795140000000) {
		t.Fatalf("first = %v", first)
	}
	if second["repo"] != "me/b" || second["state"] != "in_progress" {
		t.Fatalf("second = %v", second)
	}
	// The recent-repos lookup is cached between card refreshes.
	plugintest.MustOK(t, cs, "runs_list", nil)
	if f.repoCalls != 1 {
		t.Fatalf("repo lookups = %d", f.repoCalls)
	}
}

func TestConfiguredRepoAndLogs(t *testing.T) {
	f := newFake(t)
	cs := plugintest.Connect(t, newServer(&app{token: "tok", repos: parseRepos("me/a"), baseURL: f.URL + "/"}))
	out := plugintest.MustOK(t, cs, "runs_list", map[string]any{"limit": 1})
	if runs := out["runs"].([]any); len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}

	logs := plugintest.MustOK(t, cs, "run_logs", map[string]any{"run_id": 7, "lines": 3})
	jobs := logs["failedJobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("logs = %v", logs)
	}
	job := jobs[0].(map[string]any)
	want := "--- FAIL: TestB\n##[error]Process completed with exit code 1.\nPost job cleanup."
	if job["name"] != "test" || fmt.Sprint(job["failedSteps"]) != "[Run make test]" || job["log"] != want {
		t.Fatalf("job = %#v", job)
	}
	if f.rawAuth != "" {
		t.Fatalf("token sent to the log download host: %q", f.rawAuth)
	}

	rr := plugintest.MustOK(t, cs, "run_rerun", map[string]any{"run_id": 7})
	if rr["rerun"] != "failed_jobs" {
		t.Fatalf("rerun = %v", rr)
	}
	plugintest.MustOK(t, cs, "run_rerun", map[string]any{"run_id": 7, "all_jobs": true})

	rel := plugintest.MustOK(t, cs, "releases_list", nil)["releases"].([]any)[0].(map[string]any)
	if rel["tag"] != "v1.0.0" || rel["assets"] != float64(2) || rel["publishedAt"] == nil {
		t.Fatalf("release = %v", rel)
	}
	joined := strings.Join(f.calls, "\n")
	for _, c := range []string{"POST /repos/me/a/actions/runs/7/rerun-failed-jobs", "POST /repos/me/a/actions/runs/7/rerun"} {
		if !strings.Contains(joined, c) {
			t.Errorf("missing call %s in %s", c, joined)
		}
	}
}

func TestErrors(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{}))
	if r := plugintest.Call(t, cs, "runs_list", nil); !r.IsError || !strings.Contains(r.Text, "GITHUB_TOKEN") {
		t.Fatalf("no token: %+v", r)
	}
	f := newFake(t)
	cs = plugintest.Connect(t, newServer(&app{token: "tok", repos: parseRepos("me/a, me/b"), baseURL: f.URL + "/"}))
	if r := plugintest.Call(t, cs, "run_logs", map[string]any{"run_id": 7}); !r.IsError || !strings.Contains(r.Text, "repo") {
		t.Fatalf("ambiguous repo: %+v", r)
	}
	if r := plugintest.Call(t, cs, "releases_list", map[string]any{"repo": "not a repo"}); !r.IsError {
		t.Fatalf("bad repo accepted: %+v", r)
	}
	cs = plugintest.Connect(t, newServer(&app{token: "wrong", repos: parseRepos("me/a"), baseURL: f.URL + "/"}))
	if r := plugintest.Call(t, cs, "runs_list", nil); !r.IsError || !strings.Contains(r.Text, "Bad credentials") {
		t.Fatalf("bad token: %+v", r)
	}
}

func TestParseRepos(t *testing.T) {
	if got := fmt.Sprint(parseRepos(" me/a,,me/b \n bad ")); got != "[me/a me/b]" {
		t.Fatalf("got %s", got)
	}
}

func TestManifestMatchesTools(t *testing.T) {
	plugintest.CheckManifest(t, plugintest.Connect(t, newServer(&app{})), "../../plugins/github-actions/server.json")
}
