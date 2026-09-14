# Documentation site, phase 1: scaffolding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish everything `docs/` says today at `https://docs.spawnery.cloud`, rendered by MkDocs, built by Nix, served from the `paulwtf` cluster — without rewriting a single page.

**Architecture:** MkDocs Material renders `docs/` into a static tree. `nix/docs-site.nix` builds that tree from a narrow source set; `nix/docs-image.nix` wraps it in an OCI image shaped like `nix/operator-image.nix`. The manifests live in this repository under `k8s/docs/`, and `fluxcd` holds only a `GitRepository` and a `Kustomization` pointing at them — the pattern `apps/homepage/` uses, not the one `archive/apps/docs/` used.

**Tech Stack:** MkDocs 1.6.1, mkdocs-material 9.7.6, mkdocs-mermaid2-plugin 1.2.3, Nix (`dockerTools.buildLayeredImage`), Kustomize, Flux, Traefik, cert-manager, external-dns.

**Spec:** `docs/superpowers/specs/2026-09-14-documentation-site-design.md`

## Global Constraints

- **Everything in git is English:** page text, commit messages, comments, branch names. The conversation is German; artefacts are not.
- **Commits are Conventional Commits with a scope:** `docs(site): …`, `feat(docs): …`, `chore: …`. Subject says what changed, body says why, wrapped at 72 columns.
- **Comments must earn their place.** The default is no comment. Explain foreign-system behaviour, deliberate absences, and numbers that would otherwise look arbitrary — never that a line exists or that something was fixed.
- **Nix reads the git index, not the working tree.** `git add` every new file before `nix build` or any image target. The symptom of forgetting is a build that cannot see a file plainly on disk.
- **Every command runs in the dev shell:** `nix develop -c <command>`, with no `cd` before it. `~/.config/nix/nix.conf` already sets `experimental-features`, so no flag prefix is needed.
- **This machine is the development VM** (8 cores, 8 GB) unless `hostname` says otherwise. Container work needs `CONTAINER=podman` and `TMPDIR="$HOME/.cache/spawnery-tmp"`.
- **Site source of truth:** `docs/` is the MkDocs `docs_dir`. `docs/superpowers/` is excluded from the build.
- **No page content is rewritten in this phase.** Text changes only where a move breaks a link.

---

### Task 1: MkDocs in the dev shell, building the tree as it stands

Make a strict build possible before moving anything, so that the moves in Task 2 have a working gate to move against.

**Files:**
- Modify: `flake.nix` (the `packages = with pkgs; [` list in `devShells`)
- Create: `mkdocs.yml`
- Create: `docs/index.md`
- Delete: `docs/README.md`

**Interfaces:**
- Consumes: nothing.
- Produces: a `mkdocs.yml` whose `nav` later tasks edit, and the guarantee that `nix develop -c mkdocs build --strict` is the project's link checker.

- [ ] **Step 1: Add the three Python packages to the dev shell**

In `flake.nix`, inside `packages = with pkgs; [ … ]`, after `gradle`:

```nix
              # The documentation site. mkdocs --strict is the link checker,
              # so this is a test dependency and not only a build one.
              python3Packages.mkdocs
              python3Packages.mkdocs-material
              python3Packages.mkdocs-mermaid2-plugin
```

- [ ] **Step 2: Run the build and watch it fail**

Run: `nix develop -c mkdocs build --strict --site-dir /tmp/site-probe`

Expected: FAIL with `Config file 'mkdocs.yml' does not exist.`

- [ ] **Step 3: Write `mkdocs.yml`**

`nav` names today's filenames. Task 2 rewrites it.

