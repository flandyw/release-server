# Sourced by install.sh/publish.sh/pin-cert.sh. Loads the app's release-server.conf
# and derives every host-side name from it. Config is plain KEY=value shell.
# Lookup: $1 (if given), $RELEASE_SERVER_CONFIG, then ./release-server.conf.
load_config() {
    local file="${1:-${RELEASE_SERVER_CONFIG:-./release-server.conf}}"
    if [[ ! -f "$file" ]]; then
        echo "Config not found: $file (copy release-server.conf.example, or pass -c FILE)" >&2
        return 1
    fi
    # shellcheck disable=SC1090
    source "$file"
    : "${SLUG:?SLUG is required in $file}"
    : "${DOMAIN:?DOMAIN is required in $file}"
    NAME="${NAME:-$SLUG}"
    CHANNEL="${CHANNEL:-experimental}"
    VERSION_SCHEME="${VERSION_SCHEME:-any}"
    SOURCE_URL="${SOURCE_URL:-}"
    STABLE_URL="${STABLE_URL:-}"
    MAX_APK_MB="${MAX_APK_MB:-100}"
    PORT="${PORT:-8787}"
    CLOUDFLARE="${CLOUDFLARE:-0}"
    SERVICE="${SERVICE:-$SLUG-releases}"
    BINARY="${BINARY:-/usr/local/bin/$SLUG-release-server}"
    DATA_DIR="${DATA_DIR:-/var/lib/$SERVICE}"
    CONFIG_DIR="${CONFIG_DIR:-/etc/$SERVICE}"
    ENV_FILE="${ENV_FILE:-$CONFIG_DIR/server.env}"
    ACME_DIR="${ACME_DIR:-/var/www/$SLUG-acme}"
    NGINX_CONF="${NGINX_CONF:-$SERVICE.conf}"
    DEFAULT_APK="${DEFAULT_APK:-app/build/outputs/apk/release/app-release.apk}"
    ANDROID_HOME="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/android-sdk}}"
    BUILD_TOOLS="${RELEASE_BUILD_TOOLS:-${BUILD_TOOLS:-$ANDROID_HOME/build-tools/36.0.0}}"
    BASE_URL="https://$DOMAIN"
    # Values end up in flags, systemd and Nginx; keep them boring.
    [[ "$SLUG" =~ ^[a-z0-9][a-z0-9-]{0,39}$ ]] || { echo "Invalid SLUG: $SLUG" >&2; return 1; }
    [[ "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || { echo "Invalid DOMAIN: $DOMAIN" >&2; return 1; }
    [[ "$PORT" =~ ^[0-9]{2,5}$ ]] || { echo "Invalid PORT: $PORT" >&2; return 1; }
    [[ "$SERVICE" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || { echo "Invalid SERVICE: $SERVICE" >&2; return 1; }
    case "$NAME$CHANNEL$SOURCE_URL$STABLE_URL" in *$'\n'*|*'"'*|*"'"*|*'\'*|*'$'*|*'`'*) echo "NAME/CHANNEL/URLs may not contain quotes, backslashes, \$, backticks or newlines" >&2; return 1;; esac
}

# Render a deploy template, substituting only our own variables (Nginx uses $vars too).
render() {
    SLUG="$SLUG" NAME="$NAME" DOMAIN="$DOMAIN" PORT="$PORT" SERVICE="$SERVICE" \
        BINARY="$BINARY" ENV_FILE="$ENV_FILE" ACME_DIR="$ACME_DIR" REAL_IP_BLOCK="${REAL_IP_BLOCK:-}" \
        envsubst '${SLUG} ${NAME} ${DOMAIN} ${PORT} ${SERVICE} ${BINARY} ${ENV_FILE} ${ACME_DIR} ${REAL_IP_BLOCK}' < "$1"
}
