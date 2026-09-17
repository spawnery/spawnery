#!/usr/bin/env bash
# Screenshots of the built documentation site at desktop and phone width in
# both colour schemes, for judging the theme without a browser at hand.
#
# Usage: hack/docs-screenshots.sh <out-dir> [page ...]
# A page is a site path such as "tutorial/"; "" is the home page.
set -euo pipefail

out="${1:?usage: hack/docs-screenshots.sh <out-dir> [page ...]}"
case "$out" in
  /*) ;;
  *) out="$PWD/$out" ;;
esac
shift

cd "$(dirname "$0")/.."
pages=("$@")
[ "${#pages[@]}" -gt 0 ] || pages=("" "tutorial/" "guides/scaling-and-boosts/" "reference/crds/")

site="$(nix build --no-link --print-out-paths .#docs-site)"
chromium="$(nix build --no-link --print-out-paths --inputs-from . nixpkgs#chromium)/bin/chromium"

port=8317
if curl -fsS "http://127.0.0.1:$port/" >/dev/null 2>&1; then
  echo "hack/docs-screenshots.sh: something already answers on 127.0.0.1:$port; stop it first" >&2
  exit 1
fi

python3 -m http.server --bind 127.0.0.1 --directory "$site" "$port" >/dev/null 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true' EXIT

deadline=$((SECONDS + 10))
until curl -fsS "http://127.0.0.1:$port/" >/dev/null 2>&1; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "hack/docs-screenshots.sh: server on 127.0.0.1:$port did not come up within 10s" >&2
    exit 1
  fi
  sleep 0.2
done

mkdir -p "$out"
for page in "${pages[@]}"; do
  name="${page%/}"
  name="${name//\//_}"
  name="${name:-home}"
  for scheme in latte mocha; do
    # Blink's PreferredColorScheme enum: 0 is dark, 1 is light.
    if [ "$scheme" = mocha ]; then pref=0; else pref=1; fi
    for size in 1440,900 390,844; do
      "$chromium" --headless --no-sandbox --disable-gpu --hide-scrollbars \
        --blink-settings=preferredColorScheme="$pref" \
        --window-size="$size" --virtual-time-budget=5000 \
        --screenshot="$out/$name-$scheme-${size%%,*}.png" \
        "http://127.0.0.1:$port/$page" >/dev/null 2>&1
    done
  done
done
ls "$out"
