#!/usr/bin/env bash
# Publish a signed local release, reading the real package/version from the APK.
# Usage: publish.sh [-c release-server.conf] [path/to.apk]
# Run from your app's repo (the config and default APK path are relative to it).
# Optional guards from a build script: RELEASE_EXPECTED_APK_SHA256,
# RELEASE_EXPECTED_VERSION_CODE, RELEASE_EXPECTED_VERSION_NAME, RELEASE_BUILD_TOOLS.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
config=""
if [[ "${1:-}" == "-c" ]]; then config="${2:?-c needs a file}"; shift 2; fi
source "$here/deploy/load-config.sh"
load_config "$config"
: "${PACKAGE:?PACKAGE (the applicationId) is required in the config to publish}"
apk="${1:-$DEFAULT_APK}"
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT
# Verify and publish the same bytes even if another build replaces its output.
cp -- "$apk" "$staging/build.apk"
apk="$staging/build.apk"
tools="$BUILD_TOOLS"
[[ -n "${JAVA_HOME:-}" ]] && export JAVA_HOME

# Hash before verification and check again in the privileged copy operation.
verified_sha256=$(sha256sum -- "$apk")
verified_sha256=${verified_sha256%% *}
if [[ -n "${RELEASE_EXPECTED_APK_SHA256:-}" && "$verified_sha256" != "$RELEASE_EXPECTED_APK_SHA256" ]]; then
    echo "The release APK changed after build verification; rebuild before publishing." >&2; exit 1
fi
signers=$("$tools/apksigner" verify --print-certs "$apk")
printf '%s\n' "$signers"
badging=$("$tools/aapt" dump badging "$apk")
package=$(printf '%s\n' "$badging" | sed -n "s/^package: name='\([^']*\)'.*/\1/p")
code=$(printf '%s\n' "$badging" | sed -n "s/^package: .*versionCode='\([^']*\)'.*/\1/p")
version=$(printf '%s\n' "$badging" | sed -n "s/^package: .*versionName='\([^']*\)'.*/\1/p")
if [[ "$package" != "$PACKAGE" || ! "$code" =~ ^[0-9]+$ || -z "$version" ]]; then
    echo "Expected a signed $PACKAGE APK with a numeric versionCode and a versionName" >&2; exit 1
fi
if [[ "$VERSION_SCHEME" == folio && ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-exp\.[1-9][0-9]{0,3})?$ ]]; then
    echo "versionName $version does not match the folio X.Y.Z[-exp.N] scheme" >&2; exit 1
fi
if [[ -n "${RELEASE_EXPECTED_VERSION_CODE:-}" && "$code" != "$RELEASE_EXPECTED_VERSION_CODE" ]]; then
    echo "The release APK was replaced by another build; rebuild before publishing." >&2; exit 1
fi
if [[ -n "${RELEASE_EXPECTED_VERSION_NAME:-}" && "$version" != "$RELEASE_EXPECTED_VERSION_NAME" ]]; then
    echo "The release APK version name differs from the reserved build; rebuild before publishing." >&2; exit 1
fi

# Compare to the certificate pinned during setup, so Android can install over stable.
pin="$CONFIG_DIR/signing-cert.sha256"
if [[ ! -f "$pin" ]]; then echo "Missing signing certificate pin: $pin (run pin-cert.sh)" >&2; exit 1; fi
digest=$(printf '%s\n' "$signers" | sed -n 's/^Signer #1 certificate SHA-256 digest: //p')
if [[ ! "$digest" =~ ^[0-9a-f]{64}$ || "$digest" != "$(cat "$pin")" ]]; then echo "APK does not use the pinned $NAME release certificate" >&2; exit 1; fi
sudo env RELEASE_DATA="$DATA_DIR" RELEASE_BASE_URL="$BASE_URL" RELEASE_SLUG="$SLUG" RELEASE_NAME="$NAME" \
    RELEASE_VERSION_SCHEME="$VERSION_SCHEME" RELEASE_MAX_APK_MB="$MAX_APK_MB" \
    "$BINARY" publish -apk "$apk" -version-name "$version" -version-code "$code" -expected-sha256 "$verified_sha256"
curl -fsS "$BASE_URL/releases/latest.json"
printf '\n'
