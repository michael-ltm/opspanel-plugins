// Command github-actions is an OpsPanel plugin that shows GitHub Actions
// runs, the failing part of their logs and releases, and re-runs workflows.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-ltm/opspanel-plugins/internal/plugin"
)

var version = "dev"

func main() {
	a := &app{token: strings.TrimSpace(os.Getenv("GITHUB_TOKEN")), repos: parseRepos(os.Getenv("GITHUB_REPO"))}
	plugin.Serve(newServer(a), "github-actions", version)
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// parseRepos reads GITHUB_REPO: one or more owner/name, comma separated.
func parseRepos(s string) []string {
	var out []string
	for _, r := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if repoRE.MatchString(r) {
			out = append(out, r)
		}
	}
	return out
}

type app struct {
	token   string
	repos   []string
	baseURL string // tests

	once      sync.Once
	gh        *github.Client
	ghErr     error
	mu        sync.Mutex
	recent    []string
	recentAt  time.Time
	logClient *http.Client
}

func (a *app) client() (*github.Client, error) {
	if a.token == "" {
		return nil, errors.New("GITHUB_TOKEN is not set: add a token in the plugin settings (fine-grained: Actions read, plus Actions write to re-run; Contents read for releases)")
	}
	a.once.Do(func() {
		opts := []github.ClientOptionsFunc{github.WithAuthToken(a.token), github.WithTimeout(30 * time.Second), github.WithUserAgent("opspanel-github-actions/" + version)}
		if a.baseURL != "" {
			opts = append(opts, github.WithURLs(&a.baseURL, &a.baseURL))
		}
		a.gh, a.ghErr = github.NewClient(opts...)
		a.logClient = &http.Client{Timeout: 60 * time.Second}
	})
	return a.gh, a.ghErr
}

func split(repo string) (string, string) {
	owner, name, _ := strings.Cut(repo, "/")
	return owner, name
}

