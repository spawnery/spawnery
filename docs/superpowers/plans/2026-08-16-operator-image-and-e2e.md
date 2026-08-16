# Milestone 6a Implementation Plan — the operator in the cluster

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build an OCI image for the operator, publish all three images to
`ghcr.io/spawnery/`, and add a driven run on `kind` that watches the operator
work under its own ServiceAccount and produce no denied request.

**Architecture:** A new `buildGoModule` derivation for `cmd/spawnery-operator`
and a `nix/operator-image.nix` beside the two game images; `hack/publish.sh`
copying each freshly built archive straight to the registry with `skopeo`; and a
two-part E2E — `hack/e2e.sh` does the plumbing (cluster, image load, apply,
patch, wait) and a Go test package under `test/e2e` behind the `e2e` build tag
makes every claim.

**Tech Stack:** Nix flakes (`dockerTools.buildLayeredImage`), Go 1.x with
controller-runtime's client, `kind`, `kubectl`, `skopeo`, bash.

**Spec:** `docs/superpowers/specs/2026-08-16-operator-image-and-e2e-design.md`.
Read it before Task 1. Section references below (§4.2, §7.1, …) are to that
document.

## Global Constraints

- **Commit messages use Conventional Commits**, deliberately overriding this
  repository's own sentence-style history: `feat(6a): …`, `fix(6a): …`,
  `docs(6a): …`, `test(6a): …`. The subject says what changed; the body still
  has to say *why*, wrapped at 72 columns. Every commit ends with the trailer
  `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- **Go builds and tests:** `nix develop -c make test`. Takes about 38 seconds,
  of which `internal/controller` is about 34 — envtest boots a real API server,
  so that is normal and not a hang.
- **Anything touching images:** `nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp"
  make <target> CONTAINER=podman`. `CONTAINER=podman` because the Makefile
  defaults to `docker`, which is not installed. `TMPDIR` on a disk-backed path
  because `/tmp` is a 2 GB tmpfs that `nix develop` points `TMPDIR` at, and
  podman extracts OCI archives there.
- **`kind` on this machine runs under rootless podman**, which needs both an
  environment variable and a systemd scope:
  `systemd-run --scope --user --property=Delegate=yes -- nix develop -c env
  KIND_EXPERIMENTAL_PROVIDER=podman make e2e`. Do not hard-code either into the
  script; it honours the environment (spec §6.1).
- **`~/.config/nix/nix.conf` already sets `experimental-features = nix-command
  flakes`**, so no `--extra-experimental-features` prefix is needed anywhere.
- **The machine has 3.9 GB of RAM and no swap.** Do not run a Gradle build and
  an E2E cluster at the same time. Nothing in this milestone touches
  `agent/` or `proto/`, so `make agent` and `make agent-test` are out of scope —
  verify that with `git diff master...HEAD --name-only` before claiming it.
- **The Nix store fills quickly with unrooted image outputs.** After a round of
  image rebuilds, `nix store delete` the unrooted ones; the ones under
  `result-*` are rooted and stay.
- **No test may be claimed to pass without its output.** Every "run it and see
  it fail" step means running it and reading the failure, and every mutation in
  a task's verification block means *making the edit, running the command, and
  recording what it printed*. This project has three separate records of a test
  that was green while testing nothing.

---

### Task 1: The operator's image

**Files:**
- Create: `nix/operator-image.nix`
- Create: `hack/operator-image-test.sh`
- Modify: `flake.nix` (the `let` block of `packages`, the shared attribute set, and the `x86_64-linux` block)
- Modify: `nix/oci-common.nix` (one comment)
- Modify: `Makefile` (`--out-link` on all three images; new operator targets; `image-repro`)

**Interfaces:**
- Produces: the flake attributes `packages.spawnery-operator` (a Go package) and
  `packages.operator-image` (an OCI archive, `x86_64-linux` only), the Nix
  binding `operatorVersion = "0.1.0"`, the make targets `operator-image`,
  `operator-image-load`, `operator-image-test`, and the make variable
  `OPERATOR_IMAGE`. Tasks 3 and 4 both build `.#operator-image` and read
  `.#operator-image.imageName` / `.imageTag`.

- [ ] **Step 1: Write the failing test**

Create `hack/operator-image-test.sh`. It asserts the shape the Deployment
already assumes: a numeric user, a read-only root filesystem, no writable
directory of its own, and a binary that runs.

```bash
#!/usr/bin/env bash
# Smoke test for the operator image.
#
# It runs the image under exactly the constraints config/deploy/deployment.yaml
# imposes -- non-root, read-only root filesystem, no network -- rather than more
# comfortable ones. The operator writes nothing to disk, so unlike the two game
# images this one gets no tmpfs and no volume: if it ever needs one, this test
# is where that shows up, instead of in a CrashLoopBackOff on a real cluster.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# The image config, not the pod spec, is what resolves the identity: the
# Deployment sets runAsNonRoot with no runAsUser, so the kubelet refuses to
# start an image whose User is empty or names root.
user="$("$CONTAINER" image inspect --format '{{.Config.User}}' "$IMAGE")"
[ "$user" = "10001:10001" ] || fail "image user = '$user', want 10001:10001"

# The working directory is the root, not a game server's /data.
workdir="$("$CONTAINER" image inspect --format '{{.Config.WorkingDir}}' "$IMAGE")"
[ "$workdir" = "/" ] || fail "image workingDir = '$workdir', want /"

# /data belongs to a game server. An operator that acquired one would be
# carrying state nothing reads, and oci-common.layeredImage -- which creates it
# -- is deliberately not used here.
#
# Checked by exporting the filesystem rather than by running `test -d` inside
# the image: this image has no shell at all, so an in-container check would
# fail to start and its non-zero exit would read as "no /data" whether the
# directory was there or not. That assertion could never have failed.
cid="$("$CONTAINER" create "$IMAGE")"
if "$CONTAINER" export "$cid" | tar -t | grep -qE '^\./?data/?$'; then
	"$CONTAINER" rm "$cid" >/dev/null
	fail "the image has a /data directory; it should carry no writable directory of its own"
fi
"$CONTAINER" rm "$cid" >/dev/null

# The binary runs, statically, as uid 10001, with nothing writable and no
# network. Go's flag package prints usage and exits 2 for -h, which is the
# cheapest proof that the ELF loads and the flags are the ones the Deployment
# passes.
out="$("$CONTAINER" run --rm --read-only --network none "$IMAGE" -h 2>&1 || true)"
for flag in startup-deadline leader-elect metrics-bind-address health-probe-bind-address; do
	case "$out" in
	*"-$flag"*) ;;
	*) fail "the operator's usage does not mention -$flag; the Deployment passes it" ;;
	esac
done

echo "OK: $IMAGE"
```

Make it executable: `chmod +x hack/operator-image-test.sh`.

- [ ] **Step 2: Run it to verify it fails**

Run:

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make operator-image-test CONTAINER=podman
```

Expected: FAIL — `make: *** No rule to make target 'operator-image-test'`.
The target does not exist yet, and neither does the flake attribute behind it.

- [ ] **Step 3: Add the Go derivation and the version to `flake.nix`**

In the `let` block of `packages = forAllSystems (pkgs: let … in …)`, beside
`imageVersion`, add:

```nix
          # The operator's own version, deliberately not imageVersion.
          # imageVersion above is the *agent* version -- it reaches the
          # plugin's paper-plugin.yml and is reported to the operator as
          # Hello.version -- and hanging the operator's tag off it would mean a
          # fix in the reconciler claiming a new agent version, and an agent
          # release renaming an unchanged operator image.
          operatorVersion = "0.1.0";
```

and, beside the other four Go packages:

```nix
          # The operator itself, packaged so an image can be built from it.
          # `make build` still exists for the local loop; this is the same
          # binary produced reproducibly, which is what nix/operator-image.nix
          # and hack/publish.sh need.
          spawnery-operator = pkgs.buildGoModule {
            pname = "spawnery-operator";
            version = operatorVersion;
            src = ./.;
            vendorHash = "sha256-wFmml1cI2CocLj3ggu6PrirliDB6nSOBK6rQptMcYF0=";
            subPackages = [ "cmd/spawnery-operator" ];
            # Static, because the image carries no libc of its own for it.
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" ];
          };
```

Add it to the architecture-independent attribute set:

```nix
          inherit spawnery-slp spawnery-stubop spawnery-join spawnery-config agents spawnery-operator;
```

And in the `x86_64-linux` block, beside `paper-image` and `velocity-image`:

```nix
          # Restricted to x86_64-linux for the same reason the two game images
          # are, and the reason is in their comment above: buildLayeredImage
          # does not cross-compile but labels its output amd64 regardless.
          operator-image = pkgs.callPackage ./nix/operator-image.nix {
            inherit spawnery-operator operatorVersion oci-common;
          };
```

- [ ] **Step 4: Write `nix/operator-image.nix`**

```nix
# The operator image.
#
# Unlike the two game images this one carries no runtime, no shell and no
# writable directory: the operator is a single static binary that talks to the
# API server and writes nothing to disk. It therefore takes from oci-common
# only the identity -- the numeric user and its passwd/group entries, so all
# three images run as the same uid and runAsNonRoot has an entry to resolve --
# and builds its own frame.
#
# oci-common.layeredImage is deliberately not used: it creates /data and /tmp,
# chmods them, and sets WorkingDir=/data. That is a game server's shape. An
# operator running with readOnlyRootFilesystem and no state would carry two
# directories nothing reads, and hack/operator-image-test.sh fails if one
# appears.
{ dockerTools
, spawnery-operator
, operatorVersion
, oci-common
}:

dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/spawnery-operator";
  tag = operatorVersion;

  # A label, not a cross-compile, exactly as in nix/oci-common.nix: it is only
  # true because flake.nix exposes this attribute on x86_64-linux alone.
  architecture = "amd64";

  contents = [
    oci-common.passwd
    oci-common.group
    (oci-common.binIn { package = spawnery-operator; name = "spawnery-operator"; })
  ];

  config = {
    User = "${toString oci-common.uid}:${toString oci-common.gid}";
    WorkingDir = "/";
    Entrypoint = [ "/usr/local/bin/spawnery-operator" ];
    # Declared for documentation; the Deployment names all three itself.
    ExposedPorts = {
      "8080/tcp" = { };
      "8081/tcp" = { };
      "9443/tcp" = { };
    };
    Labels = {
      "org.opencontainers.image.title" = "Spawnery operator";
      "org.opencontainers.image.version" = operatorVersion;
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
    };
  };
}
```

- [ ] **Step 5: Add the sentence to `nix/oci-common.nix`**

The file's header comment says "What both Spawnery images need verbatim." There
are three images now, and the third takes only half of this file. Extend the
header:

```nix
# What both Spawnery *game* images need verbatim.
#
# ...
#
# The operator image is a third consumer and takes only the identity from here
# -- uid, gid, passwd, group -- because layeredImage below is a game server's
# frame: it creates /data and /tmp and sets WorkingDir=/data. See
# nix/operator-image.nix, which builds its own. That absence is deliberate and
# not an oversight.
```

Place the new paragraph after the existing header text and before the argument
set. Leave `layeredImage` itself untouched: two working images depend on it, and
adding a parameter to serve a third that does not want the directories at all
would churn the file both of them read.

- [ ] **Step 6: Rework the `Makefile`'s image targets**

Two things at once, because the second is only safe with the first.
`docs/known-issues.md`'s "Small things" already records that `make -j image-test`
can load the wrong image: `image` and `velocity-image` both run `nix build` with
no `--out-link`, so both land on `./result`, and a parallel make lets each `load`
step read whichever build most recently swapped the shared symlink. A **third**
image makes that more likely, not less, and the entry names the fix.

Replace the image variables and targets with:

```make
IMAGE ?= $(shell nix eval --raw .#paper-image.imageName):$(shell nix eval --raw .#paper-image.imageTag)
VELOCITY_IMAGE ?= $(shell nix eval --raw .#velocity-image.imageName):$(shell nix eval --raw .#velocity-image.imageTag)
OPERATOR_IMAGE ?= $(shell nix eval --raw .#operator-image.imageName):$(shell nix eval --raw .#operator-image.imageTag)
```

```make
.PHONY: image
image:
	nix build .#paper-image --out-link result-paper

.PHONY: image-load
image-load: image
	$(CONTAINER) load < result-paper

.PHONY: velocity-image
velocity-image:
	nix build .#velocity-image --out-link result-velocity

.PHONY: velocity-image-load
velocity-image-load: velocity-image
	$(CONTAINER) load < result-velocity

# Its own out-link, like the two above. Sharing ./result is what let a parallel
# `make -j` load whichever image finished last; see docs/known-issues.md.
.PHONY: operator-image
operator-image:
	nix build .#operator-image --out-link result-operator

.PHONY: operator-image-load
operator-image-load: operator-image
	$(CONTAINER) load < result-operator

.PHONY: operator-image-test
operator-image-test: operator-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(OPERATOR_IMAGE) hack/operator-image-test.sh
```

Add the operator image to `image-repro`, beside the two `nix build --rebuild`
lines already there:

```make
	nix build .#operator-image --rebuild
```

`result-*` is already in `.gitignore`; no change needed there.

- [ ] **Step 7: Run the test to verify it passes**

Run:

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make operator-image-test CONTAINER=podman
```

Expected: `OK: ghcr.io/spawnery/spawnery-operator:0.1.0`.

Then check the two existing images still build and load through their new
out-links:

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-test CONTAINER=podman
```

Expected: both smoke tests pass as before.

- [ ] **Step 8: Verify the assertions can fail**

Perform each mutation, run the command, record the output, then revert:

1. In `nix/operator-image.nix`, change `WorkingDir = "/"` to `WorkingDir = "/data"`
   and add `extraCommands = "mkdir -p data";`. Run `make operator-image-test`.
   Expected: `FAIL: the image has a /data directory`.
2. Remove `oci-common.passwd` and `oci-common.group` from `contents` and set
   `User = ""`. Run it. Expected: `FAIL: image user = '', want 10001:10001`.
3. In `cmd/spawnery-operator/main.go`, rename the `startup-deadline` flag to
   `startup-deadlin`. Run it. Expected: `FAIL: the operator's usage does not
   mention -startup-deadline`.

If any mutation leaves the test green, the assertion is not testing what it
claims and must be fixed before the commit.

- [ ] **Step 9: Commit**

```bash
git add flake.nix nix/operator-image.nix nix/oci-common.nix hack/operator-image-test.sh Makefile
git commit
```

Subject: `feat(6a): an image for the operator, and one out-link per image`.
The body says why the operator does not reuse `oci-common.layeredImage`, why
`operatorVersion` is not `imageVersion`, and that the `--out-link` change closes
the `make -j` entry in `docs/known-issues.md` rather than being incidental
tidying.

---

### Task 2: The Deployment stops carrying a test value

**Files:**
- Modify: `config/deploy/deployment.yaml` (the image reference and `--startup-deadline`)
- Modify: `internal/rbacaudit/deploy_envtest_test.go` (strict decoding, two new tests)

**Interfaces:**
- Consumes: `operatorVersion = "0.1.0"` from Task 1, as the tag the manifest names.
- Produces: a `config/deploy/deployment.yaml` whose flags are guarded. Task 4's
  `hack/e2e.sh` relies on the deadline being the production value and appends
  its own `--startup-deadline=20s` after it.

Why this is a defect and not a preference: `cmd/spawnery-operator/main.go:145`
defaults `--startup-deadline` to five minutes; the manifest overrides it to 20
seconds because §3.1 of the 2026-08-07 E2E design wanted the failure path
reachable inside one test run. Those five manifests are now what gets installed
(spec §5, §12). Twenty seconds is below what a healthy server takes: milestone
5a's evidence run measured 24 seconds from apply to `ReadyGatePassed` on an idle
single-node `kind` cluster with the image already present.

- [ ] **Step 1: Write the failing tests**

Add to `internal/rbacaudit/deploy_envtest_test.go`. Both are pure manifest
reads and need no cluster, but they belong beside the other manifest tests
rather than in a new package.

```go
// TestTheOperatorDeploymentCarriesProductionFlags is the guard
// docs/known-issues.md asked for under "The flags in the Deployment are
// unchecked": sigs.k8s.io/yaml is not strict, so a mistyped key disappears
// silently, and until now nothing looked at the container's arguments at all.
//
// The floor on --startup-deadline is the point. These manifests are what gets
// installed, not test scaffolding: milestone 5a's evidence run measured 24
// seconds from apply to ReadyGatePassed on an idle single-node kind cluster
// with the image already present -- the favourable case in every dimension
// that matters, since there was no image pull, no contention and no world to
// read. A manifest carrying 20s would fail every server on a real cluster.
// hack/e2e.sh gets its short deadline by appending a second occurrence of the
// flag, which Go's flag package resolves to the last one.
func TestTheOperatorDeploymentCarriesProductionFlags(t *testing.T) {
	var deploy appsv1.Deployment
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(deploy.Spec.Template.Spec.Containers))
	}

	args := map[string]string{}
	for _, a := range deploy.Spec.Template.Spec.Containers[0].Args {
		name, value, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !ok {
			t.Errorf("argument %q is not --flag=value, so this test cannot judge it", a)
			continue
		}
		args[name] = value
	}

	want := []string{
		"leader-elect",
		"startup-deadline",
		"metrics-bind-address",
		"health-probe-bind-address",
	}
	for _, name := range want {
		if _, ok := args[name]; !ok {
			t.Errorf("the operator container does not pass --%s", name)
		}
	}
	for name := range args {
		if !slices.Contains(want, name) {
			t.Errorf("the operator container passes --%s, which this test does not know "+
				"about. The flag package rejects nothing it is not given and the YAML "+
				"decoder accepts any string, so a mistyped flag reaches a real cluster "+
				"silently. Add it here deliberately, or fix the typo", name)
		}
	}

	deadline, err := time.ParseDuration(args["startup-deadline"])
	if err != nil {
		t.Fatalf("--startup-deadline=%q does not parse: %v", args["startup-deadline"], err)
	}
	if deadline < 5*time.Minute {
		t.Errorf("--startup-deadline=%s, want at least 5m. Milestone 5a's evidence run "+
			"measured 24 seconds from apply to ReadyGatePassed on an idle single-node "+
			"kind cluster with the image already present; a shorter deadline in the "+
			"manifest a person installs fails healthy servers. The E2E run patches its "+
			"own copy down instead", deadline)
	}
}

// TestTheOperatorImageIsNotAMutableTag guards what the manifest points at. It
// named ghcr.io/spawnery/spawnery-operator:dev until milestone 6a -- a tag
// nothing produced, so the manifest referenced nothing at all. The master
// design's §8 asks for digest references in shipped manifests because tags are
// mutable; hack/publish.sh writes one in after a push, and until it has run the
// version tag is what resolves.
func TestTheOperatorImageIsNotAMutableTag(t *testing.T) {
	var deploy appsv1.Deployment
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(deploy.Spec.Template.Spec.Containers))
	}
	ref := deploy.Spec.Template.Spec.Containers[0].Image

	const repo = "ghcr.io/spawnery/spawnery-operator"
	if digest, ok := strings.CutPrefix(ref, repo+"@"); ok {
		if !strings.HasPrefix(digest, "sha256:") {
			t.Errorf("image = %q: the digest does not start with sha256:", ref)
		}
		return
	}
	tag, ok := strings.CutPrefix(ref, repo+":")
	if !ok {
		t.Fatalf("image = %q, want %s with either a tag or a digest", ref, repo)
	}
	switch tag {
	case "", "dev", "latest":
		t.Errorf("image = %q. %q is a tag nothing publishes or a tag that moves; the "+
			"operator would either fail to pull or silently change version between "+
			"restarts", ref, tag)
	}
}
```

Add `"slices"`, `"strings"` and `"time"` to the import block if they are not
already there (`strings` is).

- [ ] **Step 2: Run them to verify they fail**

Run:

```bash
nix develop -c go test ./internal/rbacaudit/ -run 'TestTheOperatorDeploymentCarriesProductionFlags|TestTheOperatorImageIsNotAMutableTag' -v
```

Expected: FAIL twice — `--startup-deadline=20s, want at least 5m` and
`image = "ghcr.io/spawnery/spawnery-operator:dev". "dev" is a tag nothing
publishes…`.

- [ ] **Step 3: Fix the manifest**

In `config/deploy/deployment.yaml`, change the container's image and args:

```yaml
          # A tag until hack/publish.sh has run once, then the digest it writes
          # back. The master design's §8 asks for digests in shipped manifests
          # because tags are mutable; ":dev" before milestone 6a was worse than
          # either, being a tag nothing produced.
          image: ghcr.io/spawnery/spawnery-operator:0.1.0
          args:
            - --leader-elect=true
            # The production value. This manifest is installed, not test
            # scaffolding: hack/e2e.sh appends a second --startup-deadline=20s
            # for its own run, and the flag package takes the last occurrence.
            - --startup-deadline=5m
            - --metrics-bind-address=:8080
            - --health-probe-bind-address=:8081
