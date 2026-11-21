// Command uptime is an OpsPanel plugin that checks whether URLs answer and
// TCP ports accept connections, and how long TLS certificates have left.
// It checks on demand; schedule it with a panel routine.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-ltm/opspanel-plugins/internal/plugin"
)

var version = "dev"

const (
	timeout     = 10 * time.Second
	maxTargets  = 50
	parallelism = 8
	// expiringDays is when a certificate counts as expiring soon.
	expiringDays = 14
)

func main() {
	plugin.Serve(newServer(&app{targets: os.Getenv("TARGETS")}), "uptime", version)
}

type app struct {
	targets string
	// roots replaces the system roots (tests).
	roots *x509.CertPool
}

type target struct {
	raw, kind, url, addr string
}

func parseTargets(s string) []target {
	var out []target
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		out = append(out, parseTarget(raw))
	}
	return out
}

func parseTarget(raw string) target {
	t := target{raw: raw, kind: "invalid"}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			t.kind, t.url = "http", raw
		}
		return t
	}
	if host, port, err := net.SplitHostPort(raw); err == nil && host != "" {
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			t.kind, t.addr = "tcp", raw
		}
		return t
	}
	if !strings.ContainsAny(raw, "/?#@") {
		t.kind, t.url = "http", "https://"+raw
	}
	return t
}

// Result is one target's check.
type Result struct {
	Target       string `json:"target"`
	Kind         string `json:"kind" jsonschema:"http, tcp or invalid"`
	OK           bool   `json:"ok" jsonschema:"HTTP status below 400, or the TCP port accepted a connection"`
	Status       int    `json:"status,omitempty" jsonschema:"HTTP status code"`
	LatencyMs    int64  `json:"latencyMs"`
	CertDaysLeft *int   `json:"certDaysLeft" jsonschema:"days until the TLS certificate expires (negative: expired)"`
	CertExpires  *int64 `json:"certExpires" jsonschema:"certificate expiry, unix ms"`
	Error        string `json:"error"`
}

// CheckIn optionally overrides the configured targets.
type CheckIn struct {
	Targets []string `json:"targets,omitempty" jsonschema:"URLs or host:port to check instead of the configured TARGETS"`
}

// CheckOut summarises a round of checks.
type CheckOut struct {
	CheckedAt    int64    `json:"checkedAt"`
	Total        int      `json:"total"`
	Up           int      `json:"up"`
	Down         int      `json:"down"`
	ExpiringSoon int      `json:"expiringSoon" jsonschema:"certificates with fewer than 14 days left"`
	Targets      []Result `json:"targets"`
}

func (a *app) checkHTTP(ctx context.Context, t target) Result {
	r := Result{Target: t.raw, Kind: t.kind}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: a.roots}
	client := &http.Client{Transport: tr, Timeout: timeout}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	req.Header.Set("User-Agent", "opspanel-uptime/"+version)
	start := time.Now()
	resp, err := client.Do(req)
	r.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
		var cv *tls.CertificateVerificationError
		if errors.As(err, &cv) && len(cv.UnverifiedCertificates) > 0 {
			setCert(&r, cv.UnverifiedCertificates[0])
		}
		return r
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	r.Status = resp.StatusCode
	r.OK = resp.StatusCode < 400
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		setCert(&r, resp.TLS.PeerCertificates[0])
	}
	if !r.OK {
		r.Error = resp.Status
	}
	return r
}

func setCert(r *Result, c *x509.Certificate) {
	days := int(math.Floor(time.Until(c.NotAfter).Hours() / 24))
	ms := c.NotAfter.UnixMilli()
	r.CertDaysLeft, r.CertExpires = &days, &ms
}

func checkTCP(ctx context.Context, t target) Result {
	r := Result{Target: t.raw, Kind: t.kind}
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	r.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	_ = conn.Close()
	r.OK = true
	return r
}

func (a *app) check(ctx context.Context, ts []target) CheckOut {
	out := CheckOut{CheckedAt: time.Now().UnixMilli(), Total: len(ts), Targets: make([]Result, len(ts))}
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			switch t.kind {
			case "http":
				out.Targets[i] = a.checkHTTP(ctx, t)
			case "tcp":
				out.Targets[i] = checkTCP(ctx, t)
			default:
				out.Targets[i] = Result{Target: t.raw, Kind: t.kind, Error: "not an http(s) URL or host:port"}
			}
		})
	}
	wg.Wait()
	for _, r := range out.Targets {
		if r.OK {
			out.Up++
		} else {
			out.Down++
		}
		if r.CertDaysLeft != nil && *r.CertDaysLeft < expiringDays {
			out.ExpiringSoon++
		}
	}
	return out
}

func newServer(a *app) *mcp.Server {
	s := plugin.New("uptime", version)
	mcp.AddTool(s, &mcp.Tool{
		Name: "check",
		Description: "Check the configured targets now (or the ones passed in): HTTP(S) URLs report status code, response time " +
			"and TLS certificate days left; host:port targets report whether a TCP connection succeeds. A bare host name is checked as https://host.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CheckIn) (*mcp.CallToolResult, CheckOut, error) {
		ts := parseTargets(a.targets)
		if len(in.Targets) > 0 {
			ts = parseTargets(strings.Join(in.Targets, ","))
		}
		if len(ts) == 0 {
			return nil, CheckOut{}, errors.New("no targets: set TARGETS in the plugin settings (comma-separated URLs or host:port), or pass targets")
		}
		if len(ts) > maxTargets {
			return nil, CheckOut{}, fmt.Errorf("%d targets; at most %d per check", len(ts), maxTargets)
		}
		return nil, a.check(ctx, ts), nil
	})
	return s
}
