#!/usr/bin/env bash
# Refuses a toolchain whose generator and runtime versions disagree.
#
# protoc and protoc-gen-grpc-java come from nixpkgs through flake.nix;
# protobuf-java and io.grpc:grpc-* from agent/common/build.gradle.kts and
# agent/deps.json. A `nix flake update` moves only the first half of each pair.
#
# protoc 35.1 is protobuf-java 4.35.1: same release, two numbering schemes.
#
# Every uncertainty is a refusal: a check that passes when it could not look
# would report an agreement nothing measured.
#
# Usage:
#   hack/toolchain-pins-agree.sh [--gradle FILE] [--deps FILE]
#                                [--protoc VERSION] [--grpc VERSION]
#
# With no overrides it measures the toolchain on PATH and reads the repo's files.
#
# Exit status: 0 they agree, 1 they do not or something could not be read.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
gradle="$root/agent/common/build.gradle.kts"
deps="$root/agent/deps.json"
protoc_version=""
grpc_version=""

while [ $# -gt 0 ]; do
  case "$1" in
    --gradle) gradle="$2"; shift 2 ;;
    --deps)   deps="$2";   shift 2 ;;
    --protoc) protoc_version="$2"; shift 2 ;;
    --grpc)   grpc_version="$2";   shift 2 ;;
    *) echo "toolchain-pins-agree: unknown argument $1" >&2; exit 1 ;;
  esac
done

fail() { echo "toolchain-pins-agree: $*" >&2; exit 1; }

if [ -z "$protoc_version" ]; then
  raw="$(protoc --version 2>/dev/null)" ||
    fail "protoc is not on PATH; run this through \`nix develop\`"
  protoc_version="${raw#libprotoc }"
  [ "$protoc_version" != "$raw" ] ||
    fail "protoc --version said '$raw', which is not 'libprotoc <version>'"
fi

# The generator plugin has no version option; read it off its store path.
if [ -z "$grpc_version" ]; then
  bin="$(command -v protoc-gen-grpc-java 2>/dev/null)" ||
    fail "protoc-gen-grpc-java is not on PATH; run this through \`nix develop\`"
  path="$(readlink -f "$bin")"
  grpc_version="$(printf '%s' "$path" | sed -n 's|.*-protoc-gen-grpc-java-\([0-9][^/]*\)/.*|\1|p')"
  [ -n "$grpc_version" ] ||
    fail "cannot read a version out of protoc-gen-grpc-java's path '$path'"
fi

[ -r "$gradle" ] || fail "cannot read $gradle"
[ -r "$deps" ]   || fail "cannot read $deps"

want_protobuf_java="4.$protoc_version"
bad=0

# deps.json is checked too: without `make agent-deps` it still resolves the old
# version after the gradle file moved.
check_absent() {
  local file="$1" pattern="$2" what="$3"
  if ! grep -qF -- "$pattern" "$file"; then
    echo "toolchain-pins-agree: $what" >&2
    echo "  expected to find '$pattern' in $file" >&2
    bad=1
  fi
}

check_absent "$gradle" "com.google.protobuf:protobuf-java:$want_protobuf_java" \
  "protoc is $protoc_version, so protobuf-java must be $want_protobuf_java"
check_absent "$deps" "protobuf-java/$want_protobuf_java" \
  "protoc is $protoc_version, so deps.json must resolve protobuf-java $want_protobuf_java"

mismatched="$(grep -o 'io\.grpc:grpc-[a-z-]*:[0-9][0-9.]*' "$gradle" |
  grep -v ":$grpc_version\$" || true)"
if [ -n "$mismatched" ]; then
  echo "toolchain-pins-agree: protoc-gen-grpc-java is $grpc_version, so every" \
    "io.grpc:grpc-* artifact must be too" >&2
  printf '  %s\n' $mismatched >&2
  bad=1
fi

grep -q 'io\.grpc:grpc-' "$gradle" ||
  fail "$gradle names no io.grpc:grpc-* artifact; this check would pass vacuously"

deps_mismatched="$(grep -o 'grpc-[a-z-]*/[0-9][0-9.]*' "$deps" |
  grep -v "/$grpc_version\$" || true)"
if [ -n "$deps_mismatched" ]; then
  echo "toolchain-pins-agree: protoc-gen-grpc-java is $grpc_version, so every" \
    "grpc-* entry in deps.json must be too" >&2
  printf '  %s\n' $deps_mismatched >&2
  bad=1
fi

if [ "$bad" -ne 0 ]; then
  echo "toolchain-pins-agree: after a \`nix flake update\`, move the literals in" >&2
  echo "  $gradle to match, then run \`make agent-deps\`." >&2
  exit 1
fi