// defaultRepos is GITHUB_REPO, or else the token owner's five most recently
// pushed repositories (cached for ten minutes).
func (a *app) defaultRepos(ctx context.Context, gh *github.Client) ([]string, error) {
	if len(a.repos) > 0 {
		return a.repos, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recent != nil && time.Since(a.recentAt) < 10*time.Minute {
		return a.recent, nil
	}
	list, _, err := gh.Repositories.ListByAuthenticatedUser(ctx, &github.RepositoryListByAuthenticatedUserOptions{Sort: "pushed", ListOptions: github.ListOptions{PerPage: 5}})
	if err != nil {
		return nil, err
	}
	repos := []string{}
	for _, r := range list {
		repos = append(repos, r.GetFullName())
	}
	a.recent, a.recentAt = repos, time.Now()
	return repos, nil
}

// oneRepo picks the repository for a tool that works on a single one.
func (a *app) oneRepo(repo string) (string, error) {
	if repo != "" {
		if !repoRE.MatchString(repo) {
			return "", fmt.Errorf("repo must be owner/name, not %q", repo)
		}
		return repo, nil
	}
	if len(a.repos) == 1 {
		return a.repos[0], nil
	}
	return "", errors.New("pass repo (owner/name): GITHUB_REPO does not name exactly one repository")
}

// Run is one workflow run.
type Run struct {
	ID         int64  `json:"id"`
	Repo       string `json:"repo"`
	Workflow   string `json:"workflow"`
	Title      string `json:"title"`
	Number     int    `json:"number"`
	Branch     string `json:"branch"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state" jsonschema:"the conclusion once completed, else the status (queued, in_progress, …)"`
	Actor      string `json:"actor"`
	URL        string `json:"url"`
	CreatedAt  *int64 `json:"createdAt" jsonschema:"unix ms"`
	UpdatedAt  *int64 `json:"updatedAt" jsonschema:"unix ms"`
}

// RunsIn filters runs.
type RunsIn struct {
	Repo   string `json:"repo,omitempty" jsonschema:"owner/name; default: GITHUB_REPO, or your recently pushed repositories"`
	Branch string `json:"branch,omitempty"`
	Status string `json:"status,omitempty" jsonschema:"e.g. failure, success, in_progress, queued, completed"`
	Limit  int    `json:"limit,omitempty" jsonschema:"newest first, at most this many (default 20, max 100)"`
}

// RunsOut lists runs, newest first.
type RunsOut struct {
	Repos    []string `json:"repos"`
	Runs     []Run    `json:"runs"`
	Warnings []string `json:"warnings,omitempty" jsonschema:"repositories that could not be read"`
}

func (a *app) runs(ctx context.Context, in RunsIn) (RunsOut, error) {
	gh, err := a.client()
	if err != nil {
		return RunsOut{}, err
	}
	limit := min(cmp.Or(in.Limit, 20), 100)
	repos := []string{in.Repo}
	if in.Repo == "" {
		if repos, err = a.defaultRepos(ctx, gh); err != nil {
			return RunsOut{}, err
		}
	} else if !repoRE.MatchString(in.Repo) {
		return RunsOut{}, fmt.Errorf("repo must be owner/name, not %q", in.Repo)
	}
	type result struct {
		runs []Run
		err  error
	}
	results := make([]result, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Go(func() {
			owner, name := split(repo)
			list, _, err := gh.Actions.ListRepositoryWorkflowRuns(ctx, owner, name, &github.ListWorkflowRunsOptions{
				Branch: in.Branch, Status: in.Status, ListOptions: github.ListOptions{PerPage: limit},
			})
			if err != nil {
				results[i].err = fmt.Errorf("%s: %w", repo, err)
				return
			}
			for _, r := range list.WorkflowRuns {
				results[i].runs = append(results[i].runs, toRun(repo, r))
			}
		})
	}
	wg.Wait()
	out := RunsOut{Repos: repos, Runs: []Run{}}
	var errs []error
	for _, r := range results {
		out.Runs = append(out.Runs, r.runs...)
		if r.err != nil {
			errs = append(errs, r.err)
			out.Warnings = append(out.Warnings, r.err.Error())
		}
	}
	if len(errs) == len(repos) && len(errs) > 0 {
		return RunsOut{}, errors.Join(errs...)
	}
	slices.SortStableFunc(out.Runs, func(x, y Run) int { return cmp.Compare(deref(y.CreatedAt), deref(x.CreatedAt)) })
	if len(out.Runs) > limit {
		out.Runs = out.Runs[:limit]
	}
	return out, nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func toRun(repo string, r *github.WorkflowRun) Run {
	run := Run{
		ID: r.GetID(), Repo: repo, Workflow: r.GetName(), Title: r.GetDisplayTitle(), Number: r.GetRunNumber(),
		Branch: r.GetHeadBranch(), Event: r.GetEvent(), Status: r.GetStatus(), Conclusion: r.GetConclusion(),
		Actor: r.GetActor().GetLogin(), URL: r.GetHTMLURL(),
		CreatedAt: plugin.Millis(r.GetCreatedAt().Time), UpdatedAt: plugin.Millis(r.GetUpdatedAt().Time),
	}
	run.State = run.Status
	if run.Status == "completed" && run.Conclusion != "" {
		run.State = run.Conclusion
	}
	return run
}

// LogsIn picks a run.
type LogsIn struct {
	Repo  string `json:"repo,omitempty" jsonschema:"owner/name; may be left out when GITHUB_REPO names one repository"`
	RunID int64  `json:"run_id"`
	Lines int    `json:"lines,omitempty" jsonschema:"log lines per failed job, ending at the last error (default 80, max 400)"`
}

// FailedJob is the failing part of one job.
type FailedJob struct {
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	FailedSteps []string `json:"failedSteps"`
	Log         string   `json:"log"`
}

// LogsOut holds the failed jobs of a run.
type LogsOut struct {
	Repo       string      `json:"repo"`
	RunID      int64       `json:"runId"`
	FailedJobs []FailedJob `json:"failedJobs"`
	Note       string      `json:"note,omitempty"`
}

var (
	stampRE = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z `)
	ansiRE  = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

// failingTail returns up to n log lines ending just after the last
// ##[error] marker (or the last n lines), without timestamps.
func failingTail(log string, n int) string {
	lines := strings.Split(strings.TrimRight(strings.TrimPrefix(log, "\ufeff"), "\r\n"), "\n")
	for i, l := range lines {
		lines[i] = ansiRE.ReplaceAllString(stampRE.ReplaceAllString(strings.TrimRight(l, "\r"), ""), "")
	}
	end := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "##[error]") {
			end = min(i+2, len(lines))
			break
		}
	}
	start := max(0, end-n)
	out := strings.Join(lines[start:end], "\n")
	if len(out) > 16<<10 {
		out = "…" + out[len(out)-16<<10:]
	}
	return out
}

func (a *app) logs(ctx context.Context, in LogsIn) (LogsOut, error) {
	gh, err := a.client()
	if err != nil {
		return LogsOut{}, err
	}
	repo, err := a.oneRepo(in.Repo)
	if err != nil {
		return LogsOut{}, err
	}
	n := min(cmp.Or(in.Lines, 80), 400)
	owner, name := split(repo)
	jobs, _, err := gh.Actions.ListWorkflowJobs(ctx, owner, name, in.RunID, &github.ListWorkflowJobsOptions{Filter: "latest", ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return LogsOut{}, err
	}
	out := LogsOut{Repo: repo, RunID: in.RunID, FailedJobs: []FailedJob{}}
	for _, j := range jobs.Jobs {
		if c := j.GetConclusion(); c != "failure" && c != "timed_out" {
			continue
		}
		if len(out.FailedJobs) == 3 {
			out.Note = "only the first 3 failed jobs are shown"
			break
		}
		fj := FailedJob{Name: j.GetName(), URL: j.GetHTMLURL(), FailedSteps: []string{}}
		for _, s := range j.Steps {
			if c := s.GetConclusion(); c == "failure" || c == "timed_out" {
				fj.FailedSteps = append(fj.FailedSteps, s.GetName())
			}
		}
		text, err := a.jobLog(ctx, gh, owner, name, j.GetID())
		if err != nil {
			fj.Log = "(log unavailable: " + err.Error() + ")"
		} else {
			fj.Log = failingTail(text, n)
		}
		out.FailedJobs = append(out.FailedJobs, fj)
	}
	if len(out.FailedJobs) == 0 {
		out.Note = "no failed jobs in this run"
	}
	return out, nil
}

// jobLog downloads a job's log from the short-lived URL GitHub redirects to.
// That URL is pre-signed, so the token is not sent along.
func (a *app) jobLog(ctx context.Context, gh *github.Client, owner, name string, jobID int64) (string, error) {
	u, _, err := gh.Actions.GetWorkflowJobLogs(ctx, owner, name, jobID, 2)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := a.logClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("log download: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return string(b), err
}

// RerunIn picks a run to re-run.
type RerunIn struct {
	Repo    string `json:"repo,omitempty" jsonschema:"owner/name; may be left out when GITHUB_REPO names one repository"`
	RunID   int64  `json:"run_id"`
	AllJobs bool   `json:"all_jobs,omitempty" jsonschema:"re-run every job instead of only the failed ones"`
}

// RerunOut confirms the request.
type RerunOut struct {
	Repo  string `json:"repo"`
	RunID int64  `json:"runId"`
	Rerun string `json:"rerun" jsonschema:"failed_jobs or all"`
}

// ReleasesIn picks a repository.
type ReleasesIn struct {
	Repo  string `json:"repo,omitempty" jsonschema:"owner/name; may be left out when GITHUB_REPO names one repository"`
	Limit int    `json:"limit,omitempty" jsonschema:"default 10, max 50"`
}

// Release is one release.
type Release struct {
	Tag         string `json:"tag"`
	Name        string `json:"name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt *int64 `json:"publishedAt" jsonschema:"unix ms"`
	URL         string `json:"url"`
	Assets      int    `json:"assets"`
	Author      string `json:"author"`
}

// ReleasesOut lists releases, newest first.
type ReleasesOut struct {
	Repo     string    `json:"repo"`
	Releases []Release `json:"releases"`
}

func newServer(a *app) *mcp.Server {
	s := plugin.New("github-actions", version)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "runs_list",
		Description: "Recent GitHub Actions workflow runs, newest first, across GITHUB_REPO (or your recently pushed repositories), with workflow, branch, state and link.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RunsIn) (*mcp.CallToolResult, RunsOut, error) {
		out, err := a.runs(ctx, in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "run_logs",
		Description: "Why a workflow run failed: its failed jobs and steps, and the end of each failed job's log around the last error.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in LogsIn) (*mcp.CallToolResult, LogsOut, error) {
		out, err := a.logs(ctx, in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "run_rerun",
		Description: "Re-run a workflow run's failed jobs (or all jobs).",
		Annotations: plugin.Mutating(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RerunIn) (*mcp.CallToolResult, RerunOut, error) {
		gh, err := a.client()
		if err != nil {
			return nil, RerunOut{}, err
		}
		repo, err := a.oneRepo(in.Repo)
		if err != nil {
			return nil, RerunOut{}, err
		}
		owner, name := split(repo)
		out := RerunOut{Repo: repo, RunID: in.RunID, Rerun: "failed_jobs"}
		if in.AllJobs {
			out.Rerun = "all"
			_, err = gh.Actions.RerunWorkflowByID(ctx, owner, name, in.RunID)
		} else {
			_, err = gh.Actions.RerunFailedJobsByID(ctx, owner, name, in.RunID)
		}
		if err != nil {
			return nil, RerunOut{}, err
		}
		return nil, out, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "releases_list",
		Description: "A repository's releases, newest first, with tag, draft / prerelease flags, publish time and asset count.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReleasesIn) (*mcp.CallToolResult, ReleasesOut, error) {
		gh, err := a.client()
		if err != nil {
			return nil, ReleasesOut{}, err
		}
		repo, err := a.oneRepo(in.Repo)
		if err != nil {
			return nil, ReleasesOut{}, err
		}
		owner, name := split(repo)
		list, _, err := gh.Repositories.ListReleases(ctx, owner, name, &github.ListOptions{PerPage: min(cmp.Or(in.Limit, 10), 50)})
		if err != nil {
			return nil, ReleasesOut{}, err
		}
		out := ReleasesOut{Repo: repo, Releases: []Release{}}
		for _, r := range list {
			out.Releases = append(out.Releases, Release{
				Tag: r.GetTagName(), Name: r.GetName(), Draft: r.GetDraft(), Prerelease: r.GetPrerelease(),
				PublishedAt: plugin.Millis(r.GetPublishedAt().Time), URL: r.GetHTMLURL(), Assets: len(r.Assets), Author: r.GetAuthor().GetLogin(),
			})
		}
		return nil, out, nil
	})
	return s
}
