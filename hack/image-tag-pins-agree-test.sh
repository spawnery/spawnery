#!/usr/bin/env bash
# Drives hack/image-tag-pins-agree.sh through the disagreements this tree
# does not contain.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
check="$root/hack/image-tag-pins-agree.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

failures=0
run() {
  local name="$1" want="$2"; shift 2
  local out status
  out="$("$@" 2>&1)" && status=0 || status=$?
  if [ "$status" -ne "$want" ]; then
    echo "FAIL $name: exit $status, want $want"
    printf '%s\n' "$out" | sed 's/^/    /'
    failures=$((failures + 1))
  else
    echo "ok   $name"
  fi
}
says() {
  local name="$1" needle="$2"; shift 2
  local out
  out="$("$@" 2>&1)" || true
  case "$out" in
    *"$needle"*) echo "ok   $name" ;;
    *) echo "FAIL $name: output does not mention '$needle'"
       printf '%s\n' "$out" | sed 's/^/    /'
       failures=$((failures + 1)) ;;
  esac
}

run "the tree agrees" 0 "$check"

run  "a release moved imageVersion" 1 "$check" --image-version 9.9.9
says "it names the tutorial manifest" "docs/tutorial/network.yaml pins" \
  "$check" --image-version 9.9.9
says "it names the sample" "config/samples/network.yaml pins" \
  "$check" --image-version 9.9.9

cat > "$tmp/stale.yaml" <<'EOF'
image: ghcr.io/spawnery/purpur:26.2-0.2.15
EOF
run  "a manifest pinned behind the others" 1 \
  "$check" --image-version 0.2.34 --purpur-version 26.2 --manifest "$tmp/stale.yaml"
says "it names the found tag" "26.2-0.2.15" \
  "$check" --image-version 0.2.34 --purpur-version 26.2 --manifest "$tmp/stale.yaml"

cat > "$tmp/old-minecraft.yaml" <<'EOF'
image: ghcr.io/spawnery/purpur:26.2-0.2.34
EOF
run  "a tag left on the previous Minecraft version" 1 \
  "$check" --image-version 0.2.34 --purpur-version 26.3 --manifest "$tmp/old-minecraft.yaml"
says "it names the upstream version" "upstream version 26.2" \
  "$check" --image-version 0.2.34 --purpur-version 26.3 --manifest "$tmp/old-minecraft.yaml"

cat > "$tmp/velocity-shape.yaml" <<'EOF'
image: ghcr.io/spawnery/velocity:3.5.1-0.2.34
EOF
run "a two-dot upstream version parses like a one-dot one" 0 \
  "$check" --image-version 0.2.34 --velocity-version 3.5.1 --manifest "$tmp/velocity-shape.yaml"

cat > "$tmp/no-image.yaml" <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: nothing-to-see-here
EOF
run  "a manifest naming no image" 1 \
  "$check" --manifest "$tmp/no-image.yaml"
says "and says why" "would pass vacuously" \
  "$check" --manifest "$tmp/no-image.yaml"

run "a manifest that does not exist" 1 "$check" --manifest "$tmp/absent.yaml"
run "a flake.nix that does not exist" 1 "$check" --flake "$tmp/absent.nix"

run "an unknown argument" 1 "$check" --wat

if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all cases passed"
