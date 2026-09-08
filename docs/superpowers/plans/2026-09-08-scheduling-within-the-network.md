# Scheduling within the Network Implementation Plan

**Goal:** A group may only ask of the scheduler what its Network allows, and a
HostPort proxy may only take a port from the Network's range.

**Spec:** `docs/superpowers/specs/2026-09-08-scheduling-within-the-network-design.md`

## Tasks

- [ ] `api/v1alpha1`: `SchedulingPolicy` and `PortRange` types,
      `NetworkSpec.Scheduling`, reasons `SchedulingNotAllowed` and
      `HostPortNotAllowed`. `make generate manifests`.
- [ ] `internal/podspec/scheduling.go`: `EffectiveScheduling`,
      `SchedulingRefusal`, `HostPortRefusal`; both builders use
      `EffectiveScheduling`. Table tests.
- [ ] `internal/controller`: both acceptance chains call the two checks after
      the volume checks. One reconcile test per group kind.
- [ ] Docs: `network-boundaries.md` says the boundary holds for scheduling and
      names the field; `upgrading.md` carries the upgrade note.
