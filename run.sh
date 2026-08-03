#!/usr/bin/env bash
set -euo pipefail

# Foreground runner (no systemd). Ctrl-C to stop.
# Only works if the bridge binary is already built/installed.
exec "$HOME/.local/bin/opencode-zen-bridge"
