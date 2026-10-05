# Paths the sources own: spec.storage.replace

**Status:** design, in review
**Date:** 2026-10-05

## 1. What goes wrong today

`spec.storage.keep` (design of 2026-10-01) refuses a start when a path the
list does not keep holds a world, unless the sources ship every entry at and
below that path. The design accepted one gap on purpose: a world an older
version of a source shipped, and the current one no longer ships whole,
cannot be told apart from player state. Prune refuses, and someone removes
the leftover by hand.

A network that ships a lobby template through `extraFiles`, say
`worlds/templates/lobby`, which the server copies from but never loads, runs
into it with every template change. Each on-demand member has its own claim and carries the template
as it was at the member's first start. Once a region file is dropped from the
template, the copy on every existing claim no longer matches the source, and
every saved member of the group refuses to start until someone cleans its
claim by hand. With a few dozen members that is a job per claim, repeated at
the next template change.

The operator has no way to tell which paths the sources own. The group's
author knows.

## 2. The shape

An optional `spec.storage.replace`, a list of paths the sources own. A path
a replace entry matches is deleted at start like any other path keep does not
list, but the world guard does not look at it. The copy after the prune
writes back what the sources ship now, and whatever they no longer ship is
gone.

```yaml
storage:
  size: 5Gi
  keep:
    - world
    - plugins/ExampleGame/state
  replace:
    - worlds/templates
```

Unset, prune behaves as it does today and the pod is byte-for-byte what it
was.

## 3. Decisions

- **An explicit list.** Loosening the guard was the alternative: let extra
  `.mca` files through inside a world whose root a source ships, and refuse
  only on player state (`playerdata/`, `stats/`, `data/`). That would also
  delete chunks players explored in a shipped world the server loads, which
  the guard exists to protect. With a list, the author decides for the paths
  they name, and the guard stays as strict as before everywhere else.
- **Replace needs keep.** Prune runs only when keep is set, so a replace
  list without one would do nothing and look as if it did. CEL refuses it.
- **Keep wins.** A path keep matches is never queued, so a replace entry at
  or below a kept path has no effect. CEL refuses an entry that appears in
  both lists verbatim. Overlaps through globs are not detected in CEL; they
  are harmless, because keep is checked first.
- **No check that a source ships the path.** The field is for paths whose
  shipped form changed or disappeared. Requiring a source to carry the path
  would bring the refusal back for a template that was removed.
- **Same syntax as keep.** Relative paths, `*` and `?` per segment, the
  same CEL rule on items, 1 to 64 entries of at most 256 characters. A
  matched directory is replaced whole.

## 4. API

`StorageSpec.Replace []string`, optional and mutable, with the same item
validation as `Keep`. Two rules on `StorageSpec`:

- `!has(self.replace) || has(self.keep)`, message "spec.storage.replace
  needs spec.storage.keep";
- `!has(self.replace) || !has(self.keep) || self.replace.all(r, !(r in
  self.keep))`, message "a path cannot be in both spec.storage.keep and
  spec.storage.replace".

## 5. Delivery

`internal/podspec` emits `SPAWNERY_REPLACE`, the entries joined by newlines,
only when the field is set. A group without it builds the identical pod, and
the hash golden does not move. As with keep, a persistent group that sets or
changes it rolls. An on-demand group does not: the list reaches each member
at its next start, which is when prune runs.

`image/entrypoint.sh` passes `--replace "${SPAWNERY_REPLACE:-}"` to
`spawnery-config --prune`. The Velocity entrypoint is untouched.

## 6. The prune

`prune.Run` takes the replace entries next to keep.

1. Planning enters a directory that is a proper ancestor of something a
   replace entry could match, as it does for keep entries and mount points.
   A replaced path is therefore queued as its own unit, and the entries
   beside it are queued and guarded on their own.
2. The world guard skips a queued path that a replace entry matches, or that
   lies below one. A queued path whose ancestor was entered only on the way
   to a replace entry, and that ancestor holds a `level.dat*` file or a
   `region` directory itself, is guarded as part of that world: a replace
   entry inside a world keep does not list must not let the world's player
   state through piece by piece. Ancestors entered for keep or a mount are
   checked as before.
3. The overlap check (a source carrying a path keep holds) is unchanged.
   Replace adds nothing to it: a source carrying a replaced path is the
   normal case.
4. Removal logs `spawnery: keep: removing <path>` as before, so a replaced
   path shows up the same way in the start log.

`--replace` with an empty value is the same as no flag. An entry that fails
parsing refuses with exit 1 and names `spec.storage.replace`.

## 7. Versions

A new CRD field, an entrypoint change and a binary change: a minor step for
the operator, the chart and the images. The release is a separate PR. A
cluster has to run the new CRD before a group sets the field, and the new
image before the field has any effect.

## 8. Testing

- **prune:** a replace entry inside an unkept world still refuses on its
  player state; the world refusal names `spec.storage.replace` as the remedy;
  a world below a replaced path is deleted without refusing, even
  with files no source ships; a world beside a replaced path in the same
  parent still refuses; a replace entry with a glob; a replace entry at or
  below a kept path leaves the kept path alone; with no replace entries,
  every existing test passes unchanged; a bad replace entry refuses and
  names the field.
- **spawnery-config:** `--replace` is parsed; empty equals absent.
- **entrypoint:** `--replace` reaches prune with the variable's value, and
  prune still runs only when `SPAWNERY_KEEP` is set.
- **API:** accepted with keep; refused without keep; refused when an entry
  is in both lists; the item rule refuses `/x`, `a/../b`, `a//b` and `a[b`.
- **podspec:** the variable is present only when set; the golden is
  unchanged; the reserved-env test knows the new name.

## 9. Not in this design

- Detecting glob overlaps between keep and replace.
- Replacing anything on the Velocity image.
- Cleaning claims that are not started: a stopped member is fixed at its
  next start, not before.
