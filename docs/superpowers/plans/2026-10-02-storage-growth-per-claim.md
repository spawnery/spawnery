# Per-claim Storage Growth Implementation Plan

**Goal:** `spec.storage.annotations` lands on each new data claim, and `spec.storage.size` may be lowered.

**Spec:** `docs/superpowers/specs/2026-10-02-storage-growth-per-claim-design.md`

## Global Constraints

- Every command runs in the dev shell (`nix develop -c <cmd>`).
- `growClaim` stays the only write to an existing claim and never lowers a request.
- Generated files after API changes: `make manifests generate`, committed.
- Nothing about any real network or cluster in code, tests, docs or commits.

## Tasks

- [x] **API.** Delete the `storage.size must not shrink` rule, rewrite the `Size` doc, add `Annotations`. Envtest: a lowered size is accepted for persistent and on-demand groups, annotations round-trip.
- [x] **Claim.** `BuildDataClaim` clones `Storage.Annotations` onto the claim. Test: copied, not shared, labels unchanged, nil when unset.
- [x] **Wording.** `growClaim`, `storageResizeCondition`, `StorageResizeError` and `ConditionStorageResize` describe refused resizes from any source.
- [x] **Behaviour test.** A lowered size leaves an existing claim untouched (same resourceVersion, no resize error); a member created afterwards gets the lowered size and the annotations.
- [x] **Docs.** "Claims that grow by themselves" in the persistent-worlds guide, a link from the on-demand guide, regenerated CRD reference.
- [x] **Validation.** `storage.annotations` is capped at 64 keys and every key must be a valid Kubernetes annotation key (CEL). Envtest: invalid keys and 65 keys are rejected.
- [x] **Create only if missing.** `growClaim` reports whether the claim exists and the reconcile creates a claim only when it does not. Test: an existing claim is not created again, so a create the API server would refuse cannot block the pod.
