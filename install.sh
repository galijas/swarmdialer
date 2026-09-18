#!/bin/bash
# SwarmDialer install script — for deploying on a fresh Ubuntu VPS.
# See docs/gui_spec.md's portability requirement: this is meant to make
# "git clone, then run this" the whole setup process. No PBXware
# connection details are baked in here — that's what the Setup Wizard is
# for, once the GUI is running.
set -euo pipefail

GO_VERSION_URL="https://go.dev/VERSION?m=text"
INSTALL_DIR="/usr/local/go"
BIN_DIR="./bin"

log() { echo "[install] $*"; }

# --- ca-certificates: a fresh minimal Ubuntu image may not have this,
# which silently breaks every HTTPS call (found the hard way — see
# PROJECT_STATE.md's 2026-09-18 entry). Check first since it's cheap and
# everything else here needs HTTPS to work.
if ! dpkg -s ca-certificates >/dev/null 2>&1; then
  log "ca-certificates missing — installing (required for any HTTPS, including Go's own module downloads)"
  apt-get update -qq
  apt-get install -y -qq ca-certificates
fi

# --- Go: install if missing or too old to matter for our purposes (any
# reasonably recent version works; we just check presence).
if ! command -v go >/dev/null 2>&1 && [ ! -x "$INSTALL_DIR/bin/go" ]; then
  log "Go not found — installing"
  GOVER=$(curl -sL "$GO_VERSION_URL" | head -1)
  log "latest Go version: $GOVER"
  TMPFILE=$(mktemp)
  curl -sL -o "$TMPFILE" "https://go.dev/dl/${GOVER}.linux-amd64.tar.gz"
  rm -rf "$INSTALL_DIR"
  tar -C /usr/local -xzf "$TMPFILE"
  rm -f "$TMPFILE"
  ln -sf "$INSTALL_DIR/bin/go" /usr/local/bin/go
  ln -sf "$INSTALL_DIR/bin/gofmt" /usr/local/bin/gofmt
  log "Go installed: $(go version)"
else
  export PATH="$PATH:$INSTALL_DIR/bin"
  log "Go already present: $(go version)"
fi

# --- Build.
log "building binaries into $BIN_DIR"
mkdir -p "$BIN_DIR"
go build -o "$BIN_DIR/gui" ./cmd/gui
go build -o "$BIN_DIR/swarmdialer" ./cmd/swarmdialer
go build -o "$BIN_DIR/loadtest" ./cmd/loadtest
log "build complete: $BIN_DIR/gui (the main GUI server), plus provisioning/loadtest CLIs"

# --- Optional: systemd service for the GUI, so it survives reboots/SSH
# disconnects. Skipped by default — run with --systemd to set it up.
if [ "${1:-}" = "--systemd" ]; then
  UNIT=/etc/systemd/system/swarmdialer.service
  INSTALL_ROOT="$(pwd)"
  log "installing systemd service (listening on :80) at $UNIT"
  cat > "$UNIT" <<EOF
[Unit]
Description=SwarmDialer GUI
After=network.target

[Service]
WorkingDirectory=$INSTALL_ROOT
ExecStart=$INSTALL_ROOT/bin/gui -addr :80 -config $INSTALL_ROOT/swarmdialer_config.json
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now swarmdialer
  log "service started — check with: systemctl status swarmdialer"
else
  log "run 'sudo ./bin/gui' to start the GUI (port 80 needs root), or re-run this script with --systemd to install it as a service"
fi
