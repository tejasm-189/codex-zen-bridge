#!/usr/bin/env bash
set -euo pipefail

# opencode-zen-bridge installer
# Sets up: Go bridge -> background service (systemd/launchd/fallback) -> codex config.
# Works two ways:
#   1. From a cloned/extracted repo:  ./install.sh            (uses ./src)
#   2. Anywhere (one-liner):          curl -fsSL <url>/install.sh | bash
#      -> fetches src from GitHub, builds, installs. Requires Go + codex.
# Cross-platform: Linux (systemd), macOS (launchd), other *nix (nohup).
# Requires: codex CLI + Go >= 1.24 (or already-built binary). No API keys.

REPO_RAW="https://raw.githubusercontent.com/tejasm-189/codex-zen-bridge/master"
echo "==> opencode-zen-bridge installer"

# --- 1. codex must be present ------------------------------------------------
if ! command -v codex >/dev/null 2>&1; then
  echo "ERROR: codex CLI not found on PATH."
  echo "  Install it first, e.g.:  npm install -g @openai/codex"
  exit 1
fi
echo "    codex: $(codex --version 2>/dev/null || echo unknown)"

# --- 2. Determine source directory (local src/ falls back to GitHub fetch) ---
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd 2>/dev/null || echo "$(dirname "$0")")"
SRC_DIR="$SCRIPT_DIR/src"

fetch_src() {
  echo "==> No local src/ found; fetching source from $REPO_RAW"
  TMP="$(mktemp -d)"
  curl -fsSL "$REPO_RAW/src/go.mod"  -o "$TMP/go.mod"
  curl -fsSL "$REPO_RAW/src/main.go" -o "$TMP/main.go"
  SRC_DIR="$TMP"
}

if [ ! -f "$SRC_DIR/main.go" ]; then
  fetch_src
fi

# --- 3. Build or reuse an existing binary -------------------------------------
BIN_DIR="$HOME/.local/bin"
mkdir -p "$BIN_DIR"

if command -v go >/dev/null 2>&1; then
  echo "==> Building bridge with $(go version | awk '{print $3}')"
  (cd "$SRC_DIR" && CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BIN_DIR/opencode-zen-bridge" .)
else
  if [ -x "$BIN_DIR/opencode-zen-bridge" ]; then
    echo "    Go not found; using existing binary at $BIN_DIR/opencode-zen-bridge"
  else
    echo "ERROR: Go toolchain not found and no prebuilt binary at $BIN_DIR/opencode-zen-bridge."
    echo "  Install Go >= 1.24, or copy a linux/$(uname -m) build of the bridge to that path."
    exit 1
  fi
fi

# --- 4. Install as a background/service process (cross-platform) ------------
OS="$(uname -s)"
SVC_ID="opencode-zen-bridge"

start_check() {
  printf '    waiting for bridge on 127.0.0.1:6446'
  for i in $(seq 1 20); do
    if curl -sf http://127.0.0.1:6446/v1/models >/dev/null 2>&1; then
      echo " OK"
      return 0
    fi
    printf '.'
    sleep 0.5
  done
  return 1
}

if [ "$OS" = "Linux" ] && command -v systemctl >/dev/null 2>&1; then
  echo "==> Installing systemd user service"
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$HOME/.config/systemd/user/$SVC_ID.service" <<EOF
[Unit]
Description=OpenCode Zen bridge (codex free DeepSeek V4 Flash)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN_DIR/opencode-zen-bridge
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now "$SVC_ID"
  if start_check; then
    :
  else
    echo "ERROR: bridge did not come up. Check: journalctl --user -u $SVC_ID -n 50"
    exit 1
  fi

elif [ "$OS" = "Darwin" ]; then
  echo "==> Installing macOS launchd agent"
  mkdir -p "$HOME/Library/LaunchAgents"
  PLIST="$HOME/Library/LaunchAgents/io.tejasm.$SVC_ID.plist"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>io.tejasm.$SVC_ID</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN_DIR/opencode-zen-bridge</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>$HOME/.local/share/opencode/$SVC_ID.log</string>
  <key>StandardErrorPath</key>
  <string>$HOME/.local/share/opencode/$SVC_ID.err.log</string>
</dict>
</plist>
EOF
  mkdir -p "$HOME/.local/share/opencode"
  launchctl load -w "$PLIST"
  if start_check; then
    :
  else
    echo "ERROR: bridge did not come up. Logs: ~/.local/share/opencode/$SVC_ID.err.log"
    exit 1
  fi

else
  echo "==> No systemd/launchd detected; starting bridge in the background"
  echo "    (it will not auto-start on boot. Use run.sh or a cron/systemd unit for that.)"
  mkdir -p "$HOME/.local/share/opencode"
  nohup "$BIN_DIR/opencode-zen-bridge" \
    >>"$HOME/.local/share/opencode/$SVC_ID.log" 2>&1 &
  if start_check; then
    :
  else
    echo "ERROR: bridge did not come up. Logs: ~/.local/share/opencode/$SVC_ID.log"
    exit 1
  fi
fi

# --- 5. Write codex provider config --------------------------------------------
echo "==> Writing codex provider config"
CODEX_DIR="$HOME/.codex"
mkdir -p "$CODEX_DIR"
if [ -f "$CODEX_DIR/config.toml" ]; then
  cp "$CODEX_DIR/config.toml" "$CODEX_DIR/config.toml.bak.$(date +%s)"
  echo "    backed up existing config.toml -> config.toml.bak.*"
fi

cat > "$CODEX_DIR/config.toml" <<EOF
# Generated by zen-bridge install.sh
model = "deepseek-v4-flash-free"
model_provider = "zen"
web_search = "disabled"
model_reasoning_effort = "medium"
model_catalog_json = "$HOME/.local/share/opencode/codex-models.json"

[model_providers.zen]
name = "OpenCode Zen (free, via local bridge)"
base_url = "http://127.0.0.1:6446/v1"
wire_api = "responses"
model_context_window = 131072
model_max_output_tokens = 8192

[projects."$HOME"]
trust_level = "trusted"
EOF

echo "==> Done."
echo ""
echo "Try it:"
echo "  codex exec \"Use the web_search tool to find today's world population, then reply with one sentence.\""
echo ""
if [ -x "$SCRIPT_DIR/run.sh" ]; then
  echo "Manual (run in foreground): $SCRIPT_DIR/run.sh"
fi
echo "Uninstall:           curl -fsSL $REPO_RAW/uninstall.sh | bash"