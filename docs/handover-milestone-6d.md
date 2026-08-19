# Handover to milestone 6e

Status: **end of milestone 6d (2026-08-19). The operator now installs by
`helm install charts/spawnery`, and `config/deploy/` — the flat manifests
that command replaces — no longer exists anywhere in this repository. Every
`make e2e` run installs the chart into a namespace that is not its own
documented default, and `internal/rbacaudit` now audits what `helm template`
actually renders rather than an intermediate on disk.**

That first sentence is the milestone, and the rest of this status block is
what it does not mean. **No claim of reachability, and no claim about
upgrades.** Nothing in this milestone changes what any earlier one measured
about a client reaching a proxy — see `docs/handover-milestone-6c.md` §2 for
that, still true, still unrevisited here. And no `helm upgrade` runs anywhere
in this milestone: the four CRDs sit in `charts/spawnery/templates/` with
`helm.sh/resource-policy: keep` *so that* a future upgrade would carry a CRD
schema change through and an uninstall would not take every `Network` in the
cluster with it — and nothing here observed either half of that design
except the uninstall half. `helm uninstall` leaving the CRDs standing was
driven, once, against a real cluster
(`.superpowers/sdd/2026-08-19-helm-chart/task-5-report.md`, "Step 7"). The
upgrade half is built and unproven, and every document 6d writes has to say
so in those words rather than imply otherwise.

This document is not a spec. It says where 6d stopped and what 6e — CI, plus
the RKE2 rollout — finds when it starts, checked against the code as 6d
leaves it rather than against the plan that preceded it. The design decisions
live in
[`docs/superpowers/specs/2026-08-19-helm-chart-design.md`](superpowers/specs/2026-08-19-helm-chart-design.md);
the open points are in [`docs/known-issues.md`](known-issues.md).

**Why this is a new document rather than a section appended to
[`handover-milestone-6c.md`](handover-milestone-6c.md).** That document was
written *for* 6d, and its §3 ("What 6d finds in place") is the record of what
6d started from and had to decide — including its first claim, that
`spawnery-system` was still hard-wired in three places, which is exactly what
this milestone changed. Rewriting that section into the past tense would
delete the evidence base for 6d's own decisions; leaving it in the present
tense inside a document a 6e reader opens would report a settled question as
open. `handover-milestone-6c.md` now carries a header saying it is
superseded and naming what has changed, in the same pattern
`handover-milestone-6b.md` and `handover-milestone-6.md` already carry for
their own successors.

## 1. Where 6d stopped

**Built and driven, task by task:**

- **The chart, hand-migrated** (`882aeea..e95ef0e`). `charts/spawnery/`
  carries `Chart.yaml`, `values.yaml`, `templates/_helpers.tpl` and six
  templates hand-migrated from `config/deploy/`'s seven files — every one of
  that directory's load-bearing comments moved with its object, including the
  `strategy: Recreate` comment instructing the reader not to change it back,
  and the `NetworkPolicy`'s comments on why its second ingress rule has no
  `from`. The selector labels are frozen exactly as the design's §3.2
  requires: `spawnery.selectorLabels` never gains
  `app.kubernetes.io/instance`, because a Deployment's
  `spec.selector.matchLabels` is immutable and the NetworkPolicy's and
  Service's selectors both pin the same two-label pair by construction. The
  chart templates no `Namespace` object — `--create-namespace` is Helm's own
  answer to the alphabetical-apply-order hazard `hack/e2e.sh` used to
  document at length. The task's own one-time equivalence check (design
  §5.4) compared `helm template`'s output object by object against the seven
  manifests it replaces; every difference is accounted for in
  `.superpowers/sdd/2026-08-19-helm-chart/task-1-report.md` — the three Helm
  metadata labels, an inert `app.kubernetes.io/component` label picked up by
  three RBAC objects that carry no selector, and one deliberate addition
  (`imagePullPolicy`) needed to give `values.yaml`'s `image.pullPolicy` any
  effect.
- **The generated templates** (`e95ef0e..722d9e1`). `hack/chart-templates.sh`
  turns controller-gen's two outputs — `config/rbac/role.yaml` and
  `config/crd/bases/*.yaml` — into `charts/spawnery/templates/rbac.yaml` and
  `crds.yaml`, both carrying a two-line header saying they are generated and
  that hand edits are lost on the next `make manifests`
  (`Makefile:14-19` runs controller-gen, then the script, as its fourth
  line). The two `+kubebuilder:rbac` markers that carry
  `namespace=spawnery-system` (`internal/certs/store.go:57`,
  `internal/controller/setup.go:72`) keep the literal, now with a comment
  explaining it is controller-gen's required placeholder and not a statement
  about where the operator runs — the render step is what replaces it.
