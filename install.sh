#!/usr/bin/env bash
# Build/test as the invoking user; only installation/configuration needs sudo.
# Usage: install.sh [-c release-server.conf] letsencrypt-email|none
# "none" registers no contact address (certbot reuses this host's existing account).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
config=""
if [[ "${1:-}" == "-c" ]]; then config="${2:?-c needs a file}"; shift 2; fi
email="${1:?Usage: install.sh [-c release-server.conf] letsencrypt-email|none}"
if [[ "$email" == none ]]; then contact=(--register-unsafely-without-email)
elif [[ "$email" == *@* ]]; then contact=(--email "$email")
else echo "An email address (or 'none') is required" >&2; exit 1; fi
# shellcheck source=deploy/load-config.sh
source "$here/deploy/load-config.sh"
load_config "$config"
command -v envsubst >/dev/null || { echo "envsubst is required (apt install gettext-base)" >&2; exit 1; }

cd "$here"
go test ./...
go vet ./...
mkdir -p build
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/release-server .

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
render deploy/service.in > "$work/service"
render deploy/nginx-http.conf.in > "$work/http.conf"
if [[ "$CLOUDFLARE" == 1 ]]; then REAL_IP_BLOCK=$(cat deploy/nginx-cloudflare.conf); else REAL_IP_BLOCK="    # Not behind Cloudflare: client addresses come straight from the socket."; fi
render deploy/nginx-https.conf.in > "$work/https.conf"
cat > "$work/server.env" <<ENV
RELEASE_DATA=$DATA_DIR
RELEASE_BASE_URL=$BASE_URL
RELEASE_LISTEN=127.0.0.1:$PORT
RELEASE_SLUG=$SLUG
RELEASE_NAME=$NAME
RELEASE_CHANNEL=$CHANNEL
RELEASE_SOURCE_URL=$SOURCE_URL
RELEASE_STABLE_URL=$STABLE_URL
RELEASE_VERSION_SCHEME=$VERSION_SCHEME
RELEASE_MAX_APK_MB=$MAX_APK_MB
ENV

sudo install -d -o root -g root -m 0755 "$DATA_DIR" "$CONFIG_DIR" "$ACME_DIR" /etc/nginx/sites-available /etc/nginx/sites-enabled
if ! getent passwd "$SERVICE" >/dev/null; then
    sudo useradd --system --user-group --home-dir /nonexistent --shell /usr/sbin/nologin "$SERVICE"
fi
sudo install -m 0755 build/release-server "$BINARY"
sudo install -m 0644 "$work/server.env" "$ENV_FILE"
sudo install -m 0644 "$work/service" "/etc/systemd/system/$SERVICE.service"
sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE.service"
sudo systemctl restart "$SERVICE.service"
curl -fsS --retry 5 --retry-connrefused --retry-delay 1 "http://127.0.0.1:$PORT/healthz"

# Back up only this host's config, and keep its existing TLS config during renewal.
site="/etc/nginx/sites-available/$NGINX_CONF"
if [[ -e "$site" ]]; then
    sudo cp -a "$site" "$site.backup-$(date +%Y%m%d%H%M%S)"
fi
if [[ ! -f "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" ]]; then
    sudo install -m 0644 "$work/http.conf" "$site"
    sudo ln -sfn "$site" "/etc/nginx/sites-enabled/$NGINX_CONF"
    sudo nginx -t
    sudo systemctl reload nginx
fi
sudo certbot certonly --webroot --webroot-path "$ACME_DIR" \
    --domain "$DOMAIN" --cert-name "$DOMAIN" \
    "${contact[@]}" --agree-tos --non-interactive --keep-until-expiring
cat "$work/http.conf" "$work/https.conf" > "$work/site.conf"
sudo install -m 0644 "$work/site.conf" "$site"
sudo ln -sfn "$site" "/etc/nginx/sites-enabled/$NGINX_CONF"
sudo nginx -t
sudo systemctl reload nginx
sudo install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
sudo install -m 0755 deploy/renew-nginx.sh /etc/letsencrypt/renewal-hooks/deploy/release-server-nginx
sudo systemctl enable --now certbot.timer
curl -fsS --retry 3 --retry-delay 2 "$BASE_URL/healthz"
echo "$NAME $CHANNEL server installed: $BASE_URL/releases/"
echo "Next: pin your release signing certificate with pin-cert.sh, then publish with publish.sh."
