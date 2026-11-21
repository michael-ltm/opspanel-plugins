package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michael-ltm/opspanel-plugins/internal/plugintest"
)

func TestParseTargets(t *testing.T) {
	got := parseTargets(" https://a.example/health,b.example:5432\n c.example ,ftp://x , ,")
	want := []target{
		{raw: "https://a.example/health", kind: "http", url: "https://a.example/health"},
		{raw: "b.example:5432", kind: "tcp", addr: "b.example:5432"},
		{raw: "c.example", kind: "http", url: "https://c.example"},
		{raw: "ftp://x", kind: "invalid"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCheckHTTPTCPAndCertificates(t *testing.T) {
	okSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer okSrv.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := ln.Addr().String()
	defer ln.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedAddr := closed.Addr().String()
	closed.Close()

	pool := x509.NewCertPool()
	pool.AddCert(okSrv.Certificate())
	a := &app{targets: okSrv.URL + "," + tcpAddr, roots: pool}
	cs := plugintest.Connect(t, newServer(a))
	plugintest.WantKinds(t, cs, map[string]string{"check": "read"})

	out := plugintest.MustOK(t, cs, "check", nil)
	if out["total"] != float64(2) || out["up"] != float64(2) || out["down"] != float64(0) {
		t.Fatalf("out = %v", out)
	}
	rows := out["targets"].([]any)
	web := rows[0].(map[string]any)
	if web["ok"] != true || web["status"] != float64(200) || web["kind"] != "http" || web["certDaysLeft"] == nil || web["certExpires"] == nil {
		t.Fatalf("web = %v", web)
	}
	if tcp := rows[1].(map[string]any); tcp["ok"] != true || tcp["kind"] != "tcp" || tcp["certDaysLeft"] != nil {
		t.Fatalf("tcp = %v", tcp)
	}

	// Ad-hoc targets replace the configured ones.
	out = plugintest.MustOK(t, cs, "check", map[string]any{"targets": []string{okSrv.URL + "/broken", closedAddr, "gopher://x"}})
	if out["down"] != float64(3) {
		t.Fatalf("out = %v", out)
	}
	rows = out["targets"].([]any)
	if r := rows[0].(map[string]any); r["status"] != float64(502) || r["ok"] != false {
		t.Fatalf("broken = %v", r)
	}
	if r := rows[1].(map[string]any); r["error"] == "" || r["ok"] != false {
		t.Fatalf("closed = %v", r)
	}
	if r := rows[2].(map[string]any); !strings.Contains(r["error"].(string), "http(s) URL or host:port") {
		t.Fatalf("invalid = %v", r)
	}
}

func TestUntrustedCertificateStillReportsExpiry(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{}
	srv.StartTLS()
	defer srv.Close()
	cs := plugintest.Connect(t, newServer(&app{}))
	out := plugintest.MustOK(t, cs, "check", map[string]any{"targets": []string{srv.URL}})
	r := out["targets"].([]any)[0].(map[string]any)
	if r["ok"] != false || !strings.Contains(r["error"].(string), "certificate") || r["certDaysLeft"] == nil {
		t.Fatalf("r = %v", r)
	}
}

func TestNoTargets(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{}))
	r := plugintest.Call(t, cs, "check", nil)
	if !r.IsError || !strings.Contains(r.Text, "TARGETS") {
		t.Fatalf("r = %+v", r)
	}
}

func TestManifestMatchesTools(t *testing.T) {
	plugintest.CheckManifest(t, plugintest.Connect(t, newServer(&app{})), "../../plugins/uptime/server.json")
}
