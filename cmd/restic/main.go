// Command restic is an OpsPanel plugin that lists, checks, creates and
// restores restic backups through the restic CLI. The repository and its
// password come from the plugin settings (RESTIC_REPOSITORY,
// RESTIC_PASSWORD, plus the storage backend's own variables).
package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-ltm/opspanel-plugins/internal/cli"
	"github.com/michael-ltm/opspanel-plugins/internal/plugin"
)

var version = "dev"

const installHint = "Install restic: Linux `apt install restic`, `dnf install restic` or `pacman -S restic`; macOS `brew install restic`; Windows `winget install restic.restic` or `scoop install restic`"

func spec() cli.Spec {
	known := map[string][]string{
		"linux":   {"/usr/bin/restic", "/usr/local/bin/restic", "/snap/bin/restic"},
		"darwin":  {"/opt/homebrew/bin/restic", "/usr/local/bin/restic"},
		"windows": {`C:\ProgramData\chocolatey\bin\restic.exe`},
	}
	if home, err := os.UserHomeDir(); err == nil {
		known["linux"] = append(known["linux"], filepath.Join(home, ".local", "bin", "restic"))
		known["windows"] = append(known["windows"],
			filepath.Join(home, "AppData", "Local", "Microsoft", "WinGet", "Links", "restic.exe"),
			filepath.Join(home, "scoop", "shims", "restic.exe"))
	}
	return cli.Spec{Name: "restic", Known: known, Hint: installHint}
}

func main() {
	plugin.Serve(newServer(&app{find: spec().Find}), "restic", version)
}

type app struct {
	find func() (string, error)
}

var (
	idRE     = regexp.MustCompile(`^(latest|[0-9a-f]{8,64})$`)
	subsetRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?%$|^[0-9]+/[0-9]+$`)
	tagRE    = regexp.MustCompile(`^[^-\s,][^\s,]{0,63}$`)
)

// restic runs restic with the configured repository.
func (a *app) restic(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	bin, err := a.find()
	if err != nil {
		return nil, err
	}
	if os.Getenv("RESTIC_REPOSITORY") == "" && os.Getenv("RESTIC_REPOSITORY_FILE") == "" {
		return nil, errors.New("RESTIC_REPOSITORY is not set: add the repository (a path, sftp:, s3:, b2:, rest: … URL) in the plugin settings")
	}
	if os.Getenv("RESTIC_PASSWORD") == "" && os.Getenv("RESTIC_PASSWORD_FILE") == "" && os.Getenv("RESTIC_PASSWORD_COMMAND") == "" {
		return nil, errors.New("RESTIC_PASSWORD is not set: add the repository password in the plugin settings")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	out, err := cli.Run(ctx, bin, nil, args...)
	var ee *cli.ExitError
	if errors.As(err, &ee) {
		ee.Output = plainError(ee.Output)
	}
	if err != nil && strings.Contains(err.Error(), "Is there a repository at the following location?") {
		err = fmt.Errorf("%w (create the repository once with `restic init`)", err)
	}
	return out, err
}

// plainError turns restic's JSON error lines (printed with --json) back
// into their messages.
func plainError(out string) string {
	var msgs []string
	for line := range strings.SplitSeq(out, "\n") {
		var m struct {
			MessageType string `json:"message_type"`
			Message     string `json:"message"`
			Error       *struct {
				Message string `json:"message"`
			} `json:"error"`
			Item string `json:"item"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &m) != nil || m.MessageType == "" {
			msgs = append(msgs, line)
			continue
		}
		switch {
		case m.Message != "":
			msgs = append(msgs, m.Message)
		case m.Error != nil:
			msgs = append(msgs, strings.TrimSpace(m.Item+": "+m.Error.Message))
		}
	}
	return strings.TrimSpace(strings.Join(msgs, "\n"))
}

func checkTag(field, v string) error {
	if v != "" && !tagRE.MatchString(v) {
		return fmt.Errorf("invalid %s %q", field, v)
	}
	return nil
}

func checkDir(field, p string, mustExist bool) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s must be an absolute path: %q", field, p)
	}
	if mustExist {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("%s %q: %w", field, p, err)
		}
	}
	return nil
}