```yaml
site_name: Spawnery
site_description: A Kubernetes-native cloud system for Minecraft networks
site_url: https://docs.spawnery.cloud
repo_url: https://github.com/spawnery/spawnery
edit_uri: edit/master/docs/

theme:
  name: material
  palette:
    - scheme: default
      toggle:
        icon: material/weather-night
        name: Dark mode
    - scheme: slate
      toggle:
        icon: material/weather-sunny
        name: Light mode
  features:
    - navigation.sections
    - navigation.top
    - content.code.copy
    - search.suggest

# The design record: dated specs and plans, written for the person building
# the thing rather than the person running it. Kept in the repository, kept
# out of the search index.
exclude_docs: |
  superpowers/

plugins:
  - search
  - mermaid2

markdown_extensions:
  - admonition
  - pymdownx.highlight
  - pymdownx.superfences:
      custom_fences:
        - name: mermaid
          class: mermaid
          format: !!python/name:mermaid2.fence_mermaid_custom
  - toc:
      permalink: true

nav:
  - Home: index.md
  - Operating:
      - Upgrading: upgrading.md
      - Rotating the CA: ca-rotation.md
      - Rotating the forwarding secret: runbook-milestone-5c-secret-rotation.md
      - Persistent storage: persistent-storage.md
      - Network boundaries: network-boundaries.md
      - Plugins from a volume: plugins.md
      - Group environment: group-environment.md
      - Mounts: mounts.md
  - Reference:
      - Known issues: known-issues.md
      - Development: development.md
  - Archive:
      - How it was built: history.md
      - Handover 6e: handover-milestone-6e.md
      - Handover 6d: handover-milestone-6d.md
      - Handover 6c: handover-milestone-6c.md
      - Handover 6b: handover-milestone-6b.md
      - Handover 6: handover-milestone-6.md
      - Handover 5: handover-milestone-5.md
      - Handover 4b: handover-milestone-4b.md
      - Handover 4: handover-milestone-4.md
      - Handover 3: handover-milestone-3.md
      - Handover 2c: handover-milestone-2c.md
      - Handover 2b: handover-milestone-2b.md
      - RKE2 rollout: runbook-milestone-6-rollout.md
      - Secret rotation, driven: runbook-milestone-5c-evidence.md
      - Two worlds through an update: runbook-milestone-5b-evidence.md
      - A world outliving its pod: runbook-milestone-5a-evidence.md
      - Proxy drain and rolling updates: runbook-milestone-4c1-evidence.md
      - The first real join: runbook-milestone-3-evidence.md
```

- [ ] **Step 4: Run the build and watch it fail on the missing home page**

Run: `nix develop -c mkdocs build --strict --site-dir /tmp/site-probe`

Expected: FAIL, naming `index.md` as a nav entry that does not exist. `docs/README.md` may be reported as a page not in the nav — both are the same class of failure and both are fixed by the next step.

- [ ] **Step 5: Turn `docs/README.md` into `docs/index.md`**

```bash
git mv docs/README.md docs/index.md
```

Then replace its body with the site's front page, taken from the opening of the root `README.md`: the one-paragraph description, the `ServerGroup` example, the four-kind table and the mermaid diagram. Drop the file-listing tables — the nav is that now. Keep the closing pointer to `known-issues.md`.

- [ ] **Step 6: Run the build and verify it passes**

Run: `nix develop -c mkdocs build --strict --site-dir /tmp/site-probe`

Expected: PASS, with no warnings. Any remaining warning is a link into a file the nav does not carry; fix the link, not the strictness.

- [ ] **Step 7: Look at it**

Run: `nix develop -c mkdocs serve`

Open `http://127.0.0.1:8000`, confirm the mermaid diagram on the home page renders as a diagram and not as a code block, and that the dark-mode toggle works.

- [ ] **Step 8: Settle where mermaid.js comes from**

The spec leaves this open, and it has to be closed before the site is published: a documentation site on one's own cluster should not quietly depend on unpkg, and an air-gapped or blocked reader would see the diagram silently fail to render.

```bash
nix develop -c mkdocs build --strict --site-dir /tmp/site-probe
grep -rio 'src="https\?://[^"]*"' /tmp/site-probe/index.html
```

Expected either no external `src` at all, or exactly one naming a CDN. If a CDN appears, self-host it: add `python3Packages.mkdocs-mermaid2-plugin`'s bundled `mermaid.min.js` — or the `mermaid` package from nixpkgs — into `docs/assets/`, and point the plugin at it:

```yaml
plugins:
  - search
  - mermaid2:
      javascript: assets/mermaid.min.js
```

Then re-run the grep and confirm the CDN is gone. Record which of the two it was in the commit body.

- [ ] **Step 9: Commit**

```bash
rm -rf /tmp/site-probe
git add flake.nix mkdocs.yml docs/index.md
git commit -m "docs(site): mkdocs renders what docs/ already says"
```

---

### Task 2: The moves

**Files:**
- Move: the twenty-two files in the spec's table
- Modify: `mkdocs.yml` (nav), `README.md`, `CLAUDE.md`, `charts/spawnery/README.md`, and every page carrying a link to a moved file
- Create: `docs/archive/index.md`, `docs/getting-started/index.md`

**Interfaces:**
- Consumes: `mkdocs.yml` and the strict build from Task 1.
- Produces: the final directory layout every later phase writes into — `docs/{getting-started,guides,reference,explanation,contributing,archive}/`.

- [ ] **Step 1: Move the files**