- **`internal/rbacaudit` audits the rendered chart** (`722d9e1..ddfcf3a`, one
  fix round). All twenty-one `readManifest` call sites across
  `audit_envtest_test.go` and `deploy_envtest_test.go` were replaced with
  `renderedManifest`, which runs `helm template` once per package run
  (memoised with `sync.Once`) and indexes the result by `Kind/name`. The
  render namespace is the helper's own constant,
  `audit-system` (`deploy_envtest_test.go:65`) — deliberately neither the
  chart's `spawnery-system` default nor `make e2e`'s `platform-system`, for
  the reason the design's §5.1 gives: a chart that forgot
  `{{ .Release.Namespace }}` and hard-coded the default would render
  byte-identical output at `spawnery-system`, and the audit would confirm a
  chart that cannot move. `readManifest` itself was deleted in the fix round,
  by an explicit human ruling, once it had zero remaining callers.
- **`make e2e` installs somewhere else on purpose** (`ddfcf3a..a10bac4`, one
  fix round). `hack/e2e.sh` replaced its four `kubectl apply` lines with one
  `helm install spawnery charts/spawnery --namespace platform-system
  --create-namespace`, and `test/e2e/e2e_test.go:55`'s `operatorNamespace`
  constant follows it. `platform-system` shares nothing with the chart's
  default on purpose — a near-miss like `spawnery-operators` would still
  fail a leaked literal, but reads as a variant of the default and invites
  somebody to "tidy" it back. The fix round added
  `check_forwarding_secret_reader_subject`
  (`hack/e2e.sh:189-196`), which reads the applied
  `forwarding-secret-reader.yaml` RoleBinding back from the cluster and
  fails the script loudly if its rewritten subject namespace does not match
  `$OPERATOR_NAMESPACE` — closing a silent-failure path a reviewer found in
  the original `sed` rewrite. Driven clean: 18 scenarios,
  `the_operator_was_never_denied` last, `ok
  github.com/spawnery/spawnery/test/e2e 139.745s`
  (`.superpowers/sdd/2026-08-19-helm-chart/task-4-report.md`).
