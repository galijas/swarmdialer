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

# --- systemd service for the GUI, so it starts on boot and restarts on
# crash instead of needing someone to SSH in and run it by hand (see
# PROJECT_STATE.md — this bit us live: the process died with no crash
# logged, and nothing brought it back until a manual restart).
UNIT=/etc/systemd/system/swarmdialer.service
INSTALL_ROOT="$(pwd)"
log "installing systemd service (HTTPS on :443, HTTP :80 redirects to it) at $UNIT"
cat > "$UNIT" <<EOF
[Unit]
Description=SwarmDialer GUI
After=network.target

[Service]
WorkingDirectory=$INSTALL_ROOT
ExecStartPre=-$INSTALL_ROOT/scripts/tune-network.sh
ExecStart=$INSTALL_ROOT/bin/gui -addr :443 -http-redirect-addr :80 -config $INSTALL_ROOT/swarmdialer_config.json -tls-dir $INSTALL_ROOT/tls
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
# --- Admin login. The GUI holds admin API keys for the PBXware instances
# (and SERVERware), so it always requires a login. Create the admin
# password on first install and show it once; it's stored only as a hash.
CREDS=""
if [ ! -f "$INSTALL_ROOT/swarmdialer_auth.json" ]; then
  CREDS=$("$BIN_DIR/gui" -reset-password -config "$INSTALL_ROOT/swarmdialer_config.json")
fi
# Secrets on disk (API keys, password hash, TLS key) are root-only.
chmod 600 "$INSTALL_ROOT"/swarmdialer_config.json "$INSTALL_ROOT"/swarmdialer_auth.json 2>/dev/null || true

systemctl daemon-reload
systemctl enable --now swarmdialer
systemctl restart swarmdialer
log "service started and enabled on boot — check with: systemctl status swarmdialer"
log "open https://<this server's IP>/ (the certificate is self-signed, so the browser shows a one-time warning)"
if [ -n "$CREDS" ]; then
  echo
  echo "$CREDS" | head -3
  echo
  log "save this password now; it is not shown again (to create a new one: $BIN_DIR/gui -reset-password -config $INSTALL_ROOT/swarmdialer_config.json)"
fi
