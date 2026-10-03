#!/usr/bin/env sh
# Downloads the pinned frontend libraries into internal/web/static/vendor/.
# They are committed and embedded in the binary, so the app needs no CDN at runtime.
#
# To upgrade: bump a version below, run `sh scripts/vendor.sh`, test, commit.
set -eu

HTMX=2.0.11
ALPINE=3.17.4
PICO=2.1.1
INTER=5.3.0   # @fontsource-variable/inter

CDN=https://cdn.jsdelivr.net/npm
DIR="$(cd "$(dirname "$0")/.." && pwd)/internal/web/static/vendor"
mkdir -p "$DIR"

fetch() { # url dest
  echo "  $2"
  curl -fsSL "$1" -o "$DIR/$2"
}

echo "Vendoring into $DIR"
fetch "$CDN/htmx.org@$HTMX/dist/htmx.min.js"                                         htmx.min.js
fetch "$CDN/alpinejs@$ALPINE/dist/cdn.min.js"                                        alpine.min.js
fetch "$CDN/@picocss/pico@$PICO/css/pico.min.css"                                    pico.min.css
fetch "$CDN/@fontsource-variable/inter@$INTER/files/inter-latin-wght-normal.woff2"     inter-latin.woff2
fetch "$CDN/@fontsource-variable/inter@$INTER/files/inter-latin-ext-wght-normal.woff2" inter-latin-ext.woff2

cat > "$DIR/VERSIONS" <<EOF
htmx.org                    $HTMX     BSD Zero Clause   https://github.com/bigskysoftware/htmx
alpinejs                    $ALPINE    MIT               https://github.com/alpinejs/alpine
@picocss/pico               $PICO     MIT               https://github.com/picocss/pico
@fontsource-variable/inter  $INTER     OFL-1.1 (font)    https://github.com/rsms/inter
EOF
echo "Done."
