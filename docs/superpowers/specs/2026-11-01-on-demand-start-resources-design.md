# Resources chosen at the start of an on-demand server

**Status:** proposed
**Date:** 2026-11-01

## 1. What is missing

Every member of an `OnDemand` group gets the group's `spec.resources` and
the group's `spec.env`. `StartServerRequest` carries a group and a key and
nothing else (`proto/spawnery/agent/v1alpha1/agent.proto`), so a network
cannot give one player's world more memory than another's.

An example: a network offers private worlds in several sizes, and a player
who paid for more gets a bigger one. How much bigger is the network's
decision and changes with its offer, so a fixed list of sizes in the group
is not enough; the caller wants to pass the numbers. Today the only way is
one group per size, which splits one
kind of world over several groups, each with its own claims, its own
`maxInstances` and its own key namespace.

Memory has two numbers on a JVM server, and both must move together: the
container's limit and the heap. A group that sets `-Xmx` through
`JDK_JAVA_OPTIONS` and raises only `resources.limits.memory` gets no larger
heap, because `-Xmx` overrides the entrypoint's `MaxRAMPercentage`. The
server then runs out of heap while gigabytes of its limit stay unused, and
it shows as GC pauses and long ticks, not as an OOM kill. A start that
raises the limit has to raise the heap with it.

## 2. Goals, non-goals, anti-goals

Goals:

- The caller of `startServer` may set memory request, memory limit, heap,
  CPU request and CPU limit for that member. Whatever it leaves out comes
  from the group.
- The group sets a ceiling per member. A start above it is refused.
- A start cannot produce a heap that does not fit its limit: values that
  contradict each other are refused, and a limit given without a heap
  brings a heap derived from it.
- A plugin can see what a member runs with.
- The request shape leaves room for passing more per-start settings later
  (section 8) without a second call or a breaking change.

Non-goals:

- Deciding who gets how much. That is a question about a player (rank,
  purchase, ban), and the system that knows the player answers it, as it
  already answers who may have a private server at all. The operator stores
  no ranks and no per-player quota.
- Resizing a running member. A JVM cannot grow its maximum heap, so the
  values take effect at a start. Stop and start with new values is the way,
  and the world survives it as it survives any stop.
- Free-form environment variables per start. Wanted later, sketched in
  section 8, not built now.
- Per-start resources for `Ephemeral` and `Persistent` groups. Their
  servers are not asked for one by one.

Anti-goals, which would make this a failure even if it works:

- A member whose heap does not fit its limit. A member that OOM-kills its
  own pod, or that leaves gigabytes of limit unused by the heap, repeats
  the case of section 1.
- A start that silently runs with other values than the ones asked for,
  for instance against an operator or an image that predates this feature
  (sections 4 and 6).
- Any change to groups that do not opt in. A group without
  `memberResources` renders byte-identical pods and refuses every
  per-start value.

## 3. The API

### 3.1 The group

`ServerGroupSpec` gets an optional block, allowed only for `type: OnDemand`.
Without it a start may not set any value (section 3.2):

```yaml
spec:
  type: OnDemand
  resources:                 # unchanged: what a start without values gets
    requests: { memory: 2Gi }
    limits:   { memory: 4Gi }
  memberResources:
    max:
      memory: 16Gi
      cpu: "8"
    nonHeap: 2Gi
```

- `max`: a `corev1.ResourceList`, required, with `memory` required and
  `cpu` optional. It bounds every request and every limit a start sets,
  and also the group's own `spec.resources` combined with them: a start that
  sets only a CPU limit is still refused if the group's memory limit is
  above `max.memory`. A resource not named in `max` cannot be set at a
  start.
- `nonHeap`: a `resource.Quantity`, default `2Gi`. When a start sets a
  memory limit and no heap, the heap is the limit minus `nonHeap`. The
  default leaves room for what a Paper JVM holds outside the heap
  (metaspace, code cache, thread stacks, direct buffers); a network that
  has measured its own servers sets its own value.