```

- [ ] **Step 4: Make the manifest decoder strict**

`docs/known-issues.md` names the cause: "`sigs.k8s.io/yaml` is not strict, so a
mistyped key disappears silently". In `readManifest`, replace
`yaml.Unmarshal(raw, into)` with `yaml.UnmarshalStrict(raw, into)`, and extend
the doc comment:

```go
// readManifest decodes a single-document YAML manifest from the repository.
// Strictly: sigs.k8s.io/yaml's plain Unmarshal drops keys the target type does
// not have, so `serviceAccountNam:` or `readOnlyRootFilesytem:` would decode
// into a zero value and every assertion below would then be checking a field
// nobody set.
```

- [ ] **Step 5: Run the whole suite**

Run:

```bash
nix develop -c make test
```

Expected: PASS. Roughly 38 seconds; `internal/controller` is about 34 of them.
If `UnmarshalStrict` turns some *other* manifest read red, that is a real find
in that manifest — fix it and say so in the commit body rather than reverting
to the loose decoder.

- [ ] **Step 6: Verify the assertions can fail**

Perform each mutation, run `nix develop -c go test ./internal/rbacaudit/ -v`,
record the output, revert:

1. Set `--startup-deadline=20s` again → the deadline assertion fails, naming 5a's
   measurement.
2. Change `--leader-elect=true` to `--leader-elct=true` → the unknown-flag
   assertion fails naming `leader-elct`, *and* the missing-flag assertion fails
   naming `leader-elect`. Both should fire; if only one does, the loop is wrong.
3. Set the image back to `:dev` → the tag assertion fails.
4. In `config/deploy/deployment.yaml`, rename `serviceAccountName` to
   `serviceAccountNam`. Expected: `TestDeployManifestsAreAcceptedAndConsistent`
   now fails with a decode error naming the unknown field. Before Step 4 it
   would have failed with an empty-string comparison instead, or passed if the
   comparison had been absent — that is the defect the strict decoder closes,
   so record both the before and the after here.

- [ ] **Step 7: Commit**

```bash
git add config/deploy/deployment.yaml internal/rbacaudit/deploy_envtest_test.go
git commit
```

Subject: `fix(6a): the installed manifest stops carrying a test value`.
The body says that `--startup-deadline=20s` was correct while `config/deploy/`
was scaffolding and is wrong now that it is installed, quotes 5a's 24-second
measurement, and notes that the strict decoder closes the known-issues entry
about unchecked flags.

---

### Task 3: Publishing all three images

**Files:**
- Create: `hack/publish.sh`
- Modify: `flake.nix` (add `skopeo` to the dev shell)
- Modify: `Makefile` (a `publish` target)

**Interfaces:**
- Consumes: `packages.operator-image` from Task 1, and the existing
  `packages.paper-image` and `packages.velocity-image`.
- Produces: `hack/publish.sh`, honouring `DRY_RUN=1`, `FORCE=1` and
  `WRITE_DIGEST=1`; the make target `publish`.

The registry is `ghcr.io/spawnery/` and is public, so nothing downstream needs
an `imagePullSecret` (spec §4.1). Authentication for the push comes from the
environment — a GitHub token with `write:packages` — and never from a file in
the repository.

**This task's acceptance stops at the dry run.** The real push needs a token
this plan's implementer does not have; it is a step the repository owner drives,
like an evidence run.

- [ ] **Step 1: Add `skopeo` to the dev shell**

In `flake.nix`, in the `mkShell` package list, after `k3d`:

```nix
              # hack/publish.sh copies each image archive straight from the Nix
              # store to the registry. A local container store in between would
              # publish whatever a stale `podman load` left behind rather than
              # what the flake describes.
              skopeo
```

- [ ] **Step 2: Write `hack/publish.sh`**

```bash
#!/usr/bin/env bash
# Publish the three Spawnery images to ghcr.io.
#
# Every image is built by Nix and copied from its archive straight to the
# registry: no local container store in between, so what lands there is what
# the flake describes and not what a previous `podman load` left behind.
#
# Environment:
#   DRY_RUN=1       print what would be copied where; contact nothing.
#   FORCE=1         overwrite a tag that already exists.
#   WRITE_DIGEST=1  rewrite config/deploy/deployment.yaml's operator image to
#                   the digest the registry returned.
set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
FORCE="${FORCE:-0}"
WRITE_DIGEST="${WRITE_DIGEST:-0}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# attr:out-link, in the order they are published. The operator is last so that
# a WRITE_DIGEST run does not rewrite the manifest before the two large images
# have succeeded.
images=(
	"paper-image:result-paper"
	"velocity-image:result-velocity"
	"operator-image:result-operator"
)

operator_digest=""

for entry in "${images[@]}"; do
	attr="${entry%%:*}"
	link="${entry##*:}"

	nix build ".#${attr}" --out-link "$link"
	name="$(nix eval --raw ".#${attr}.imageName")"
	tag="$(nix eval --raw ".#${attr}.imageTag")"
	ref="docker://${name}:${tag}"

	if [ "$DRY_RUN" = "1" ]; then
		echo "would copy docker-archive:${link} -> ${ref}"
		continue
	fi

	# Refuse rather than replace. A tag that already exists and is not the
	# archive in hand means somebody else published it, or this version was
	# never bumped -- both are worth stopping for, and neither is worth
	# discovering from a cluster that pulled something unexpected.
	if [ "$FORCE" != "1" ] && skopeo inspect "$ref" >/dev/null 2>&1; then
		echo "refusing to overwrite ${name}:${tag}, which already exists. Bump the" >&2
		echo "version in flake.nix, or re-run with FORCE=1 if you mean it." >&2
		exit 1
	fi

	skopeo copy "docker-archive:${link}" "$ref"
	digest="$(skopeo inspect --format '{{.Digest}}' "$ref")"
	echo "published ${name}:${tag} @ ${digest}"

	if [ "$attr" = "operator-image" ]; then
		operator_digest="$digest"
	fi
done

if [ "$WRITE_DIGEST" = "1" ] && [ -n "$operator_digest" ]; then
	manifest="config/deploy/deployment.yaml"
	name="$(nix eval --raw '.#operator-image.imageName')"
	# The one line that names the operator image, replaced with a digest
	# reference. The master design's §8 asks for this in shipped manifests
	# because a tag can move under a running cluster.
	sed -i -E "s|(^[[:space:]]*image:[[:space:]]*)${name}[:@].*$|\1${name}@${operator_digest}|" "$manifest"
	echo "wrote ${name}@${operator_digest} into ${manifest}"
fi
```

Make it executable: `chmod +x hack/publish.sh`.

- [ ] **Step 3: Add the make target**

```make
# Not part of `all`: it contacts a registry and needs a token. DRY_RUN=1 prints
# what it would copy and contacts nothing.
.PHONY: publish
publish:
	hack/publish.sh
