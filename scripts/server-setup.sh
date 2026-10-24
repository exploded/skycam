#!/bin/bash
# server-setup.sh - one-time setup for skycam-web on the Linode
# Run as root:
#   curl -fsSL https://raw.githubusercontent.com/exploded/skycam/main/scripts/server-setup.sh | sudo bash
set -e

APP_DIR="/var/www/skycam"
SERVICE="skycam"
PORT=8996
REPORT_DIR="/var/www/deepspaceplace/reports/skycam"

echo "=== skycam-web - server setup ==="

# 1. Application directory
mkdir -p "$APP_DIR/data"
chown -R www-data:www-data "$APP_DIR"
echo "[ok] $APP_DIR"

# 2. deepspaceplace report folder. www-data owns it, as deepspaceplace's
# own deploy chowns its tree to www-data on every ship.
mkdir -p "$REPORT_DIR"
chown www-data:www-data "$REPORT_DIR"
echo "[ok] $REPORT_DIR"

# 3. Environment file
ENV_FILE="$APP_DIR/.env"
if [ -f "$ENV_FILE" ]; then
    echo "[ok] $ENV_FILE already exists (not overwriting)"
else
    cat > "$ENV_FILE" << ENV_TEMPLATE
PORT=$PORT
# The Pi's bearer token: the same value as relay.token in the Pi's
# /etc/skycam/config.json. Generate with: openssl rand -hex 32
SKYCAM_TOKEN=
# Viewer login for https://skycam.mchugh.au
SKYCAM_USER=james
SKYCAM_PASSWORD=
DATA_DIR=$APP_DIR/data
REPORT_DIR=$REPORT_DIR
REPORT_BASE_URL=https://www.deepspaceplace.com/reports/skycam/
# Monitor portal (copy the key line from another app's .env)
MONITOR_URL=https://monitor.mchugh.au
MONITOR_API_KEY=
ENV_TEMPLATE
    chown www-data:www-data "$ENV_FILE"
    chmod 600 "$ENV_FILE"
    echo "[ok] created $ENV_FILE - fill in the token, password and monitor key"
fi

# 4. systemd unit
cat > "/etc/systemd/system/$SERVICE.service" << UNIT
[Unit]
Description=skycam-web (observatory all-sky relay)
After=network.target

[Service]
Type=simple
User=www-data
Group=www-data
WorkingDirectory=$APP_DIR
EnvironmentFile=$APP_DIR/.env
ExecStart=$APP_DIR/skycam-web
Restart=on-failure
RestartSec=5

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
echo "[ok] /etc/systemd/system/$SERVICE.service"

# 5. Deploy script
curl -fsSL "https://raw.githubusercontent.com/exploded/skycam/main/scripts/deploy-skycam" -o /usr/local/bin/deploy-skycam
chmod 755 /usr/local/bin/deploy-skycam
echo "[ok] /usr/local/bin/deploy-skycam"

# 6. Sudoers
cat > /etc/sudoers.d/skycam-deploy << 'EOF2'
deploy ALL=(ALL) NOPASSWD: /usr/local/bin/deploy-skycam
deploy ALL=(ALL) NOPASSWD: /usr/bin/systemctl stop skycam
EOF2
chmod 440 /etc/sudoers.d/skycam-deploy
visudo -c -f /etc/sudoers.d/skycam-deploy
echo "[ok] sudoers"

echo ""
echo "=== Setup complete ==="
echo ""
echo "Add to /etc/caddy/Caddyfile, then: sudo systemctl reload caddy"
echo ""
echo "  skycam.mchugh.au {"
echo "      import access_log"
echo "      reverse_proxy localhost:$PORT {"
echo "          flush_interval -1"
echo "      }"
echo "  }"
echo ""
echo "flush_interval -1 stops Caddy buffering the live MJPEG stream."
echo "Then edit $ENV_FILE and: sudo systemctl enable --now $SERVICE"
