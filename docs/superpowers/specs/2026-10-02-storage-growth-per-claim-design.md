# Per-claim storage growth: spec.storage.annotations

**Status:** design, approved 2026-10-02
**Date:** 2026-10-02

Supersedes the "one field, upward only" part of
[the 5b design](2026-08-16-persistent-updates-design.md) section 4 for lowering
`spec.storage.size`. 5b stays as history. `growClaim` still only writes
`requests.storage`, and only upward.

## 1. The problem

Every claim of a group is created at `spec.storage.size`, and raising the size
grows all of them. A group with many members (an on-demand group above all)
pays for the headroom of every claim, although only a few fill up. A per-PVC
autoresizer such as topolvm/pvc-autoresizer grows exactly the claims that need
it, but wants to start small and reads its ceiling from a PVC annotation
(`resize.topolvm.io/storage_limit`), which the operator has no way to set.

## 2. The shape

- An optional `spec.storage.annotations`, copied onto each data claim when it
  is created. Shape and naming mirror
  `ProxyGroup.spec.expose.loadBalancer.annotations`. At most 64 keys, each a valid
  Kubernetes annotation key (checked by the CRD), and no labels field. The
  reconcile also creates a claim only when none exists, so a claim the API
  server would refuse on create can never keep an existing server from getting
  a pod.
- Only new claims get annotations. Existing claims are not patched, so
  `growClaim` stays the only write to an existing claim. Backfilling is one
  `kubectl annotate pvc -l spawnery.cloud/group=<group> ... --overwrite`, in the
  persistent-worlds guide.
- The CEL rule "storage.size must not shrink" is removed for every group type.
  Lowering `size` affects claims created afterwards. `growClaim` never lowers
  (`want.Cmp(have) <= 0`) and the API server refuses a PVC shrink, so a claim
  larger than `size` is left alone, whoever grew it.
- Raising `size` above an annotated ceiling still grows claims; the
  autoresizer stops at its own ceiling.

## 3. Status wording

Another controller can now resize a claim, so `StorageResize` and
`status.storageResizeError` report whether a resize of the claim was refused,
whoever requested it, rather than whether the claim matches `size`.

## 4. Out of scope

Patching annotations onto existing claims, a labels field, and any
autoresizer-specific logic in the operator.
