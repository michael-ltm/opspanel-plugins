// Command notify is an OpsPanel plugin that sends messages to ntfy, Telegram
// and Feishu (Lark) bots, so agents and panel routines can reach the operator.
//
// Each channel is a single HTTPS POST, so this uses net/http directly rather
// than one SDK per service.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-ltm/opspanel-plugins/internal/cli"
	"github.com/michael-ltm/opspanel-plugins/internal/plugin"
)

var version = "dev"

type config struct {
	ntfyURL, ntfyTopic, ntfyToken string
	telegramToken, telegramChat   string
	feishuWebhook, feishuSecret   string
}

func configFromEnv() config {
	c := config{
		ntfyURL: strings.TrimSpace(os.Getenv("NTFY_URL")), ntfyTopic: strings.TrimSpace(os.Getenv("NTFY_TOPIC")), ntfyToken: os.Getenv("NTFY_TOKEN"),
		telegramToken: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")), telegramChat: strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")),
		feishuWebhook: strings.TrimSpace(os.Getenv("FEISHU_WEBHOOK")), feishuSecret: os.Getenv("FEISHU_SECRET"),
	}
	if c.ntfyURL == "" {
		c.ntfyURL = "https://ntfy.sh"
	}
	return c
}

func main() {
	plugin.Serve(newServer(&app{cfg: configFromEnv()}), "notify", version)
}

type app struct {
	cfg config
	// telegramAPI replaces https://api.telegram.org (tests).
	telegramAPI string
}

var client = &http.Client{Timeout: 20 * time.Second}

// Channel is one delivery channel; secrets are never included.
type Channel struct {
	ID         string `json:"id" jsonschema:"ntfy, telegram or feishu"`
	Configured bool   `json:"configured"`
	Target     string `json:"target" jsonschema:"where messages go, without secrets"`
	Missing    string `json:"missing,omitempty" jsonschema:"settings still needed to use this channel"`
}

func (a *app) channels() []Channel {
	c := a.cfg
	ntfy := Channel{ID: "ntfy", Configured: c.ntfyTopic != ""}
	if ntfy.Configured {
		host := c.ntfyURL
		if u, err := url.Parse(c.ntfyURL); err == nil && u.Host != "" {
			host = u.Host + strings.TrimSuffix(u.Path, "/")
		}
		ntfy.Target = host + "/" + c.ntfyTopic
	} else {
		ntfy.Missing = "NTFY_TOPIC"
	}
	tg := Channel{ID: "telegram", Configured: c.telegramToken != "" && c.telegramChat != ""}
	if tg.Configured {
		tg.Target = "chat " + c.telegramChat
	} else {
		var miss []string
		if c.telegramToken == "" {
			miss = append(miss, "TELEGRAM_BOT_TOKEN")
		}
		if c.telegramChat == "" {
			miss = append(miss, "TELEGRAM_CHAT_ID")
		}
		tg.Missing = strings.Join(miss, ", ")
	}
	fs := Channel{ID: "feishu", Configured: c.feishuWebhook != ""}
	if fs.Configured {
		fs.Target = "webhook"
		if u, err := url.Parse(c.feishuWebhook); err == nil {
			fs.Target = u.Host + " webhook"
		}
	} else {
		fs.Missing = "FEISHU_WEBHOOK"
	}
	return []Channel{ntfy, tg, fs}
}

func text(title, body string) string {
	if title == "" {
		return body
	}
	return title + "\n\n" + body
}

func post(ctx context.Context, rawURL string, payload any, header map[string]string) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Errors from net/http quote the URL, which can hold a token.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return body, fmt.Errorf("HTTP %d: %s", resp.StatusCode, cli.Tail(string(body), 300))
	}
	return body, nil
}

func (a *app) sendNtfy(ctx context.Context, title, body string) error {
	payload := map[string]any{"topic": a.cfg.ntfyTopic, "message": body}
	if title != "" {
		payload["title"] = title
	}
	h := map[string]string{}
	if a.cfg.ntfyToken != "" {
		h["Authorization"] = "Bearer " + a.cfg.ntfyToken
	}
	_, err := post(ctx, strings.TrimSuffix(a.cfg.ntfyURL, "/")+"/", payload, h)
	return err
}

