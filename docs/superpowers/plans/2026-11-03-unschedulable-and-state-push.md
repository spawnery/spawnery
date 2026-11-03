# Unschedulable servers and lifecycle pushes: implementation plan

**Spec:** `docs/superpowers/specs/2026-11-03-unschedulable-and-state-push-design.md`

Every command runs in the dev shell. After the proto change, regenerate with
`make proto` and `go test ./internal/agentpb -run TestTheWireFormatMatchesTheGolden -update`.

## Tasks

1. **Proto.** `ServerState.unschedulable = 16`; regenerate the Go and Java code
   and the wire golden.
2. **netstate.** `Unschedulable(srv)` as the shared predicate; one
   `serverState` builder for `Build` and for `Visible`; the server group
   controller uses the predicate. Tests: the reasons that do and do not count,
   a node clearing the flag, an on-demand member reaching proxies only,
   `Visible` ignoring what reports move and following what lifecycle moves,
   `Visible` agreeing with `Build`.
3. **statepush.** `Notifier` with a per-namespace timer. Tests: the first
   change goes out at once, a burst shares one push, a quiet namespace pushes
   at once again, namespaces are throttled separately, `Start` ends with its
   context.
4. **Controller wiring.** `StateNotifier`, the memo, `noteVisible` after
   `fetchPod` and `forget` on NotFound; `newServerReconciler` so a test can
   assert `Options.State` arrives; `main.go` builds and adds the notifier.
   Tests in the controller fixture (new server, `PodUnschedulable` once, a bind,
   a player count alone, a deleted server once) and an envtest with a real
   manager asserting the picture built at push time already holds the change.
5. **Agent.** `ServerInfo.unschedulable` with a compatibility constructor,
   `NetworkMirror` fills it. Tests: `RecordCompatibilityTest`, `NetworkMirrorTest`.
6. **Docs.** Waiting for room in the on-demand guide, the freshness paragraphs
   in the plugin API pages and `SpawneryApi`.

## Review focus

- The memo is computed from the cache read, before the adoption block
  rewrites `status.podName`.
- A push never targets backends.
- `hash_golden_test.go` passes unchanged.