- **The last consumers, and the deletion** (`a10bac4..c6ab97d`).
  `hack/publish.sh`'s `WRITE_DIGEST=1` path now writes
  `charts/spawnery/values.yaml`'s `image.digest` key instead of rewriting an
  `image:` line, with a pre-check (the anchor exists) and a post-check (the
  substitution actually landed) — the same two-sided guard shape Task 2's
  review asked for elsewhere in this milestone. `make chart-lint` (`helm
  lint` then `helm template ... >/dev/null`) is wired into `make test`.
  `config/deploy/` was deleted outright (`git rm -r`, all seven files);
  `config/rbac/role.yaml`, `config/rbac/forwarding-secret-reader.yaml` and
  `config/crd/bases/` stay, because the first two are still real inputs
  (controller-gen's output and a file applied by hand outside the chart) and
  the third is what `internal/testenv` loads into envtest. `helm uninstall`
  was driven once against a real cluster and confirmed to leave the four
  CRDs standing while removing the Deployment, Service and NetworkPolicy
  (`.superpowers/sdd/2026-08-19-helm-chart/task-5-report.md`, "Step 7").

**Verified at the end of the milestone:** `nix --extra-experimental-features
'nix-command flakes' develop -c make test` green, including `chart-lint`
ahead of `go test -race ./...` (`Makefile:64-65`); the last driven `make e2e`
green with 18 scenarios in 139.775s
(`.superpowers/sdd/2026-08-19-helm-chart/task-5-report.md`, "Step 5"), no
`kind` cluster left standing afterward.

**Not driven, and not drivable here:** `helm upgrade`, anywhere, on any
chart version. Chart publishing — no OCI push, no chart repository, no `helm
package` in CI; that is 6e's or the repository owner's, exactly as design §7
says. And, unchanged from every milestone since 6b, whether a client can
reach anything through any expose strategy — nothing in 6d touches that
question.

## 2. What 6e must not misread

**Design §5.2's claim that `theOperatorWasNeverDenied` "becomes, without
being modified at all, the guard over this milestone's principal risk" is
not available as written, and 6e should not repeat it.** Task 4's Mutation 1
put a literal `spawnery-system` on `charts/spawnery/templates/rolebinding.yaml`'s
own `metadata.namespace` — the RoleBinding's own field, not its subject's —
and expected the failure to surface inside `test/e2e` as a named scenario
going red. It did not. `helm install` itself refused first, with
`namespaces "spawnery-system" not found`, because Kubernetes validates a
RoleBinding's own `metadata.namespace` for existence at admission time —
`hack/e2e.sh` aborted under `set -e` before `go test` ever ran, and
`theOperatorWasNeverDenied` was neither a pass nor a failure: it never
executed
(`.superpowers/sdd/2026-08-19-helm-chart/task-4-report.md`, "Mutation 1").

The defensible version of the claim, from the SDD ledger's Task 4 `WORDING`
entry, splits the hazard in two: a `spawnery-system` literal surviving in
the chart's **own-namespace** RBAC fields — a RoleBinding's or
ClusterRoleBinding's own `metadata.namespace`, or the generated Role's
`namespace:` field — is caught at install time by Kubernetes' own
namespace-existence admission check, demonstrated by the mutation above. A
literal surviving in the chart's **subject**-namespace fields — a
RoleBinding's or ClusterRoleBinding's `subjects[].namespace`, which
`charts/spawnery/templates/rolebinding.yaml` and `clusterrolebinding.yaml`
both template as `{{ .Release.Namespace }}` — is not validated by the API
server at all: it applies cleanly, binds the wrong ServiceAccount, and the
real operator gets no permissions from that grant. That is, by the design's
own reasoning, the path `theOperatorWasNeverDenied` exists to catch, once
the resulting denial lands on a write verb. **But that second path was
never mutated by this milestone.** Nothing here demonstrates that the
runtime check actually catches it; the claim for that half is reasoning, not
measurement, and 6e should not read it as proven.

**`make chart-lint`'s second line does not catch what the plan justified it
by.** The plan expected `helm template`, run with no real namespace, to
catch a chart that lints but does not render — Task 5 tested that directly
with a typo'd `{{ .Release.Namspace }}` in `templates/service.yaml`, and
**neither `helm lint` nor `helm template` caught it.** Helm v4.2.3 treats an
unresolved `.Release` field as empty rather than as a template error, so
`helm template` renders a `Service` with `namespace: ` (empty) and exits 0
(`.superpowers/sdd/2026-08-19-helm-chart/task-5-report.md`, "Mutation 1").
`internal/rbacaudit`'s own `TestTheChartRendersIntoTheNamespaceItIsGiven`
does not catch it either — it asserts only the Deployment's and the Role's
namespaces, never the Service's. What does catch it is
`TestAgentServiceReachesTheOperatorPods`
(`internal/rbacaudit/deploy_envtest_test.go:538`), and incidentally rather
than by design: it applies the rendered Service into envtest's real API
server, which refuses to create a `Service` with an empty `namespace`. The
real backstop for this whole class of typo is the envtest-backed tests, not
`chart-lint` — `chart-lint` still earns its place for the failure mode it
does catch, a template that does not render at all, but 6e should not credit
it with more.

**The forwarding-secret-reader manual step is real, and the failure it
produces is narrower than the design and the task brief both say.** Both
`docs/superpowers/specs/2026-08-19-helm-chart-design.md` §9 and
`.superpowers/sdd/2026-08-19-helm-chart/task-6-brief.md` state that a
misconfigured `config/rbac/forwarding-secret-reader.yaml:65` leaves "every
group in the namespace refuses with `NetworkNotAccepted`." Reading
`internal/controller/network_controller.go`'s `Reconcile` against that claim
before writing it into the chart's README (this task's own Step 5) shows it
does not hold: `ConditionAccepted` is set `True` and persisted
unconditionally, before the forwarding secret is ever read
(`network_controller.go:93-97`, persisted at `:171`; the forwarding secret
read follows at `:142`), and the read's outcome
only ever reaches `ConditionForwardingSecretResolved` and
`ConditionForwardingSecretRotationPending` — never `Accepted`. A `Network`
whose reader Role is missing or points at the wrong operator namespace stays
`Accepted=True`, and every `ServerGroup`/`ProxyGroup` in that namespace keeps
scheduling normally; only forwarding-secret rotation detection breaks,
silently, for that namespace. `ReasonNetworkNotAccepted` is real
(`internal/controller/servergroup_controller.go:152`,
`internal/controller/proxygroup_controller.go:217`) but nothing in this
code path ever sets it. `charts/spawnery/README.md` states the narrower,
grep-verified consequence rather than the design's and the brief's broader
one; this discrepancy is recorded here because it is exactly the shape of
claim Step 5 exists to catch, and both of this milestone's own authoritative
documents made it.

