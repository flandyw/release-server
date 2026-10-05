# Folio experimental update server

This Go service serves the **opt-in experimental channel** at
<https://folio.flandolf.me/releases/>. Stable updates remain on GitHub Releases.
It does not poll GitHub or upload builds there.

In Folio, open **Settings → Account & updates → Enable experimental builds**, then
**Check Folio server**. The toggle defaults to off and persists across launches.
Automatic checks use the selected source. Each source has separate check timestamps
and retry cooldowns. Switching off returns to GitHub; Android will install the next
stable release whose version code exceeds the installed experimental build.

## Build and test

Go 1.24 or newer; Linux. The binary uses only Go's standard library.

```sh
cd release-server
go test -race ./...
go vet ./...
mkdir -p build
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/folio-release-server .
mkdir -p /tmp/folio-releases
./build/folio-release-server serve -data /tmp/folio-releases -listen 127.0.0.1:8787
```

## VPS installation

Nginx, Certbot, systemd, curl, sudo and Go must already be available; DNS must point
`folio.flandolf.me` to this VPS and ports 80/443 must be reachable. Cloudflare can
proxy the hostname; use **Full (strict)** TLS and avoid caching the mutable JSON
endpoint or putting browser challenges in front of the update/download paths.

```sh
./release-server/install.sh your-email@example.com
```

The installer builds/tests, installs `/usr/local/bin/folio-release-server`, creates
the unprivileged `folio-releases` account, enables/restarts
`folio-releases.service`, and sets up a dedicated Nginx host. Other Nginx hosts are
left in place. Go listens only on `127.0.0.1:8787`; no new public application port
is needed. The service filesystem is read-only. Publishing runs locally with sudo.

Certbot uses `/var/www/folio-acme` for HTTP-01, stores the certificate in
`/etc/letsencrypt/live/folio.flandolf.me`, and renews through `certbot.timer`.
`deploy/renew-nginx.sh` reloads Nginx after successful renewal. Re-running the
installer upgrades the binary and keeps a backup of this host's Nginx config.

## Publish an experimental build

Use the **same signing key as the stable GitHub APK**. During setup, pin its
certificate fingerprint in `/etc/folio-releases/signing-cert.sha256`. This file
contains one lowercase SHA-256 hex string, not a private key. To provision a new
server, download a trusted official stable APK and run:

```sh
export JAVA_HOME=/usr/lib/jvm/java-17-openjdk-arm64
export ANDROID_HOME="$HOME/android-sdk"
sudo install -d -m 0755 /etc/folio-releases
"$ANDROID_HOME/build-tools/36.0.0/apksigner" verify --print-certs official-stable.apk \
  | sed -n 's/^Signer #1 certificate SHA-256 digest: //p' \
  | sudo tee /etc/folio-releases/signing-cert.sha256 >/dev/null
sudo chmod 0644 /etc/folio-releases/signing-cert.sha256
```

Build a signed release with the repository's documented signing environment. On
this VPS, `build.sh` loads `.signing/` and asks whether to publish after a successful
build. Answer `y` to publish, or `n`/Enter to keep the APK local:

```sh
bash ./build.sh
# Or publish an already-built signed APK separately:
./release-server/publish.sh /absolute/path/to/app-release.apk
```

`publish.sh` verifies the APK signature and pinned certificate, reads the package
and versions directly with `aapt`, requires `com.folio.notes`, and publishes with
the actual embedded versionCode/versionName. It defaults to Android SDK build tools
36.0.0; override their directory with `FOLIO_BUILD_TOOLS`. These SDK tools are only
needed for publishing, not for the server or Go build.

Releases live in `/var/lib/folio-releases/vX.Y.Z-exp.N/` with an APK, `SHA256SUMS`, and
`release.json`. A temporary directory holds the complete release until rename;
the latest manifest changes atomically after that. A file lock serializes publishers,
and publishing an older version never downgrades the latest pointer. Re-publishing
identical bytes is safe; changing an existing version fails. Retain old directories
so cached update URLs remain usable. Back up `/var/lib/folio-releases`.

Stable version codes are `commitCount * 10000`. Every run of `build.sh` reserves a
local build number from 1 to 9999 and adds it to that base; the display name is
`X.Y.Z-exp.N`. For example, commit count 217 gives stable code 2,170,000 and
experimental codes 2,170,001, 2,170,002, etc. The next stable commit's code,
2,180,000, supersedes every experimental build from count 217. You can edit and
build repeatedly on the VPS without making commits between experimental builds.

The high-water code is saved atomically in ignored `.tooling/experimental-version.json`.
The allocator also reads the server's latest manifest and the last built release
APK's `output-metadata.json`, so deleting its state does not reuse a published or
successfully built version. Both allocation and the full build/publish flow are
locked. Failed or unpublished attempts can leave harmless gaps. Exhausting 9999
builds, or checking out an older commit than your last build, stops with a clear
error; advance to a newer commit in that case. Python 3 and `flock` are required
by the VPS build helper. To run the counter/prompt checks: `python3 tools/build-version-smoke.py`.

GitHub releases keep their readable `vX.Y.Z` tags and include `update.json` containing
the APK's actual version code, version name, and tag. The updater reads that asset
before comparing versions; older releases without it retain the legacy tag rules.
Existing server releases such as `/releases/v2.1.7/` remain downloadable. The new
experimental APK can update an installed legacy code-217 APK using the same key.

The binary also exposes a trusted local publishing primitive (the wrapper is the
recommended entry point because it checks signing and the embedded APK versions):

```sh
sudo /usr/local/bin/folio-release-server publish \
  -apk /path/to/signed.apk -version-name 2.1.7-exp.1 -version-code 2170001
```

## Endpoints and operations

| Path | Behavior |
| --- | --- |
| `/` | Redirect to `/releases/` |
| `/releases/` | Experimental download history; link to stable GitHub releases |
| `/releases/latest.json` | Updater metadata; 404 when empty, 503 on corrupt metadata |
| `/releases/latest` | Redirect to the newest experimental build's page |
| `/releases/latest.apk` | Redirect to its immutable APK path |
| `/releases/vX.Y.Z-exp.N/` | One build's download page |
| `/releases/vX.Y.Z-exp.N/folio-X.Y.Z-exp.N.apk` | APK, supports HEAD, byte ranges and ETag |
| `/releases/vX.Y.Z-exp.N/SHA256SUMS` | SHA-256 checksum in the existing updater's format |
| `/releases/vX.Y.Z-exp.N/release.json` | That build's metadata |
| `/healthz` | Service liveness |

All endpoints accept only GET/HEAD. There is no network upload API or publishing
secret. The app requires HTTPS on official hosts, checks SHA-256 before exposing
an APK to the installer, and Android verifies its signing identity.

```sh
sudo systemctl status folio-releases
sudo journalctl -u folio-releases -n 50 --no-pager
sudo systemctl restart folio-releases
curl -f https://folio.flandolf.me/healthz
curl -f https://folio.flandolf.me/releases/latest.json
sudo certbot renew --cert-name folio.flandolf.me --dry-run --no-random-sleep-on-renew
```

Android verification: `./gradlew :app:assembleDebug :app:lintDebug :app:prepareBackupSmoke`, then
`node tools/update-smoke.cjs` (optionally pass a downloaded server manifest path).
The smoke check exercises the actual app parser, stable/experimental source
selection, independent cooldown keys, trusted URLs and version ordering on the JVM.
On a device, verify the toggle persists, each check uses the selected source, and
a newer signed experimental build downloads and installs over stable without
removing notebooks.
