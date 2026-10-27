#!/bin/bash
# Build ngmgmt for linux/amd64 and install or upgrade it on the server.
#
#   mgmt/deploy/deploy.sh                 upgrade binary + units, restart
#   mgmt/deploy/deploy.sh --init          first install: also user, cert, tokens, config
#
# Secrets never enter the repository: --init writes the plain admin and enrollment tokens to
# ~/.config/ngctl/ (config.json for ngctl, enroll_token + pin for APK builds) on this machine and
# only their SHA-256 hashes to the server.
set -euo pipefail
# NGMGMT_HOST (ssh target), NGMGMT_IP (public IP for the certificate/URL), NGMGMT_SSH_KEY
[[ -f "$HOME/.config/ngctl/deploy.env" ]] && source "$HOME/.config/ngctl/deploy.env"
HOST=${NGMGMT_HOST:?set NGMGMT_HOST, e.g. in ~/.config/ngctl/deploy.env}
SSH_KEY=${NGMGMT_SSH_KEY:-$HOME/.ssh/id_ed25519}
PUBLIC_IP=${NGMGMT_IP:?set NGMGMT_IP}
PORT=${NGMGMT_PORT:-9443}
here=$(cd "$(dirname "$0")" && pwd)
ssh_() { ssh -i "$SSH_KEY" -o ConnectTimeout=15 "$HOST" "$@"; }

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
(cd "$here/../server" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$out/ngmgmt" .)
scp -q -i "$SSH_KEY" "$out/ngmgmt" "$here/ngmgmt.service" "$here/ngmgmt-backup.sh" "$HOST:/tmp/"

if [[ "${1:-}" == "--init" ]]; then
  conf="$HOME/.config/ngctl"
  mkdir -p "$conf" && chmod 700 "$conf"
  [[ -e "$conf/config.json" ]] && { echo "$conf/config.json exists; refusing to re-init" >&2; exit 1; }
  adm=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')
  enr=$(openssl rand -base64 24 | tr '+/' '-_' | tr -d '=')
  adm_h=$(printf %s "$adm" | shasum -a 256 | cut -d' ' -f1)
  enr_h=$(printf %s "$enr" | shasum -a 256 | cut -d' ' -f1)
  ssh_ bash -s <<EOF
set -euo pipefail
id ngmgmt >/dev/null 2>&1 || useradd --system --home /var/lib/ngmgmt --shell /usr/sbin/nologin ngmgmt
install -d -m 755 /opt/ngmgmt
install -d -m 750 -o root -g ngmgmt /etc/ngmgmt
install -d -m 750 -o ngmgmt -g ngmgmt /var/lib/ngmgmt
install -m 755 /tmp/ngmgmt /opt/ngmgmt/ngmgmt
/opt/ngmgmt/ngmgmt gen-cert -cert /etc/ngmgmt/tls.crt -key /etc/ngmgmt/tls.key -ip $PUBLIC_IP > /etc/ngmgmt/pin
chgrp ngmgmt /etc/ngmgmt/tls.key /etc/ngmgmt/tls.crt && chmod 640 /etc/ngmgmt/tls.key
cat > /etc/ngmgmt/config.json <<JSON
{
  "listen": ":$PORT",
  "db": "/var/lib/ngmgmt/ngmgmt.db",
  "tls": {"cert": "/etc/ngmgmt/tls.crt", "key": "/etc/ngmgmt/tls.key"},
  "admin_token_sha256": ["$adm_h"],
  "enroll_token_sha256": ["$enr_h"],
  "default_profile": "family"
}
JSON
chgrp ngmgmt /etc/ngmgmt/config.json && chmod 640 /etc/ngmgmt/config.json
EOF
  pin=$(ssh_ cat /etc/ngmgmt/pin)
  umask 077
  printf '{\n  "url": "https://%s:%s",\n  "token": "%s",\n  "pin": "%s"\n}\n' "$PUBLIC_IP" "$PORT" "$adm" "$pin" > "$conf/config.json"
  printf '%s\n' "$enr" > "$conf/enroll_token"
  printf '%s\n' "$pin" > "$conf/pin"
  echo "wrote $conf/config.json, enroll_token, pin"
fi

ssh_ bash -s <<'EOF'
set -euo pipefail
install -m 755 /tmp/ngmgmt /opt/ngmgmt/ngmgmt
install -m 644 /tmp/ngmgmt.service /etc/systemd/system/ngmgmt.service
install -m 755 /tmp/ngmgmt-backup.sh /usr/local/sbin/ngmgmt-backup.sh
cat > /etc/systemd/system/ngmgmt-backup.service <<'U'
[Unit]
Description=ngmgmt database backup
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/ngmgmt-backup.sh
Nice=10
U
cat > /etc/systemd/system/ngmgmt-backup.timer <<'U'
[Unit]
Description=daily ngmgmt database backup
[Timer]
OnCalendar=*-*-* 01:45:00
Persistent=true
[Install]
WantedBy=timers.target
U
rm -f /tmp/ngmgmt /tmp/ngmgmt.service /tmp/ngmgmt-backup.sh
systemctl daemon-reload
systemctl enable --now ngmgmt-backup.timer >/dev/null
systemctl enable ngmgmt >/dev/null
systemctl restart ngmgmt
sleep 1
systemctl is-active ngmgmt
journalctl -u ngmgmt -n 3 --no-pager -o cat
EOF