type rawSnapshot struct {
	Time     time.Time `json:"time"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	ShortID  string    `json:"short_id"`
	ID       string    `json:"id"`
}

// Snapshot is one snapshot as the panel shows it.
type Snapshot struct {
	ID       string   `json:"id"`
	Time     *int64   `json:"time" jsonschema:"unix ms"`
	Hostname string   `json:"hostname"`
	Paths    string   `json:"paths"`
	Tags     string   `json:"tags"`
	PathList []string `json:"pathList"`
}

// SnapshotsIn filters snapshots.
type SnapshotsIn struct {
	Host  string `json:"host,omitempty" jsonschema:"only snapshots from this host"`
	Tag   string `json:"tag,omitempty" jsonschema:"only snapshots with this tag"`
	Path  string `json:"path,omitempty" jsonschema:"only snapshots that include this absolute path"`
	Limit int    `json:"limit,omitempty" jsonschema:"newest first, at most this many (default 50)"`
}

// SnapshotsOut lists snapshots, newest first.
type SnapshotsOut struct {
	Count     int        `json:"count" jsonschema:"matching snapshots in the repository"`
	Latest    *int64     `json:"latest" jsonschema:"newest snapshot time, unix ms"`
	Snapshots []Snapshot `json:"snapshots"`
}

// StatsIn picks the statistic.
type StatsIn struct {
	Mode string `json:"mode,omitempty" jsonschema:"restore-size (default: what a restore of every snapshot would write) or raw-data (what the repository stores)"`
}

// StatsOut is repository size.
type StatsOut struct {
	Mode           string  `json:"mode"`
	TotalSize      int64   `json:"totalSize"`
	TotalSizeHuman string  `json:"totalSizeHuman"`
	Files          int64   `json:"files"`
	Snapshots      int64   `json:"snapshots"`
	Compression    float64 `json:"compressionRatio,omitempty"`
}

// CheckIn sets how deep to check.
type CheckIn struct {
	ReadDataSubset string `json:"read_data_subset,omitempty" jsonschema:"also read and verify this share of the data, e.g. 5% or 1/10 (slower)"`
}

// CheckOut is the check's verdict.
type CheckOut struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

// BackupIn says what to back up.
type BackupIn struct {
	Paths   []string `json:"paths" jsonschema:"absolute paths to back up"`
	Tags    []string `json:"tags,omitempty"`
	Exclude []string `json:"exclude,omitempty" jsonschema:"exclude patterns (restic --exclude)"`
}

// BackupOut summarises a backup.
type BackupOut struct {
	SnapshotID     string  `json:"snapshotId"`
	FilesNew       int64   `json:"filesNew"`
	FilesChanged   int64   `json:"filesChanged"`
	FilesUnchanged int64   `json:"filesUnmodified"`
	DataAdded      int64   `json:"dataAdded" jsonschema:"bytes added to the repository"`
	BytesProcessed int64   `json:"bytesProcessed"`
	DurationSec    float64 `json:"durationSeconds"`
}

// RestoreIn says what to restore where.
type RestoreIn struct {
	SnapshotID string `json:"snapshot_id" jsonschema:"snapshot id or latest"`
	Target     string `json:"target" jsonschema:"absolute directory to restore into; existing files there may be overwritten"`
	Include    string `json:"include,omitempty" jsonschema:"only restore this path inside the snapshot"`
}

// RestoreOut is restic's report.
type RestoreOut struct {
	Target string `json:"target"`
	Output string `json:"output"`
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (a *app) snapshots(ctx context.Context, in SnapshotsIn) (SnapshotsOut, error) {
	for _, f := range [][2]string{{"host", in.Host}, {"tag", in.Tag}} {
		if err := checkTag(f[0], f[1]); err != nil {
			return SnapshotsOut{}, err
		}
	}
	args := []string{"snapshots", "--json"}
	if in.Host != "" {
		args = append(args, "--host", in.Host)
	}
	if in.Tag != "" {
		args = append(args, "--tag", in.Tag)
	}
	if in.Path != "" {
		if err := checkDir("path", in.Path, false); err != nil {
			return SnapshotsOut{}, err
		}
		args = append(args, "--path", in.Path)
	}
	out, err := a.restic(ctx, 90*time.Second, args...)
	if err != nil {
		return SnapshotsOut{}, err
	}
	var raw []rawSnapshot
	if err := json.Unmarshal(out, &raw); err != nil {
		return SnapshotsOut{}, fmt.Errorf("unexpected restic snapshots output: %w", err)
	}
	slices.SortFunc(raw, func(x, y rawSnapshot) int {
		return y.Time.Compare(x.Time)
	})
	limit := cmp.Or(in.Limit, 50)
	res := SnapshotsOut{Count: len(raw), Snapshots: []Snapshot{}}
	for i, r := range raw {
		if i == 0 {
			res.Latest = plugin.Millis(r.Time)
		}
		if i >= limit {
			break
		}
		id := cmp.Or(r.ShortID, short(r.ID))
		paths := r.Paths
		if paths == nil {
			paths = []string{}
		}
		res.Snapshots = append(res.Snapshots, Snapshot{ID: id, Time: plugin.Millis(r.Time), Hostname: r.Hostname, Paths: strings.Join(paths, ", "), PathList: paths, Tags: strings.Join(r.Tags, ", ")})
	}
	return res, nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (a *app) backup(ctx context.Context, in BackupIn) (BackupOut, error) {
	if len(in.Paths) == 0 {
		return BackupOut{}, errors.New("paths is empty")
	}
	for _, p := range in.Paths {
		if err := checkDir("path", p, true); err != nil {
			return BackupOut{}, err
		}
	}
	args := []string{"backup", "--json"}
	for _, t := range in.Tags {
		if err := checkTag("tag", t); err != nil {
			return BackupOut{}, err
		}
		args = append(args, "--tag", t)
	}
	for _, e := range in.Exclude {
		if e == "" || strings.HasPrefix(e, "-") {
			return BackupOut{}, fmt.Errorf("invalid exclude %q", e)
		}
		args = append(args, "--exclude="+e)
	}
	args = append(append(args, "--"), in.Paths...)
	out, err := a.restic(ctx, 0, args...)
	if err != nil {
		return BackupOut{}, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var m struct {
			MessageType    string  `json:"message_type"`
			FilesNew       int64   `json:"files_new"`
			FilesChanged   int64   `json:"files_changed"`
			FilesUnmod     int64   `json:"files_unmodified"`
			DataAdded      int64   `json:"data_added"`
			BytesProcessed int64   `json:"total_bytes_processed"`
			Duration       float64 `json:"total_duration"`
			SnapshotID     string  `json:"snapshot_id"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.MessageType == "summary" {
			return BackupOut{SnapshotID: short(m.SnapshotID), FilesNew: m.FilesNew, FilesChanged: m.FilesChanged, FilesUnchanged: m.FilesUnmod,
				DataAdded: m.DataAdded, BytesProcessed: m.BytesProcessed, DurationSec: m.Duration}, nil
		}
	}
	return BackupOut{}, errors.New("restic backup finished without a summary: " + cli.Tail(string(out), 500))
}