CEL validation on the group:

- `memberResources` only with `type == 'OnDemand'`.
- `max.memory` above `nonHeap`, so a derived heap can exist at all.

### 3.2 The request

```proto
// Values for one member, as Kubernetes quantities ("6Gi", "500m"). An
// empty field takes the group's value.
message StartResources {
  string memory_request = 1;
  string memory_limit = 2;
  string cpu_request = 3;
  string cpu_limit = 4;
  string heap = 5;
}

message StartServerRequest {
  string group = 1;
  string key = 2;
  StartResources resources = 3;
}

message StartServerResult {
  string server = 1;
  bool already_running = 2;
  // What the member's Server carries: the resolved values, with the
  // group's filled in where the start left them out. Empty for a member of
  // a group without memberResources and for one started before this field
  // existed.
  StartResources resources = 3;
  // Set by an operator that knows per-start resources, on every answer to
  // a request that carried any. An older operator cannot set it, which is
  // how the agent tells it apart (section 6).
  bool resources_understood = 4;
}
```

Quantities travel as strings because callers take them from configuration
written in that form, and the operator parses them with
`resource.ParseQuantity` anyway.

"The group's value" is what the pod would get today: the group's
`spec.resources`, or the network's `spec.defaults.resources` when the group
has none, taken as a whole like `BuildServerPod` takes it. Within that, a
request that is not written down counts as its limit, because that is what
Kubernetes makes of a container with only a limit.

How the operator resolves a start, in this order:

1. Each empty field takes the group's value. A memory request left empty
   takes the group's request, lowered to the new limit if the group's is
   above it; the same for CPU. The resolved request is always written out
   explicitly, so neither Kubernetes nor a namespace `LimitRange` raises it
   to a new, larger limit.
2. Heap: the given heap; otherwise, if the start set a memory limit, that
   limit minus `nonHeap`; otherwise none, and the member gets the group's
   behaviour of today.
3. Refuse (`REFUSED`) when the start carries values and the group has no
   `memberResources`; when a field
   does not parse or is not positive; when a request is above its limit;
   when the heap is not below the memory limit, or a heap is given without
   any memory limit; when any resolved request or limit, including one
   taken from the group or the network, is above `max`.

In a group with `memberResources` these steps run for every start, also
one without values, and the result is stamped on the `Server` (3.3). In a
group without it a start without values stamps nothing and its answer
carries no `resources`, so its pods stay as they are today. `heap` is empty
in every answer for a member without `Spec.Heap`: the operator does not
guess what `MaxRAMPercentage` or an env `-Xmx` made of it. A caller
compares only the fields it set.

The refusal message names the field and both numbers, because a person
reads it in a log next to a player's failed start.

A start of a running member is answered with that member's values before
the request's values are looked at, so a second press never fails: not
with other values, and not with values the group's `max` no longer
allows. The caller compares `resources` with what it asked for and decides
whether to stop and start again.

### 3.3 The Server

`ServerSpec` gets two fields, set by `StartServer` and never changed
afterwards (CEL `self == oldSelf`):

- `resources`: the resolved `corev1.ResourceRequirements` of step 1.
- `heap`: the resolved heap of step 2, or unset.

The member carries its own copy because its pod is created after the
`Server`, sometimes much later: the controller waits for the namespace
bootstrap, a claim it may reject, or world sync
(`internal/controller/server_controller.go`). A group edited in that window
must not change what the pod gets. Once the pod exists it is never rebuilt
for the same `Server`, so the copy is also what the member reports for its
whole run.

`ServerState` in `NetworkState` gets `StartResources resources = 15` (the
next free number at the time of writing), filled from the `Server`, so
`ServerInfo.resources()` shows it. Only the proxies see on-demand members
(`netstate.Audience`). The member's own plugin needs nothing new: its heap
is `Runtime.maxMemory()`.

### 3.4 The plugin API

`SpawneryApi` gains an overload; the existing method stays and means "the
group's values":