```bash
mkdir -p docs/{getting-started,guides,reference,explanation,contributing,archive/handovers,archive/runbooks}

git mv docs/upgrading.md                              docs/guides/upgrading.md
git mv docs/ca-rotation.md                            docs/guides/rotating-the-ca.md
git mv docs/runbook-milestone-5c-secret-rotation.md   docs/guides/rotating-the-forwarding-secret.md
git mv docs/persistent-storage.md                     docs/guides/persistent-worlds.md
git mv docs/plugins.md                                docs/guides/plugins-from-a-volume.md
git mv docs/mounts.md                                 docs/guides/mounts-and-files.md
git mv docs/group-environment.md                      docs/guides/group-environment.md
git mv docs/network-boundaries.md                     docs/explanation/network-boundaries.md
git mv docs/development.md                            docs/contributing/development.md
git mv docs/known-issues.md                           docs/reference/known-issues.md
git mv docs/history.md                                docs/archive/history.md
git mv docs/handover-milestone-*.md                   docs/archive/handovers/
git mv docs/runbook-milestone-*.md                    docs/archive/runbooks/
```

- [ ] **Step 2: Run the build and watch it fail loudly**

Run: `nix develop -c mkdocs build --strict --site-dir /tmp/site-probe`

Expected: FAIL, many entries — every nav path and every inter-page link now points at a path that does not exist. This failure is the task's test; it names precisely what Steps 3 to 6 have to fix.

- [ ] **Step 3: Rewrite the nav**

```yaml
nav:
  - Home: index.md
  - Getting started:
      - Install: getting-started/index.md
  - Guides:
      - Upgrading: guides/upgrading.md
      - Persistent worlds: guides/persistent-worlds.md
      - Plugins from a volume: guides/plugins-from-a-volume.md
      - Mounts and files: guides/mounts-and-files.md
      - Group environment: guides/group-environment.md
      - Rotating the CA: guides/rotating-the-ca.md
      - Rotating the forwarding secret: guides/rotating-the-forwarding-secret.md
  - Reference:
      - Known issues: reference/known-issues.md
  - Explanation:
      - Network boundaries: explanation/network-boundaries.md
  - Contributing:
      - Development: contributing/development.md
  - Archive:
      - What this is: archive/index.md
      - How it was built: archive/history.md
      - Handovers:
          - 6e: archive/handovers/handover-milestone-6e.md
          - 6d: archive/handovers/handover-milestone-6d.md
          - 6c: archive/handovers/handover-milestone-6c.md
          - 6b: archive/handovers/handover-milestone-6b.md
          - 6: archive/handovers/handover-milestone-6.md
          - 5: archive/handovers/handover-milestone-5.md
          - 4b: archive/handovers/handover-milestone-4b.md
          - 4: archive/handovers/handover-milestone-4.md
          - 3: archive/handovers/handover-milestone-3.md
          - 2c: archive/handovers/handover-milestone-2c.md
          - 2b: archive/handovers/handover-milestone-2b.md
      - Runbooks:
          - The RKE2 rollout: archive/runbooks/runbook-milestone-6-rollout.md
          - Secret rotation, driven: archive/runbooks/runbook-milestone-5c-evidence.md
          - Two worlds through an update: archive/runbooks/runbook-milestone-5b-evidence.md
          - A world outliving its pod: archive/runbooks/runbook-milestone-5a-evidence.md
          - Proxy drain and rolling updates: archive/runbooks/runbook-milestone-4c1-evidence.md
          - The first real join: archive/runbooks/runbook-milestone-3-evidence.md
```

- [ ] **Step 4: Write `docs/archive/index.md`**

Without this frame a search hit from August reads as current. Say four things and stop: what these files are, that each was accurate on its own date, that nothing in them has been maintained since, and where to go instead (Guides and Reference).

- [ ] **Step 5: Move the chart README's content into Getting started**

`charts/spawnery/README.md` is the full installation reference. Its body — installing the chart, `--create-namespace`, the one manual step per game namespace, choosing a game namespace as one trust domain — becomes `docs/getting-started/index.md`. What stays behind in `charts/spawnery/README.md` is a short pointer, the same treatment `agent/api/README.md` gets in phase 4: a two-sentence description, the `helm install` command, and a link to the site, because that page is what GitHub renders for someone who lands on `charts/spawnery/`.

Anchors matter here: `README.md` links to `charts/spawnery/README.md#choosing-a-game-namespace`, and that anchor must survive as a heading in the new page.

- [ ] **Step 6: Fix every link the build named**

Two classes, and they are fixed differently:

- **Links between pages** become relative paths inside `docs/` (`../guides/upgrading.md`).
- **Links out of `docs/` into the repository** (`../README.md`, `../charts/spawnery/README.md`, `../config/samples/network.yaml`) cannot resolve under `docs_dir` and must become absolute GitHub URLs: `https://github.com/spawnery/spawnery/blob/master/config/samples/network.yaml`.

