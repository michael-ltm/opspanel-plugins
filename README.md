# OpsPanel plugin catalog

The signed catalog OpsPanel's plugin center reads by default. Each plugin is an
[MCP](https://modelcontextprotocol.io) server described by a standard MCP
Registry `server.json` (schema `2025-12-11`), plus OpsPanel's extension fields
under `_meta["io.github.opspanel/plugin"]`. Clients that ignore `_meta` still
read a valid `server.json`.

| Plugin | What it does |
| --- | --- |
| [Browpilot](plugins/browpilot/server.json) | Lets AI agents drive the browser profiles you choose; the panel shows which profiles are online and which agent session holds which tab. |
| [Containers](plugins/containers/server.json) | See and manage this machine's Docker / Podman containers: list, logs, start, stop, restart, remove. |
| [GitHub Actions](plugins/github-actions/server.json) | Recent workflow runs, failed-job log tails, releases; re-run failed jobs. |
| [Notify](plugins/notify/server.json) | Push messages to ntfy, Telegram or a Feishu (Lark) bot. |
| [restic](plugins/restic/server.json) | Snapshots, repository stats and checks; back up and restore. |
| [Tailscale](plugins/tailscale/server.json) | This machine's tailnet state and peers (read-only). |
| [Uptime](plugins/uptime/server.json) | On-demand HTTP / TCP probes with TLS certificate days left. |

## How OpsPanel trusts this catalog

- `index.json` lists every plugin as `{"manifest": "<exact server.json text>", "signature": "<minisign signature>"}`.
- The signature covers the manifest's exact bytes, and the manifest pins every
  download with a SHA-256, so a valid signature covers everything that runs.
- OpsPanel ships the public key in [`minisign.pub`](minisign.pub) (key id
  `E214E7A897817451`). Entries signed by it show as **verified**: they install
  without extra prompts, can be used by agents and update automatically.
  Anything else is **unverified** (installable after confirmation, kept away
  from agents by default, updated by hand) or **tampered** (refused).

Verify the catalog yourself with the stock [minisign](https://jedisct1.github.io/minisign/)
tool or with `go run ./tools/catalog verify`.

## `_meta["io.github.opspanel/plugin"]`

| Field | Meaning |
| --- | --- |
| `artifacts` | `{"linux/amd64": {"url", "sha256", "args"}, …}` — single-file native binaries per platform. |
| `npm` | `{"tarball", "sha256", "bin"}` — a self-contained npm tarball, installed with `--ignore-scripts`. |
| `i18n` | `{"en": {"title", "description", "<titleKey>", …}}`; the top-level fields are Simplified Chinese. |
| `ui` | Status cards bound to read-only tools: `stat`, `table`, `list`, `image`, `json`, with a JSON Pointer `path`. Table columns may set `format`: `session` (an OpsPanel terminal session id) or `time` (unix ms). |
| `ui[].rowActions` / `ui[].actions` | Buttons on a table row / in a card header. Each does one of `tool` (with `arguments`; `{key}` fills in a field of the row), `copy` or `link`. `when` / `unless` (`{key, in}`) show it only for some rows; `display: "dialog"` shows the result (`resultPath` picks a part); `confirm` asks first. A card's `tone: "alert"` turns a stat red while it is not zero. Column `format` also takes `badge`, `bool`, `ms` and `days`. |
| `tools` | `{"<tool>": {"title", "description"}}` — Chinese wording for the panel's tool list; other languages use `i18n` keys `tool.<name>.title` / `tool.<name>.description`. |
| `settings` | `{"<ENV_NAME>": {"title", "description", "group"}}` — a readable title, wording and section for each setting; other languages use `i18n` keys `env.<NAME>.title` / `.description` / `.group`. Settings with the same `group` fold together in the panel. |

## Publishing (maintainers)

1. Add or update `plugins/<name>/server.json` (bump `version`, pin the new URL and SHA-256).
2. `go run ./tools/catalog sign -key ~/.config/opspanel-plugins/minisign.key` rewrites and verifies `index.json`.
3. Commit and push; panels pick the change up within about ten minutes.

The private key never enters this repository.

## Your own catalog

Serve an `index.json` in the same shape, sign the manifests with your own
minisign key, then add the URL under **Plugins → Sources & trust** in OpsPanel
and trust your public key there.

## License

Apache-2.0
