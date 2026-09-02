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
# agent/api/build/staging-deploy, and what is left is to sign that tree and
# post it.
#
# # Why gpg signs and Gradle does not
#
# Gradle's signing plugin reads a key through a Bouncy Castle it bundles, and
# measured on a real key it answers "Could not read PGP secret key" for what
# recent GnuPG versions write by default. The failure is not the problem; where
# it arrives is. It comes out of the middle of a Gradle build, about a key
# format the person who generated the key never chose and cannot see from
# there. Signing here with gpg means the tool that reads the key is the one
# that wrote it, and every key gpg can produce is a key this can sign with.
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
#                             and all. Imported into a throwaway GNUPGHOME for
#                             the length of this script, because a runner has no
#                             keyring and a file left behind would be one more
#                             thing to shred.
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

# The same two lines hack/publish.sh and hack/publish-chart.sh open with, and
# they are here because this script did not have them.
#
# .github/workflows/release.yml passes DRY_RUN=0 to mean "this is the real
# thing". An earlier version of this file asked whether the variable was
# non-empty, and the string "0" is not empty -- so the tagged release reported
# success and uploaded nothing. That is the worst shape a bug in a publisher
# can take: it looks exactly like the thing having worked.
DRY_RUN="${DRY_RUN:-0}"
readonly DRY_RUN

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
if [[ "$DRY_RUN" != "1" && ( -z "${SIGNING_KEY:-}" || -z "${SIGNING_PASSWORD:-}" ) ]]; then
  echo "publish-api: SIGNING_KEY and SIGNING_PASSWORD are unset, and Central takes no unsigned bundle." >&2
  echo "             Run with DRY_RUN=1 to rehearse without them." >&2
  exit 1
fi

# Said once, at the top, because a reader who has to work out from the absence
# of an upload whether one was meant is a reader this script has already
# failed.
if [[ "$DRY_RUN" = "1" ]]; then
  echo "publish-api: rehearsing cloud.spawnery:spawnery-api:$version -- nothing will be uploaded"
else
  echo "publish-api: publishing cloud.spawnery:spawnery-api:$version to $PORTAL as $PUBLISHING_TYPE"
fi
echo "publish-api: building cloud.spawnery:spawnery-api:$version"
rm -rf "$staging"
(cd "$REPO_ROOT/agent" && gradle --console=plain -q \
  ":api:publishApiPublicationToStagingRepository" "-PagentVersion=$version")

# maven-metadata.xml describes every version of an artifact and is the
# registry's to write, not a bundle's to carry. Central rejects a bundle that
# brings its own.
find "$staging" -name 'maven-metadata*' -delete

# Signed, unless this is a rehearsal without a key. A bundle Central would
# refuse is not worth assembling, so the failures here are loud and name the
# thing that is wrong rather than the step that noticed.
if [[ -n "${SIGNING_KEY:-}" ]]; then
  if [[ "$SIGNING_KEY" != *"BEGIN PGP PRIVATE KEY BLOCK"* ]]; then
    echo "publish-api: SIGNING_KEY is not an ASCII-armoured private key." >&2
    echo "             Export it with: gpg --export-secret-keys --armor <keyid>" >&2
    exit 1
  fi

  # Both shapes of the same key are accepted, because both circulate and only
  # one of them is anybody's fault.
  #
  # Gradle's signing plugin takes a key whose newlines are backslash-n, and
  # every guide to it says so -- so that is the form a key reaches a secret
  # store in, and it is what this script met the first time it ran with a real
  # one. gpg needs the newlines to be newlines, and what it says otherwise is
  # `invalid armor header`, about a value the log has masked to three
  # asterisks. Nobody was ever going to read that and think of printf.
  #
  # The test is only whether a backslash-n is in there, and that is the whole
  # of it. An earlier version also required the value to carry no real newline,
  # reasoning that a key with both was already fine -- and it met a secret that
  # was one escaped line with a trailing newline, which is what a paste into a
  # text box leaves behind. The extra condition did nothing but decide, on that
  # evidence, to leave the key unreadable.
  #
  # Safe without it: armour is base64 and two header lines, and no backslash
  # occurs in either, so there is no key this conversion can damage.
  key="$SIGNING_KEY"
  if [[ "$key" == *'\n'* ]]; then
    key="$(printf '%b' "$key")"
    echo "publish-api: SIGNING_KEY arrived with escaped newlines; reading it as a key"
  fi

  # Armour is a marker, then either headers or a blank line, then base64.
  # Without that blank line gpg reads the first base64 line as a header and
  # says "invalid armor header: <that line>" -- and on a runner that line is
  # part of the secret, so the log masks it and what is left to read is three
  # asterisks. Reproduced exactly by deleting the blank line from a healthy
  # key, which is how this was found rather than guessed at.
  #
  # Something between the export and the secret store drops it; a text box that
  # trims blank lines is the obvious candidate. Putting it back is unambiguous:
  # a second line that is neither empty nor "Key: value" cannot be a header,
  # and armour with no headers has to have the blank line.
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
    # What the value is, without any of what it says.
    #
    # gpg's own complaint quotes the offending line, and a log masks every
    # occurrence of the secret in it -- so what reaches a reader is
    # "invalid armor header: ***" and no way to tell an escaped key from a
    # CRLF one from a truncated one. These are counts and yes-or-nos derived
    # from the value; none of them narrows what the key is, and between them
    # they name the shape.
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
