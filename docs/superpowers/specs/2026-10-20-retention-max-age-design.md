# A maximum age for world history

**Status:** implemented (branch feat/retention-max-age)
**Date:** 2026-10-20

## 1. Goal

`storage.retention` (see `2026-10-08-world-history-design.md`) only counts.
`last`, `hourly` and the other options keep a number of generations, and a
prune runs only where a generation is written: at an upload, at the release
of a node's lease, and after a restore. A world that nobody starts again
never prunes, so the history it had at its last stop stays in the bucket for
good.

That is expensive when the history is large and the current state is small.
A player builds a large world, resets it in game, and plays the fresh one for
a while. The old generations, each holding the large world, stay as restore
points under `last: N`. When the member is not started again, they stay
forever. In a deployment with many idle worlds, most of the bucket was such
history, and lowering the counts barely helped: the counts still kept the
large generations, and the idle worlds never pruned by the new numbers.

This design adds an age limit to the policy and an hourly pass in the
operator that applies it to worlds nobody plays.

## 2. API

```yaml
spec:
  storage:
    retention:
      last: 12
      daily: 7
      maxAgeDays: 30
```

`maxAgeDays` is optional; unset means no age limit. When set it is at least
1. It sits beside the counts in `RetentionSpec` and in
`worldsync.Retention`, so it travels in `.retention/<namespace>/<group>.json`
like them.

## 3. Semantics

A history entry is a generation that was current once and was then
replaced. It **expires** when the generation that replaced it, the next
newer point, was taken more than `maxAgeDays` ago:

```
points newest first, points[0] the current generation
points[i] expires  ⇔  now − points[i−1].taken > maxAgeDays × 24 h   (i ≥ 1)
```

- An expired entry is dropped even when a count option would keep it.
- The current generation is never an entry and is always kept.
- The pin of a restore (the generation it replaced) is kept as before.
- A replacing point without a time (a manifest written before `taken`
  existed) expires nothing.

Measuring from the replacement rather than from the entry's own time is what
keeps the restore promise. A restore writes its new generation with `taken`
now, so the state it replaced stays restorable for `maxAgeDays` after the
restore, however old that state was. Likewise, the state before a reset
stays a restore point for `maxAgeDays` after the first snapshot of the reset
world, and not merely until the old state's own time is that old.

Expired entries form the oldest end of the list (times rise with
generations), so the count options select the newer generations as before.

`Prune` takes `now` in `PruneRequest`. The node agent passes its clock, the
restore its `now`. A zero `now` expires nothing.

## 4. The aging pass

The operator gains `worldsync.Ager`, a runnable beside the deletion
`Sweeper`, run by the leader only, once an hour. Its first pass runs one
interval after the start, so that the ServerGroup reconciler has published
every group's policy by then.

**Where the policies come from.** The reconciler already writes each group's
policy through `worldsync.Policies`, which remembers what it wrote. The
pass reads that memory and takes every group with `maxAgeDays > 0`. A group
that was deleted keeps its entry until the operator restarts, the same way
its `.retention` file stays in the bucket.

**Per group:**

1. List the group's prefix once. History keys carry generation and time
   (`HistoryKey`), so the listing tells which worlds have history and how
   old it is, without reading a manifest.
2. A world is a candidate when an entry other than the newest is expired by
   the listing alone, or when the newest entry's own time is older than
   `maxAgeDays` (its replacement, the current manifest, is not in the
   listing, but cannot be older).
3. For a candidate: skip it while a deletion marker exists or its lease is
   held. Read the current manifest and decide again with the current
   generation as `points[0]`. A world with nothing expired but the
   restore pin of step 4 is left alone, without taking its lease.
4. Take the lease as `spawnery-operator/<random>`, as a restore does, read
   the current manifest again under the lease, and prune with the group's
   policy and `now`. When the current manifest was written by a restore
   (`restoredFrom` set), the generation below it is pinned, as the restore
   pinned it. Release the lease, also after an error.

The pass does not sweep. A sweep deletes every object no kept manifest
names, which is safe only right after a commit under the lease. Without a
commit, the pass deletes only what the dropped entries named and neither a
kept entry nor the current manifest names.

A world in play is never touched: its lease is held. It prunes by the same
policy, `maxAgeDays` included, at its next upload.

**Metric.** `spawnery_world_aged_objects_total` (operator): objects and packs
the pass deleted. It is not a `spawnery_worldsync_` metric, which are the
node agents'.

## 5. Cost

One listing per group per hour, which pages through every object key of the
group's worlds. A candidate costs a `HEAD` of the deletion marker, a read of
the lease and one of the manifest. Only a world with something to drop costs
a lease, the reads of its kept entries, and the deletes.

## 6. Limits

- Only an expired entry makes the pass prune a world. Once it does, it
  prunes by the whole policy, so a lowered count reaches an idle world then
  and not before.
- An entry whose replacement has no time does not expire.
- After an operator restart, the first pass waits an hour.

## 7. Testing

- Selection: an idle world after a reset loses its old generations once they
  were replaced long enough ago; a recent restore's replaced state survives;
  the current generation is always kept; a pin is kept; zero means no limit.
- The pass against the in-memory store: a held lease is skipped, a released
  lease is taken and released again, nothing else is touched, and a world of
  a group without `maxAgeDays` is not looked at.
- CRD: envtest refuses `maxAgeDays: 0`.
