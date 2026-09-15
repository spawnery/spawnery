# Documentation site design

A published documentation site for Spawnery, built with MkDocs, served from the
`paulwtf` cluster at `docs.spawnery.cloud`, with the reference half generated
from the sources it describes.

## The problem is the shape, not the maintenance

`docs/` holds 15,894 lines. Roughly ten thousand of them are the milestone
record — ten handovers and six runbooks — and the nine pages that document how
to operate Spawnery are, with two exceptions, current: `network-boundaries.md`,
`mounts.md` and `upgrading.md` were last touched on 2026-09-08, the same day as
the most recent release.

So this is not a stale-content problem. It is a shape problem, and it has three
symptoms.

**The ordering is milestone-shaped, and the milestones ended.** The last one
was 6e on 2026-08-20. Everything since has arrived as a dated spec and plan,
and `docs/history.md` reflects that: its final section, "Since the rollout",
covers two changes. Between 2026-08-25 and 2026-09-08 there were 94 `feat`
commits. The narrative layer has no mechanism for catching up because nothing
tells it to.

**There is no reference.** Six fields of the API appear in no page at all:
`spec.expose`, `spec.routing`, `spec.resources`, `spec.maxPlayers`, `spec.type`
and `spec.config`. `ScaleBoost` has no page. The seventeen operator flags in
`cmd/spawnery-operator/main.go` have no page. The chart's
`values.schema.json` has no page. Every one of these was written down
somewhere — in a Go doc comment, in a flag's usage string, in a JSON schema —
and none of it reaches a reader.

**There is no entry.** A reader who wants to run a network and a reader who
wants to write a plugin are handed the same table of filenames. The pages grew
one per feature, which works up to about nine pages and not past it.

The Java plugin API is the one part that reads better than its reputation.
`agent/api/README.md` is thorough, the Javadoc in `agent/api/src` is thorough
(17 blocks in 289 lines of `SpawneryApi.java`), and the javadoc jar has been on
Maven Central since 0.2.22 — `javadoc.io/doc/cloud.spawnery/spawnery-api`
serves it today. What is missing is not text. It is a place where the prose and
the generated reference stand together, and a link to either from anywhere.

## What this is not

**Not a rewrite of the nine guide pages.** They move; their text is edited only
where a move breaks a link or a cross-reference now points at a different
place.

**Not a repository split.** The question was asked and answered against, and
the reasons belong here because they will be asked again:
`internal/podspec/kotlin_agreement_test.go` and
`internal/cloudevent/kotlin_agreement_test.go` are Go tests that read Kotlin
source under `agent/`, as are `internal/agentpb/contract_test.go` and
`internal/podspec/env_test.go`; one `.proto` generates both `internal/agentpb`
and `agent/common/src/proto/java`; `flake.nix` builds the operator, both agent
plugins and four images from one tree, with `imageVersion` naming the agent
version stamped into the images; and `make e2e` brings all of it into one kind
cluster. A split turns each of those into a release round trip. The one
component with an external consumer contract, `cloud.spawnery:spawnery-api`, is
already decoupled the right way — through Maven Central and `compileOnly`, not
through a repository boundary.