## 3. What 6e finds in place

- **`make manifests` has a second half.** `Makefile:14-19`: controller-gen
  writes `config/crd/bases/` and `config/rbac/role.yaml` as it always has,
  and `hack/chart-templates.sh` then turns those into
  `charts/spawnery/templates/rbac.yaml` and `crds.yaml`. Both carry the
  two-line "Generated by `hack/chart-templates.sh`... Do not edit" header.
  The script checks both ends. Two guards check that controller-gen's
  *output* still has the shape the `sed` transforms are anchored to, and
  three postconditions then check the files that were actually written:
  `crds.yaml` carries `helm.sh/resource-policy: keep` once per CRD file
  processed — counted against the files this run walked, not a literal four —
  and `rbac.yaml` carries exactly one `{{ .Release.Namespace }}` and no
  surviving `spawnery-system`. The postconditions were added by the
  whole-branch review; until then the input guards were all there was, and a
  broken `sed` with an intact input exited 0 and wrote a corrupted template.
  The CRD half was the one that mattered: nothing else in the repository
  looks at that annotation, and without it `helm uninstall` deletes all four
  CRDs and every `Network`, `ServerGroup`, `ProxyGroup` and `Server` in the
  cluster with them.
- **`internal/rbacaudit`'s tests require `helm` on `PATH`.** `renderChart`
  (`internal/rbacaudit/deploy_envtest_test.go:231-254`) runs `helm template`
  once per package run and fails with "helm is not on PATH; run this through
  `nix develop`" rather than a parse error on empty input if it is missing —
  not a new class of requirement, since envtest already needs its API server
  binaries from the flake, but one more thing that can be absent from a
  shell that skipped `nix develop`.
- **Three distinct namespaces exist, and each is what it is on purpose.**
  `spawnery-system` is the chart's documented default. No chart template
  carries it as a literal — `TestTheChartRendersIntoTheNamespaceItIsGiven`
  scans every rendered object for it — but it is *not* absent from the
  repository, and a reader auditing this discipline should expect to find it
  in all of these: `charts/spawnery/README.md`, in the install command and in
  the manual-edit instruction; the two `+kubebuilder:rbac` markers and their
  placeholder comments (`internal/certs/store.go:58-62`,
  `internal/controller/setup.go:73-77`); `config/rbac/role.yaml:137`, which
  is controller-gen's output from those markers;
  `config/rbac/forwarding-secret-reader.yaml:65`, the hand-applied grant §4
  is entirely about; `hack/chart-templates.sh`, both as the anchor its rbac
  `sed` matches and as the literal its postcondition refuses to find in the
  output; and `hack/e2e.sh`, as the same anchor in the two `sed` calls that
  rewrite the reader file for its own run. Every one of those is an input to
  a transform, a document about the default, or a guard against the literal —
  none is a namespace anything is installed into.
  `platform-system` is where `hack/e2e.sh` actually installs the chart for
  `make e2e` (`hack/e2e.sh:45`, `test/e2e/e2e_test.go:55`). `audit-system` is
  `internal/rbacaudit`'s own render constant
  (`deploy_envtest_test.go:65`). They exist as three, not two, so that two
  checks can never pass for the same wrong reason: a chart hard-coding its
  default would still pass an audit rendered at that same default, and an
  audit sharing `make e2e`'s namespace would not separately prove the chart
  moves. Collapsing any two of them back into one is a finding to report,
  not a simplification to make.
- **`config/rbac/forwarding-secret-reader.yaml` is unchanged and still
  outside the chart.** It is applied once per game namespace, by hand, after
  the chart installs — `charts/spawnery/README.md` now carries the exact
  manual edit its RoleBinding subject needs whenever the chart runs outside
  its documented default namespace. `hack/e2e.sh` performs the identical
  edit programmatically for its own run, with an outcome check
  (`check_forwarding_secret_reader_subject`) reading the applied object back
  from the cluster rather than trusting the `sed` that rewrote it — see §5.
