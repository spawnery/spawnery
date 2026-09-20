# Operator flags

`cmd/spawnery-operator/main.go` reads these from the command line. This page
is hand-written -- a flag's meaning is not in its usage string, `--help`
already prints that -- and a Go test,
[`cmd/spawnery-operator/flags_docs_test.go`](https://github.com/spawnery/spawnery/blob/master/cmd/spawnery-operator/flags_docs_test.go),
fails if a flag is added here and not there, or there and not here.

The chart sets most of these through `values.yaml`'s `operator.*` keys; see
[chart values](chart-values.md) for the ones it exposes and their own
defaults, which are not always the flag's own.

The manager also accepts controller-runtime's own logging flags --
`--zap-log-level`, `--zap-devel`, `--zap-encoder`, `--zap-stacktrace-level`,
`--zap-time-encoding` -- registered by `zap.Options.BindFlags` rather than by
this file. They belong to controller-runtime, not to spawnery, and are out of
scope here.

## Manager

### `--metrics-bind-address`

Default: `:8080`

Where the Prometheus metrics endpoint listens. See
[metrics and alerts](metrics-and-alerts.md) for what it serves.

### `--health-probe-bind-address`

Default: `:8081`

Where `/healthz` and `/readyz` listen. `/readyz` reports ready only once this
replica holds the leader-election lock -- a standby answers `not the leader
yet` rather than ready, because the agent gRPC endpoint is leader-bound and a
standby serving readiness would attract agents into a registry no controller
reads.

### `--leader-elect`

Default: `true`

Runs leader election among replicas. On by default so that running more than
one replica is never a decision made twice, once when the chart was written
and once when the flag was flipped. Set to `false` for a local `go run`
outside a cluster: left on, controller-runtime looks for a ServiceAccount
token mount that only exists inside a pod, and a local run dies at startup.

### `--namespace`

Default: empty, meaning every namespace

Restricts the manager's cache, and therefore every controller, to one
namespace. A multi-tenant installation running one operator per tenant sets
this; the default is a cluster-wide operator watching every `Network` there
is.

### `--operator-namespace`

Default: `$POD_NAMESPACE`

The namespace the operator itself runs in: where the TLS secret for the agent
channel lives and where the agent-facing Service is expected. The chart sets
`POD_NAMESPACE` from `metadata.namespace`, so this is normally never passed
explicitly. Empty and unset together refuse to start -- see
[`--agent-session-deadline`](#-agent-session-deadline) below for why a wrong
value here is worse than no value.

## The agent channel

Every game server and proxy pod holds exactly one gRPC stream to the leader.
These three flags bound that stream's lifetime and how the operator judges it.

### `--report-interval`

Default: `5s`

How often an agent reports its player count. A count older than twice this is
stale. It also sets the ceiling on how quickly the operator can notice a
server on a node that has died and start moving players off it -- the
operator logs a warning at startup if this value leaves too little room
before Velocity's own read timeout disconnects them first.

### `--startup-deadline`

Default: `5m`

How long a server may take to reach `Ready` before `phase.Decide` counts it
`Failed`.

### `--player-status-interval`

Default: `30s`

How often an unchanged player count is still written into the `Server`
status. Status is for observers, not for the control loop -- nothing here
reads it back -- so this trades API write volume against how stale the
number looks to `kubectl get`.

### `--permission-check-interval`

Default: `10m`

How often the operator asks the API server, via `SelfSubjectAccessReview`,
whether it still holds the permissions `internal/rbacaudit` expects. A
negative value checks once at startup and never again. The repeat cost was
measured at 73 reviews per check, 54ms.

### `--agent-bind-address`

Default: `:9443`

Where the agent gRPC endpoint listens. This is the port behind the Service a
game server pod actually dials -- the leader, not the pod, since the
endpoint is a leader-bound runnable.

### `--agent-session-renew-after`

Default: `8m`

When an agent should open a fresh stream rather than keep using the one it
has. Must be below `--agent-session-deadline`, or the operator would cut
every stream mid-renewal; the operator refuses to start otherwise.

### `--agent-session-deadline`

Default: `10m`

When the operator closes an agent's stream regardless of activity. Bounded
above by the agent token's own lifetime: a stream is authenticated once, when
it opens, so it may not outlive the token that opened it -- otherwise a
stolen token would stay useful for longer than its own expiry says. The
operator refuses to start if this is set above that lifetime. A CA rotation
also waits out this same duration before switching the serving certificate,
so that every stream opened under the old CA has closed and reopened under
the new one by the time the switch happens.

## Orphan sweep

### `--orphan-interval`

Default: `1m`

How often the orphan reconciler runs: the pass that catches the two
directions no watch covers by itself -- a managed pod whose `Server` is
gone, and a `Server` whose group is gone -- and drops registry entries for
pods that no longer exist, so that map stays bounded.

## Node draining

### `--drain-taint`

Default: none; repeatable

A taint key, beside `spec.unschedulable`, that marks a node as departing.
Pass it once per key: `--drain-taint karpenter.sh/disrupted --drain-taint
node.kubernetes.io/unreachable`. Only the `NoSchedule` and `NoExecute`
effects on a matching key count -- `PreferNoSchedule` does not stop the
scheduler putting the replacement pod straight back on the same node, so
honouring it would condemn a pod, rebuild it there, and condemn that one
next pass.

This is a bare key, not `key=value:Effect` -- the value is never compared,
only the key and the effect read off the node's own taint. Passing a whole
taint, the shape `kubectl taint` and most tutorials use, matches nothing:
the flag is accepted, nothing ever drains, and nothing says why. The operator
warns only for a small set of taints other autoscalers are known to use
(`ToBeDeletedByClusterAutoscaler`, `karpenter.sh/disrupted`,
`karpenter.sh/disruption`); a key of an operator's own choosing that is
simply absent from the cluster cannot be told from a typo by anything here.

**What condemning a server actually does depends on whether its group is
ephemeral, persistent or on-demand, and this is the flag's real edge.** An
ephemeral group treats a condemned server exactly like a stale one: a
replacement is created before the condemned server is removed, so the group
never drops below its target count. A persistent group cannot do that. Its ordinal is
tied to a `ReadWriteOnce` claim, and a second pod mounting that claim while
the first is still draining does not fail cleanly, it hangs on the volume --
so the replacement for a persistent ordinal waits for the condemned server's
drain to finish, bounded by `spec.drain.timeoutSeconds`. Draining a node that
holds a persistent world is a real, bounded gap for that world, not a hot
changeover: the group cannot replace what it condemns until it has finished
condemning it. An on-demand group's member is simply removed, players moved
first: nothing replaces it and nothing waits on it, because its world is on its
claim and its key is free for its owner to start again.

## Claim-backed sources

Three switches, one per field that can point a group at a
`PersistentVolumeClaim` an administrator already filled. All three are off by
default, and turning one on is a values edit and a Deployment restart, not a
CRD change.

**None of the three is a security control**, and this page will not pretend
otherwise: a `PersistentVolumeClaim` is a namespaced object in the same trust
domain as the group that names it, so anybody who could write the group could
have written the claim. What a switch actually buys is an installation being
able to say *this cluster runs no third-party plugins* -- or files, or
arbitrary mounts -- and have that be a fact the operator enforces rather than
a convention nobody checks. The chart's own `values.schema.json` says the same
thing about the matching values.

Until 0.2.x, a single flag -- what is now `--allow-plugin-volumes` -- governed
both `spec.extraPlugins` and `spec.mounts` together. An installation upgrading
from before that split needs both flags now if it relied on the old one for
mount-backed claims; leaving only the old flag on will start refusing groups
that name a claim-backed mount.

### `--allow-plugin-volumes`

Default: `false`

Lets a group name a `spec.extraPlugins` claim, whose contents the entrypoint
copies into every server's `plugins/` directory on start. See
[plugins from a volume](../guides/plugins-from-a-volume.md).

### `--allow-file-volumes`

Default: `false`

Lets a group name a `spec.extraFiles` claim, whose tree the entrypoint copies
into every server's whole working directory on start -- for files that do not
belong under `plugins/`, such as a Sponge configuration. See
[files from a volume](../guides/plugins-from-a-volume.md#files-from-a-volume).

### `--allow-mount-volumes`

Default: `false`

Lets a group's `spec.mounts` name a `PersistentVolumeClaim` rather than only a
`ConfigMap` or a `Secret`. Split out of `--allow-plugin-volumes` in 0.2.x, so
that turning on plugin claims does not also turn on arbitrary claim mounts.
See [mounts and files](../guides/mounts-and-files.md).
