#!/usr/bin/env bash
# Renders the chart with world sync off and on and checks what each must hold.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() {
	echo "$1" >&2
	exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

off="$(helm template t charts/spawnery)"
if grep -q 'worldsync.spawnery.cloud' <<<"$off"; then
	fail "world sync is off by default, yet the chart renders the driver"
fi
grep -qF -- '--world-sync=false' <<<"$off" || fail "world sync off: the operator lacks --world-sync=false"

helm template t charts/spawnery --namespace ops \
	--set worldSync.enabled=true \
	--set worldSync.namespace=ws \
	--set worldSync.objectStore.endpoint=https://s3.example \
	--set worldSync.objectStore.region=r \
	--set worldSync.objectStore.bucket=b \
	--set worldSync.objectStore.prefix=private-servers \
	--set worldSync.objectStore.credentialsSecret=creds >"$tmp/on.yaml"
on="$(cat "$tmp/on.yaml")"
for want in 'kind: CSIDriver' 'name: worldsync.spawnery.cloud' 'kind: DaemonSet' 'namespace: ws' \
	'--world-sync=true' '--world-sync-snapshot-interval=5m' 'WORLDSYNC_BUCKET' \
	'mountPropagation: Bidirectional' 'privileged: true' \
	'podInfoOnMount: true' 'attachRequired: false' '- Ephemeral'; do
	grep -qF -- "$want" <<<"$on" || fail "world sync on: the render lacks '$want'"
done

python3 - "$tmp/on.yaml" <<'PY' || fail "world sync on: operator and node agent disagree"
import sys

import yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]


def env_of(kind, container):
    for d in docs:
        if d["kind"] != kind:
            continue
        for c in d["spec"]["template"]["spec"]["containers"]:
            if c["name"] == container:
                return {e["name"]: e.get("value") for e in c.get("env", [])}
    raise SystemExit(f"no {kind} container {container}")


operator = env_of("Deployment", "operator")
agent = env_of("DaemonSet", "worldsync")
for name in ("WORLDSYNC_ENDPOINT", "WORLDSYNC_REGION", "WORLDSYNC_BUCKET", "WORLDSYNC_PREFIX"):
    if operator.get(name) is None or operator.get(name) != agent.get(name):
        raise SystemExit(f"{name}: operator {operator.get(name)!r}, node agent {agent.get(name)!r}")
if agent["WORLDSYNC_PREFIX"] != "private-servers":
    raise SystemExit("the prefix did not reach the node agent")
PY

if helm template t charts/spawnery --set worldSync.enabled=true >/dev/null 2>&1; then
	fail "world sync on without a bucket rendered; it must fail"
fi
echo ok