- **`config/deploy/` does not exist.** `config/rbac/role.yaml` and
  `config/crd/bases/*.yaml` remain as controller-gen's output and
  `hack/chart-templates.sh`'s input; `config/rbac/forwarding-secret-reader.yaml`
  remains as the one hand-applied grant the chart cannot template.
- **`make chart-lint`** (`Makefile:67-74`) runs `helm lint charts/spawnery`
  then `helm template spawnery charts/spawnery --namespace chart-lint-check
  >/dev/null`, wired into `make test` ahead of `go test -race ./...`. What
  it does and does not catch is §2 above.

## 4. What the RKE2 rollout owes

`docs/handover-milestone-6c.md` §4 stands unchanged in what it listed: CIS
`restricted` pod security and `HostPort` cannot both hold in one namespace,
and the runbook — not the code — has to choose between a relaxed-label
namespace for the `HostPort` `ProxyGroup` or dropping the `HostPort` leg of
the rollout. 6d adds one item to that list, not a replacement for any of it:

**`config/rbac/forwarding-secret-reader.yaml:65` names
`namespace: spawnery-system` in its RoleBinding subject, and the chart
cannot template it (design §9).** The file is applied per game namespace, by
hand, after the chart is installed. An operator installed in any namespace
other than `spawnery-system` must have that line changed first, or the
`Network` in every such game namespace loses forwarding-secret rotation
detection silently — see §2 above for the narrower, code-verified
consequence, which is not what the design document states.
`charts/spawnery/README.md` carries this in its installation steps, not a
footnote, per the same instruction.

## 5. Every finding this milestone's reviews produced

The SDD ledger
(`.superpowers/sdd/2026-08-19-helm-chart/progress.md`) is the only place
this list exists in full; it is restated here with what caught each one.

1. **Task 1, minor, deferred.** The plan's own "Interfaces / Produces" list
   for Task 1 omitted `.Values.image.pullPolicy`, which Task 4 later sets
   via `--set` and which therefore had to be templated somewhere for that
   interface to mean anything — the plan under-specified its own interface.
   Caught by reading.
2. **Task 1, sourcing correction, fix round 1.** The task's own report
   attributed the `pullPolicy` rationale to a "Context the brief cannot
   know" section of the brief file; that heading is in the coordinator's
   dispatch message, not in any committed file, so a reviewer with only the
   repository found nothing to check it against. The claim was true; the
   citation was wrong. Fixed by naming the dispatch message as the source.
3. **Task 1, complete**, review clean after that one fix round; the
   re-reviewer independently verified both occurrences, the `IfNotPresent`
   default against the Kubernetes rule it rests on, and spot-checked the
   report's "14 citations checked" claim as exact.
4. **Task 2, minor, deferred — carried to the whole-branch review and fixed
   there.** `hack/chart-templates.sh`'s two guards check controller-gen's
   *input* shape, not that either `sed` transform actually applied: a broken
   `sed` with an intact input exits 0 and writes a corrupted `rbac.yaml` or
   `crds.yaml`. The reviewer reproduced both bypasses directly. The script now
   carries postconditions over both written files, and the CRD one counts the
   `keep` annotation against the number of files the run actually processed
   rather than a literal four. Closure confirmed by breaking the CRD `sed`'s
   anchor and observing `make manifests` exit non-zero.
5. **Task 2, complete**, review clean; the reviewer independently reproduced
   both guard bypasses above and reran `make manifests` twice to confirm
   determinism.
6. **Task 3, fix round 1, two findings addressed.** The re-reviewer ran the
   missing-key mutation against both the pre- and post-fix branches and
   reconfirmed the `rbac.yaml` corruption case still fails both assertions
   it is supposed to.
7. **Task 3, minor, deferred — fixed by the whole-branch review.**
   `audit_envtest_test.go`'s own comment on `readForwardingSecretReader`
   still claimed it uses "the same multi-document split `readGeneratedRoles`
   uses" — Task 3 moved `readGeneratedRoles` off that shared path onto
   `renderedManifest`, so the comment was false. The sibling comment in
   `deploy_envtest_test.go` was corrected in the same fix round; this one was
   missed. Caught by reading.
8. **Task 3, complete**, review clean after the fix round; a human partner
   overrode the plan's own text on one point and ruled `readManifest`
   deleted outright once it had no remaining callers.
