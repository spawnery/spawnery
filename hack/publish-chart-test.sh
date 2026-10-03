#!/usr/bin/env bash
# Nine cases for hack/publish-chart.sh. None of them reaches a registry: the
# existence check goes through CHART_INSPECT_CMD. Cases needing a different
# HEAD copy the script into a throwaway repository, where it finds its root
# from BASH_SOURCE.
#
# Requires: helm and git. No network and no token.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

sut="$repo_root/hack/publish-chart.sh"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

failures=0
pass() { echo "ok   - $1"; }
fail() {
	echo "FAIL - $1" >&2
	failures=$((failures + 1))
}

# The three answers a registry can give, as commands.
absent="$workdir/inspect-absent.sh"
cat >"$absent" <<'STUB'
#!/usr/bin/env bash
echo "Error: reading manifest 0.0.0 in ghcr.io/spawnery/charts/spawnery: manifest unknown" >&2
exit 1
STUB
present="$workdir/inspect-present.sh"
cat >"$present" <<'STUB'
#!/usr/bin/env bash
echo '{"schemaVersion":2}'
exit 0
STUB
unreadable="$workdir/inspect-unreadable.sh"
cat >"$unreadable" <<'STUB'
#!/usr/bin/env bash
echo "Error: reading manifest: unauthorized: authentication required" >&2
exit 1
STUB
chmod +x "$absent" "$present" "$unreadable"

# From HEAD, as the script reads it, so an uncommitted bump does not break this.
chart_version="$(git show HEAD:charts/spawnery/Chart.yaml \
	| grep -E '^version:' | head -1 | awk '{print $2}')"

make_fixture() {
	local dir="$1"
	mkdir -p "$dir/hack"
	cp "$sut" "$dir/hack/publish-chart.sh"
	mkdir -p "$dir/charts"
	git -C "$repo_root" archive HEAD charts/spawnery | tar -x -C "$dir"
	git -C "$dir" init --quiet
	git -C "$dir" add -A
	git -C "$dir" -c user.name=t -c user.email=t@t commit --quiet -m fixture
}

# ---------------------------------------------------------------------------
# A dry run names the version Chart.yaml carries, and pushes nothing.
out="$(DRY_RUN=1 CHART_INSPECT_CMD="$absent" "$sut" 2>&1)" || out="EXIT $?"
if [[ "$out" == *"would push spawnery-${chart_version}.tgz -> oci://ghcr.io/spawnery/charts/spawnery:${chart_version}"* ]]; then
	pass "a dry run describes the push and names the chart's own version"
else
	fail "a dry run describes the push and names the chart's own version -- got: ${out}"
fi

# ---------------------------------------------------------------------------
# The registry saying "no such tag" is permission to proceed.
status=0
DRY_RUN=1 CHART_INSPECT_CMD="$absent" "$sut" >/dev/null 2>&1 || status=$?
if [ "$status" -eq 0 ]; then
	pass "an absent version proceeds"
else
	fail "an absent version proceeds -- exit ${status}"
fi

# ---------------------------------------------------------------------------
# Already there: exit 3 specifically, because release.yml turns that one into
# a notice and every other non-zero status into a failed release.
status=0
CHART_INSPECT_CMD="$present" "$sut" >/dev/null 2>&1 || status=$?
if [ "$status" -eq 3 ]; then
	pass "a version already on the registry exits 3 and overwrites nothing"
else
	fail "a version already on the registry exits 3 -- exit ${status}"
fi

# ---------------------------------------------------------------------------
# Not knowing is not the same as it not being there (a write:packages-only token).
status=0
out="$(CHART_INSPECT_CMD="$unreadable" "$sut" 2>&1)" || status=$?
if [ "$status" -eq 1 ] && [[ "$out" == *"cannot tell whether"* ]]; then
	pass "an unreadable answer stops the run rather than publishing blind"
else
	fail "an unreadable answer stops the run -- exit ${status}, output: ${out}"
fi

# ---------------------------------------------------------------------------
# FORCE=1 is about overwriting a version, so it gets past a present one.
status=0
DRY_RUN=1 FORCE=1 CHART_INSPECT_CMD="$present" "$sut" >/dev/null 2>&1 || status=$?
if [ "$status" -eq 0 ]; then
	pass "FORCE=1 gets past a version that is already there"