- [ ] **Step 7: Run the build and verify it passes**

Run: `nix develop -c mkdocs build --strict --site-dir /tmp/site-probe`

Expected: PASS, zero warnings.

- [ ] **Step 8: Point the repository's own front matter at the site**

In `README.md`, replace the two "Documentation" tables with a short paragraph and one link to `https://docs.spawnery.cloud`, keeping the direct links to `docs/reference/known-issues.md` and `LICENSE`. Update every remaining `docs/…` path in `README.md` and in `CLAUDE.md` to its new location — `CLAUDE.md` names `docs/known-issues.md`, `docs/superpowers/specs/` and `docs/superpowers/plans/`, of which only the first moved.

- [ ] **Step 9: Verify no path was missed anywhere in the tree**

```bash
grep -rn 'docs/upgrading\.md\|docs/ca-rotation\.md\|docs/persistent-storage\.md\|docs/plugins\.md\|docs/mounts\.md\|docs/group-environment\.md\|docs/network-boundaries\.md\|docs/development\.md\|docs/known-issues\.md\|docs/history\.md\|docs/handover-\|docs/runbook-' \
  --include='*.md' --include='*.go' --include='*.sh' --include='*.yaml' --include='*.nix' . | grep -v '^./docs/archive/'
```

Expected: no output. Occurrences inside `docs/archive/` are left alone — an archived document quoting the path a file had at the time is correct.

- [ ] **Step 10: Commit**

```bash
rm -rf /tmp/site-probe
git add -A
git commit -m "docs(site): one place per reader, not one file per feature"
```

---

### Task 3: Build the site through Nix

**Files:**
- Create: `nix/docs-site.nix`
- Modify: `flake.nix` (the `packages` output set, beside `spawnery-operator` and the image attributes)

**Interfaces:**
- Consumes: `mkdocs.yml` and `docs/` from Task 2.
- Produces: the flake attribute `docs-site`, whose output is the rendered tree at `$out`. Task 4 consumes it as the `docs-site` argument.

- [ ] **Step 1: Write `nix/docs-site.nix`**

```nix
# The rendered documentation site.
#
# The source set is narrow deliberately. Every image derivation in this tree
# takes the whole working tree as its src, so a line changed under internal/
# moves all three hashes; this one reads docs/ and mkdocs.yml alone, which is
# what keeps `make image-repro` from rebuilding a game image because a
# sentence moved.
{ lib
, stdenvNoCC
, python3Packages
}:

stdenvNoCC.mkDerivation {
  pname = "spawnery-docs-site";
  version = "0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [ ../docs ../mkdocs.yml ];
  };

  nativeBuildInputs = with python3Packages; [
    mkdocs
    mkdocs-material
    mkdocs-mermaid2-plugin
  ];

  # --strict turns an unresolved internal link and a page missing from the nav
  # into a build failure. It is the only link checker this project has.
  buildPhase = ''
    runHook preBuild
    export HOME="$TMPDIR"
    mkdocs build --strict --site-dir "$out"
    runHook postBuild
  '';

  dontInstall = true;
}
```

- [ ] **Step 2: Wire it into `flake.nix`**

Beside the existing package attributes:

```nix
            docs-site = pkgs.callPackage ./nix/docs-site.nix { };
```

- [ ] **Step 3: Stage the new files, because Nix cannot see them otherwise**

```bash
git add nix/docs-site.nix flake.nix
```

- [ ] **Step 4: Build and verify**

Run: `nix build .#docs-site --no-link --print-out-paths`

Expected: a store path. Then confirm the tree is a site and carries the archive:

```bash
ls "$(nix build .#docs-site --no-link --print-out-paths)"/index.html
ls "$(nix build .#docs-site --no-link --print-out-paths)"/archive/handovers/handover-milestone-6e/index.html
```

- [ ] **Step 5: Prove `--strict` bites, in a throwaway worktree**

A build that is always green looks from the inside exactly like one that passed.

```bash
git worktree add --detach /tmp/docs-mutation
cd /tmp/docs-mutation
printf '\n[a link that does not resolve](./no-such-page.md)\n' >> docs/index.md
git add docs/index.md
nix build /tmp/docs-mutation#docs-site --no-link 2>&1 | tail -20
```

Expected: FAIL, naming `no-such-page.md`. Record the message. Then:

```bash
cd /home/paul/git/spawnery
git worktree remove --force /tmp/docs-mutation
```

- [ ] **Step 6: Commit**