func newServer(a *app) *mcp.Server {
	s := plugin.New("restic", version)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "snapshots",
		Description: "Snapshots in the restic repository, newest first, with host, paths and tags.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SnapshotsIn) (*mcp.CallToolResult, SnapshotsOut, error) {
		out, err := a.snapshots(ctx, in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "stats",
		Description: "Repository size: what restoring every snapshot would write (restore-size) or what the repository stores (raw-data).",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in StatsIn) (*mcp.CallToolResult, StatsOut, error) {
		mode := cmp.Or(in.Mode, "restore-size")
		if mode != "restore-size" && mode != "raw-data" {
			return nil, StatsOut{}, fmt.Errorf("mode must be restore-size or raw-data, not %q", mode)
		}
		out, err := a.restic(ctx, 0, "stats", "--json", "--mode", mode)
		if err != nil {
			return nil, StatsOut{}, err
		}
		var r struct {
			TotalSize        int64   `json:"total_size"`
			TotalFileCount   int64   `json:"total_file_count"`
			SnapshotsCount   int64   `json:"snapshots_count"`
			CompressionRatio float64 `json:"compression_ratio"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			return nil, StatsOut{}, fmt.Errorf("unexpected restic stats output: %w", err)
		}
		return nil, StatsOut{Mode: mode, TotalSize: r.TotalSize, TotalSizeHuman: human(r.TotalSize), Files: r.TotalFileCount, Snapshots: r.SnapshotsCount, Compression: r.CompressionRatio}, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "check",
		Description: "Verify the repository's structure (restic check). With read_data_subset it also reads part of the data, which takes longer. Changes no data.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CheckIn) (*mcp.CallToolResult, CheckOut, error) {
		args := []string{"check"}
		if in.ReadDataSubset != "" {
			if !subsetRE.MatchString(in.ReadDataSubset) {
				return nil, CheckOut{}, fmt.Errorf("read_data_subset must look like 5%% or 1/10, not %q", in.ReadDataSubset)
			}
			args = append(args, "--read-data-subset="+in.ReadDataSubset)
		}
		out, err := a.restic(ctx, 0, args...)
		if err != nil {
			var ee *cli.ExitError
			if errors.As(err, &ee) {
				return nil, CheckOut{OK: false, Output: ee.Output}, nil
			}
			return nil, CheckOut{}, err
		}
		return nil, CheckOut{OK: true, Output: cli.Tail(string(out), 4000)}, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "backup",
		Description: "Back up the given absolute paths into the repository and return the new snapshot. It runs until done, " +
			"so very large first backups are better started from a terminal.",
		Annotations: plugin.Mutating(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in BackupIn) (*mcp.CallToolResult, BackupOut, error) {
		out, err := a.backup(ctx, in)
		return nil, out, err
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "restore",
		Description: "Restore a snapshot (or one path inside it) into a target directory. Files already in the target may be overwritten.",
		Annotations: plugin.Destructive(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RestoreIn) (*mcp.CallToolResult, RestoreOut, error) {
		if !idRE.MatchString(in.SnapshotID) {
			return nil, RestoreOut{}, fmt.Errorf("snapshot_id must be a snapshot id or latest, not %q", in.SnapshotID)
		}
		if err := checkDir("target", in.Target, false); err != nil {
			return nil, RestoreOut{}, err
		}
		args := []string{"restore", in.SnapshotID, "--target", in.Target}
		if in.Include != "" {
			if !strings.HasPrefix(in.Include, "/") && !filepath.IsAbs(in.Include) {
				return nil, RestoreOut{}, fmt.Errorf("include must be an absolute path inside the snapshot: %q", in.Include)
			}
			args = append(args, "--include="+in.Include)
		}
		out, err := a.restic(ctx, 0, args...)
		if err != nil {
			return nil, RestoreOut{}, err
		}
		return nil, RestoreOut{Target: in.Target, Output: cli.Tail(string(out), 4000)}, nil
	})
	return s
}