9. **Task 4, `FINDING`, load-bearing for this handover.** Mutation 1 (a
   leaked `spawnery-system` literal on the chart's own RoleBinding
   `metadata.namespace`) was caught by `helm install` itself, aborting
   `hack/e2e.sh` under `set -e` before `go test` ran —
   `theOperatorWasNeverDenied` never executed. §2 above carries the full,
   defensible restatement of design §5.2's claim. Caught by mutation.
10. **Task 4, `WORDING`, load-bearing for this handover.** The precise split
    between the chart's own-namespace RBAC fields (caught at install time by
    Kubernetes' admission check) and its subject-namespace fields (by design
    caught at runtime by `theOperatorWasNeverDenied`, on a write verb, and
    never itself mutated) — see §2 above.
11. **Task 4, minor, deferred.** The forwarding-secret path is invisible to
    `test/e2e` by design: `readForwardingSecret` folds a `403` into a
    condition message carrying no `is forbidden:` substring and logs
    nothing (`test/e2e/e2e_test.go:187-193`), and no scenario reads
    `ConditionForwardingSecretResolved` or `SecretReadForbidden`. Caught by
    reading.
12. **Task 4, Important, fix round 1 dispatched and closed.** The `sed`
    rewriting `forwarding-secret-reader.yaml`'s subject namespace exits 0
    whether or not it matched, and a RoleBinding subject naming a
    nonexistent namespace passes `kubectl apply` — a broken anchor would
    have given a green 18/18 run with a grant that binds nothing. Fixed by
    `check_forwarding_secret_reader_subject`
    (`hack/e2e.sh:189-196`), which reads the applied object back and exits
    the script loudly on a mismatch. Caught by review reading; closure
    confirmed by mutating the anchor and observing the new check fire.
13. **Task 4, fix round 1/5, one finding addressed.** The re-reviewer traced
    the `set -e` path through the fix and confirmed the
    local-variable/assignment split in the new check avoids the exit-status
    masking pitfall that shape of Bash is prone to.
14. **Task 4, minor, deferred.** Commit `a10bac4`'s body text reads
    "RoleNameding" for "RoleBinding" — prose only, no code affected.
15. **Task 4, minor, deferred, predates this milestone.**
    `test/e2e/e2e_test.go:163-164` cites `(task-4-report.md, "Fix round
    1")`, a gitignored scratch file from milestone 6c that no longer
    exists — a committed source file citing an ephemeral artifact no future
    reader can retrieve. Blamed to `8d2f3277`, outside this milestone's own
    commits.
16. **Task 4, complete**, review clean after one fix round; 18 scenarios
    green in `platform-system`, `4m19s` wall clock.
17. **Task 5, `WORDING`, load-bearing for this handover, measured rather
    than assumed.** `make chart-lint` does not catch a typo'd
    `{{ .Release.Namspace }}`; the envtest-backed tests are the real
    backstop for that class of defect. See §2 above.
18. **Task 5, complete**, review clean; the reviewer independently
    reproduced the typo mutation, diffed the `write_digest.sh` harness
    against `hack/publish.sh`'s real logic byte-for-byte, and confirmed the
    digest guard checks the substitution's outcome rather than only its
    anchor.
19. **Task 6, `FINDING`, load-bearing for this handover.** Design §9 and this
    milestone's own Task 6 brief both claimed a misconfigured
    `config/rbac/forwarding-secret-reader.yaml` leaves "every group in the
    namespace refuses with `NetworkNotAccepted`".
    `internal/controller/network_controller.go` sets `ConditionAccepted`
    `True` at `:93-97` and persists everything together at `:171`, while the
    forwarding secret is not read until `:142`, so groups keep scheduling and
    only rotation detection breaks, silently. Caught during Step 5's citation
    pass, before the claim was written into `charts/spawnery/README.md`. §2
    above carries the corrected version.
20. **Task 6, minor, deferred — fixed by the whole-branch review.** The design
    spec still asserted both disproven claims unedited: §5.2's "guard over
    this milestone's principal risk" and §9's `NetworkNotAccepted`. This
    project groups specs with `known-issues.md` as *living* references and
    plans as historical ones, and `known-issues.md` had both corrections
    while the higher-authority document had neither. Both are now corrected
    in place with a visible marker, in the form
    `docs/superpowers/specs/2026-08-17-network-policies-design.md` §2.4
    already used.