```

- [ ] **Step 4: Run the dry run**

Run:

```bash
nix develop -c env DRY_RUN=1 make publish
```

Expected output, three lines naming the three archives and their references:

```
would copy docker-archive:result-paper -> docker://ghcr.io/spawnery/paper:26.2-0.2.0
would copy docker-archive:result-velocity -> docker://ghcr.io/spawnery/velocity:3.5.1-0.2.0
would copy docker-archive:result-operator -> docker://ghcr.io/spawnery/spawnery-operator:0.1.0
```

The exact versions come from the flake; what matters is that all three resolve
and none is empty.

- [ ] **Step 5: Verify the refusal path without a registry**

`skopeo inspect` against a reference that does not exist fails, which is the
path that lets a publish proceed. Check the opposite branch is reachable by
pointing it at something that does exist:

```bash
nix develop -c skopeo inspect docker://ghcr.io/spawnery/paper:26.2-0.2.0 >/dev/null 2>&1; echo "exit=$?"
```

Record the exit code. If it is 0, a real `make publish` would refuse without
`FORCE=1`, which is the intended behaviour; if it is non-zero, nothing has been
published yet and the first real run will proceed. Either answer is fine — the
point is to know which, and to write it into the commit body rather than
guessing.

- [ ] **Step 6: Commit**

```bash
git add flake.nix hack/publish.sh Makefile
git commit
```

Subject: `feat(6a): one publish path for all three images`.
The body says why `skopeo` copies from the archive rather than from a container
store, why the script refuses an existing tag by default, and that the real push
is a hand-driven step the repository owner takes with a `write:packages` token —
`docs/known-issues.md`'s "No image is published" entry is closed by the
mechanism, and by the owner running it.

---

### Task 4: The E2E harness

**Files:**
- Create: `hack/e2e.sh`
- Create: `test/e2e/e2e_test.go`
- Create: `test/e2e/manifests/e2e.yaml`
- Modify: `Makefile` (an `e2e` target)

**Interfaces:**
- Consumes: `packages.operator-image` (Task 1) and the fixed manifest (Task 2).
- Produces, for Tasks 5–8: the package-level `k8s client.Client`, `clientset
  *kubernetes.Clientset`, `ctx context.Context`; the helpers `eventually(t,
  deadline, what, cond)`, `applyManifest(t, rel string, opts ...client.CreateOption)`,
  `operatorLog(t) string`; the constants `testNamespace = "minecraft"` and
  `operatorNamespace = "spawnery-system"`; and the ordered driver
  `TestSpawneryUnderItsOwnServiceAccount`, into which every later task inserts
  one `t.Run` line **above** the final `"the operator was never denied"` line.

- [ ] **Step 1: Write the test manifest**

Create `test/e2e/manifests/e2e.yaml`. It is deliberately not
`config/samples/network.yaml`: this one needs `failedRetentionSeconds: 30` so
the failure path closes inside one run, and the sample should stay a realistic
starting point.

```yaml
# The manifest the driven run works on. Not config/samples/network.yaml -- that
# one is a realistic starting point for a user and should stay one; this one is
# bent to fit a test run, and says where.
#
# The images are deliberately unresolvable. Since milestone 6a publishes Paper
# and Velocity to a public registry, naming a real tag here would make the
# kubelet pull 724 MB into a fresh kind node on every run and start actual
# servers -- which is milestone 6a's declared non-goal (spec §1.4, §7.4). Pods
# staying in ErrImagePull is a decision, and this is where it is taken.
apiVersion: v1
kind: Namespace
metadata:
  name: minecraft
---
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  secret: e2e-forwarding-secret
---
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret
  defaults:
    minecraftVersion: "26.2"
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Ephemeral
  image: ghcr.io/spawnery/paper:e2e-no-such-tag
  maxPlayers: 100
  # Short, so scenario 6 -- Failed, then the corpse cleared -- closes inside a
  # single run rather than an hour.
  failedRetentionSeconds: 30
  drain:
    timeoutSeconds: 30
  scaling:
    minReplicas: 2
    maxReplicas: 4
    spareSlots: 10
```

- [ ] **Step 2: Write the failing test**

Create `test/e2e/e2e_test.go`.

```go
//go:build e2e

// Package e2e drives the operator in a real cluster.
//
// It exists for one assertion internal/rbacaudit structurally cannot make.
// That audit compares the generated ClusterRole against a hand-maintained
// table in both directions, so it catches drift -- but a permission missing
// from *both* leaves the suite green while the operator still walks into a
// Forbidden the first time it runs under its own ServiceAccount. Proving
// completeness needs a real process under a real authorizer, and that is what
// this package watches.
//
// The build tag keeps it out of `go test ./...` and out of `make test`: it
// needs a cluster that hack/e2e.sh builds, and the commit loop stays where it
// is.
package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	// operatorNamespace is where config/deploy/ puts everything, and the
	// literal the kubebuilder markers carry for the operator's own Secret and
	// Lease rights. See docs/known-issues.md, "spawnery-system is hard-wired
	// into the RBAC markers".
	operatorNamespace = "spawnery-system"

	// testNamespace is where test/e2e/manifests/e2e.yaml puts its objects.
	testNamespace = "minecraft"

	// repoRoot is relative because `go test` runs each binary with its own
	// package directory as the working directory.
	repoRoot = "../.."
)

var (
	k8s       client.Client
	clientset *kubernetes.Clientset
	ctx       context.Context
)

func TestMain(m *testing.M) {
	cfg, err := config.GetConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no usable kubeconfig: %v\nRun this through hack/e2e.sh.\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "build scheme: %v\n", err)
		os.Exit(1)
	}
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "build scheme: %v\n", err)
		os.Exit(1)
	}

	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		os.Exit(1)
	}
	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build clientset: %v\n", err)
		os.Exit(1)
	}
	ctx = context.Background()

	os.Exit(m.Run())
}

// TestSpawneryUnderItsOwnServiceAccount is the whole run, ordered explicitly.
//
// Go runs top-level tests in the order they appear and files in alphabetical
// order, which would make the order between scenarios an accident of file
// naming. These depend on one another -- the manifest has to exist before
// anything can scale it -- so the order is written down instead. The denial
// check is last because it judges everything the run did.
func TestSpawneryUnderItsOwnServiceAccount(t *testing.T) {
	t.Run("the operator is up and has not restarted", theOperatorIsUp)
	t.Run("the operator was never denied", theOperatorWasNeverDenied)
}

// theOperatorIsUp checks the pod the whole run depends on. A crash loop here
// reads as every later scenario timing out, which says nothing about the cause.
func theOperatorIsUp(t *testing.T) {
	pod := operatorPod(t)

	ready := false
	for _, c := range pod.Status.ContainerStatuses {
		if c.Ready {
			ready = true
		}
		if c.RestartCount > 0 {
			t.Errorf("the operator container has restarted %d time(s); the log below is "+
				"the current process only, so an earlier denial may not appear in it",
				c.RestartCount)
		}
	}
	if !ready {
		t.Fatalf("the operator pod %s is not ready: phase %s", pod.Name, pod.Status.Phase)
	}
}

