#!/usr/bin/env bash
# Publish the plugin API to Maven Central as
# `cloud.spawnery:spawnery-api:<imageVersion>`.
#
# Usage:
#   hack/publish-api.sh
#
# A script rather than a Gradle plugin: every Central Portal plugin is
# third-party and would enter agent/deps.json. gpg signs rather than Gradle,
# whose bundled Bouncy Castle cannot read the key format recent GnuPG writes.
#
# Environment:
#   DRY_RUN=1                 build and assemble the bundle, print what would
#                             be uploaded where, and upload nothing. No
#                             credential is needed.
#   CENTRAL_USERNAME=...      the Central Portal token's user name.
#   CENTRAL_PASSWORD=...      the token itself, not the account password.
#   SIGNING_KEY=...           an ASCII-armoured private key, imported into a
#                             throwaway GNUPGHOME.
#   SIGNING_PASSWORD=...      its passphrase.
#   PUBLISHING_TYPE=...       AUTOMATIC (the default) releases as soon as
#                             validation passes. USER_MANAGED leaves the
#                             deployment to be released by hand in the Portal.
#
# Exit status:
#   0  the bundle was uploaded (or, under DRY_RUN, described).
#   3  Central refused it because that version is already published.
#   1  anything else.
set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly PORTAL="${PORTAL:-https://central.sonatype.com}"
readonly PUBLISHING_TYPE="${PUBLISHING_TYPE:-AUTOMATIC}"

# release.yml passes DRY_RUN=0, so compare against "1", never test non-empty.
DRY_RUN="${DRY_RUN:-0}"
readonly DRY_RUN

version="$(grep -oE 'imageVersion = "[^"]+"' "$REPO_ROOT/flake.nix" | head -1 | cut -d'"' -f2)"
if [[ -z "$version" ]]; then
  echo "publish-api: no imageVersion in flake.nix; nothing says which version this would be" >&2
  exit 1
fi

readonly staging="$REPO_ROOT/agent/api/build/staging-deploy"
readonly bundle="$REPO_ROOT/agent/api/build/spawnery-api-$version-bundle.zip"

# Central's own rejection only mentions a missing .asc.
if [[ "$DRY_RUN" != "1" && ( -z "${SIGNING_KEY:-}" || -z "${SIGNING_PASSWORD:-}" ) ]]; then
  echo "publish-api: SIGNING_KEY and SIGNING_PASSWORD are unset, and Central takes no unsigned bundle." >&2
  echo "             Run with DRY_RUN=1 to rehearse without them." >&2
  exit 1
fi

if [[ "$DRY_RUN" = "1" ]]; then
  echo "publish-api: rehearsing cloud.spawnery:spawnery-api:$version -- nothing will be uploaded"
else
  echo "publish-api: publishing cloud.spawnery:spawnery-api:$version to $PORTAL as $PUBLISHING_TYPE"
fi
echo "publish-api: building cloud.spawnery:spawnery-api:$version"
rm -rf "$staging"
(cd "$REPO_ROOT/agent" && gradle --console=plain -q \
  ":api:publishApiPublicationToStagingRepository" "-PagentVersion=$version")

# Central rejects a bundle carrying its own maven-metadata.xml.
find "$staging" -name 'maven-metadata*' -delete

if [[ -n "${SIGNING_KEY:-}" ]]; then
  if [[ "$SIGNING_KEY" != *"BEGIN PGP PRIVATE KEY BLOCK"* ]]; then
    echo "publish-api: SIGNING_KEY is not an ASCII-armoured private key." >&2
    echo "             Export it with: gpg --export-secret-keys --armor <keyid>" >&2
    exit 1
  fi

  # Keys prepared for Gradle's signing plugin carry backslash-n newlines.
  # Armour never contains a backslash, so this cannot damage a key.
  key="$SIGNING_KEY"
  if [[ "$key" == *'\n'* ]]; then
    key="$(printf '%b' "$key")"
    echo "publish-api: SIGNING_KEY arrived with escaped newlines; reading it as a key"
  fi

  # Secret stores can drop the blank line after the armour marker, and gpg
  # then reads the first base64 line as a header. A second line that is
  # neither empty nor "Key: value" means it is missing.
  second="$(sed -n '2p' <<<"$key")"
  if [[ -n "$second" && ! "$second" =~ ^[A-Za-z][A-Za-z-]*:\  ]]; then
    key="$(awk 'NR==1 {print; print ""; next} {print}' <<<"$key")"
    echo "publish-api: SIGNING_KEY had no blank line after its armour marker; put one back"
  fi

  GNUPGHOME="$(mktemp -d)"
  export GNUPGHOME
  chmod 700 "$GNUPGHOME"
  trap 'rm -rf "$GNUPGHOME"' EXIT

  if ! printf '%s\n' "$key" | gpg --batch --quiet --import; then
    echo "publish-api: gpg would not import SIGNING_KEY." >&2
    # gpg quotes the offending line, which the runner's log masks; describe
    # the shape without revealing any of the value.
    {
      echo "             what the value looks like, without any of it:"
      echo "               bytes:            ${#key}"
      echo "               real newlines:    $(grep -c '' <<<"$key")"
      echo "               carriage returns: $(tr -cd '\r' <<<"$key" | wc -c)"
      echo "               backslashes:      $(tr -cd '\\\\' <<<"$key" | wc -c)"
      echo "               begins with the armour marker: \
$([[ "$key" == "-----BEGIN PGP PRIVATE KEY BLOCK-----"* ]] && echo yes || echo no)"
      echo "               ends with it:                  \
$([[ "$(tr -d '[:space:]' <<<"$key")" == *"-----ENDPGPPRIVATEKEYBLOCK-----" ]] && echo yes || echo no)"
    } >&2
    exit 1
  fi

  signed=0
  while IFS= read -r -d '' file; do
    gpg --batch --quiet --pinentry-mode loopback \
      --passphrase "$SIGNING_PASSWORD" \
      --armor --detach-sign --output "$file.asc" "$file"
    signed=$((signed + 1))
  done < <(find "$staging" -type f \
    ! -name '*.asc' ! -name '*.md5' ! -name '*.sha1' \
    ! -name '*.sha256' ! -name '*.sha512' -print0)

  if [[ "$signed" -eq 0 ]]; then
    echo "publish-api: nothing was signed; the staging tree is empty." >&2
    exit 1
  fi
  echo "publish-api: signed $signed files"
elif [[ "$DRY_RUN" != "1" ]]; then
  echo "publish-api: no SIGNING_KEY, and Central takes no unsigned bundle." >&2
  exit 1
fi

rm -f "$bundle"
(cd "$staging" && zip -qr "$bundle" .)
echo "publish-api: bundle $(basename "$bundle") ($(du -h "$bundle" | cut -f1))"

if [[ "$DRY_RUN" = "1" ]]; then
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
    # Central answers either for a version it already has, with varying wording.
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
