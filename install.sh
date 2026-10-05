#!/usr/bin/env bash
# Build/test as the invoking user; only installation/configuration needs sudo.
set -euo pipefail
cd "$(dirname "$0")"
email="${1:?Usage: ./install.sh letsencrypt-email}"
if [[ "$email" != *@* ]]; then echo "An email address is required" >&2; exit 1; fi
go test ./...
go vet ./...
mkdir -p build
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/folio-release-server .

sudo install -d -m 0755 /var/lib/folio-releases /var/www/folio-acme /etc/nginx/sites-available /etc/nginx/sites-enabled
if ! getent passwd folio-releases >/dev/null; then
    sudo useradd --system --user-group --home-dir /nonexistent --shell /usr/sbin/nologin folio-releases
fi
sudo install -m 0755 build/folio-release-server /usr/local/bin/folio-release-server
sudo install -m 0644 deploy/folio-releases.service /etc/systemd/system/folio-releases.service
sudo systemctl daemon-reload
sudo systemctl enable folio-releases.service
sudo systemctl restart folio-releases.service
curl -fsS --retry 5 --retry-connrefused --retry-delay 1 http://127.0.0.1:8787/healthz

# Back up only this host's config, and keep its existing TLS config during renewal.
if [[ -e /etc/nginx/sites-available/folio-releases.conf ]]; then
    sudo cp -a /etc/nginx/sites-available/folio-releases.conf "/etc/nginx/sites-available/folio-releases.conf.backup-$(date +%Y%m%d%H%M%S)"
fi
if [[ ! -f /etc/letsencrypt/live/folio.flandolf.me/fullchain.pem ]]; then
    sudo install -m 0644 deploy/nginx-http.conf /etc/nginx/sites-available/folio-releases.conf
    sudo ln -sfn /etc/nginx/sites-available/folio-releases.conf /etc/nginx/sites-enabled/folio-releases.conf
    sudo nginx -t
    sudo systemctl reload nginx
fi
sudo certbot certonly --webroot --webroot-path /var/www/folio-acme \
    --domain folio.flandolf.me --cert-name folio.flandolf.me \
    --email "$email" --agree-tos --non-interactive --keep-until-expiring
config=$(mktemp)
trap 'rm -f "$config"' EXIT
cat deploy/nginx-http.conf deploy/nginx-https.conf > "$config"
sudo install -m 0644 "$config" /etc/nginx/sites-available/folio-releases.conf
sudo ln -sfn /etc/nginx/sites-available/folio-releases.conf /etc/nginx/sites-enabled/folio-releases.conf
sudo nginx -t
sudo systemctl reload nginx
sudo install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
sudo install -m 0755 deploy/renew-nginx.sh /etc/letsencrypt/renewal-hooks/deploy/folio-nginx
sudo systemctl enable --now certbot.timer
curl -fsS --retry 3 --retry-delay 2 https://folio.flandolf.me/healthz
echo "Folio experimental server installed: https://folio.flandolf.me/releases/"
