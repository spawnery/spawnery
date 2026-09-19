# A failure streak belongs to the attempt, not to the generation

**Status:** design, decided 2026-09-19
**Date:** 2026-09-19

## 1. The two open problems

`docs/reference/known-issues.md` carries two entries with one cause: a
`ServerGroup`'s failure streak (`status.consecutiveFailures`, and with it
`BackingOff`, `Degraded` and the give-up latch at six rounds) is reset by
`metadata.generation` moving, and by nothing else
(`internal/controller/servergroup_controller.go`, the
`group.Generation != group.Status.ObservedGeneration` block).

- **Too eager.** Any spec edit clears the streak, including one that changes
  nothing a server starts with. Raising `minReplicas` on a crash-looping group
  starts a fresh window against the same broken image.
- **Too blind.** A corrected `spec.configOverlay` ConfigMap is the fix, and it
  moves neither the generation nor anything else the operator reads. The group
  stays latched until somebody makes an edit that means nothing, which the
  latch message tells them to do (`spec.attributes`).

## 2. The rule

The streak belongs to the **attempt**: what the group's servers start with.
It resets when the attempt changes and only then. The attempt is three things:

1. **`DesiredServerHash`**: everything the operator renders into a server pod.
   Already the key every other staleness decision uses since milestone 7a.
2. **The overlay ConfigMap's `resourceVersion`**, when `spec.configOverlay`
   is set. The operator mounts it without reading it, so its content is in no
   hash, but it is what the servers start with.
3. **The `spawnery.cloud/retry` annotation's value**, when set. A cause
   outside the group (a missing Secret, a registry, a database that was down)
   is fixed without touching anything above, and somebody has to be able to
   say "try again" without editing the spec:

       kubectl annotate servergroup lobby spawnery.cloud/retry="$(date +%s)" --overwrite

The key is recorded in a new status field, `status.failureStreakKey`. A
streak whose recorded key differs from the current one is reset.

### What changes for whom

| Edit | Before | After |
|---|---|---|
| image, env, resources, mounts, `maxPlayers`, … (pod-shaping) | resets | resets |
| `minReplicas`, `maxReplicas`, scaling, drain, update strategy, `attributes` | resets | **no reset** |
| overlay ConfigMap content | no reset | **resets** |
| `spawnery.cloud/retry` annotation | no reset | **resets** |

## 3. Mechanics

**The reset keeps the watermark.** Today's reset sets `lastFailureAt` to nil,
and that is why `ofGeneration` has to exist: with the watermark at zero, the
corpse of the previous attempt would be counted straight back into the fresh
streak. Keeping `lastFailureAt` where it is makes the old corpse too old to
count (`CountFailures` counts only failures after it). It also removes the
persistent-group quirk where a reset came back as 1 instead of 0.

**Ephemeral counting filters by hash, not generation.** `ofGeneration` becomes
`ofAttempt`: views whose `spec.podHash` equals the current desired hash. That
keeps what the generation filter was for: a server of the previous spec going
Ready says nothing about this one. When the hash is not computable (the
Network is unusable), the filter uses the hash recorded in the key, so a group
whose Network went away keeps counting its own failures, the case the
known-issue entry named as the reason the hash could not be used.

**The hash moves earlier.** It is computed today after the counting and only
when `mayResize`; it moves above the counting, same gate. Nothing between the
two positions reads it.

**The overlay is read only while a streak is running.** A group with
`consecutiveFailures == 0` needs no key and issues no read. Once a streak
starts, each pass reads the overlay ConfigMap through the uncached reader the
reconciler already has for claims (`ClaimReader`), one GET per group per
pass while it is failing. No watch: the operator's ConfigMap cache is narrowed
to its own label (`cmd/spawnery-operator`), and widening it to every
ConfigMap in the cluster for this would cost more than the problem.
A missing overlay ConfigMap contributes `missing` to the key, so creating it
also counts as a change.

**Upgrade adopts, it does not reset.** A group upgraded with a streak running
has an empty `failureStreakKey`. An empty key is set to the current one
without resetting: a latched group stays latched across the upgrade, and its
message tells whoever reads it how to retry.

**The latch message** names the annotation instead of `spec.attributes`.

## 4. What does not change

- The capacity arithmetic stays generation- and hash-blind (the
  `ofGeneration` comment's standing constraint; `ofAttempt` is on the counting
  path only).
- `DesiredServerHash` itself, and so its golden: nothing rolls on upgrade.
- `ProxyGroup` has no failure streak.

## 5. Version

A new status field and a new annotation with behaviour: a **minor** step
(0.4.0), per the SemVer rule of 2026-09-08. `operatorVersion` and the chart
move; `imageVersion` does not.
