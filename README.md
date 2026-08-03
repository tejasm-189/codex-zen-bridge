# opencode-zen-bridge — report & setup guide

A ~17 MB Go process that gives the **codex** CLI (Rust, light) the web-search
and web-fetch superpowers opencode has, using the same **free, keyless
OpenCode Zen** DeepSeek V4 Flash endpoint opencode itself uses — while
keeping total RAM around **~74 MB peak** vs opencode's ~490 MB.

## What problem it solves

- **opencode is heavy.** `opencode run` peaked ~490 MB RSS even for trivial
  prompts (Bun runtime floor). Config tuning alone can't fix that.
- **codex is light but can't do web.** codex's `web_search` is a
  provider-native tool that Zen/DeepSeek rejects, and codex's remote MCP
  tool loading is buggy (tools never get injected into the request).
- **Solution:** a tiny local HTTP bridge that presents an OpenAI
  *Responses* API to codex, forwards to Zen's chat-completions API, and
  implements `web_search` + `webfetch` *itself* (in-process) so no MCP /
  provider support is needed.

## Architecture

```
codex (Rust CLI, ~57 MB)            ~/.codex/config.toml -> base_url http://127.0.0.1:6446/v1
   |  POST /v1/responses  (SSE)
   v
opencode-zen-bridge (Go, ~17 MB)     listens on 127.0.0.1:6446
   |  agent loop (up to 12 rounds): streams assistant text, runs web tools
   |  POST https://opencode.ai/zen/v1/chat/completions   (keyless, free DeepSeek V4 Flash)
   |  POST https://mcp.exa.ai/mcp                         (web_search / web_fetch, keyless)
   |  POST https://mcp.firecrawl.dev/v2/mcp               (backup, keyless)
   v
DeepSeek V4 Flash (free via OpenCode Zen)
```

- **No API keys anywhere.** Free tier model is picked automatically from
  `https://opencode.ai/zen/v1/models` (all `*-free` + `big-pickle`).
- The bridge **auto-writes** `~/.local/share/opencode/codex-models.json`
  (model catalog for codex) on startup.
- `web_search`: Exa MCP first, Firecrawl MCP as fallback.
- `webfetch`: Exa `web_fetch` first → HTML-strip fallback → Firecrawl scrape.
- Tool calls are executed **server-side and hidden from codex's SSE**, so
  codex never tries to route `web_search` itself (that caused `unsupported
  call` errors and Zen 400s).
- Zen "thinking mode" bug workaround: when tools are present the bridge
  sends `thinking: {"type": "disabled"}` to avoid Zen's 400 error.

## RAM (measured on this machine)

| Component                 | RSS         |
|---------------------------|-------------|
| bridge (idle/active)      | ~17 MB      |
| codex main process (run)  | ~57 MB      |
| **combined peak**         | **~74 MB**  |
| opencode (for comparison) | ~490 MB     |

## Requirements (host machine)

- **codex CLI** — `npm install -g @openai/codex` (or `cargo install codex`).
- **Go ≥ 1.24** — only needed once, to build the ~6 MB binary
  (or copy a prebuilt `linux/$(uname -m)` binary instead).
- **systemd user session** — optional; you can run `run.sh` in a terminal
  instead.
- **Outbound HTTPS** to `opencode.ai`, `mcp.exa.ai`, `mcp.firecrawl.dev`.

## Install

```
tar -xzf zen-bridge-pack.tar.gz
cd zen-bridge-pack
./install.sh
```

That: builds the bridge, installs a systemd user service, waits for it to
answer, and writes `~/.codex/config.toml` (backs up an existing one).

## Verify

```
curl -s http://127.0.0.1:6446/v1/models | head -c 200          # lists free models
codex exec "Use the web_search tool to find today's world population, then reply with one sentence."
codex exec "Use the webfetch tool to fetch https://example.com, then reply with one sentence summarizing it."
codex exec "Reply with exactly: HI"
```

Expect a single clean answer, no `unsupported call` / `Reconnecting`
errors. Watch tool use: `journalctl --user -u opencode-zen-bridge -f` shows
`internal tool web_search -> N bytes`.

## Files

| File                  | Purpose                                            |
|-----------------------|----------------------------------------------------|
| `install.sh`          | Build + systemd + codex config (idempotent)        |
| `run.sh`              | Foreground runner (no systemd)                     |
| `uninstall.sh`        | Reverts everything                                 |
| `src/main.go`         | The bridge (single Go file, no deps beyond stdlib) |
| `src/go.mod`          | Module file (`go 1.24.4`, stdlib only)             |

## Notes / troubleshooting

- **Plain plug-and-play?** Nearly — prerequisites are just codex + Go (or a
  prebuilt binary) and internet. No keys, no model setup, no MCP servers.
- **Bridge won't start** → `journalctl --user -u opencode-zen-bridge -n 50`;
  usually missing outbound HTTPS to `opencode.ai`.
- **codex errors `unsupported call: web_search`** → you're hitting a bridge
  without the internal-tool fix (rebuild from `src/`). Or you enabled a
  real `[mcp_servers.*]` entry in codex config that injects a competing tool.
- **Port conflict** → change `listen` at the top of `src/main.go` and the
  `base_url` in codex config to match.
- The `[mcp_servers.exa]` entry seen in the original config is **not
  needed** (codex's remote MCP is broken for this) and is omitted here.
- Only `linux/$(uname -m)` binaries are supported by the build scripts.
