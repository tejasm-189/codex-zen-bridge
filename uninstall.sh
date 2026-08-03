#!/usr/bin/env bash
set -euo pipefail

# Removes everything installed by install.sh (keeps config backups and src/).

echo "==> Stopping and disabling service"
systemctl --user disable --now opencode-zen-bridge 2>/dev/null || true
rm -f "$HOME/.config/systemd/user/opencode-zen-bridge.service"
systemctl --user daemon-reload

echo "==> Removing binary"
rm -f "$HOME/.local/bin/opencode-zen-bridge"

echo "==> Removing bridge-generated model catalog"
rm -f "$HOME/.local/share/opencode/codex-models.json"

echo "==> Reverting codex config"
if [ -f "$HOME/.codex/config.toml.bak."* ] 2>/dev/null; then
  latest=$(ls -t "$HOME"/.codex/config.toml.bak.* 2>/dev/null | head -1)
  mv "$latest" "$HOME/.codex/config.toml"
  echo "    restored $latest"
else
  echo "    no backup found; leaving $HOME/.codex/config.toml as-is"
fi

echo "==> Done."
