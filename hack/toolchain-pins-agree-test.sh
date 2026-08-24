#!/usr/bin/env bash
# Drives hack/toolchain-pins-agree.sh through the disagreements this tree does
# not contain.
#
# The overrides the script takes exist for this file. Two of the cases below
# are the ones that actually happen -- a `nix flake update` that moved protoc,
# and one that moved protoc-gen-grpc-java -- and the rest are the ways a check
# like this passes without having looked: a file it cannot read, and a file
# with nothing in it to disagree with.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
check="$root/hack/toolchain-pins-agree.sh"
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

# The tree as it stands, measuring the toolchain on PATH rather than taking
# either override. Without this the whole file would only ever exercise the
# comparison against values it made up.
run "the tree agrees" 0 "$check"

# A flake update that moved protoc. protobuf-java 4.35.1 is protoc 35.1, so
# protoc 34.0 wants 4.34.0 and finds neither.
run  "protoc moved"   1 "$check" --protoc 34.0
says "protoc moved names protobuf-java" "protobuf-java:4.34.0" "$check" --protoc 34.0

# A flake update that moved the generator. Every io.grpc artifact is then
# behind it, in both files.
run  "the generator moved" 1 "$check" --grpc 1.84.0
says "the generator moved names an artifact" "io.grpc:grpc-api:1.83.1" "$check" --grpc 1.84.0
says "the generator moved names deps.json"   "grpc-protobuf/1.83.1"    "$check" --grpc 1.84.0

# `make agent-deps` not run: the gradle file was edited and the lock was not.
sed 's|grpc-api/1\.83\.1|grpc-api/1.82.0|' "$root/agent/deps.json" > "$tmp/stale-deps.json"
run  "the lock is stale" 1 "$check" --deps "$tmp/stale-deps.json"
says "the lock is stale names the entry" "grpc-api/1.82.0" "$check" --deps "$tmp/stale-deps.json"

# The two ways this check could pass without having looked.
printf 'dependencies {\n    api("com.google.protobuf:protobuf-java:4.35.1")\n}\n' > "$tmp/no-grpc.kts"
run  "a gradle file naming no grpc artifact" 1 "$check" --gradle "$tmp/no-grpc.kts"
says "and says why" "would pass vacuously" "$check" --gradle "$tmp/no-grpc.kts"

run "a gradle file that does not exist" 1 "$check" --gradle "$tmp/absent.kts"
run "a deps file that does not exist"   1 "$check" --deps   "$tmp/absent.json"

run "an unknown argument" 1 "$check" --wat

if [ "$failures" -ne 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all cases passed"
