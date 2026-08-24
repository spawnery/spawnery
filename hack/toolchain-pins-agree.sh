#!/usr/bin/env bash
# Refuses a toolchain whose generator and runtime versions disagree.
#
# Two versions are pinned twice each. protoc and protoc-gen-grpc-java come from
# nixpkgs through flake.nix; protobuf-java and the io.grpc:grpc-* artifacts come
# from agent/common/build.gradle.kts, resolved through agent/deps.json. A `nix
# flake update` moves the first half of each pair and nothing moves the second,
# so `make proto` can then regenerate stubs that demand a runtime the build does
# not resolve. The failure is loud -- compileProtoJava: "cannot find symbol", or
# a ProtobufRuntimeVersionException at class init -- but it appears nowhere near
# the pin that caused it, and only after a Gradle build that takes minutes.
# flake.nix has named the coupling at both edit sites since milestone 2c; this
# is the standing check that entry asked for.
#
# protoc and protobuf-java are the same release under two numbering schemes --
# protoc 35.1 is protobuf-java 4.35.1 -- so the expected artifact version is
# built from the measured one rather than compared to it as a string.
# protoc-gen-grpc-java and the grpc-java artifacts share one version outright.
#
# Every uncertainty is a refusal, which is the opposite of
# hack/image-derivations-changed.sh and deliberate: there, not knowing means
# building an image nobody needed, and here it would mean reporting an
# agreement nothing measured. The whole point is to fail at the pin rather than
# in the compiler, and a check that passes when it could not look does neither.
#
# Usage:
#   hack/toolchain-pins-agree.sh [--gradle FILE] [--deps FILE]
#                                [--protoc VERSION] [--grpc VERSION]
#
# The four overrides exist for hack/toolchain-pins-agree-test.sh, which has to
# drive disagreements this tree does not contain. With none of them the script
# measures the toolchain on PATH and reads the repository's own files.
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

# protoc answers for itself.
if [ -z "$protoc_version" ]; then
  raw="$(protoc --version 2>/dev/null)" ||
    fail "protoc is not on PATH; run this through \`nix develop\`"
  protoc_version="${raw#libprotoc }"
  [ "$protoc_version" != "$raw" ] ||
    fail "protoc --version said '$raw', which is not 'libprotoc <version>'"
fi

# The generator plugin takes no version option at all, so it is read off the
# store path nixpkgs put it at -- which is where its version is recorded.
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

# The gradle file is the pin a person edits; deps.json is the resolved lock the
# build actually downloads. Both are checked, because they can disagree with
# each other as well as with the toolchain -- `make agent-deps` regenerates the
# second from the first, and a tree where that has not been run is a tree where
# the build resolves the old version.
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

# Every io.grpc:grpc-* coordinate in the gradle file, whichever configuration
# it sits in: api, implementation and testImplementation all end up on a
# classpath the generated stubs run against.
mismatched="$(grep -o 'io\.grpc:grpc-[a-z-]*:[0-9][0-9.]*' "$gradle" |
  grep -v ":$grpc_version\$" || true)"
if [ -n "$mismatched" ]; then
  echo "toolchain-pins-agree: protoc-gen-grpc-java is $grpc_version, so every" \
    "io.grpc:grpc-* artifact must be too" >&2
  printf '  %s\n' $mismatched >&2
  bad=1
fi

# A gradle file naming no grpc artifact at all would pass the loop above by
# having nothing to disagree with.
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
