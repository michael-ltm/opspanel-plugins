// Command tailscale is an OpsPanel plugin that reports the Tailscale node's
// state and its peers, read from `tailscale status --json`.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/michael-ltm/opspanel-plugins/internal/cli"
	"github.com/michael-ltm/opspanel-plugins/internal/plugin"
)

var version = "dev"

var spec = cli.Spec{
	Name: "tailscale",
	Known: map[string][]string{
		"darwin":  {"/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/opt/homebrew/bin/tailscale", "/usr/local/bin/tailscale"},
		"windows": {`C:\Program Files\Tailscale\tailscale.exe`},
		"linux":   {"/usr/bin/tailscale", "/usr/sbin/tailscale", "/usr/local/bin/tailscale"},
	},
	Hint: "Install Tailscale from https://tailscale.com/download",
}

func main() {
	plugin.Serve(newServer(&app{find: spec.Find}), "tailscale", version)
}

type app struct {
	find func() (string, error)
}

// Raw shapes of `tailscale status --json` (only what is read).
type rawPeer struct {
	HostName       string
	DNSName        string
	OS             string
	TailscaleIPs   []string
	Online         bool
	LastSeen       time.Time
	KeyExpiry      *time.Time
	ExitNode       bool
	ExitNodeOption bool
	Relay          string
	CurAddr        string
	RxBytes        int64
	TxBytes        int64
}

type rawStatus struct {
	Version        string
	BackendState   string
	MagicDNSSuffix string
	CurrentTailnet *struct{ Name string }
	Health         []string
	Self           *rawPeer
	Peer           map[string]rawPeer
}

// Peer is one node as the panel shows it.
type Peer struct {
	Name       string   `json:"name"`
	DNSName    string   `json:"dnsName"`
	IP         string   `json:"ip"`
	IPs        []string `json:"ips"`
	OS         string   `json:"os"`
	Online     bool     `json:"online"`
	LastSeen   *int64   `json:"lastSeen" jsonschema:"unix ms; null while online"`
	KeyExpiry  *int64   `json:"keyExpiry,omitempty"`
	Connection string   `json:"connection" jsonschema:"direct, relay:<region> or empty when idle"`
	ExitNode   bool     `json:"exitNode" jsonschema:"this peer is the exit node in use"`
	CanExit    bool     `json:"canExit" jsonschema:"this peer offers itself as an exit node"`
	RxBytes    int64    `json:"rxBytes"`
	TxBytes    int64    `json:"txBytes"`
}

func toPeer(r rawPeer) Peer {
	name, _, _ := strings.Cut(strings.TrimSuffix(r.DNSName, "."), ".")
	if name == "" {
		name = r.HostName
	}
	p := Peer{
		Name: name, DNSName: strings.TrimSuffix(r.DNSName, "."), IPs: r.TailscaleIPs, OS: r.OS, Online: r.Online,
		ExitNode: r.ExitNode, CanExit: r.ExitNodeOption, RxBytes: r.RxBytes, TxBytes: r.TxBytes,
	}
	if len(r.TailscaleIPs) > 0 {
		p.IP = r.TailscaleIPs[0]
	}
	if p.IPs == nil {
		p.IPs = []string{}
	}
	if !r.Online {
		p.LastSeen = plugin.Millis(r.LastSeen)
	}
	if r.KeyExpiry != nil {
		p.KeyExpiry = plugin.Millis(*r.KeyExpiry)
	}
	switch {
	case r.CurAddr != "":
		p.Connection = "direct"
	case r.Relay != "":
		p.Connection = "relay:" + r.Relay
	}
	return p
}

func (a *app) status(ctx context.Context) (*rawStatus, error) {
	bin, err := a.find()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := cli.Run(ctx, bin, nil, "status", "--json")
	if err != nil {
		return nil, err
	}
	var st rawStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("unexpected output from tailscale status --json: %w", err)
	}
	return &st, nil
}

func peers(st *rawStatus) []Peer {
	out := make([]Peer, 0, len(st.Peer))
	for _, r := range st.Peer {
		out = append(out, toPeer(r))
	}
	slices.SortFunc(out, func(a, b Peer) int {
		if a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// StatusOut summarises this node.
type StatusOut struct {
	BackendState   string   `json:"backendState" jsonschema:"Running, Stopped, NeedsLogin, …"`
	Tailnet        string   `json:"tailnet"`
	MagicDNSSuffix string   `json:"magicDNSSuffix"`
	Version        string   `json:"version"`
	Self           *Peer    `json:"self"`
	Online         int      `json:"online" jsonschema:"peers online now"`
	Total          int      `json:"total" jsonschema:"peers in the tailnet"`
	Health         []string `json:"health" jsonschema:"warnings reported by tailscaled"`
}

// PeersIn filters the peer list.
type PeersIn struct {
	OnlineOnly bool `json:"online_only,omitempty" jsonschema:"only list peers that are online"`
}

// PeersOut lists peers, online first.
type PeersOut struct {
	Peers []Peer `json:"peers"`
}

func newServer(a *app) *mcp.Server {
	s := plugin.New("tailscale", version)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "status",
		Description: "This machine's Tailscale state: backend state, tailnet, its own IPs, how many peers are online, and health warnings.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, StatusOut, error) {
		st, err := a.status(ctx)
		if err != nil {
			return nil, StatusOut{}, err
		}
		out := StatusOut{BackendState: st.BackendState, MagicDNSSuffix: st.MagicDNSSuffix, Version: st.Version, Health: st.Health}
		if out.Health == nil {
			out.Health = []string{}
		}
		if st.CurrentTailnet != nil {
			out.Tailnet = st.CurrentTailnet.Name
		}
		if st.Self != nil {
			p := toPeer(*st.Self)
			out.Self = &p
		}
		for _, p := range st.Peer {
			out.Total++
			if p.Online {
				out.Online++
			}
		}
		return nil, out, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "peers",
		Description: "Peers in the tailnet with their Tailscale IP, OS, online state, last seen time, and whether the connection is direct or relayed.",
		Annotations: plugin.ReadOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PeersIn) (*mcp.CallToolResult, PeersOut, error) {
		st, err := a.status(ctx)
		if err != nil {
			return nil, PeersOut{}, err
		}
		all := peers(st)
		if in.OnlineOnly {
			all = slices.DeleteFunc(all, func(p Peer) bool { return !p.Online })
		}
		return nil, PeersOut{Peers: all}, nil
	})
	return s
}
