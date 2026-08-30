#!/usr/bin/env bash
# Nine cases for hack/publish-chart.sh. None of them reaches a registry.
#
# The existence check goes through CHART_INSPECT_CMD -- the seam that script's
# header documents -- because the three answers that matter ("it is there",
# "it is not there", "I could not tell") cannot be summoned to order from
# ghcr.io, and a test that needed a token to distinguish them would not run
# where the rest of `make` does. Everything else is real: `git archive`, `helm
# package` and the packaged tarball are the ones a release would produce.
#
# Five cases run against this repository. The other four need a HEAD that this
# repository does not have and must not be given -- a committed image.digest,
# a Chart.yaml at some other version -- so they build a throwaway git
# repository, copy the script under test into it, and run it there. The script
# finds its own repository root from BASH_SOURCE, so a copy in a fixture's
# hack/ directory is the real script operating on the fixture's HEAD, not a
# reimplementation of it.
#
# Requires: helm and git, both in the dev shell. No network and no token.
# Run it with `make publish-chart-test`.
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

# The three answers a registry can give, as commands. Each is handed the
# docker:// reference the script built, and ignores it: what is under test is
# how the script reads the answer, not how it phrased the question.
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

# The version this repository's chart is at, read the way the script reads it
# -- from HEAD, not from the working tree. Every case below that runs against
# this repository asserts on it rather than on a number written here, so a
# chart version bump does not turn this file red. Reading it off disk instead
# does turn it red, on exactly the commit that bumps the version and before it
# is committed, which is how this line was wrong for one run.
chart_version="$(git show HEAD:charts/spawnery/Chart.yaml \
	| grep -E '^version:' | head -1 | awk '{print $2}')"

# Builds a git repository containing hack/publish-chart.sh and charts/spawnery
# as this repository has them at HEAD, and commits it. Callers then edit and
# re-commit whatever the case is about. `git -c` rather than `git config`:
# the identity is a property of this one command and is not written anywhere.
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
# Not knowing is not the same as it not being there. This is the case a
# `write:packages`-only token produces, and reading it as "absent" would
# silently defeat the refusal above.
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
# The chart is packaged from HEAD, not from the working tree. This is the
# property that lets release.yml run this after hack/publish.sh, which
# rewrites values.yaml's image.digest in place on the runner: a working tree
# carrying a digest must not put one in the artefact.
#
# A `helm` earlier on PATH copies the directory `helm package` was handed and
# then execs the real one, so the packaging still happens for real and what is
# asserted is the source the script chose. A copy and not the path: the script
# packages out of its own mktemp directory and removes it on the way out, so by
# the time this file could read the path it names, there is nothing there.
fixture="$workdir/from-head"
make_fixture "$fixture"
sed -i -E 's|^([[:space:]]*digest:[[:space:]]*)""|\1"sha256:'"$(printf 'a%.0s' {1..64})"'"|' \
	"$fixture/charts/spawnery/values.yaml"
if ! grep -qE '^[[:space:]]*digest:[[:space:]]*"sha256:a+"' "$fixture/charts/spawnery/values.yaml"; then
	fail "fixture setup: the working tree's values.yaml was not given a digest"
fi
# The same fixture answers the other half of the question: the version the
# artefact is named after has to come from HEAD too. Uncommitted here, so a
# script reading Chart.yaml off disk would publish 7.7.7.
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
# A *committed* digest is the case nothing else in this repository catches:
# rbacaudit's TestTheOperatorImageIsNotAMutableTag returns early when one is
# set rather than failing, so this refusal is the only thing standing between
# a committed digest and every installation pinned to it.
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
