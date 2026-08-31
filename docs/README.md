# Documentation

Start at the [README](../README.md) for what Spawnery is and how to install it.
This page is the index of everything under `docs/`.

## Operating a cluster

| | |
|---|---|
| [`../charts/spawnery/README.md`](../charts/spawnery/README.md) | Installing the chart, the one manual step per game namespace, and why a game namespace is one trust domain |
| [`upgrading.md`](upgrading.md) | What an operator upgrade moves — including the two things it moves without anyone editing a spec |
| [`ca-rotation.md`](ca-rotation.md) | Rotating the CA the agent channel is built on |
| [`runbook-milestone-5c-secret-rotation.md`](runbook-milestone-5c-secret-rotation.md) | Rotating a network's Velocity forwarding secret |
| [`persistent-storage.md`](persistent-storage.md) | How persistent worlds are stored, and why a claim outlives its server on purpose |
| [`network-boundaries.md`](network-boundaries.md) | What the two `NetworkPolicy` objects buy, and what they do not. Read before treating them as protection |
| [`plugins.md`](plugins.md) | Loading third-party plugins from a volume, without rebuilding an image — and what Longhorn's `ReadWriteMany` adds to the failure surface |
| [`group-environment.md`](group-environment.md) | `spec.env` on a group: the reserved `SPAWNERY_` prefix, and why `JAVA_TOOL_OPTIONS` is the only way to reach the JVM |
| [`mounts.md`](mounts.md) | `spec.mounts`: putting a ConfigMap, a Secret or a claim at a path in a group's pods, and the one claim rule that has no exceptions |

## Working on Spawnery

| | |
|---|---|
| [`known-issues.md`](known-issues.md) | Everything open right now. An entry is deleted when it closes, so an empty file means nothing is open |
| [`development.md`](development.md) | Building, testing, the images, publishing, and the hand-driven local `kind` flow |
| [`../agent/api/README.md`](../agent/api/README.md) | The plugin API: what a Paper or Velocity plugin can ask the cloud, and the one rule that breaks a consumer |
| [`history.md`](history.md) | How it was built, milestone by milestone, and what each one measured rather than assumed |
| [`cloudnet-parity.md`](cloudnet-parity.md) | What a running CloudNET network needs that Spawnery does not have, measured against one — deleted when it is empty |
| [`superpowers/specs/`](superpowers/specs/) | The design documents, one per milestone |
| [`superpowers/plans/`](superpowers/plans/) | The implementation plans the designs became |

## The milestone record

Two families of file, kept rather than compressed, because between them they are
the answer to "why is it like this".

**Handovers** say where a milestone stopped: what was actually driven versus what
only exists on paper, what the next milestone finds in place, and what is still
owed. [`handover-milestone-6e.md`](handover-milestone-6e.md) is the most recent
and the one to read first — it is written for someone with no memory of how any
of this was built. Its predecessors are kept because each one's opening sections
are the record of what the milestone after it started from and had to decide:
[6d](handover-milestone-6d.md), [6c](handover-milestone-6c.md),
[6b](handover-milestone-6b.md), [6](handover-milestone-6.md),
[5](handover-milestone-5.md), [4b](handover-milestone-4b.md),
[4](handover-milestone-4.md), [3](handover-milestone-3.md),
[2c](handover-milestone-2c.md), [2b](handover-milestone-2b.md).

**Runbooks** are the procedures, and the logs from the runs that drove them. They
are what turns a claim into a measurement, which is why several of them record a
result nobody wanted:

| | |
|---|---|
| [`runbook-milestone-6-rollout.md`](runbook-milestone-6-rollout.md) | The RKE2 rollout: the first time either game image ran in any cluster |
| [`runbook-milestone-5c-evidence.md`](runbook-milestone-5c-evidence.md) | Forwarding secret rotation, driven against the standing procedure rather than around it |
| [`runbook-milestone-5b-evidence.md`](runbook-milestone-5b-evidence.md) | Two worlds surviving an update that recreated both, one ordinal at a time |
| [`runbook-milestone-5a-evidence.md`](runbook-milestone-5a-evidence.md) | A world outliving its pod: blocks placed, pod deleted, blocks still there |
| [`runbook-milestone-4c1-evidence.md`](runbook-milestone-4c1-evidence.md) | Proxy drain and proxy rolling updates, with a licensed client |
| [`runbook-milestone-3-evidence.md`](runbook-milestone-3-evidence.md) | The first real join, and the drain finding that came with it |