else
	fail "FORCE=1 gets past a version that is already there -- exit ${status}"
fi

# ---------------------------------------------------------------------------
# The chart is packaged from HEAD, not from the working tree.
#
# The helm stub copies what it was handed: the script removes its own mktemp
# directory on the way out.
fixture="$workdir/from-head"
make_fixture "$fixture"
sed -i -E 's|^([[:space:]]*digest:[[:space:]]*)""|\1"sha256:'"$(printf 'a%.0s' {1..64})"'"|' \
	"$fixture/charts/spawnery/values.yaml"
if ! grep -qE '^[[:space:]]*digest:[[:space:]]*"sha256:a+"' "$fixture/charts/spawnery/values.yaml"; then
	fail "fixture setup: the working tree's values.yaml was not given a digest"
fi
# Uncommitted, so a script reading Chart.yaml off disk would publish 7.7.7.
sed -i -E 's|^version: .*|version: 7.7.7|' "$fixture/charts/spawnery/Chart.yaml"

real_helm="$(command -v helm)"
stub_bin="$workdir/bin"
mkdir -p "$stub_bin"
cat >"$stub_bin/helm" <<STUB
#!/usr/bin/env bash
if [ "\$1" = "package" ]; then
	cp -R "\$2" "\$HELM_PACKAGE_CAPTURE"
fi
exec "${real_helm}" "\$@"
STUB
chmod +x "$stub_bin/helm"

capture="$workdir/packaged-from"
status=0
out="$(PATH="$stub_bin:$PATH" HELM_PACKAGE_CAPTURE="$capture" DRY_RUN=1 CHART_INSPECT_CMD="$absent" \
	"$fixture/hack/publish-chart.sh" 2>&1)" || status=$?
if [ "$status" -eq 0 ] && [ -f "$capture/values.yaml" ] &&
	grep -qE '^[[:space:]]*digest:[[:space:]]*""' "$capture/values.yaml"; then
	pass "a working tree carrying a digest does not put one in the artefact"
else
	fail "a working tree carrying a digest does not put one in the artefact -- exit ${status}"
fi
if [[ "$out" == *"spawnery-${chart_version}.tgz"* ]] && [[ "$out" != *7.7.7* ]]; then
	pass "the version is HEAD's as well, not the working tree's"
else
	fail "the version is HEAD's as well, not the working tree's -- got: ${out}"
fi

# ---------------------------------------------------------------------------
# A committed digest refuses.
fixture="$workdir/committed-digest"
make_fixture "$fixture"
sed -i -E 's|^([[:space:]]*digest:[[:space:]]*)""|\1"sha256:'"$(printf 'b%.0s' {1..64})"'"|' \
	"$fixture/charts/spawnery/values.yaml"
git -C "$fixture" add -A
git -C "$fixture" -c user.name=t -c user.email=t@t commit --quiet -m "a digest in the chart"
status=0
out="$(DRY_RUN=1 CHART_INSPECT_CMD="$absent" "$fixture/hack/publish-chart.sh" 2>&1)" || status=$?
if [ "$status" -eq 1 ] && [[ "$out" == *"non-empty image.digest"* ]]; then
	pass "a committed image.digest refuses to publish, and says which line"
else
	fail "a committed image.digest refuses to publish -- exit ${status}, output: ${out}"
fi

# ---------------------------------------------------------------------------
# The version comes from the chart being published and from nothing else.
fixture="$workdir/other-version"
make_fixture "$fixture"
sed -i -E 's|^version: .*|version: 9.9.9|' "$fixture/charts/spawnery/Chart.yaml"
git -C "$fixture" add -A
git -C "$fixture" -c user.name=t -c user.email=t@t commit --quiet -m "some other version"
out="$(DRY_RUN=1 CHART_INSPECT_CMD="$absent" "$fixture/hack/publish-chart.sh" 2>&1)" || out="EXIT $?"
if [[ "$out" == *"spawnery-9.9.9.tgz -> oci://ghcr.io/spawnery/charts/spawnery:9.9.9"* ]]; then
	pass "the pushed version is the one Chart.yaml at HEAD names"
else
	fail "the pushed version is the one Chart.yaml at HEAD names -- got: ${out}"
fi

echo
if [ "$failures" -ne 0 ]; then
	echo "${failures} failing case(s)" >&2
	exit 1
fi
echo "all cases passed"