**Not versioned documentation.** Three version numbers already move
independently (`imageVersion`, `operatorVersion`, the chart's). A fourth for
the documentation would be maintained for readers who do not exist yet. The
site follows `master` and names the current release in its header. `mike` is in
the pinned nixpkgs and can be added later without moving a file.

## Structure

Eight top-level sections. The organising question is what the reader is trying
to do, not which milestone built the thing.

```
Home                     what Spawnery is, the ServerGroup example, the diagram
Getting started          install the chart, define a network, watch a join
Guides                   the how-to pages, one task each
Reference                generated: CRDs, chart values, flags, metrics
Plugin API               prose sections plus the generated Javadoc
Explanation              architecture, the agent channel, trust boundaries
Contributing             building, testing, releasing
Archive                  the milestone record, framed
```

### Where the existing files go

| From | To |
|---|---|
| `docs/upgrading.md` | `docs/guides/upgrading.md` |
| `docs/ca-rotation.md` | `docs/guides/rotating-the-ca.md` |
| `docs/runbook-milestone-5c-secret-rotation.md` | `docs/guides/rotating-the-forwarding-secret.md` |
| `docs/persistent-storage.md` | `docs/guides/persistent-worlds.md` |
| `docs/plugins.md` | `docs/guides/plugins-from-a-volume.md` |
| `docs/mounts.md` | `docs/guides/mounts-and-files.md` |
| `docs/group-environment.md` | `docs/guides/group-environment.md` |
| `docs/network-boundaries.md` | `docs/explanation/network-boundaries.md` |
| `docs/development.md` | `docs/contributing/development.md` |
| `docs/known-issues.md` | `docs/reference/known-issues.md` |
| `docs/history.md` | `docs/archive/history.md` |
| `docs/handover-milestone-*.md` | `docs/archive/handovers/` |
| `docs/runbook-milestone-*-evidence.md`, `docs/runbook-milestone-6-rollout.md` | `docs/archive/runbooks/` |
| `charts/spawnery/README.md` | its content becomes `docs/getting-started/`; the file is cut to a pointer |

`runbook-milestone-5c-secret-rotation.md` is the one file whose classification
changes rather than its location. It is a procedure filed among the evidence
logs, and both `README.md` and `docs/README.md` already list it under
operating. It becomes a guide and loses the milestone from its name.

`charts/spawnery/README.md` gets the same treatment as
`agent/api/README.md`: the installation reference — installing the chart, the
one manual step per game namespace, choosing a game namespace — becomes the
`Getting started` section, and what stays in the chart directory is a short
pointer, because that is the page GitHub renders for someone who lands on
`charts/spawnery/`.

Moving these breaks GitHub blob links from outside the repository. That cost is
accepted: the first commit is from 2026-07-25, the site becomes the address,
and `README.md` will point there.

### New pages

Guides that describe shipped behaviour nobody has written down:

- `guides/expose-strategies.md` — `NodePort`, `LoadBalancer`, `HostPort`,
  `ClusterIP`, what each costs and which one a home cluster wants
- `guides/scaling-and-boosts.md` — `spec.scaling`, spare slots, and `ScaleBoost`
  as capacity that is not a spec edit
- `guides/updates-and-drain.md` — rolling updates, `spec.update`, `spec.drain`,
  node drain, and what a player experiences during each
- `guides/scheduling.md` — `spec.scheduling` on a group and the `Network`
  permission that gates it
- `guides/the-cloud-command.md` — `/cloud` and its permission

Reference pages, all generated (see below):

- `reference/crds.md` — `Network`, `ServerGroup`, `ProxyGroup`, `Server`,
  `ScaleBoost`
- `reference/chart-values.md`
- `reference/operator-flags.md`
- `reference/metrics-and-alerts.md`

Explanation, drawn from the existing specs rather than written fresh:

- `explanation/architecture.md` — the two directions, the two registries,
  `netstate`, pure cores and thin controllers
- `explanation/the-agent-channel.md` — why game pods never read the Kubernetes
  API, and how identity is established
- `explanation/versions-and-releases.md` — the three numbers and which one moves

### What stays off the site

`docs/superpowers/plans/` (49 files) and `docs/superpowers/specs/` (39 files)
are excluded from the build via `exclude_docs`, the same way
`fluxcd/mkdocs.yml` excludes its own. They are the design process, they are
dated, and 88 files of it is noise in a search index. The archive index links
to the specs directory on GitHub.

The handovers and runbooks do go on the site, in `Archive`, behind an index
page that says plainly what they are: the record of how it was built and what
each milestone measured, accurate as of its own date and not maintained since.
Without that frame a search hit from August reads as current.

## The reference is generated

This is the part that decides whether the site is stale again in three weeks.
Every reference page is written by a script from the artefact it describes,
committed, and diffed by CI — the pattern `hack/chart-templates.sh` already
established and CI's "the generated files are in step with their sources" step
already enforces.

| Page | Generator | Source |
|---|---|---|
| `reference/crds.md` | `hack/crd-docs.sh` | `config/crd/bases/*.yaml` |
| `reference/chart-values.md` | `hack/chart-values-docs.sh` | `charts/spawnery/values.schema.json` |
| `reference/metrics-and-alerts.md` | `hack/metrics-docs.sh` | `charts/spawnery/templates/prometheusrule.yaml` |

The CRD generator reads the `description` fields that controller-gen already
writes from the Go doc comments, so the source of truth stays where a developer
edits it. `make manifests` gains a call to the generators, immediately after
`hack/chart-templates.sh`, so the CRD reference cannot drift from the schema
any more than `charts/spawnery/templates/crds.yaml` can.

Each generator gets a `-test.sh` companion, as every load-bearing script in
`hack/` has.

**`reference/operator-flags.md` is the exception and is written by hand**,
because a flag's meaning is not in its usage string. It is held in step by a
source-reading test in the idiom the repository already uses for constants that
cross a boundary (`internal/podspec/env_test.go`,
`internal/agentpb/contract_test.go`): a Go test that parses every `flag.*Var`
call in `cmd/spawnery-operator/main.go` and fails if a flag name is missing
from the page. Adding a flag then turns a test red, which is the same bargain
`internal/rbacaudit` already makes for RBAC markers.

## The plugin API section

`agent/api/README.md` is split into five pages under `Plugin API`: getting
started (`compileOnly`, the `plugin.yml` dependency, the two-classloader
failure), reading the network, moving players and changing the fleet, rounds
and readiness holds, and events.

`agent/api/README.md` itself stays, cut down to the code example, the
`compileOnly` rule and a link to the site. It is what Maven Central shows, so
it must survive on its own.

The Javadoc is built into the site at `/plugin-api/javadoc/` from the Gradle
task that already produces the javadoc jar (`withJavadocJar()` in
`agent/api/build.gradle.kts`), rather than linked to javadoc.io, so that the
prose and the generated reference always describe the same version.

## Building and serving

### The site builds through Nix

`mkdocs`, `mkdocs-material` 9.7.6, `mkdocs-mermaid2-plugin` and
`pymdown-extensions` are all in the nixpkgs `flake.lock` already pins. No new
flake input.

`fluxcd/docs.Dockerfile` — `pip install mkdocs-material==9.*` into a
`python:3.14-slim`, then `nginx:alpine` — is the proven local pattern and is
deliberately not copied. This repository builds every image through
`nix/*.nix`, has a `make image-repro` target that checks bit-for-bit
reproducibility, and an unpinned `pip install` reaching the network at build
time would be the one thing in the tree that does neither.

Instead:

- `nix/docs-site.nix` runs `mkdocs build --strict` over `docs/` and
  `mkdocs.yml`, and copies the Gradle javadoc output into
  `plugin-api/javadoc/`.
- `nix/docs-image.nix` builds `ghcr.io/spawnery/docs` in the shape of
  `nix/operator-image.nix`: identity from `oci-common` (uid 10001, the passwd
  and group entries), no `/data`, no shell, a single static server binary and
  the site. Caddy is the intended server — one binary, correct content types,
  a non-root listener on 8080 out of a six-line Caddyfile. nginx is the
  fallback if Caddy's config surface turns out to want more than that.

`mkdocs.yml` follows `fluxcd/mkdocs.yml` where there is no reason to differ:
material theme, light/dark toggle, `navigation.sections`, `content.code.copy`,
`search.suggest`, the mermaid2 custom fence, `admonition`, `toc` with
permalinks. One thing to settle during implementation: whether the mermaid
runtime is fetched from a CDN or self-hosted. A documentation site on one's own
cluster should not quietly depend on unpkg, and the answer is not visible from
the configuration alone.

### Make targets

```
make docs        # nix build .#docs-site -- runs mkdocs --strict, which is the link checker
make docs-serve  # mkdocs serve, for writing
make docs-gen    # the reference generators; also called by make manifests
```

`make docs` stays out of `make test`. `make test` already takes 85 seconds in
`internal/controller` alone and gains nothing from rendering HTML. It gets its
own CI job, alongside `test`, `lint`, `images` and `deps`.

### Deployment

Pattern B, after `apps/homepage/`, not pattern A, after `archive/apps/docs/`.
The manifests live with the thing they deploy:

- **In this repository**, `k8s/docs/`: namespace, Deployment, Service, Ingress,
  a cert-manager `Certificate` for `docs.spawnery.cloud`, and the NetworkPolicy
  that admits Traefik and Prometheus and nothing else — the standalone shape
  `apps/homepage/networkpolicy.yaml` uses, written with an explicit namespace
  because the Flux `Kustomization` for an externally-sourced app sets none.
- **In `fluxcd`**, `apps/spawnery-docs/`: a `GitRepository` on
  `spawnery/spawnery` and a `Kustomization` on `./k8s/docs`. Nothing else —
  every object the app needs travels with the app.

The Ingress carries `external-dns.kubernetes.io/target: ingress.paul.wtf`, so
`docs.spawnery.cloud` becomes a cross-zone CNAME onto the name that follows
node health, exactly as `paul.wtf` itself does.

Two costs that pattern A carried and this one does not: the spawnery images are
publicly pullable, so there is no GHCR pull secret and no Vault path; and there
is no Authentik forward-auth middleware, because the site is public.

Three details taken from what the cluster is now rather than from the archived
manifests, which predate it:

- The annotation prefix is `external-dns.kubernetes.io/`. external-dns v0.22
  runs with `--annotation-prefix=external-dns.kubernetes.io/` set explicitly,
  so the `external-dns.alpha.kubernetes.io/` annotations in
  `archive/apps/docs/ingressroute.yaml` would be invisible.
- The DNS target is the name `ingress.paul.wtf`, not a node's address. The
  archived manifest pins `45.137.203.198`, from before Traefik moved to
  hostPorts on 2026-09-10.
- external-dns runs with `--policy=sync` and no `--domain-filter`, so its reach
  is exactly what its Cloudflare token covers, and that token is scoped to
  every zone on the account. The record therefore appears from the annotation
  with no DNS work.

**The certificate needs one line changed in `fluxcd`, and it is not the
token.** The `lets-encrypt` ClusterIssuer solves DNS-01 through Cloudflare with
a token that also reaches every zone, but
`infrastructure/cert-manager/issuer/cloudflare-issuer.yaml` carries a single
solver whose selector reads `dnsZones: ["paul.wtf"]`. cert-manager matches that
selector against the name being issued, so a `Certificate` for
`docs.spawnery.cloud` finds no solver at all and its Order stalls rather than
failing loudly. Adding `"spawnery.cloud"` to that list is part of phase 1 and
belongs in the same change as `apps/spawnery-docs/`.

**The image tag is moved by Flux image automation.** `image-reflector-controller`
and `image-automation-controller` are in `clusters/paulwtf/flux-system/gotk-components.yaml`
and no object in the repository uses them. An `ImageRepository`, an
`ImagePolicy` and an `ImageUpdateAutomation` are what this problem is for, and
they remove the step that made the archived pattern tiresome: `apps/docs`
required a `sha-<short>` to be pasted into a second repository by hand after
every CI run, which its own runbook documents as step 4.

`apps/docs` was archived on 2026-08-06 because nobody read it and it cost
resources for nothing. That is a statement about readership, not about the
cluster, and it does not transfer to documentation whose purpose is to be read.

### Why the cluster

Node availability over the measured period: server01 99.9983 %, server02
99.8975 %, server03 99.9364 %, and much of the recorded downtime is maintenance
during which HetrixTools was not put into maintenance mode.

The figure that matters for a served site is better than the worst node.
Traefik is a DaemonSet binding hostPorts on all three nodes, and
`infrastructure/traefik/dns-service.yaml` keeps only healthy, uncordoned nodes
in `ingress.paul.wtf` at TTL 60 with a one-minute sync interval. A node that
fails costs at most about two minutes of new connections; a planned drain with
two minutes of cordon lead costs none.

The one hedge kept: every GitHub Release body already names the `helm install`
command with its chart version. That stays true, so the single line a reader
needs while something is broken does not live behind the same door as
everything else.

## Testing

- `mkdocs build --strict` fails on a broken internal link or a page missing
  from the nav. This is the link checker and it runs in CI.
- The generated reference pages are diffed by CI's existing "the generated
  files are in step with their sources" step, so a CRD field added without
  regenerating fails the build.
- Each generator has a `hack/*-test.sh` companion.
- `reference/operator-flags.md` is covered by a Go test that reads
  `cmd/spawnery-operator/main.go`.
- Each source-reading test is proved to bite before it is trusted: the change
  it guards is reverted in a throwaway worktree, the failure is recorded, the
  change is restored, the pass is recorded. A test that is always green looks
  from the inside exactly like a test that passed.
- The deployment is verified against the running cluster, not against a
  manifest that applies cleanly: `flux get kustomization spawnery-docs` Ready,
  the pod Running, `kubectl -n spawnery-docs get certificate` Ready, and
  `https://docs.spawnery.cloud` serving the built site over a valid
  certificate.

## Phases

Each phase leaves the tree better than it found it and is a place to stop.

**1 — Scaffolding.** `mkdocs.yml`, the nixpkgs additions to the dev shell, the
three make targets, the CI job, `nix/docs-site.nix`, `nix/docs-image.nix`,
`k8s/docs/`, `apps/spawnery-docs/` in `fluxcd`, the Flux image automation
objects, the Cloudflare token check, and the file moves with every link in
`README.md`, `CLAUDE.md` and the pages themselves brought along. No page is
rewritten. The site is up and says what `docs/` says today.

**2 — Generated reference.** The three generators and their tests, the
hand-written flags page and its Go test, wired into `make manifests` and CI.
This closes the largest gap.

**3 — The missing guides.** Expose strategies, scaling and boosts, updates and
drain, scheduling, `/cloud`.

**4 — Plugin API.** The five prose pages, `agent/api/README.md` cut down to its
Maven Central role, and the Javadoc built into the site.

**5 — Explanation and archive.** The three explanation pages, the archive index
with its frame, and `docs/history.md` brought forward over the 94 commits since
the rollout — told as six or seven chapters (round lifecycle, server numbers,
`/cloud`, boosts, the volume family, scheduling), not listed.

>**Phases 3 to 5 below are superseded** by
> `2026-09-15-documentation-show-before-explain-design.md`, written after the
> site went up and its first reader found it a wall of text. Phases 1 and 2
> stand as built.

### Which phases get an implementation plan

Two of them, not five.

Phase 1 and phase 2 are code — Nix derivations, manifests, a workflow, three
generators with test companions, a source-reading Go test — with interfaces
between tasks and a real test cycle at each. They get plans under
`docs/superpowers/plans/`. The Javadoc build wiring from phase 4 belongs to
phase 2's plan rather than its own, because it is the same kind of work.

Phases 3 and 5, and the prose half of phase 4, are pages. Their test is
`mkdocs build --strict` and somebody reading them, and the list of pages with
what each covers is already above. A plan document for those would restate this
section at greater length and then be a second place to keep in step. They are
written directly, one page per commit.

## Open questions

- Does the mermaid runtime come from a CDN under `mkdocs-mermaid2-plugin`'s
  defaults? If so it is self-hosted before the site is published.
- Caddy or nginx in `nix/docs-image.nix`. Caddy is the intent; the decision is
  made against the actual configuration surface during phase 1 and recorded in
  the commit.
