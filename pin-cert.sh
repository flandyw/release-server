#!/usr/bin/env bash
# Pin the release signing certificate publish.sh will require.
# Usage: pin-cert.sh [-c release-server.conf] path/to/trusted-signed.apk
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
config=""
if [[ "${1:-}" == "-c" ]]; then config="${2:?-c needs a file}"; shift 2; fi
apk="${1:?Usage: pin-cert.sh [-c release-server.conf] trusted-signed.apk}"
source "$here/deploy/load-config.sh"
load_config "$config"
[[ -n "${JAVA_HOME:-}" ]] && export JAVA_HOME
digest=$("$BUILD_TOOLS/apksigner" verify --print-certs "$apk" | sed -n 's/^Signer #1 certificate SHA-256 digest: //p')
[[ "$digest" =~ ^[0-9a-f]{64}$ ]] || { echo "Could not read a certificate digest from $apk" >&2; exit 1; }
sudo install -d -m 0755 "$CONFIG_DIR"
printf '%s\n' "$digest" | sudo tee "$CONFIG_DIR/signing-cert.sha256" >/dev/null
sudo chmod 0644 "$CONFIG_DIR/signing-cert.sha256"
echo "Pinned $digest in $CONFIG_DIR/signing-cert.sha256"
