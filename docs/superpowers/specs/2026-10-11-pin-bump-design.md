# Weekly pull requests for the Paper and Purpur pins

**Status:** implemented (branch feat/upstream-pin-pr)
**Date:** 2026-10-11

## 1. Goal

`nix/paper.nix` and `nix/purpur.nix` pin one upstream build each. Until now
`paper-watch.yml` opened an issue when PaperMC published a newer build, and
nothing watched Purpur at all. Taking a build was manual: run the pin script,
run `make purpur-image-test agent-test`, move `imageVersion` and the image
tags in the docs, open a pull request. PR #123 (Purpur 2642 to 2647, released
as 0.26.1) is that sequence done by hand.

This design replaces the issue with a pull request that a maintainer only
has to merge and tag.

## 2. Why there was an issue and not a pull request

`paper-watch.yml` refused to open a pull request because "no CI job runs a
Paper server, so a green PR would only prove the hashes". That is still true
of `ci.yml`. But `hack/image-test.sh` and `hack/agent-test.sh` need only a
container runtime, not kind, and the hosted runner has docker. The workflow
that opens the pull request runs both and writes the result into the
description. A failed run leaves the pull request as a draft and fails the
workflow run.

The tests do not cover the plugins a network installs on top, and PR #123
was about one of those (FastAsyncWorldEdit). For the reviewer, the
description lists the upstream commits between the old and the new build.

## 3. What the workflow does

`.github/workflows/pin-bump.yml`, Mondays at 04:23 UTC and on dispatch:

1. `hack/paper-pin.sh`, then `hack/purpur-pin.sh`, both on the pinned
   Minecraft version. Paper goes first because the Purpur script refuses a
   build that wants another Mojang jar than `nix/paper.nix` pins.
2. Nothing moved: close the open pull request, if there is one, and stop.
3. Commit the pins (`fix(images): …`), then run `hack/image-version-bump.sh`
   and commit its result (`chore: <version>, …`).
4. If `origin/auto/upstream-pins` already carries this tree, stop. Otherwise
   run `make purpur-image-test agent-test`, force-push the branch and open
   or update the one pull request.

One branch and one pull request for both projects: both move `imageVersion`,
and two pull requests would conflict with each other on every line they
touch.

## 4. Numbering the release

`hack/image-version-bump.sh` decides the number, against the `v*` tags:

- `imageVersion` names no tag yet: it already belongs to the coming release
  and stays.
- `operatorVersion` or the chart version names a number above every tag: a
  release is already numbered on master, and the images join it.
- Otherwise the patch after the highest tag. A newer upstream build of the
  same Minecraft version is a fix under the SemVer rule of 2026-09-08.

It rewrites the image tags and `spawnery-api` coordinates in the files
`hack/image-tag-pins-agree.sh --list` names, so the list stays in one place,
and the two e2e manifests that run a game image. Those stay out of the lint
because they pin a Velocity tag that must not exist; `hack/e2e-ondemand.sh`
and `hack/e2e-worldsync.sh` check their Purpur tag themselves.
`make image-tag-lint` checks the result in the same run.

A pull request that bumps only the pin was the alternative. It would conflict
less, but `release.yml` skips an image whose tag already exists, so a merged
pin without a new `imageVersion` would quietly never ship.

## 5. Credentials

A pull request opened with `GITHUB_TOKEN` starts no `ci.yml` run, and this
repository does not let `GITHUB_TOKEN` open pull requests. The workflow
takes a token from a GitHub App instead
(`actions/create-github-app-token`): the secrets `PIN_BOT_APP_ID` and
`PIN_BOT_PRIVATE_KEY`. The app needs Contents and Pull requests,
read and write, on this repository only. Its commits are unsigned, and the
merge commit is the maintainer's.

## 6. Left out

- A new Minecraft version. Both scripts stay on the pinned one, and a
  version change touches the 26.2 images, the JRE module lists and the
  docs. `paper-watch.yml` did not report one either.
- Velocity. `nix/velocity.nix` has no pin script yet.