```bash
git commit -m "docs(site): the site builds through Nix, like everything else"
```

---

### Task 4: The image

**Files:**
- Create: `nix/docs-image.nix`
- Modify: `flake.nix` (the `packages` output set, x86_64-linux only, beside `operator-image`)

**Interfaces:**
- Consumes: the `docs-site` attribute from Task 3, `oci-common` for the identity.
- Produces: the flake attribute `docs-image`, a `docker-archive` whose image is `ghcr.io/spawnery/docs:dev`. Task 6's Deployment names `ghcr.io/spawnery/docs`; Task 7's workflow retags `dev` to `sha-<short>` on publish.

- [ ] **Step 1: Write `nix/docs-image.nix`**

The frame is `nix/operator-image.nix`'s, not `oci-common.layeredImage`'s: no `/data`, no shell, no writable directory.

```nix
# The documentation site's image: a static tree and something to serve it.
#
# Like the operator image and unlike the two game images, this takes from
# oci-common only the identity -- so all images run as the same uid and
# runAsNonRoot has a passwd entry to resolve -- and builds its own frame.
#
# The server writes nothing. Caddy nevertheless initialises a data directory
# at startup even with automatic HTTPS off, so the Deployment gives it an
# emptyDir at /tmp and points XDG_DATA_HOME and XDG_CONFIG_HOME there; that
# is what lets the root filesystem stay read-only.
{ dockerTools
, writeTextDir
, caddy
, docs-site
, oci-common
, runCommand
}:

let
  caddyfile = writeTextDir "etc/caddy/Caddyfile" ''
    {
      auto_https off
      admin off
    }
    :8080 {
      root * /site
      encode gzip
      file_server
    }
  '';

  site = runCommand "docs-site-tree" { } ''
    mkdir -p $out/site
    cp -r ${docs-site}/. $out/site/
  '';
in
dockerTools.buildLayeredImage {
  name = "ghcr.io/spawnery/docs";
  tag = "dev";

  # A label, not a cross-compile, exactly as in nix/oci-common.nix.
  architecture = "amd64";

  contents = [
    oci-common.passwd
    oci-common.group
    caddyfile
    site
    (oci-common.binIn { package = caddy; name = "caddy"; })
  ];

  config = {
    User = "${toString oci-common.uid}:${toString oci-common.gid}";
    WorkingDir = "/";
    Entrypoint = [ "/usr/local/bin/caddy" "run" "--config" "/etc/caddy/Caddyfile" ];
    Env = [ "XDG_DATA_HOME=/tmp" "XDG_CONFIG_HOME=/tmp" ];
    ExposedPorts = { "8080/tcp" = { }; };
    Labels = {
      "org.opencontainers.image.title" = "Spawnery documentation";
      "org.opencontainers.image.source" = "https://github.com/spawnery/spawnery";
    };
  };
}
```

- [ ] **Step 2: Wire it into `flake.nix`**

Beside `operator-image`, in the x86_64-linux-only block:

```nix
            docs-image = pkgs.callPackage ./nix/docs-image.nix {
              inherit oci-common;
              docs-site = self.packages.${system}.docs-site;
            };
```

- [ ] **Step 3: Stage and build**

```bash
git add nix/docs-image.nix flake.nix
nix build .#docs-image --no-link --print-out-paths
```

- [ ] **Step 4: Load it and prove it serves**

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  podman load -i "$(nix build .#docs-image --no-link --print-out-paths)"
podman run --rm -d --name docs-probe -p 8099:8080 \
  --read-only --tmpfs /tmp ghcr.io/spawnery/docs:dev
curl -sS -o /dev/null -w '%{http_code} %{content_type}\n' http://127.0.0.1:8099/
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8099/guides/upgrading/
podman logs docs-probe
podman rm -f docs-probe
```

Expected: `200 text/html; charset=utf-8` and `200`. `--read-only --tmpfs /tmp` is the pod's shape from Task 6; proving it here is what stops the same failure appearing in the cluster instead.

**If Caddy will not start under a read-only root** — the spec leaves this open — replace it with `darkhttpd`, which holds no state and takes its whole configuration on the command line: `Entrypoint = [ "/usr/local/bin/darkhttpd" "/site" "--port" "8080" "--no-listing" ]`, no Caddyfile, no `Env`. Record which one was chosen and why in the commit body.

- [ ] **Step 5: Commit**

```bash
git commit -m "docs(site): an image with a static tree and no state"
```

---

### Task 5: Make targets and CI

**Files:**
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: the `docs-site` attribute from Task 3.
- Produces: `make docs`, `make docs-serve`, and a CI job named `docs`.

- [ ] **Step 1: Add the targets**

In the style of the surrounding targets, each with its own `.PHONY`:

```make
.PHONY: docs
docs:
	nix build .#docs-site --no-link

