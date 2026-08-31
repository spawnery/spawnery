# Environment variables, and the one way to reach the JVM

A `ServerGroup` and a `ProxyGroup` each take `spec.env`, an ordinary list of
Kubernetes `EnvVar`s that is appended to the ones the operator sets:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: bingo-solo
spec:
  # ...
  env:
    - name: JAVA_TOOL_OPTIONS
      value: "-Dgame.amountOfTeams=0"
```

`valueFrom` works too — a `secretKeyRef` is the right way to hand a plugin an
API token, and it is no wider a door than `spec.mounts` already is: a namespace
is one trust domain, and anybody who can write a group can already mount any
Secret in it.

## Why this is how you set a JVM flag

Neither entrypoint takes arguments from any spec. Both build their own `java`
command line — `MaxRAMPercentage`, G1 tuning, `AlwaysPreTouch` unless the
container is unbounded — and `exec` it, so that the JVM is PID 1 and receives
`SIGTERM` directly. Threading a per-group argument list through that would mean
a group could replace those flags, and the first thing anybody would replace is
the memory sizing that the group's own `resources.limits.memory` is supposed to
govern.

`JAVA_TOOL_OPTIONS` is the seam the JVM itself offers, and it has the
precedence this needs. **Measured on OpenJDK 21.0.12:**

| | `JAVA_TOOL_OPTIONS` | command line | what the process got |
|---|---|---|---|
| `-Dfoo` | `fromenv` | — | `fromenv` |
| `-Dfoo` | `fromenv` | `fromcmd` | `fromcmd` |
| `-Xmx` | `100m` | `200m` | 200 MB |

So a group adds what the entrypoint does not set, and cannot displace what it
does. The JVM prints `Picked up JAVA_TOOL_OPTIONS: …` on stderr at every start,
which is worth knowing before somebody files it as a warning.

It carries **JVM** options only. A Paper or Velocity *program* argument —
`--world-dir`, `--nogui` — is not reachable this way and is not reachable at
all; those are the entrypoint's.

## The reserved prefix

A name may not begin with `SPAWNERY_`. That prefix is the operator's: it writes
`SPAWNERY_NETWORK`, `SPAWNERY_GROUP`, `SPAWNERY_SERVER` or `SPAWNERY_PROXY`,
the agent's endpoint, and for a proxy its player limit and fallback groups. The
agent reads them to know what it is and whom to call.

Kubernetes does not refuse a duplicate name in a container's env list — it
keeps both entries and the last one wins. A group shadowing `SPAWNERY_GROUP`
would therefore be admitted, and `kubectl describe pod` would print both values
with nothing saying which one the process read. A CEL rule on the CRD refuses
the prefix at admission instead, so the error lands on the object somebody just
wrote:

```
The ServerGroup "lobby" is invalid: spec.env: Invalid value: "array":
the SPAWNERY_ prefix is reserved for the environment variables the
operator sets itself
```

The prefix is reserved whole rather than the individual names being denied, so
a variable added in a later release cannot collide with one an installation
already set. It also covers `SPAWNERY_PLUGIN_SOURCE` and
`SPAWNERY_CGROUP_ROOT`, which exist so the image tests can point them at a
temporary directory and would break a start if a group moved them.

The list is a map keyed by name, so the same name twice is refused as well.

## Editing it replaces the group's servers

`spec.env` shapes the pod, so it is part of `podspec.DesiredServerHash` and
`DesiredProxyHash`. Changing it makes every server of the group stale and rolls
them exactly the way an image bump does, through `maxUnavailable` and the cold
start. Nobody is kicked; it still costs a changeover.

That is the opposite of [`extraPlugins`](plugins.md), whose contents reach no
hash at all. The difference is not a preference: the operator holds a claim
name and cannot digest a filesystem, while an env list it renders itself it
can. If you want a value you can change without rolling anything, it belongs on
the volume or in a config the plugin re-reads — not here.