// theOperatorWasNeverDenied is the reason this package exists.
//
// It matches the API server's own phrasing -- `is forbidden:` -- rather than
// the bare word. Spawnery has a condition reason called SecretReadForbidden
// (milestone 5c), and matching "forbidden" alone would turn a correctly
// reported missing secret into a false accusation about RBAC.
func theOperatorWasNeverDenied(t *testing.T) {
	var offenders []string
	for _, line := range strings.Split(operatorLog(t), "\n") {
		if strings.Contains(line, "is forbidden:") {
			offenders = append(offenders, line)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("the operator was denied %d time(s) under its own ServiceAccount:\n%s\n\n"+
			"This is the assertion internal/rbacaudit cannot make. It compares the "+
			"generated role against its table in both directions, so a permission "+
			"missing from both leaves it green while this fails.",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

// operatorPod returns the single operator pod, or fails.
func operatorPod(t *testing.T) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	err := k8s.List(ctx, &pods,
		client.InNamespace(operatorNamespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":      "spawnery",
			"app.kubernetes.io/component": "operator",
		})
	if err != nil {
		t.Fatalf("list operator pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("got %d operator pods, want exactly one", len(pods.Items))
	}
	return &pods.Items[0]
}

// operatorLog reads the operator's whole log through the API.
func operatorLog(t *testing.T) string {
	t.Helper()
	pod := operatorPod(t)
	stream, err := clientset.CoreV1().
		Pods(operatorNamespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{}).
		Stream(ctx)
	if err != nil {
		t.Fatalf("stream logs of %s: %v", pod.Name, err)
	}
	defer func() { _ = stream.Close() }()

	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read logs of %s: %v", pod.Name, err)
	}
	return string(body)
}

// eventually polls cond until it holds or the deadline passes, and reports the
// last thing it saw when it gives up.
//
// It is the only waiting construct in this package. A run built on fixed sleeps
// turns flaky under load, and a flaky E2E run is ignored within weeks -- which
// is §4 of the 2026-08-07 E2E design, kept.
func eventually(t *testing.T, deadline time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	stop := time.Now().Add(deadline)
	last := "nothing observed yet"
	for time.Now().Before(stop) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s; last seen: %s", deadline, what, last)
}

// applyManifest creates every document of a multi-document manifest, tolerating
// objects that are already there. Pass client.DryRunAll to check that a
// manifest is *accepted* without creating anything.
func applyManifest(t *testing.T, rel string, opts ...client.CreateOption) {
	t.Helper()

	f, err := os.Open(repoRoot + "/" + rel)
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	defer func() { _ = f.Close() }()

	docs := utilyaml.NewYAMLReader(bufio.NewReader(f))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.TrimSpace(string(doc)) == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, obj); err != nil {
			t.Fatalf("decode a document of %s: %v", rel, err)
		}
		if obj.GetKind() == "" {
			continue
		}
		if err := k8s.Create(ctx, obj, opts...); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %s %s/%s from %s: %v",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), rel, err)
		}
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run:

```bash
nix develop -c go test -tags e2e -count=1 ./test/e2e/... -v
```

Expected: FAIL — `no usable kubeconfig` (there is no cluster yet), or, if a
kubeconfig happens to be present, a failure listing zero operator pods. Either
is the right kind of failure: the assertions are real and nothing satisfies them.

- [ ] **Step 4: Write `hack/e2e.sh`**

```bash
#!/usr/bin/env bash
# The driven end-to-end run: the operator inside a real cluster, under its own
# ServiceAccount.
#
# This script is plumbing only. It creates the cluster, gets the operator
# running in it, and hands over; every claim is made by the Go package in
# test/e2e. The split is the 2026-08-07 E2E design's §4, kept, and the reason is
# that a shell script asserting on cluster state produces failure messages
# nobody can act on.
#
# On this machine kind runs under rootless podman, which needs both an
# environment variable and a systemd scope. The script deliberately hard-codes
# neither:
#
#   systemd-run --scope --user --property=Delegate=yes -- \
#     nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

workdir="$(mktemp -d)"
KUBECONFIG="$workdir/kubeconfig"
export KUBECONFIG

dump() {
	echo "================ operator log ================"
	kubectl -n spawnery-system logs deployment/spawnery-operator --tail=-1 2>&1 || true
	echo "================ objects ================"
	kubectl get networks,servergroups,proxygroups,servers,pods,pvc -A 2>&1 || true
	echo "================ events ================"
	kubectl get events -A --sort-by=.lastTimestamp 2>&1 || true
}

cleanup() {
	local status=$?
	if [ "$status" -ne 0 ]; then
		dump
	fi
	if [ "$E2E_KEEP" = "1" ]; then
		echo "E2E_KEEP=1: cluster '$CLUSTER' left standing; KUBECONFIG=$KUBECONFIG"
	else
		kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
		rm -rf "$workdir"
	fi
	exit "$status"
}
trap cleanup EXIT

nix build .#operator-image --out-link result-operator
image="$(nix eval --raw '.#operator-image.imageName'):$(nix eval --raw '.#operator-image.imageTag')"

# dockerTools.buildLayeredImage emits a gzipped archive; `kind load
# image-archive` wants a plain tar. Decompress if it is compressed and copy if
# it is not, so this keeps working either way.
archive="$workdir/operator.tar"
if gunzip -t result-operator 2>/dev/null; then
	gunzip -c result-operator >"$archive"
else
	cp -L result-operator "$archive"
fi

kind create cluster --name "$CLUSTER" --wait 120s
kind load image-archive "$archive" --name "$CLUSTER"

kubectl apply -f config/crd/bases/
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/deploy/

# The per-namespace grant milestone 5c deliberately kept out of config/deploy/.
# The ClusterRole grants no access to secrets outside the operator's own
# namespace, so an administrator opens exactly the namespaces holding a
# Network -- and this run is the first thing in the repository that has to
# actually be that administrator. Without it the operator's read of the
# forwarding secret is refused, the Network reports
# Unknown/SecretReadForbidden, and test/e2e's denial check fires on a denial
# that is nobody's bug but this script's.
#
# The namespace is created here rather than by the test manifest so the grant
# can exist before the operator ever looks; applyManifest tolerates the
# namespace already being there.
kubectl create namespace minecraft
kubectl apply -n minecraft -f config/rbac/forwarding-secret-reader.yaml

# Three edits, none of which belongs in the manifest itself.
#
# The image: the run tests the bits just built, not whatever the registry
# happens to hold. imagePullPolicy Never makes that a guarantee rather than a
# hope -- with it, a missing local image fails loudly instead of being fetched.
#
# The deadline: config/deploy/deployment.yaml carries the production five
# minutes, which is longer than this run. Appending a second occurrence rather
# than rewriting the list means the manifest stays the single place the flags
# are written; Go's flag package resolves a repeated flag to the last one, and
# scenario 6 is what proves it did.
kubectl -n spawnery-system patch deployment spawnery-operator --type=json -p "$(
	cat <<EOF
[
  {"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "${image}"},
  {"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"},
  {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--startup-deadline=20s"}
]
EOF
)"

kubectl -n spawnery-system rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"

go test -tags e2e -count=1 -v -timeout 20m ./test/e2e/...
```

Make it executable: `chmod +x hack/e2e.sh`.

- [ ] **Step 5: Add the make target**

```make
# The driven run from the milestone 6a design. Explicitly not part of `test` or
# `all`: it builds a cluster and takes minutes, and the commit loop stays at
# around 25 seconds. See hack/e2e.sh's header for the rootless-podman
# invocation this machine needs.
.PHONY: e2e
e2e:
	hack/e2e.sh
```

- [ ] **Step 6: Run it to verify it passes**

Run:

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

Expected: the cluster comes up, the rollout succeeds, and both subtests pass:

```
--- PASS: TestSpawneryUnderItsOwnServiceAccount
    --- PASS: TestSpawneryUnderItsOwnServiceAccount/the_operator_is_up_and_has_not_restarted
    --- PASS: TestSpawneryUnderItsOwnServiceAccount/the_operator_was_never_denied
```

If the rollout times out, read the dump the script prints on failure. The most
likely first-run causes, in order: `kind load` did not place the image where the
kubelet looks (the pod shows `ErrImageNeverPull`), or the operator crashed on a
permission the markers never granted — which is exactly what this milestone
exists to find, and belongs in the commit body rather than being quietly fixed.

- [ ] **Step 7: Verify the denial assertion can fail**

This is the mutation that matters most in the whole milestone, because a
denial check that cannot fire would make every later task's green run
meaningless.

Remove one verb the operator uses on every pass — `list` on `pods` — from its
marker, regenerate, and run again:

```bash
# in internal/controller/, find the marker granting pods and drop `list`
nix develop -c make manifests
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

Expected: `the operator was denied N time(s) under its own ServiceAccount`,
quoting a line containing `is forbidden:`. Record the quoted line. Note that
`make test` also goes red here, from `internal/rbacaudit` — that is level A
catching the drift, and it is a different failure from this one. Revert both.

- [ ] **Step 8: Commit**

```bash
git add hack/e2e.sh test/e2e/ Makefile
git commit
```

Subject: `feat(6a): the operator runs in a cluster, and says nothing is denied`.
The body says what the harness does and does not do, why the claims live in Go
and the plumbing in shell, why the denial check matches `is forbidden:` rather
than the bare word, and what the mutation in Step 7 printed.

---

### Task 5: Scenarios 1–3 — accepted, scale up, scale down

**Files:**
- Create: `test/e2e/lifecycle_test.go`
- Modify: `test/e2e/e2e_test.go` (three `t.Run` lines)

**Interfaces:**
- Consumes: `k8s`, `ctx`, `eventually`, `applyManifest`, `testNamespace` from Task 4.
- Produces: `serversInGroup(t, group string) []spawneryv1alpha1.Server` and
  `patchMinReplicas(t, group string, n int32)`, both used by Task 6.

- [ ] **Step 1: Write the failing tests**

Create `test/e2e/lifecycle_test.go`.

```go
//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// theTestManifestIsAccepted applies the run's own manifest and waits for the
// group to build what it asks for.
//
// It also checks that config/samples/network.yaml is accepted, so the example
// cannot rot unnoticed -- with client.DryRunAll, because the sample names the
// real Paper and Velocity images. Since milestone 6a publishes those to a
// public registry, actually creating it would make the kubelet pull 724 MB into
// this node and start real servers, which is the run's declared non-goal.
//
// The order of the two applies is load-bearing and not cosmetic. Every object
// in the sample collides by name with one in the run's own manifest -- both
// describe a Network `production`, a ServerGroup `lobby` and a ProxyGroup
// `gateway` in `minecraft` -- and applyManifest tolerates AlreadyExists. Run
// the other way round, the sample check would pass without the API server
// having validated a single one of its objects.
func theTestManifestIsAccepted(t *testing.T) {
	applyManifest(t, "config/samples/network.yaml", client.DryRunAll)
	applyManifest(t, "test/e2e/manifests/e2e.yaml")

	eventually(t, 2*time.Minute, "the lobby group's two Servers", func() (bool, string) {
		servers := serversInGroup(t, "lobby")
		return len(servers) == 2, fmt.Sprintf("%d Servers", len(servers))
	})

	eventually(t, 2*time.Minute, "a pod per Server", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods,
			client.InNamespace(testNamespace),
			client.MatchingLabels{podspec.LabelGroup: "lobby"}); err != nil {
			return false, err.Error()
		}
		return len(pods.Items) == 2, fmt.Sprintf("%d pods", len(pods.Items))
	})

	// The pods stay in ErrImagePull, and that is the expected end state:
	// test/e2e/manifests/e2e.yaml names an unresolvable image on purpose. This
	// assertion is what keeps that a decision -- if somebody points the manifest
	// at a real tag, this fails and says why rather than quietly making every
	// run pull 724 MB.
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{podspec.LabelGroup: "lobby"}); err != nil {
		t.Fatalf("list lobby pods: %v", err)
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if c.State.Running != nil {
				t.Errorf("pod %s is running. Milestone 6a loads no game image and its "+
					"manifest names an unresolvable one on purpose (spec §7.4); a running "+
					"server here means the manifest was pointed at a real tag", p.Name)
			}
		}
	}
}

// theGroupScalesUp raises minReplicas and waits for the operator to build the
// difference.
func theGroupScalesUp(t *testing.T) {
	patchMinReplicas(t, "lobby", 3)
	eventually(t, 2*time.Minute, "a third Server", func() (bool, string) {
		servers := serversInGroup(t, "lobby")
		return len(servers) == 3, fmt.Sprintf("%d Servers", len(servers))
	})
}

// theGroupScalesDown lowers minReplicas again. The servers carry no players --
// nothing is Ready, so nothing can -- so the drain has nothing to wait for and
// the surplus goes.
func theGroupScalesDown(t *testing.T) {
	patchMinReplicas(t, "lobby", 2)
	eventually(t, 3*time.Minute, "the surplus Server to go", func() (bool, string) {
		servers := serversInGroup(t, "lobby")
		return len(servers) == 2, fmt.Sprintf("%d Servers", len(servers))
	})
}

// serversInGroup lists the Servers of one group in the test namespace.
func serversInGroup(t *testing.T, group string) []spawneryv1alpha1.Server {
	t.Helper()
	var list spawneryv1alpha1.ServerList
	if err := k8s.List(ctx, &list,
		client.InNamespace(testNamespace),
		client.MatchingLabels{podspec.LabelGroup: group}); err != nil {
		t.Fatalf("list Servers of group %s: %v", group, err)
	}
	return list.Items
}

// patchMinReplicas edits one group's floor and nothing else.
func patchMinReplicas(t *testing.T, group string, n int32) {
	t.Helper()
	var g spawneryv1alpha1.ServerGroup
	key := client.ObjectKey{Namespace: testNamespace, Name: group}
	if err := k8s.Get(ctx, key, &g); err != nil {
		t.Fatalf("get ServerGroup %s: %v", group, err)
	}
	patch := client.MergeFrom(g.DeepCopy())
	if g.Spec.Scaling == nil {
		t.Fatalf("ServerGroup %s has no scaling block; this test edits its floor", group)
	}
	g.Spec.Scaling.MinReplicas = n
	if err := k8s.Patch(ctx, &g, patch); err != nil {
		t.Fatalf("patch ServerGroup %s to minReplicas=%d: %v", group, n, err)
	}
}
```

Insert the three subtests into `TestSpawneryUnderItsOwnServiceAccount`, above
the denial line:

```go
	t.Run("the operator is up and has not restarted", theOperatorIsUp)
	t.Run("the test manifest is accepted", theTestManifestIsAccepted)
	t.Run("the group scales up", theGroupScalesUp)
	t.Run("the group scales down", theGroupScalesDown)
	t.Run("the operator was never denied", theOperatorWasNeverDenied)
```

- [ ] **Step 2: Run to verify they fail**

Temporarily set `minReplicas: 0` in `test/e2e/manifests/e2e.yaml` — an
ephemeral group with no floor and no demand builds nothing — and run:

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

Expected: `timed out after 2m0s waiting for the lobby group's two Servers; last
seen: 0 Servers`. Restore `minReplicas: 2` afterwards.

- [ ] **Step 3: Run to verify they pass**

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

Expected: five subtests pass.

- [ ] **Step 4: Verify the assertions can fail**

Perform, run, record, revert:

1. In `internal/controller/scaling.go`, make `DecideSize` return an empty
   `SizeDecision` unconditionally. Expected: "the lobby group's two Servers"
   times out at `0 Servers`.
2. Make the deletion branch of `DecideSize` return no `Delete` names. Expected:
   scenario 3 times out at `3 Servers` while 1 and 2 still pass — which is the
   check that scenario 3 is not riding on its neighbours.
3. Point `test/e2e/manifests/e2e.yaml` at `ghcr.io/spawnery/paper:26.2-0.2.0`.
   Expected: the "pod is running" assertion fires. Note the run also gets much
   slower, which is the cost the unresolvable tag avoids.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/
git commit
```

Subject: `test(6a): the group builds what it is asked for, and gives it back`.

---

### Task 6: Scenarios 4–6 — the sweep, the finalizer, and the failure path

**Files:**
- Create: `test/e2e/cleanup_test.go`
- Modify: `test/e2e/e2e_test.go` (three `t.Run` lines)

**Interfaces:**
- Consumes: `serversInGroup`, `patchMinReplicas` (Task 5); `eventually`,
  `k8s`, `ctx`, `testNamespace` (Task 4).

- [ ] **Step 1: Write the failing tests**

Create `test/e2e/cleanup_test.go`.

```go
//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// theOrphanSweepRemovesAStrayPod plants a pod that carries the managed labels
// but belongs to no Server object, and waits for the sweep to take it.
//
// This is the one scenario that checks a code path nothing else in the run
// reaches: every other pod here was created by the operator itself.
func theOrphanSweepRemovesAStrayPod(t *testing.T) {
	orphan := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "e2e-orphan",
			Namespace: testNamespace,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   "production",
				podspec.LabelGroup:     "lobby",
				podspec.LabelServer:    "e2e-orphan",
			},
		},
		Spec: corev1.PodSpec{
			// Never pulled: the sweep deletes it long before the kubelet
			// gives up, and a real image would cost this run a download.
			Containers: []corev1.Container{{
				Name:  "orphan",
				Image: "ghcr.io/spawnery/paper:e2e-no-such-tag",
			}},
		},
	}
	if err := k8s.Create(ctx, orphan); err != nil {
		t.Fatalf("create the orphan pod: %v", err)
	}

	eventually(t, 2*time.Minute, "the orphan sweep to delete e2e-orphan", func() (bool, string) {
		var got corev1.Pod
		err := k8s.Get(ctx, client.ObjectKeyFromObject(orphan), &got)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		if !got.DeletionTimestamp.IsZero() {
			return true, ""
		}
		return false, "still there, with no deletion timestamp"
	})
}

