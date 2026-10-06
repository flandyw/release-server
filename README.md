# release-server

A small, read-only Go server (standard library only) plus a local publisher for
distributing **signed Android APKs from your own VPS**: an opt-in beta/experimental
channel next to your stable store or GitHub releases. There is no upload API or
publishing secret; you publish locally with `sudo`, and the HTTP process runs in a
read-only systemd sandbox behind Nginx.

One checkout can serve any number of apps: each app has a `release-server.conf`
(slug, domain, package, …) and gets its own service, port, Nginx site and storage.

## Quick start (new app)

Requires Linux, Go, Nginx, Certbot, systemd, curl, sudo, `envsubst` (gettext-base) and,
for publishing, Android build-tools (`apksigner`, `aapt`). DNS for the domain must point at the VPS.

```sh
# in your app's repo (add this repo as a submodule, or clone it anywhere)
git submodule add https://github.com/flandyw/release-server.git release-server
cp release-server/release-server.conf.example release-server.conf   # edit SLUG, DOMAIN, PACKAGE, NAME

./release-server/install.sh your-email@example.com        # build, test, install service + Nginx + TLS
./release-server/pin-cert.sh path/to/trusted-signed.apk   # pin your release signing cert
./release-server/publish.sh app/build/outputs/apk/release/app-release.apk
```

All scripts read `./release-server.conf` (override with `-c FILE` or `RELEASE_SERVER_CONFIG`).
Re-running `install.sh` upgrades the binary and keeps a backup of that app's Nginx config.
Other Nginx sites are never touched.

## Configuration

Only `SLUG` and `DOMAIN` are required (plus `PACKAGE` to publish). See
[`release-server.conf.example`](release-server.conf.example) for every key.

| Key | Default | Meaning |
| --- | --- | --- |
| `SLUG` | – | lowercase id; APKs are `<slug>-<version>.apk` |
| `DOMAIN` | – | public host (served over HTTPS at `https://DOMAIN`) |
| `PACKAGE` | – | applicationId `publish.sh` requires |
| `NAME`, `CHANNEL` | `SLUG`, `experimental` | page text: "`NAME` `CHANNEL` builds" |
| `SOURCE_URL`, `STABLE_URL` | empty | optional links on the download page |
| `VERSION_SCHEME` | `any` | `any`: any safe versionName, positive versionCode. `folio`: `X.Y.Z[-exp.N]` with code = commitCount·10000+N |
| `MAX_APK_MB` | `100` | APK size limit |
| `CLOUDFLARE` | `0` | `1` trusts Cloudflare's ranges to restore visitor IPs for rate limiting |
| `PORT` | `8787` | loopback port (give each app on one VPS its own) |
| `SERVICE` | `SLUG-releases` | systemd unit, system user, `/etc/SERVICE`, `/var/lib/SERVICE`, Nginx site/logs |
| `BINARY`, `DATA_DIR`, `CONFIG_DIR`, `ACME_DIR` | derived | host paths |
| `DEFAULT_APK`, `BUILD_TOOLS`, `ANDROID_HOME`, `JAVA_HOME` | Gradle default / `$ANDROID_HOME/build-tools/36.0.0` | publish-time tooling |

The Go binary takes the same settings as flags or `RELEASE_*` environment variables
(`-data -base-url -slug -name -channel -source-url -stable-url -version-scheme
-max-apk-mb -listen`); `install.sh` writes them to `/etc/SERVICE/server.env` for systemd.
`base-url` must be a bare HTTPS origin.

## Publishing

`publish.sh [-c conf] [apk]` copies the APK to a private snapshot, hashes it, runs
`apksigner verify`, reads the real package/versionCode/versionName with `aapt`,
requires `PACKAGE`, requires the signing certificate to equal the one pinned in
`/etc/SERVICE/signing-cert.sha256` (so Android can update over your stable builds),
then runs `sudo BINARY publish …`, which re-checks the SHA-256 on the bytes it copies.

A build script can add guards through `RELEASE_EXPECTED_APK_SHA256`,
`RELEASE_EXPECTED_VERSION_CODE` and `RELEASE_EXPECTED_VERSION_NAME`; a mismatch aborts.
`RELEASE_BUILD_TOOLS` overrides the build-tools directory.

Releases live in `DATA_DIR/v<version>/` (APK, `SHA256SUMS`, `release.json`). A temporary
directory holds the full release until an atomic rename; `latest.json` changes atomically
afterwards. A file lock serializes publishers, publishing an older version never
downgrades `latest`, republishing identical bytes is safe, and changing an existing
version (or reusing a versionCode for another version) fails. Keep old directories so
cached update URLs stay valid, and back up `DATA_DIR`. Storage must stay root-owned
and unwritable by the service.

## Endpoints

| Path | Behavior |
| --- | --- |
| `/` | redirect to `/releases/` |
| `/releases/` | download history page |
| `/releases/latest.json` | updater metadata (404 when empty, 503 if corrupt) |
| `/releases/latest`, `/releases/latest.apk` | redirect to the newest build's page / immutable APK |
| `/releases/v<version>/` | one build's page |
| `/releases/v<version>/<slug>-<version>.apk` | APK (HEAD, single byte ranges, ETag) |
| `/releases/v<version>/SHA256SUMS` | `<sha256>  <apk name>` |
| `/releases/v<version>/release.json` | that build's metadata |
| `/healthz` | liveness |

`latest.json` is GitHub-release-shaped (`tag_name`, `assets[].browser_download_url`,
`assets[].digest`, …) plus explicit `version_code` / `version_name`, so an app updater
can reuse a GitHub-releases parser. Your app must check the SHA-256 before installing
and let Android verify the signature; keep the updater's trusted host list in sync
with `DOMAIN`.

Hardening: GET/HEAD only, no request bodies, no multipart ranges, reads confined to
the storage root (no symlink/FIFO escapes), manifests re-validated on every read
(official URLs, digests, sizes), 128 concurrent requests, strict CSP/no-sniff/
anti-framing headers; Nginx adds HSTS, 20 req/s per IP (burst 40), 60 s stall timeouts.
With `CLOUDFLARE=1`, review the proxy ranges in `deploy/nginx-cloudflare.conf` when
Cloudflare changes them (use Full (strict) TLS and don't cache `latest.json`).

## Develop

```sh
go test -race ./... && go vet ./...
mkdir -p build && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/release-server .
mkdir -p /tmp/releases
RELEASE_BASE_URL=https://updates.example.com RELEASE_SLUG=myapp \
  ./build/release-server serve -data /tmp/releases -listen 127.0.0.1:8787
```

## Operations

```sh
sudo systemctl status SERVICE
sudo journalctl -u SERVICE -n 50 --no-pager
curl -f https://DOMAIN/releases/latest.json
sudo certbot renew --cert-name DOMAIN --dry-run --no-random-sleep-on-renew
```