.PHONY: docs-serve
docs-serve:
	mkdocs serve
```

`docs` stays out of `test`. `make test` already spends 85 seconds in `internal/controller` alone and gains nothing from rendering HTML; the site gets its own CI job instead.

The spec's third target, `docs-gen`, is not added here. It runs the reference generators, which phase 2 writes; a target that calls nothing would have to be remembered and removed.

- [ ] **Step 2: Add the CI job**

In `.github/workflows/ci.yml`, a job beside `lint`. Copy the `Install Nix` step, comment and all, from the `lint` job at lines 102-122 — the `log-lines = 40` setting was measured and its reason travels with it:

```yaml
  docs:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@v4

      - name: Install Nix
        uses: cachix/install-nix-action@v31
        with:
          extra_nix_config: |
            experimental-features = nix-command flakes
            log-lines = 40

      - name: make docs
        run: nix develop -c make docs
```

- [ ] **Step 3: Verify locally**

Run: `nix develop -c make docs`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add Makefile .github/workflows/ci.yml
git commit -m "docs(site): make docs is the link checker, and CI runs it"
```

---

### Task 6: The manifests, in this repository

**Files:**
- Create: `k8s/docs/{kustomization,namespace,deployment,service,ingress,certificate,networkpolicy}.yaml`

**Interfaces:**
- Consumes: the image `ghcr.io/spawnery/docs` from Task 4.
- Produces: a Kustomize directory at `./k8s/docs`, which Task 7's Flux `Kustomization` names as its `path`.

- [ ] **Step 1: Write the manifests**

`k8s/docs/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: spawnery-docs

resources:
  - namespace.yaml
  - deployment.yaml
  - service.yaml
  - certificate.yaml
  - ingress.yaml
  - networkpolicy.yaml
```

`k8s/docs/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: spawnery-docs
  labels:
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/warn: restricted
```

`k8s/docs/deployment.yaml` — two replicas so that a node reboot is not an outage; the resource numbers are the ones `archive/apps/docs/deployment.yaml` settled on for the same job, an nginx serving a static tree:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: docs
spec:
  replicas: 2
  selector:
    matchLabels:
      app: docs
  template:
    metadata:
      labels:
        app: docs
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
        runAsGroup: 10001
      containers:
        - name: caddy
          image: ghcr.io/spawnery/docs:dev
          ports:
            - name: http
              containerPort: 8080
          securityContext:
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          # The server writes nothing to the site, but Caddy initialises a
          # data directory at startup even with automatic HTTPS off, and
          # XDG_DATA_HOME in the image points here.
          volumeMounts:
            - name: tmp
              mountPath: /tmp
          resources:
            requests:
              cpu: 10m
              memory: 32Mi
            limits:
              cpu: 100m
              memory: 64Mi
          readinessProbe:
            httpGet:
              path: /
              port: http
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /
              port: http
            initialDelaySeconds: 5
            periodSeconds: 30
      volumes:
        - name: tmp
          emptyDir: {}
```

`k8s/docs/service.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: docs
spec:
  selector:
    app: docs
  ports:
    - name: http
      port: 80
      targetPort: http
```

`k8s/docs/certificate.yaml`:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: docs-tls
spec:
  secretName: docs-tls
  issuerRef:
    name: lets-encrypt
    kind: ClusterIssuer
  dnsNames:
    - docs.spawnery.cloud
```

`k8s/docs/ingress.yaml`:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: docs
  annotations:
    # The prefix is external-dns.kubernetes.io/, not the alpha form in
    # archive/apps/docs/ingressroute.yaml: external-dns v0.22 runs with
    # --annotation-prefix set explicitly, and an alpha-prefixed annotation
    # is read by nothing and reports no error.
    #
    # The target is the name that follows node health, not a node's address.
    # infrastructure/traefik/dns-service.yaml keeps only healthy, uncordoned
    # nodes in it.
    external-dns.kubernetes.io/target: ingress.paul.wtf
spec:
  ingressClassName: traefik
  tls:
    - hosts:
        - docs.spawnery.cloud
      secretName: docs-tls
  rules:
    - host: docs.spawnery.cloud
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: docs
                port:
                  number: 80
```

`k8s/docs/networkpolicy.yaml` — the standalone shape of `apps/homepage/networkpolicy.yaml`, with an explicit namespace because the Flux `Kustomization` for an externally-sourced app sets none:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: baseline-ingress
  namespace: spawnery-docs
spec:
  podSelector: {}
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector: {}
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: traefik
          podSelector:
            matchLabels:
              app.kubernetes.io/name: traefik
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
          podSelector:
            matchLabels:
              app.kubernetes.io/name: prometheus
```