// theFinalizerIsReleased deletes a Server by hand and waits for the object to
// go. The Server carries a finalizer, so the object survives its own deletion
// until the controller has taken the pod down and released it -- a stuck
// finalizer is invisible from a diff and shows up only as an object that never
// disappears.
func theFinalizerIsReleased(t *testing.T) {
	servers := serversInGroup(t, "lobby")
	if len(servers) == 0 {
		t.Fatal("no Servers in the lobby group to delete")
	}
	victim := servers[0]

	if err := k8s.Delete(ctx, &victim); err != nil {
		t.Fatalf("delete Server %s: %v", victim.Name, err)
	}

	eventually(t, 2*time.Minute, "Server "+victim.Name+" to disappear", func() (bool, string) {
		var got spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKeyFromObject(&victim), &got)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, fmt.Sprintf("still there, finalizers %v, deletionTimestamp %v",
			got.Finalizers, got.DeletionTimestamp)
	})
}

// theStartupDeadlineFailsAServerAndClearsIt is scenario 6, and it proves two
// things at once.
//
// The first is the failure path itself: a server whose image never resolves
// cannot become Ready, so --startup-deadline is what ends the attempt, and
// failedRetentionSeconds: 30 is what clears the corpse afterwards.
//
// The second is indirect and worth naming. config/deploy/deployment.yaml
// carries --startup-deadline=5m and hack/e2e.sh appends a second occurrence of
// the flag rather than rewriting the list. If Go's flag package did not resolve
// a repeated flag to the last one, nothing would fail loudly -- this test would
// simply time out. That makes this the only place the append is checked.
func theStartupDeadlineFailsAServerAndClearsIt(t *testing.T) {
	eventually(t, 3*time.Minute, "a Server to reach phase Failed", func() (bool, string) {
		var seen []string
		for _, s := range serversInGroup(t, "lobby") {
			if s.Status.Phase == "Failed" {
				return true, ""
			}
			seen = append(seen, fmt.Sprintf("%s=%s", s.Name, s.Status.Phase))
		}
		return false, fmt.Sprintf("phases: %v", seen)
	})

	eventually(t, 3*time.Minute, "the failed Server's corpse to be cleared", func() (bool, string) {
		for _, s := range serversInGroup(t, "lobby") {
			if s.Status.Phase == "Failed" {
				age := time.Since(s.Status.FailedAt.Time)
				return false, fmt.Sprintf("%s still Failed, %s old", s.Name, age.Round(time.Second))
			}
		}
		return true, ""
	})
}
```

`Status.FailedAt` is `*metav1.Time` (`api/v1alpha1/server_types.go:129`) and
`Status.Phase` is a plain `string` (`:66`), so the comparison against `"Failed"`
is a string comparison and not a typed constant.

Insert the three subtests above the denial line, after the scaling ones:

```go
	t.Run("the orphan sweep removes a stray pod", theOrphanSweepRemovesAStrayPod)
	t.Run("the finalizer is released", theFinalizerIsReleased)
	t.Run("the startup deadline fails a server and clears it", theStartupDeadlineFailsAServerAndClearsIt)
```

- [ ] **Step 2: Run to verify they fail**

Temporarily raise the appended deadline in `hack/e2e.sh` from
`--startup-deadline=20s` to `--startup-deadline=30m`, then run the suite.
Expected: the first two pass, and `a Server to reach phase Failed` times out
listing phases such as `lobby-xxxxx=Starting`. Restore `20s`.

- [ ] **Step 3: Run to verify they pass**

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

Expected: eight subtests pass.

- [ ] **Step 4: Verify the assertions can fail**

Perform, run, record, revert:

1. In `internal/controller/orphan.go`, return from `Sweep` before its `Delete`.
   Expected: scenario 4 times out with "still there, with no deletion
   timestamp".
2. Remove the finalizer-release branch in
   `internal/controller/server_controller.go`. Expected: scenario 5 times out
   printing the finalizer list and a non-nil deletion timestamp — which is the
   difference between "the object went" and "the object was asked to go".
3. Set `failedRetentionSeconds: 3600` in `test/e2e/manifests/e2e.yaml`.
   Expected: the *second* wait in scenario 6 times out while the first passes.
   That is the check that the two waits are independent rather than one
   assertion written twice.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/
git commit
```

Subject: `test(6a): the sweep, the finalizer and the failure path, in a cluster`.
The body notes that scenario 6 is the only check on `hack/e2e.sh`'s appended
flag.

---

### Task 7: Scenarios 7–9 — claims, the proxy Service, certs and the lease

**Files:**
- Create: `test/e2e/persistence_test.go`
- Modify: `test/e2e/manifests/e2e.yaml` (a persistent group and a ProxyGroup)
- Modify: `test/e2e/e2e_test.go` (three `t.Run` lines)

