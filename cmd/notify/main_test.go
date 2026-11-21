package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/michael-ltm/opspanel-plugins/internal/plugintest"
)

type hit struct {
	path, auth string
	body       map[string]any
}

func recorder(t *testing.T, reply func(path string) (int, string)) (*httptest.Server, func() []hit) {
	var mu sync.Mutex
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		hits = append(hits, hit{r.URL.Path, r.Header.Get("Authorization"), body})
		mu.Unlock()
		code, text := reply(r.URL.Path)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, text)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []hit { mu.Lock(); defer mu.Unlock(); return append([]hit(nil), hits...) }
}

func TestChannelsAndAnnotations(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{cfg: config{ntfyURL: "https://ntfy.sh", ntfyTopic: "ops", telegramToken: "123:abc", telegramChat: "42"}}))
	plugintest.WantKinds(t, cs, map[string]string{"channels": "read", "send": "write"})
	chans := plugintest.MustOK(t, cs, "channels", nil)["channels"].([]any)
	if len(chans) != 3 {
		t.Fatalf("chans = %v", chans)
	}
	byID := map[string]map[string]any{}
	for _, c := range chans {
		m := c.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if byID["ntfy"]["configured"] != true || byID["ntfy"]["target"] != "ntfy.sh/ops" || byID["telegram"]["target"] != "chat 42" || byID["feishu"]["configured"] != false {
		t.Fatalf("byID = %v", byID)
	}
	// Secrets never appear in the output.
	for _, c := range chans {
		if b, _ := json.Marshal(c); strings.Contains(string(b), "abc") {
			t.Fatalf("secret leaked: %s", b)
		}
	}
}

func TestSendToEveryConfiguredChannel(t *testing.T) {
	srv, hits := recorder(t, func(path string) (int, string) {
		switch {
		case strings.HasPrefix(path, "/bot"):
			return 200, `{"ok":true}`
		case path == "/hook":
			return 200, `{"code":0,"msg":"success"}`
		}
		return 200, `{}`
	})
	a := &app{cfg: config{
		ntfyURL: srv.URL, ntfyTopic: "ops", ntfyToken: "tk",
		telegramToken: "123:abc", telegramChat: "42",
		feishuWebhook: srv.URL + "/hook", feishuSecret: "s3cret",
	}, telegramAPI: srv.URL}
	cs := plugintest.Connect(t, newServer(a))
	out := plugintest.MustOK(t, cs, "send", map[string]any{"title": "部署完成", "body": "v1.2 已上线"})
	if out["sent"] != float64(3) || out["failed"] != float64(0) {
		t.Fatalf("out = %v", out)
	}
	got := map[string]hit{}
	for _, h := range hits() {
		got[h.path] = h
	}
	if h := got["/"]; h.auth != "Bearer tk" || h.body["topic"] != "ops" || h.body["title"] != "部署完成" || h.body["message"] != "v1.2 已上线" {
		t.Fatalf("ntfy = %+v", h)
	}
	if h := got["/bot123:abc/sendMessage"]; h.body["chat_id"] != "42" || h.body["text"] != "部署完成\n\nv1.2 已上线" {
		t.Fatalf("telegram = %+v", h)
	}
	h := got["/hook"]
	ts, _ := h.body["timestamp"].(string)
	mac := hmac.New(sha256.New, []byte(ts+"\ns3cret"))
	if h.body["msg_type"] != "text" || h.body["sign"] != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("feishu = %+v", h)
	}
	if text := h.body["content"].(map[string]any)["text"]; text != "部署完成\n\nv1.2 已上线" {
		t.Fatalf("feishu text = %v", text)
	}
}

func TestSendReportsPerChannelFailures(t *testing.T) {
	srv, _ := recorder(t, func(path string) (int, string) {
		if path == "/hook" {
			return 200, `{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}`
		}
		return 401, `{"ok":false,"description":"Unauthorized"}`
	})
	a := &app{cfg: config{telegramToken: "bad", telegramChat: "1", feishuWebhook: srv.URL + "/hook"}, telegramAPI: srv.URL}
	cs := plugintest.Connect(t, newServer(a))
	r := plugintest.Call(t, cs, "send", map[string]any{"body": "x"})
	if !r.IsError || !strings.Contains(r.Text, "Unauthorized") || !strings.Contains(r.Text, "sign match fail") || strings.Contains(r.Text, "bad") {
		t.Fatalf("r = %+v", r)
	}

	// One channel only.
	r = plugintest.Call(t, cs, "send", map[string]any{"body": "x", "channel": "ntfy"})
	if !r.IsError || !strings.Contains(r.Text, "NTFY_TOPIC") {
		t.Fatalf("unconfigured channel: %+v", r)
	}
}

func TestSendWithNothingConfigured(t *testing.T) {
	cs := plugintest.Connect(t, newServer(&app{}))
	r := plugintest.Call(t, cs, "send", map[string]any{"body": "x"})
	if !r.IsError || !strings.Contains(r.Text, "FEISHU_WEBHOOK") {
		t.Fatalf("r = %+v", r)
	}
}

func TestManifestMatchesTools(t *testing.T) {
	plugintest.CheckManifest(t, plugintest.Connect(t, newServer(&app{})), "../../plugins/notify/server.json")
}