There is deliberately no egress policy: the pod makes no outbound connection at all, and a policy saying so would be a fourth object to keep in step with a container that cannot reach anything anyway.

If Task 4 chose `darkhttpd` over Caddy, the `/tmp` volume, its mount and the comment above it come out — darkhttpd holds no state.

- [ ] **Step 2: Verify the kustomization renders**

```bash
nix develop -c kustomize build k8s/docs
```

Expected: every object, with `namespace: spawnery-docs` on each.

- [ ] **Step 3: Verify the API server accepts them**

```bash
nix develop -c kustomize build k8s/docs | kubectl apply --dry-run=server -f -
```

Expected: each object `(server dry run)`. If no kubeconfig reaches the cluster, `--dry-run=client` is the fallback and the server check moves to Task 8.

- [ ] **Step 4: Commit**

```bash
git add k8s/docs
git commit -m "docs(site): the manifests travel with the site"
```

---

### Task 7: Publish the image, and let the tag move itself

**Files:**
- Create: `.github/workflows/docs.yml`

**Interfaces:**
- Consumes: the `docs-image` attribute from Task 4 and `k8s/docs/deployment.yaml` from Task 6.
- Produces: `ghcr.io/spawnery/docs:sha-<short>` in GHCR on every push to master that touches the site, and a commit that moves `k8s/docs/deployment.yaml` to it.

**This replaces the spec's Flux image automation, and the reason is worth recording.** `ImageUpdateAutomation` would have to write to `spawnery/spawnery`, and the cluster holds no write credential for it — the only SSH `GitRepository` with a `secretRef` is `paulwtf-infra/spawnery`, a different repository. Provisioning a deploy key and a Vault path to move one line is more machinery than the line is worth, and CI already has `contents: write` on this repository through `GITHUB_TOKEN`. Nothing is lost: the tag still moves without a human, which is the whole complaint against `archive/apps/docs`.

- [ ] **Step 1: Write the workflow**

```yaml
name: Docs

on:
  push:
    branches: [master]
    paths:
      - 'docs/**'
      - 'mkdocs.yml'
      - 'nix/docs-site.nix'
      - 'nix/docs-image.nix'

permissions:
  contents: write
  packages: write

jobs:
  publish:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v4

      - name: Install Nix
        uses: cachix/install-nix-action@v31
        with:
          extra_nix_config: |
            experimental-features = nix-command flakes
            log-lines = 40

      - name: Build the image
        run: echo "archive=$(nix build .#docs-image --no-link --print-out-paths)" >> "$GITHUB_ENV"
      - name: Push it
        run: |
          short="$(git rev-parse --short HEAD)"
          # skopeo comes from the dev shell, not from the runner image, the
          # same way hack/publish.sh gets it.
          echo "${{ secrets.GITHUB_TOKEN }}" | \
            nix develop -c skopeo login ghcr.io -u "${{ github.actor }}" --password-stdin
          nix develop -c skopeo copy \
            "docker-archive:$archive" "docker://ghcr.io/spawnery/docs:sha-$short"
          echo "tag=sha-$short" >> "$GITHUB_ENV"
      - name: Move the Deployment to it
        run: |
          sed -i "s|image: ghcr.io/spawnery/docs:.*|image: ghcr.io/spawnery/docs:$tag|" k8s/docs/deployment.yaml
          git config user.name  'github-actions[bot]'
          git config user.email 'github-actions[bot]@users.noreply.github.com'
          git commit -am "chore(docs): $tag" -m "[skip ci]"
          git push
```

The path filter is what stops this looping: the commit it makes touches `k8s/`, which is not in `paths`, so the push does not retrigger the workflow. `[skip ci]` is the second belt.

- [ ] **Step 2: Verify the workflow is well-formed before it can run**

```bash
nix develop -c python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/docs.yml'))"
```

Expected: no output. A malformed workflow fails silently on GitHub rather than reporting.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/docs.yml
git commit -m "docs(site): CI publishes the image and moves the tag"
```

---

### Task 8: The cluster half, and the driven run

This task spans two repositories and has an order that cannot be rearranged: the Flux `Kustomization` reads `master` of this repository, so the branch must be merged before anything in `fluxcd` can find a manifest to apply.

**Files:**
- Modify (in `../fluxcd`): `infrastructure/cert-manager/issuer/cloudflare-issuer.yaml`
- Create (in `../fluxcd`): `apps/spawnery-docs/{kustomization,repository,sync}.yaml`
- Modify (in `../fluxcd`): `apps/kustomization.yaml`

**Interfaces:**
- Consumes: `k8s/docs/` on this repository's `master`, and `ghcr.io/spawnery/docs:sha-<short>` in GHCR.
- Produces: `https://docs.spawnery.cloud`.