func (a *app) sendTelegram(ctx context.Context, title, body string) error {
	api := a.telegramAPI
	if api == "" {
		api = "https://api.telegram.org"
	}
	resp, err := post(ctx, api+"/bot"+a.cfg.telegramToken+"/sendMessage", map[string]any{"chat_id": a.cfg.telegramChat, "text": text(title, body)}, nil)
	if err != nil {
		var r struct{ Description string }
		if json.Unmarshal(resp, &r) == nil && r.Description != "" {
			return errors.New(r.Description)
		}
		return err
	}
	return nil
}

func (a *app) sendFeishu(ctx context.Context, title, body string) error {
	payload := map[string]any{"msg_type": "text", "content": map[string]string{"text": text(title, body)}}
	if a.cfg.feishuSecret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(ts+"\n"+a.cfg.feishuSecret))
		payload["timestamp"] = ts
		payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	resp, err := post(ctx, a.cfg.feishuWebhook, payload, nil)
	if err != nil {
		return err
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(resp, &r) == nil && r.Code != 0 {
		return fmt.Errorf("feishu error %d: %s", r.Code, r.Msg)
	}
	return nil
}

// SendIn is one message.
type SendIn struct {
	Title   string `json:"title,omitempty"`
	Body    string `json:"body" jsonschema:"message text"`
	Channel string `json:"channel,omitempty" jsonschema:"ntfy, telegram or feishu; default: every configured channel"`
}

// Delivery is one channel's outcome.
type Delivery struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// SendOut reports each delivery.
type SendOut struct {
	Sent    int        `json:"sent"`
	Failed  int        `json:"failed"`
	Results []Delivery `json:"results"`
}

// ChannelsOut lists the channels.
type ChannelsOut struct {
	Channels []Channel `json:"channels"`
}

func (a *app) send(ctx context.Context, in SendIn) (SendOut, error) {
	if strings.TrimSpace(in.Body) == "" {
		return SendOut{}, errors.New("body is empty")
	}
	senders := map[string]func(context.Context, string, string) error{"ntfy": a.sendNtfy, "telegram": a.sendTelegram, "feishu": a.sendFeishu}
	var targets []Channel
	for _, c := range a.channels() {
		switch {
		case in.Channel != "" && c.ID != in.Channel:
		case in.Channel != "" && !c.Configured:
			return SendOut{}, fmt.Errorf("%s is not configured: set %s in the plugin settings", c.ID, c.Missing)
		case c.Configured:
			targets = append(targets, c)
		}
	}
	if in.Channel != "" && len(targets) == 0 {
		return SendOut{}, fmt.Errorf("unknown channel %q: use ntfy, telegram or feishu", in.Channel)
	}
	if len(targets) == 0 {
		return SendOut{}, errors.New("no channel is configured: set NTFY_TOPIC, TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID, or FEISHU_WEBHOOK in the plugin settings")
	}
	out := SendOut{Results: []Delivery{}}
	var failures []string
	for _, c := range targets {
		d := Delivery{Channel: c.ID}
		if err := senders[c.ID](ctx, in.Title, in.Body); err != nil {
			d.Error = err.Error()
			out.Failed++
			failures = append(failures, c.ID+": "+d.Error)
		} else {
			d.OK = true
			out.Sent++
		}
		out.Results = append(out.Results, d)
	}
	if out.Sent == 0 {
		return out, errors.New("nothing was delivered: " + strings.Join(failures, "; "))
	}
	return out, nil
}

func newServer(a *app) *mcp.Server {
	s := plugin.New("notify", version)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "channels",
		Description: "The notification channels (ntfy, Telegram, Feishu) and whether each is configured.",
		Annotations: plugin.ReadOnly(),
	}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, ChannelsOut, error) {
		return nil, ChannelsOut{Channels: a.channels()}, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send",
		Description: "Send a message to the operator through every configured channel, or only the one named. Plain text.",
		Annotations: plugin.Mutating(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SendIn) (*mcp.CallToolResult, SendOut, error) {
		out, err := a.send(ctx, in)
		return nil, out, err
	})
	return s
}