21. **Task 6, minor, deferred — now recorded rather than fixed.**
    `TestARecreatedOrdinalCreatesItsPodOnceThePredecessorIsGone`
    (`internal/controller/server_controller_test.go`) failed once during Task
    6's `make test`, passed in isolation and on rerun, and is unrelated to
    any 6d change. `docs/known-issues.md` carries it under "From milestone
    6d" so that the next milestone does not rediscover it from scratch.
22. **Task 6, complete**, review clean; the verification pass caught six
    citation errors including a false claim in the design spec, which the
    reviewer independently confirmed against `network_controller.go`.

**The whole-branch review, run before merge.** It produced five Important
findings and seven minors, all fixed as one change set, plus two points left
to the fixer's judgement. The Importants: `hack/chart-templates.sh` had no
outcome check on the CRD transform and nothing in the repository covered it
(item 4 above); `TestTheForwardingSecretReaderOpensExactlyOneNamespace` had
replaced a falsifiable assertion with one that cannot fail, leaving the reader
file's subject ServiceAccount *name* asserted nowhere; `hack/e2e.sh`'s and
`test/e2e/e2e_test.go`'s namespace comments still carried the blanket claim
§2 above disproves; the design spec still carried both disproven claims (item
20); and `internal/rbacaudit` audited a hand-listed object set rather than the
chart's whole output, so a new template could ship an unaudited cluster-wide
grant with every test green. Each fix was demonstrated by re-running the
reviewer's own mutation against it. The two judgement calls: the chart gained
a `templates/NOTES.txt`, so that the manual `forwarding-secret-reader.yaml`
step is printed by `helm install` and not only by the README; and
`values.schema.json` was **not** written — it is recorded in
`docs/known-issues.md` with the three exact inputs measured, two that render
cleanly and then fail flag parsing at container start and one that fails
render with a message naming neither the key nor the value. A schema is a new
install-time validation surface that would need its own tests, and `make e2e`
passes four `--set` values a wrong schema would break in a target nobody runs
on the commit loop.

## 6. The environment

Unchanged from 6c's own §6, with one addition: `make test` now also needs
`helm` on `PATH` for `chart-lint` and for `internal/rbacaudit`'s tests
(`flake.nix`'s `kubernetes-helm`, resolving to Helm **v4.2.3** in this shell —
design §6, fact 11). Every command runs inside `nix develop`, and on this
machine that means the full flag, every time:

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make test
systemd-run --scope --user --property=Delegate=yes -- \
  nix --extra-experimental-features 'nix-command flakes' develop -c \
  env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

- `make e2e` is part of neither `make test` nor `make all`, deliberately.
- `kind` runs under rootless Podman here, which needs both
  `KIND_EXPERIMENTAL_PROVIDER=podman` and a systemd scope with
  `Delegate=yes`.
- `TMPDIR` matters: the default `/tmp` is too small for an image archive
  here.
- The machine has 8 GB and no swap. Run one cluster at a time; `E2E_KEEP=1`
  leaves it standing and prints its `KUBECONFIG`.
- Every image derivation takes the working tree as its source, so editing a
  file under `docs/` changes the operator image's derivation hash and makes
  the next `make e2e` rebuild it — slow, not wrong.

## 7. Where everything lives

- Design:
  [`docs/superpowers/specs/2026-08-19-helm-chart-design.md`](superpowers/specs/2026-08-19-helm-chart-design.md).
- Open points: [`docs/known-issues.md`](known-issues.md).
- The chart: `charts/spawnery/`, and its own
  [`README.md`](../charts/spawnery/README.md) for installing, the manual
  forwarding-secret step, the values table and what uninstalling does and
  does not remove.
- The generation step: `hack/chart-templates.sh`, run from `Makefile:14-19`.
- The audit: `internal/rbacaudit/deploy_envtest_test.go`,
  `internal/rbacaudit/audit_envtest_test.go`.
- The driven run: `hack/e2e.sh`, `test/e2e/e2e_test.go`.
- The publish path: `hack/publish.sh`.
- The SDD record of how this milestone was built, task by task, including
  every mutation run and its verbatim output:
  [`.superpowers/sdd/2026-08-19-helm-chart/`](../.superpowers/sdd/2026-08-19-helm-chart/)
  (`task-1-report.md` through `task-6-report.md`, and `progress.md`, the
  ledger §5 restates).
- 6c's record, and what 6d started from:
  [`handover-milestone-6c.md`](handover-milestone-6c.md).