- [ ] **Step 1: Merge this branch and wait for the image**

```bash
gh pr create --fill
# after merge:
gh run list --workflow=docs.yml --limit 3
```

Expected: one successful `Docs` run, and a follow-up `chore(docs): sha-…` commit on master pinning the Deployment.

Confirm the image is publicly pullable, the way `spawnery/charts` was checked on 2026-08-31:

```bash
skopeo inspect --no-creds docker://ghcr.io/spawnery/docs:sha-<short> | head -5
```

If it is private, make the GHCR package public — the manifests carry no pull secret on purpose.

- [ ] **Step 2: Give cert-manager a solver that matches the name**

In `../fluxcd/infrastructure/cert-manager/issuer/cloudflare-issuer.yaml`:

```yaml
        selector:
          dnsZones:
            - "paul.wtf"
            - "spawnery.cloud"
```

The Cloudflare token already reaches every zone; the selector is the gate. cert-manager matches it against the name being issued, so without this line a `Certificate` for `docs.spawnery.cloud` finds no solver at all and its Order stalls rather than failing.

- [ ] **Step 3: Write the three files in `fluxcd`**

`apps/spawnery-docs/repository.yaml`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: spawnery-docs
  namespace: flux-system
spec:
  interval: 1h
  url: https://github.com/spawnery/spawnery.git
  ref:
    branch: master
  # Only the manifests. The rest of the repository is a Go operator, two
  # Gradle projects and the documentation itself -- none of which this
  # Kustomization reads, and all of which would be re-fetched hourly.
  ignore: |
    /*
    !/k8s/
```

`apps/spawnery-docs/sync.yaml`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: spawnery-docs
  namespace: flux-system
spec:
  interval: 10m
  path: ./k8s/docs
  prune: true
  sourceRef:
    kind: GitRepository
    name: spawnery-docs
  wait: true
  timeout: 5m
```

`apps/spawnery-docs/kustomization.yaml` lists both, and `apps/kustomization.yaml` gains `- spawnery-docs/`.

- [ ] **Step 4: Commit and push in `fluxcd`**

```bash
git -C ../fluxcd add -A
git -C ../fluxcd commit -m "feat(spawnery-docs): the project's documentation site

The manifests live in spawnery/spawnery beside the site they serve, so a
docs change and its image tag move in one commit in one repository --
the step archive/apps/docs needed a person for.

cert-manager's Cloudflare token already reaches every zone, but the
lets-encrypt ClusterIssuer had one solver selected on dnsZones:
[paul.wtf]. A Certificate for a spawnery.cloud name would have found no
solver and stalled."
git -C ../fluxcd push
```

- [ ] **Step 5: Drive it, and read the cluster rather than the manifest**

```bash
flux reconcile kustomization apps --with-source
flux get kustomization spawnery-docs
kubectl -n spawnery-docs get pods
kubectl -n spawnery-docs get certificate docs-tls
kubectl -n spawnery-docs describe certificate docs-tls | tail -20
dig +short docs.spawnery.cloud
curl -sSI https://docs.spawnery.cloud | head -3
curl -sS https://docs.spawnery.cloud/guides/upgrading/ -o /dev/null -w '%{http_code}\n'
```

Expected, in this order: the Kustomization Ready; two pods Running with no `ImagePullBackOff`; `docs-tls` Ready; `docs.spawnery.cloud` resolving through `ingress.paul.wtf`; `HTTP/2 200` with a valid certificate; and the moved guide answering 200 at its new path.

Two failures worth naming in advance, both from the archived rollout's own list: an `ImagePullBackOff` means the GHCR package is still private, and a certificate that stays `False` with no challenge means Step 2 did not reach the cluster.

- [ ] **Step 6: Record the run**

Add what the run measured to the commit body or to `docs/archive/`, in the project's habit of recording measurements rather than assumptions: the time from push to Ready, and whether the certificate issued on the first attempt.

---

## What this phase does not do

Phases 2 to 5 of the spec — the generated reference, the missing guides, the plugin API section with its Javadoc, and the explanation pages with `history.md` brought forward — each get their own plan. Each is independently useful and none blocks the others. Phase 2 is the one to write next: it closes the largest gap and, unlike the rest, it makes a class of staleness structurally impossible.