```java
CompletionStage<StartedServer> startServer(String group, String key, StartOptions options);

StartOptions.builder()
    .memoryLimit("7Gi")          // heap 5Gi with the default nonHeap
    .build();

StartOptions.builder()
    .memoryRequest("6Gi").memoryLimit("8Gi").heap("5500Mi")
    .cpuLimit("4")
    .build();
```

`StartOptions` is immutable and built, so later options (section 8) are new
builder methods rather than new overloads. The builder does not validate
quantities; the operator is the one place that does. `StartedServer` and
`ServerInfo` gain `resources()`, a small value with the five fields as
`Optional<String>`.

## 4. The pod

`podspec.BuildServerPod`:

- Resources: `srv.Spec.Resources` when set, otherwise the group's, otherwise
  the network default, as today.
- Heap: when `srv.Spec.Heap` is set, the container gets
  `SPAWNERY_HEAP=<heap in MiB, rounded down>m`. The name falls under
  `ReservedEnvPrefix`, so a group's `spec.env` cannot set it. The
  entrypoint reads it as `${SPAWNERY_HEAP:-}`, the form
  `image/reserved_env_test.go` scans for; a bare `$SPAWNERY_HEAP` would
  escape that test.

`image/entrypoint.sh`: when `SPAWNERY_HEAP` is set, it passes
`-Xmx$SPAWNERY_HEAP` in place of `-XX:MaxRAMPercentage=75`. On the command
line it comes after whatever `JDK_JAVA_OPTIONS` and `JAVA_TOOL_OPTIONS`
carry, and the JVM takes the last `-Xmx`, so the start's heap wins over a
group-wide `-Xmx` in `spec.env`. That is intended: the start is the more
specific choice. The guide says so, because a group that relied on its env
`-Xmx` will see members started with values ignore it. `_JAVA_OPTIONS` is
the exception: the JVM applies it after the command line, so an `-Xmx`
there beats the start's. The guide names it, and `HeapMismatch` (below)
reports it.

An image whose entrypoint predates `SPAWNERY_HEAP` ignores it and runs a
`MaxRAMPercentage` heap or the group's env `-Xmx`, the silent mismatch the
anti-goals rule out. Gating by image tag would miss images built on top of
spawnery's, so the operator checks the result instead: the agent already
reports `heap_max_bytes`, and when it differs from `Spec.Heap` by more than
5 % the `Server` gets a `HeapMismatch` warning event and a condition naming
both numbers. The release notes name the first image version that reads
`SPAWNERY_HEAP`.

`DesiredServerHash` renders an empty `Server` and stays as it is. On-demand
members are never rolled, so per-member values cannot make one look stale.

## 5. The operator's start

`KubeWriter.StartServer` keeps its order. The loop over members answers a
running key first, with the existing member's values, and `occupant` does
the same on the create race. After the loop, and before the ceiling is
checked, it resolves and validates the request's values as in 3.2, so a
refused value is refused even when the group is full. The resolved values
go into the new `Server`.

Every answer to a request that carried values sets `resources_understood`.

`maxInstances` stays a count of members. A budget across members (total
memory of the group) is not part of this design: how much of the cluster a
network spends on large worlds is the caller's decision, `max` keeps one
mistake from being large, and the scheduler refuses what does not fit (a
member then waits in `Pending`, which the group condition reports).

## 6. Compatibility

- A new agent against an old operator: proto3 drops the unknown
  `resources` field, and the member would start with the group's values
  without anybody noticing. The agent therefore fails the stage when it
  sent values and `resources_understood` comes back false, with an
  `IllegalStateException` saying the operator does not know per-start
  resources. This is the agent's own failure, not a `RequestError.Reason`,
  since no operator sent it; the member that the old operator did start is
  left running and the message says so. A start without values is
  unaffected.
- An old agent against a new operator sends no values, and nothing
  changes.
- A group without `memberResources` sets no new field and no env var, and
  its pods stay identical.
