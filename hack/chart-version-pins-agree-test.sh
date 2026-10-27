#!/usr/bin/env bash
# Drives hack/chart-version-pins-agree.sh through the disagreements this tree
# does not contain.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
check="$root/hack/chart-version-pins-agree.sh"
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

run  "a release moved the chart" 1 "$check" --chart-version 9.9.9
says "it names the README" "README.md:" "$check" --chart-version 9.9.9
says "it names the getting-started page" "docs/getting-started/index.md:" \
  "$check" --chart-version 9.9.9
says "it names the chart's README" "charts/spawnery/README.md:" \
  "$check" --chart-version 9.9.9

cat > "$tmp/continued.md" <<'EOF'
```bash
helm install spawnery oci://ghcr.io/spawnery/charts/spawnery \
  --version 0.2.15 \
  --namespace spawnery-system --create-namespace
```
EOF
run  "a stale version on a continuation line" 1 \
  "$check" --chart-version 0.30.0 --manifest "$tmp/continued.md"
says "it names the found version and the command's first line" "continued.md:2 installs the chart with --version 0.2.15" \
  "$check" --chart-version 0.30.0 --manifest "$tmp/continued.md"
run  "the same command on the current version" 0 \
  "$check" --chart-version 0.2.15 --manifest "$tmp/continued.md"

cat > "$tmp/upgrade.md" <<'EOF'
helm upgrade spawnery oci://ghcr.io/spawnery/charts/spawnery --version=0.29.0 --reuse-values
EOF
run "an upgrade written with --version=" 1 \
  "$check" --chart-version 0.30.0 --manifest "$tmp/upgrade.md"

cat > "$tmp/other-chart.md" <<'EOF'
helm install spawnery oci://ghcr.io/spawnery/charts/spawnery --version 0.30.0
helm install prometheus prometheus-community/kube-prometheus-stack \
  --version 61.3.2
kind --version 0.24.0
EOF
run "another chart's --version is not the spawnery chart's" 0 \
  "$check" --chart-version 0.30.0 --manifest "$tmp/other-chart.md"

cat > "$tmp/Chart.yaml" <<'EOF'
apiVersion: v2
name: spawnery
version: "0.29.0"
appVersion: "0.30.0"
EOF
run "the version is read from Chart.yaml, quoted or not" 1 \
  "$check" --chart "$tmp/Chart.yaml" --manifest "$tmp/other-chart.md"
says "and appVersion is not mistaken for it" "Chart.yaml is 0.29.0" \
  "$check" --chart "$tmp/Chart.yaml" --manifest "$tmp/other-chart.md"

cat > "$tmp/no-install.md" <<'EOF'
`--version` takes the chart's own number.
EOF
run  "a file installing nothing" 1 "$check" --chart-version 0.30.0 --manifest "$tmp/no-install.md"
says "and says why" "would pass vacuously" \
  "$check" --chart-version 0.30.0 --manifest "$tmp/no-install.md"

run "a file that does not exist" 1 "$check" --manifest "$tmp/absent.md"
run "a Chart.yaml that does not exist" 1 "$check" --chart "$tmp/absent.yaml"
run "an unknown argument" 1 "$check" --wat

if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all cases passed"
