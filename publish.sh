#!/usr/bin/env bash
# Publish a signed local release, reading the real package/version from the APK.
set -euo pipefail
cd "$(dirname "$0")/.."
apk="${1:-app/build/outputs/apk/release/app-release.apk}"
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT
# Verify and publish the same bytes even if another Gradle build replaces its output.
cp -- "$apk" "$staging/build.apk"
apk="$staging/build.apk"
sdk="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/android-sdk}}"
tools="${FOLIO_BUILD_TOOLS:-$sdk/build-tools/36.0.0}"
export JAVA_HOME="${JAVA_HOME:-/usr/lib/jvm/java-17-openjdk-arm64}"

# Hash before verification and check again in the privileged copy operation.
verified_sha256=$(sha256sum -- "$apk")
verified_sha256=${verified_sha256%% *}
if [[ -n "${FOLIO_EXPECTED_APK_SHA256:-}" && "$verified_sha256" != "$FOLIO_EXPECTED_APK_SHA256" ]]; then
    echo "The release APK changed after build verification; rerun build.sh before publishing." >&2; exit 1
fi
signers=$("$tools/apksigner" verify --print-certs "$apk")
printf '%s\n' "$signers"
badging=$("$tools/aapt" dump badging "$apk")
package=$(printf '%s\n' "$badging" | sed -n "s/^package: name='\([^']*\)'.*/\1/p")
code=$(printf '%s\n' "$badging" | sed -n "s/^package: .*versionCode='\([^']*\)'.*/\1/p")
version=$(printf '%s\n' "$badging" | sed -n "s/^package: .*versionName='\([^']*\)'.*/\1/p")
if [[ "$package" != com.folio.notes || ! "$code" =~ ^[0-9]+$ || ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-exp\.[1-9][0-9]{0,3})?$ ]]; then
    echo "Expected a signed com.folio.notes APK with a Folio version" >&2; exit 1
fi
if [[ -n "${FOLIO_EXPECTED_VERSION_CODE:-}" && "$code" != "$FOLIO_EXPECTED_VERSION_CODE" ]]; then
    echo "The release APK was replaced by another build; rerun build.sh before publishing." >&2; exit 1
fi

if [[ -n "${FOLIO_EXPECTED_VERSION_NAME:-}" && "$version" != "$FOLIO_EXPECTED_VERSION_NAME" ]]; then
    echo "The release APK version name differs from the reserved build; rerun build.sh before publishing." >&2; exit 1
fi

# Compare to the certificate pinned during setup, so Android can install over stable.
pin=/etc/folio-releases/signing-cert.sha256
if [[ ! -f "$pin" ]]; then echo "Missing signing certificate pin: $pin" >&2; exit 1; fi
digest=$(printf '%s\n' "$signers" | sed -n 's/^Signer #1 certificate SHA-256 digest: //p')
if [[ ! "$digest" =~ ^[0-9a-f]{64}$ || "$digest" != "$(cat "$pin")" ]]; then echo "APK does not use the pinned Folio release certificate" >&2; exit 1; fi
sudo /usr/local/bin/folio-release-server publish -apk "$apk" -version-name "$version" -version-code "$code" -expected-sha256 "$verified_sha256"
curl -fsS https://folio.flandolf.me/releases/latest.json
printf '\n'
