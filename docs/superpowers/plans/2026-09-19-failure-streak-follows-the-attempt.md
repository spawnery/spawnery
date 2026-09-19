# Plan: a failure streak belongs to the attempt

Spec: `docs/superpowers/specs/2026-09-19-failure-streak-follows-the-attempt-design.md`.

1. **API.** `ServerGroupStatus.FailureStreakKey string`; the annotation
   constant `spawnery.cloud/retry` beside the other `spawnery.cloud/` names.
   `make manifests generate`, commit the generated files.
2. **Key.** A pure `attemptKey(podHash, overlay, retry string) string` and its
   parse-back of the hash, unit-tested.
3. **Reconcile.** Move the `DesiredServerHash` block above the counting.
   Replace the generation reset with: streak running, key computable, key
   differs from `status.failureStreakKey` → `consecutiveFailures = 0`,
   watermark kept; empty recorded key → adopt. Record the key whenever the
   streak is non-zero. Read the overlay ConfigMap through `ClaimReader` only
   while the streak is non-zero.
4. **Counting filter.** `ofGeneration` → `ofAttempt` (hash, empty hash counts
   as current the way adoption reads it; the recorded hash when the current
   one is not computable).
5. **Message and docs.** The give-up message names the annotation; the two
   known-issues entries go; the guide that documents the latch says what
   resets it.
6. **Tests.** Rewrite the generation-reset tests to the new rule and add:
   `minReplicas` edit keeps the streak, overlay `resourceVersion` change
   resets, annotation resets, empty key adopts, an unusable Network keeps
   counting, the reset does not count the previous attempt's corpse back.
   Each new assertion is shown failing against the old code first.