**Interfaces:**
- Consumes: everything from Tasks 4 and 5.

These three could not have existed when the 2026-08-07 design was written.
Scenario 7 is milestone 5a's load-bearing property — a world outliving its
server — checked outside envtest for the first time by anything other than a
person at a terminal.

- [ ] **Step 1: Extend the test manifest**

Append to `test/e2e/manifests/e2e.yaml`:

```yaml
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: survival
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Persistent
  image: ghcr.io/spawnery/paper:e2e-no-such-tag
  maxPlayers: 20
  replicas: 2
  drain:
    timeoutSeconds: 30
  storage:
    # kind's default class is rancher.io/local-path with
    # volumeBindingMode: WaitForFirstConsumer, so these claims stay Pending for
    # as long as no pod runs -- which is the whole run, since the image never
    # resolves. That is fine: this scenario is about the claim's existence, its
    # missing owner reference and its survival, none of which needs a bound
    # volume. BuildDataClaim declines to wait for Bound for the same reason.
    size: 1Gi
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 1
  image: ghcr.io/spawnery/velocity:e2e-no-such-tag
  expose:
    type: NodePort
    nodePort:
      port: 30765
  routing:
    fallbackGroups:
      - lobby
  config:
    playerLimit: 100
    motd: "spawnery e2e"
```

- [ ] **Step 2: Write the failing tests**

Create `test/e2e/persistence_test.go`.

```go
//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// aPersistentGroupsClaimOutlivesItsServer is milestone 5a's central property,
// checked in a real cluster for the first time by anything but a person.
//
// Two halves, and the second is the one that matters. That the claims appear is
// ordinary. That a claim carries no owner reference, and therefore survives
// when its Server goes, is what makes a world durable -- and it cannot be shown
// in envtest at all, because envtest runs no garbage collector, so an owned
// claim outlives its deleted owner there exactly as an unowned one would. That
// limitation is written into the doc comment of
// TestDeletingAPersistentServerLeavesItsClaim; this is where it is answered.
func aPersistentGroupsClaimOutlivesItsServer(t *testing.T) {
	eventually(t, 2*time.Minute, "both ordinals' claims", func() (bool, string) {
		claims := claimsIn(t)
		return len(claims) == 2, fmt.Sprintf("%d claims: %v", len(claims), claimNames(claims))
	})

	for _, c := range claimsIn(t) {
		if len(c.OwnerReferences) != 0 {
			t.Errorf("claim %s carries %d owner reference(s): %v. A world with an owner "+
				"is deleted with it -- by the garbage collector, silently, and only in a "+
				"real cluster",
				c.Name, len(c.OwnerReferences), c.OwnerReferences)
		}
	}

	// The top ordinal goes; its claim stays. This is the route milestone 5a's
	// own evidence run never drove -- it deleted a pod by hand -- and 5b's did.
	var g spawneryv1alpha1.ServerGroup
	key := client.ObjectKey{Namespace: testNamespace, Name: "survival"}
	if err := k8s.Get(ctx, key, &g); err != nil {
		t.Fatalf("get ServerGroup survival: %v", err)
	}
	before := claimNames(claimsIn(t))
	patch := client.MergeFrom(g.DeepCopy())
	one := int32(1)
	g.Spec.Replicas = &one
	if err := k8s.Patch(ctx, &g, patch); err != nil {
		t.Fatalf("patch survival to replicas=1: %v", err)
	}

	eventually(t, 3*time.Minute, "survival-1 to go", func() (bool, string) {
		var s spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "survival-1"}, &s)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "still there, phase " + s.Status.Phase
	})

	after := claimNames(claimsIn(t))
	if len(after) != len(before) {
		t.Errorf("claims went from %v to %v when survival-1 was removed. The operator "+
			"holds no delete on persistentvolumeclaims and must never acquire one; if "+
			"this fires, either it did, or something else in the cluster is deleting "+
			"worlds", before, after)
	}
}

// theProxyGroupGetsItsService checks the one object a ProxyGroup owns that
// nothing else in this run produces.
func theProxyGroupGetsItsService(t *testing.T) {
	eventually(t, 2*time.Minute, "the gateway Service", func() (bool, string) {
		var svc corev1.Service
		err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway"}, &svc)
		if err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeNodePort {
			return false, "type is " + string(svc.Spec.Type)
		}
		for _, p := range svc.Spec.Ports {
			if p.NodePort == 30765 {
				return true, ""
			}
		}
		return false, fmt.Sprintf("ports %v", svc.Spec.Ports)
	})
}

// theOperatorHoldsItsSecretAndItsLease checks the two things startup alone
// drives, and the two that run through the markers carrying
// namespace=spawnery-system as a literal. If that qualifier is ever wrong, the
// operator fails at certs.Ensure or during leader election -- and RBAC never
// says where the problem is. See docs/known-issues.md, "spawnery-system is
// hard-wired into the RBAC markers".
func theOperatorHoldsItsSecretAndItsLease(t *testing.T) {
	var secrets corev1.SecretList
	if err := k8s.List(ctx, &secrets, client.InNamespace(operatorNamespace)); err != nil {
		t.Fatalf("list secrets in %s: %v", operatorNamespace, err)
	}
	found := false
	for _, s := range secrets.Items {
		if s.Type == corev1.SecretTypeTLS {
			found = true
		}
	}
	if !found {
		t.Errorf("no TLS secret in %s: certs.Store.Ensure never wrote its serving "+
			"certificate, and every agent would fail its handshake", operatorNamespace)
	}

	var leases coordinationv1.LeaseList
	if err := k8s.List(ctx, &leases, client.InNamespace(operatorNamespace)); err != nil {
		t.Fatalf("list leases in %s: %v", operatorNamespace, err)
	}
	if len(leases.Items) == 0 {
		t.Errorf("no Lease in %s, yet the Deployment passes --leader-elect=true and the "+
			"readiness probe only turns green once the lock is held", operatorNamespace)
	}
}

func claimsIn(t *testing.T) []corev1.PersistentVolumeClaim {
	t.Helper()
	var list corev1.PersistentVolumeClaimList
	if err := k8s.List(ctx, &list, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list claims: %v", err)
	}
	return list.Items
}

func claimNames(claims []corev1.PersistentVolumeClaim) []string {
	names := make([]string, 0, len(claims))
	for _, c := range claims {
		names = append(names, c.Name)
	}
	return names
}
```

No scheme change is needed: `clientgoscheme.AddToScheme` already registers
`coordination.k8s.io/v1`, which Task 4's `TestMain` calls. The import above is
only for the `LeaseList` type.

Insert the three subtests above the denial line:

```go
	t.Run("a persistent group's claim outlives its server", aPersistentGroupsClaimOutlivesItsServer)
	t.Run("the proxy group gets its Service", theProxyGroupGetsItsService)
	t.Run("the operator holds its secret and its lease", theOperatorHoldsItsSecretAndItsLease)
```

- [ ] **Step 3: Run to verify they fail**

Temporarily remove the `storage:` block from the `survival` group in the
manifest. Expected: the API server rejects the group (a `Persistent` group needs
storage) or the claims never appear — either way, `both ordinals' claims` times
out at `0 claims`. Restore it.

- [ ] **Step 4: Run to verify they pass**

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

Expected: eleven subtests pass.

- [ ] **Step 5: Verify the assertions can fail**

Perform, run, record, revert:

1. In `internal/podspec/claim.go`, set an owner reference on the built claim
   (`controllerutil.SetControllerReference` against the Server). Expected: the
   owner-reference assertion fires. **This is the mutation that matters**: it is
   invisible to the envtest that guards the same property, because envtest runs
   no garbage collector. Record the message.
2. In `internal/controller/proxygroup_controller.go`'s `reconcileService`,
   change the assigned `NodePort`. Expected: scenario 8 times out reporting the
   ports it saw.
3. Add `--leader-elect=false` to `hack/e2e.sh`'s patch. Expected: the Lease
   assertion fires. Note that this also changes what the readiness probe means,
   so read the whole failure rather than only the first line.

- [ ] **Step 6: Commit**

```bash
git add test/e2e/
git commit
```

Subject: `test(6a): a world outlives its server, outside envtest`.
The body says why mutation 1 is the one worth having, and that envtest cannot
make this claim because it runs no garbage collector.

---

### Task 8: The permission table against a real authorizer

**Files:**
- Create: `test/e2e/rbac_test.go`
- Modify: `test/e2e/e2e_test.go` (one `t.Run` line)

**Interfaces:**
- Consumes: `k8s`, `ctx`, `testNamespace`, `operatorNamespace` (Task 4), and
  `rbacaudit.RequiredCluster` / `RequiredNamespaced` / `RequiredNetworkNamespace`.

Level A already runs this in envtest. Running it again here is cheap and covers
a different failure: that the role as it lands in a real cluster is not the role
the envtest suite applied.

- [ ] **Step 1: Write the failing test**

Create `test/e2e/rbac_test.go`.

