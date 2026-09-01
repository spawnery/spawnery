#!/usr/bin/env bash
# Publish the plugin API to Maven Central, so that a plugin can compile against
# it without a checkout and without a jar somebody carries by hand.
#
# Usage:
#   hack/publish-api.sh
#
# The result is `cloud.spawnery:spawnery-api:<version>` on Maven Central, at
# the version flake.nix's imageVersion names -- the same number the agent jar
# inside the game images carries, because they are built from the same source
# and a plugin compiled against one runs against the other.
#
# # Why this is a script and not a Gradle plugin
#
# The Central Portal takes one signed archive over its own HTTP API rather than
# a Maven deploy, and every Gradle plugin that speaks it is a third-party
# plugin. Any of those would enter agent/deps.json, which is the lockfile that
# makes `nix build .#agents` reproducible -- so the cost of the convenience is
# paid by every build of this repository, forever, to save one curl.
#
# Gradle's own `maven-publish` writes the repository layout into
# agent/api/build/staging-deploy, `signing` puts a .asc beside every file, and
# what is left is to zip that tree and post it. Both of those plugins ship with
# Gradle.
#
# Environment:
#   DRY_RUN=1                 build and assemble the bundle, print what would
#                             be uploaded where, and upload nothing. No
#                             credential is needed, which is what makes a
#                             release rehearsable.
#   CENTRAL_USERNAME=...      the Central Portal token's user name.
#   CENTRAL_PASSWORD=...      the token itself. Not the account password: the
#                             Portal issues a token pair, and it is the pair
#                             this sends.
#   SIGNING_KEY=...           an ASCII-armoured private key, whole, newlines
#                             and all. Gradle reads it from the environment
#                             rather than from a keyring, because a runner has
#                             no keyring and a file would be one more thing to
#                             shred.
#   SIGNING_PASSWORD=...      its passphrase.
#   PUBLISHING_TYPE=...       AUTOMATIC (the default) releases as soon as
#                             validation passes. USER_MANAGED leaves the
#                             deployment for somebody to release by hand in the
#                             Portal, which is what to use the first time.
#
# Exit status:
#   0  the bundle was uploaded (or, under DRY_RUN, described).
#   3  Central refused it because that version is already published. Separate
#      from 1 for the reason hack/publish.sh separates it: a release that
#      changes no agent code republishes nothing, and the workflow has to be
#      able to tell that from a failure.
#   1  anything else.
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly PORTAL="${PORTAL:-https://central.sonatype.com}"
readonly PUBLISHING_TYPE="${PUBLISHING_TYPE:-AUTOMATIC}"

version="$(grep -oE 'imageVersion = "[^"]+"' "$REPO_ROOT/flake.nix" | head -1 | cut -d'"' -f2)"
if [[ -z "$version" ]]; then
  echo "publish-api: no imageVersion in flake.nix; nothing says which version this would be" >&2
  exit 1
fi

readonly staging="$REPO_ROOT/agent/api/build/staging-deploy"
readonly bundle="$REPO_ROOT/agent/api/build/spawnery-api-$version-bundle.zip"

# Signed here rather than checked later: Central rejects an unsigned bundle
# with a message about a missing .asc, which is a long way from "the release
# has no signing key".
if [[ -z "${DRY_RUN:-}" && ( -z "${SIGNING_KEY:-}" || -z "${SIGNING_PASSWORD:-}" ) ]]; then
  echo "publish-api: SIGNING_KEY and SIGNING_PASSWORD are unset, and Central takes no unsigned bundle." >&2
  echo "             Run with DRY_RUN=1 to rehearse without them." >&2
  exit 1
fi

echo "publish-api: building cloud.spawnery:spawnery-api:$version"
rm -rf "$staging"
(cd "$REPO_ROOT/agent" && gradle --console=plain -q \
  ":api:publishApiPublicationToStagingRepository" "-PagentVersion=$version")

# maven-metadata.xml describes every version of an artifact and is the
# registry's to write, not a bundle's to carry. Central rejects a bundle that
# brings its own.
find "$staging" -name 'maven-metadata*' -delete

if [[ -z "${DRY_RUN:-}" ]] && ! find "$staging" -name '*.asc' | grep -q .; then
  echo "publish-api: the staging tree carries no signatures; the key was set but Gradle did not sign." >&2
  exit 1
fi

rm -f "$bundle"
(cd "$staging" && zip -qr "$bundle" .)
echo "publish-api: bundle $(basename "$bundle") ($(du -h "$bundle" | cut -f1))"

if [[ -n "${DRY_RUN:-}" ]]; then
  echo "publish-api: DRY_RUN, so nothing is uploaded. It would go to $PORTAL as $PUBLISHING_TYPE:"
  (cd "$staging" && find . -type f | sort | sed 's/^/  /')
  exit 0
fi

if [[ -z "${CENTRAL_USERNAME:-}" || -z "${CENTRAL_PASSWORD:-}" ]]; then
  echo "publish-api: CENTRAL_USERNAME and CENTRAL_PASSWORD are unset; nothing to authenticate with." >&2
  exit 1
fi

token="$(printf '%s:%s' "$CENTRAL_USERNAME" "$CENTRAL_PASSWORD" | base64 -w0)"
response="$(mktemp)"
trap 'rm -f "$response"' EXIT

code="$(curl -sS -o "$response" -w '%{http_code}' \
  -X POST \
  -H "Authorization: Bearer $token" \
  -F "bundle=@$bundle" \
  "$PORTAL/api/v1/publisher/upload?name=spawnery-api-$version&publishingType=$PUBLISHING_TYPE")"

body="$(cat "$response")"
case "$code" in
  20*)
    echo "publish-api: uploaded, deployment $body"
    echo "publish-api: $PORTAL/publishing/deployments shows what it does next"
    ;;
  409|400)
    # Central answers both for a version it already has, and the wording moves
    # between them; the substring is what distinguishes an already-published
    # version from a bundle it disliked for another reason.
    if grep -qiE 'already (exists|published)|version.*published' <<<"$body"; then
      echo "publish-api: $version is already on Central; nothing was overwritten."
      exit 3
    fi
    echo "publish-api: Central refused the bundle ($code): $body" >&2
    exit 1
    ;;
  *)
    echo "publish-api: upload failed ($code): $body" >&2
    exit 1
    ;;
esac