- The new `ServerGroup` and `Server` fields are optional, so stored objects
  stay valid. No conversion is needed within `v1alpha1`.

## 7. Documentation

- `docs/guides/on-demand-servers.md`: a section on per-start resources with
  the example of 3.1, how empty fields and the derived heap resolve, the
  refusals, the note that a start's heap overrides an env `-Xmx`, and that
  values take effect at the next start.
- `docs/reference/crds.md` regenerated.
- Release notes: the overload, the new fields, the old-operator failure
  from section 6, and the first image that reads `SPAWNERY_HEAP`.

## 8. Later: passing more at a start

This is wanted but not built now. The design leaves room for it as
follows:

- `StartOptions.builder().env(name, value)` and `StartServerRequest.env`
  (a map).
- The group declares which names a start may set, for instance
  `spec.memberEnv: [{name: GAME_MODE}]`. A name not listed is refused, and
  the reserved prefix stays reserved. Without such a list a plugin could set
  `JAVA_TOOL_OPTIONS` or anything else the image reads, which is a larger
  boundary than choosing numbers under a ceiling.
- Values copied onto the `Server` like the resources, for the same reason.

Nothing in sections 3 to 6 has to change for it.

## 9. Testing

- envtest for the CEL rules of 3.1 and the immutability of 3.3.
- `internal/agentserver`, resolution and refusals as a table: every empty
  field taking the group's value, also from the network default when the
  group has none; a group with a limit and no request, started with a
  larger limit, keeping the group's limit as request; the request lowered
  to a new limit; the derived heap; a start without values stamped only in
  a group with `memberResources`; each refusal of 3.2 step 3, also at the ceiling; a group
  without `memberResources` refusing any value; a running member answering
  with its own values whatever was asked; `resources_understood` set on
  every path that carried values.
- `internal/podspec`: resources and `SPAWNERY_HEAP` from the `Server`; a
  group without `memberResources` renders the same pod as before (golden
  comparison).
- `internal/controller`: a reported heap off `Spec.Heap` by more than 5 %
  raises `HeapMismatch`; one within it does not.
- `image/entrypoint_test.go`: `-Xmx` replaces `MaxRAMPercentage` when
  `SPAWNERY_HEAP` is set, and comes after an `-Xmx` in `JDK_JAVA_OPTIONS`.
- e2e: the on-demand manifest keeps its resources on the network default,
  which exercises the network fallback of 3.2.
- Agent tests: the overload sends the values; `resources_understood` false
  fails the stage; an `already_running` answer with different values
  succeeds and carries them.
- e2e: the on-demand manifest gains `memberResources`, and the test starts
  a member with a memory limit only and checks the pod's limit, the
  derived heap the agent reports (`heap_max_bytes`), and a refusal above
  `max`.

## 10. Calls made without the requester

Decided by the author of this design, not by the person who asked for it,
and cheap to turn:

- An empty field takes the group's value per field (3.2), rather than the
  start having to give all of them. "The group's value" includes the
  network default and Kubernetes' request-equals-limit rule.
- A group with `memberResources` stamps resolved values on every member,
  also one started without values; a group without it stamps none (3.2).
- A memory limit without a heap derives the heap as limit minus `nonHeap`
  (default 2 GiB), rather than falling back to `MaxRAMPercentage` or being
  refused.
- A request left empty takes the group's, lowered to the new limit, rather
  than following the limit up. A larger limit therefore does not reserve
  more on the node unless the caller asks for it.
- `max` also bounds the group's own values combined with a start's (3.1).
- A running member answers with its own values, even values `max` no
  longer allows (3.2).
- `resources_understood` as the old-operator marker (3.2, 6).
- Quantities as strings on the wire and in Java (3.2, 3.4).
- An old image is detected after the start from the reported heap (4),
  rather than refused before it by image tag.

## 11. Left to the builder

- Where the refusal errors live and how their messages are worded.
- Whether `StartOptions` and the resources value are records or final
  classes.
- Whether the 5 % heap tolerance is a constant or a flag.