```go
//go:build e2e

package e2e

import (
	"testing"

	authzv1 "k8s.io/api/authorization/v1"

	"github.com/spawnery/spawnery/internal/rbacaudit"
)

// theTableHoldsAgainstTheRealAuthorizer asks the cluster, one permission at a
// time, whether the operator's ServiceAccount may do what the table says it
// needs.
//
// SubjectAccessReview and not SelfSubjectAccessReview: the question is about a
// third party's permissions, which lets this test keep its own admin rights and
// still read logs and events.
func theTableHoldsAgainstTheRealAuthorizer(t *testing.T) {
	const subject = "system:serviceaccount:" + operatorNamespace + ":spawnery-operator"

	check := func(p rbacaudit.Permission, namespace string) {
		review := &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{
				User: subject,
				ResourceAttributes: &authzv1.ResourceAttributes{
					Namespace:   namespace,
					Group:       p.Group,
					Resource:    p.Resource,
					Subresource: p.Subresource,
					Verb:        p.Verb,
				},
			},
		}
		if err := k8s.Create(ctx, review); err != nil {
			t.Fatalf("SubjectAccessReview for %s: %v", p, err)
		}
		if !review.Status.Allowed {
			where := "cluster-wide"
			if namespace != "" {
				where = "in namespace " + namespace
			}
			t.Errorf("%s is denied %s %s: %s. The table says the code needs it (%s)",
				subject, p, where, review.Status.Reason, p.Why)
		}
	}

	for _, p := range rbacaudit.RequiredCluster {
		check(p, "")
	}
	for _, p := range rbacaudit.RequiredNamespaced {
		check(p, operatorNamespace)
	}
	for _, p := range rbacaudit.RequiredNetworkNamespace {
		check(p, testNamespace)
	}

	// Printed, not merely counted: a loop over an empty slice passes without
	// asking the cluster anything, and PASS would look identical.
	t.Logf("checked %d cluster, %d namespaced and %d per-network permissions",
		len(rbacaudit.RequiredCluster), len(rbacaudit.RequiredNamespaced),
		len(rbacaudit.RequiredNetworkNamespace))
}
```

`RequiredNetworkNamespace` holds one entry — `secrets: get`, for
`readForwardingSecret` — and it is granted by
`config/rbac/forwarding-secret-reader.yaml`, which carries no
`metadata.namespace` and is applied with `-n <namespace>` per network
namespace. `hack/e2e.sh` applies it into `minecraft` (Task 4), which is why
this third loop can pass at all. If it fails, the script's apply is what to
look at first, not the table.

Insert above the denial line:

```go
	t.Run("the table holds against the real authorizer", theTableHoldsAgainstTheRealAuthorizer)
```

- [ ] **Step 2: Run to verify it fails**

Remove one verb from a marker, `nix develop -c make manifests`, and run the
suite. Expected: this test names the exact triple as denied, and the message
carries the table's own `Why`.

- [ ] **Step 3: Run to verify it passes**

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

Expected: twelve subtests pass, and the printed counts are non-zero for all
three tables. A zero count would mean the loop ran over an empty slice and the
test proved nothing — check the numbers, do not just read PASS.

- [ ] **Step 4: Record what no scenario reaches**

Spec §7.4 says this list is *measured*, not guessed. With the cluster still up
(`E2E_KEEP=1`), work out which table entries no driven scenario exercises — the
candidates are the verbs behind paths needing a `Ready` server, at least
`pods/patch` (the occupied label) and `persistentvolumeclaims/patch` (growing a
world). Confirm each by checking the operator log for the call, or by reasoning
from the scenario list, and write the confirmed list into the commit body. It
becomes a `docs/known-issues.md` entry in Task 9.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/ hack/e2e.sh
git commit
```

Subject: `test(6a): the permission table, against a cluster's own authorizer`.
The body carries the measured list from Step 4.

---

### Task 9: The documentation this milestone owes

**Files:**
- Modify: `README.md` (five sub-milestones of backlog, plus what 6a adds)
- Modify: `docs/known-issues.md` (a new section; four entries closed or amended)
- Modify: `docs/superpowers/specs/2026-08-07-e2e-testcluster-design.md` (a status header)
- Create: `docs/handover-milestone-6.md`

**Interfaces:**
- Consumes: everything. This task runs last and reports what actually happened,
  not what the plan expected.

- [ ] **Step 1: The status header on the old E2E design**

Add immediately under its title, before `**Date:**`:

```markdown
> **Status, 2026-08-16.** Level A (§5.1) is built and in service as
> `internal/rbacaudit`. Level B was never built, and milestone 6a builds it
> differently: see
> [`2026-08-16-operator-image-and-e2e-design.md`](2026-08-16-operator-image-and-e2e-design.md).
> **§2 (the NixOS VM), §3 (the three flake outputs) and §7 (`make e2e` calling
> `nix build .#checks…`) are superseded** by that document; §4, §5.2, §5.3, §6
> and §8 are carried forward there, amended. The text below is kept as written
> rather than corrected in place, because §2's argument — that this machine has
> no container runtime — was true when it was made and stopped being true at
> milestone 2b.
```

- [ ] **Step 2: `docs/known-issues.md`**

Amend the four entries 6a closes, in place, each keeping its original text and
gaining a sentence naming what closed it:

- "No image is published" → the mechanism exists (`hack/publish.sh`,
  `make publish`); say whether the first real push has been driven, and by
  whom, rather than implying it from the script's existence.
- "The local kind flow needs a `Service` nothing creates" → closed for anything
  that runs the operator in the cluster; still true for a `go run` from a
  terminal, so say which half is closed.
- "`make -j image-test` can load the wrong image" → closed by `--out-link`.
- "Whether the operator runs inside the cluster for the E2E flow is still open"
  → decided.
- "The flags in the Deployment are unchecked" → closed, *and* its own required
  value corrected: it names `--startup-deadline=20s`, which is now the wrong
  number for a manifest that gets installed.

Then add a "From milestone 6a" section carrying, at minimum: the measured list
of permissions no driven scenario reaches (Task 8, Step 4); that the digest
reference in `config/deploy/deployment.yaml` is never exercised by `make e2e`;
that the E2E is single-node, so nothing about node drain or `HostPort` is
touched; and anything the run itself turned up that this plan did not predict.

- [ ] **Step 3: `README.md`**

Two jobs. The second is larger than it looks.

**What 6a adds**, in the development section: `make e2e` with the
rootless-podman invocation, `make publish`, `make operator-image-test`, and the
sentence that the operator now runs in the cluster from its own image.

**The five-milestone backlog.** The README's last commit is `d7aefb0`, the
milestone 4b handover; it describes 4a as the newest work and calls 4b and 4c
future. 4b, 4c-1, 4c-2, 4c-3, 4d, 5a, 5b and 5c have all landed since.
Reconstruct each from `docs/known-issues.md`'s per-milestone sections,
`docs/handover-milestone-5.md`, and the specs under
`docs/superpowers/specs/` — **not from memory, and not from the plans**, which
describe what was intended rather than what shipped. Follow the shape the
existing milestone paragraphs use: what it does, the one thing worth naming,
and what it left open.

Where the existing text is now false rather than merely incomplete — "Milestone
4 continues with 4b, rolling updates of ephemeral groups, and 4c, proxy and node
drain" — rewrite it. Point the "anyone starting X begins at" paragraph at
`docs/handover-milestone-6.md`.

- [ ] **Step 4: `docs/handover-milestone-6.md`**

Written to be picked up cold by a session with no memory of this one, following
`docs/handover-milestone-5.md`'s shape. It must contain:

- where 6a stopped, and whether the first real publish and the RKE2 rollout have
  been driven;
- what 6b (NetworkPolicies) finds in place, checked against the code as 6a
  leaves it rather than against this plan — in particular that the operator now
  runs in-cluster, that the E2E has a seam for new scenarios, and that
  `rbacaudit` will go red the moment a `networkpolicies` marker appears without
  a table entry;
- the one thing 6b has to decide, argued and not chosen, in the manner of the
  4b handover's soft-drain section;
- what 6c and 6d inherit, including that `spawnery-system` is still hard-wired
  in the markers and that the chart has to parameterize it there;
- what the RKE2 rollout at the end of the milestone owes (spec §12);
- the environment, and the exact command forms from this plan's Global
  Constraints.

- [ ] **Step 5: Run the absolute-word sweep over the whole diff**

```bash
git diff master...HEAD -- '*.md' | grep -n -i -E "\b(never|only|nothing|exactly one|cannot|always|every|no|none|any|all|both)\b"
```

Read every hit against the state of the tree. Milestone 5 recorded nine
instances of a sentence that reads plausibly while describing a mechanism the
code does not have, and the one the sweep cannot catch is a claim about wiring
that does not exist yet — which reads as ordinary prose and trips none of these
words. Check the tense of every claim about what "now" happens.

- [ ] **Step 6: Full verification**

```bash
nix develop -c make test
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-test CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make operator-image-test CONTAINER=podman
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
git diff master...HEAD --name-only
```

The last command is the evidence for the claim that `make agent` and
`make agent-test` are out of scope: nothing under `agent/` or `proto/` may
appear. Do not assert it without running it.

- [ ] **Step 7: Commit**

```bash
git add README.md docs/
git commit
```

Subject: `docs(6a): what the operator running in a cluster changes, and what it leaves`.

---

## Self-review notes for the executing session

**Spec coverage.** §3 → Task 1. §4 → Task 3. §4.2 and §4.3 → Tasks 2 and 4
(the manifest's reference, and the E2E overriding it). §5 → Task 2. §6 → Task 4.
§7.1 scenarios 1–3 → Task 5, 4–6 → Task 6, 7–9 → Task 7. §7.2 → Task 4. §7.3 →
Task 8. §7.4 → Tasks 5 and 8 (the decision is enforced in Task 5's manifest
assertion; the measured list is Task 8). §8 → Tasks 2 and every task's mutation
block. §9 → the acceptance criteria are distributed across Tasks 1–8 and
re-verified in Task 9, Step 6. §10 → Task 9. §11 and §12 → Task 9's handover.

**The one thing this plan does not do.** Acceptance criterion 7 — that the
digest reference resolves — cannot be met by an implementer without a
`write:packages` token. Task 3 stops at the dry run and the criterion is
discharged by the repository owner's first real `make publish`. Say that in
Task 9's handover rather than leaving the criterion looking met.
