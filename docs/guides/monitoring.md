# Monitoring

The operator serves Prometheus metrics on port 8080: its own health (agent
connections, certificates, changeovers) and a picture of every network it
runs: players, groups, servers and proxies. The full list is in
[metrics and alerts](../reference/metrics-and-alerts.md).

## Turning it on

Both need the Prometheus Operator's CRDs or Grafana's dashboard sidecar, so
both are off by default:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: monitoring      # whatever your Prometheus selects on
  dashboard:
    enabled: true
    labels:
      grafana_dashboard: "1"   # the sidecar's label (kube-prometheus-stack default)
    annotations:
      grafana_folder: Minecraft
```

## Running more than one operator replica

The chart runs two operator replicas (`operator.replicas`, default 2). One is
the leader and does all the work. The other is a standby with warm caches,
and it takes over when the leader's lease is released or runs out. The agent
Service `spawnery-operator` has no selector: the leader writes its own address
into the EndpointSlices `spawnery-operator-agent-ipv4` and `-ipv6`, so agents
reach only the leader. A standby is `Ready` and is not in those slices.

The standby is there for the loss of a node. Measured on a four-node kind
cluster, a planned handover (a rollout, or deleting the leader) is served by
the next leader after 5.8 to 7.3 s, and a killed leader pod after 15.8 to
18.4 s. With the leader's node lost, the next leader served after 15.6 to
17.6 s and every agent was back by about 21 s, inside the reconnect grace: in
5 of 5 runs no server left `Ready`. Agents ping every 15 s and give up after
5 s, so a dead leader is noticed in about 20 s; a reconnect not answered
within 10 s is retired and redialled.

Those node-loss figures need game and proxy images with the agent of the
release that brings standby replicas, since the 15 s keepalive and the 10 s
reconnect bound are in the agent. The agent ships in the image a group names,
so upgrading the operator rolls no server (`DesiredServerHash` does not
move). Until its group moves to a new image, or rolls for another reason, a
server keeps its old agent, which pings only after 45 s of silence and waits
20 s for the answer.

It costs one more operator's memory (the chart requests 128Mi and limits 256Mi
per replica) and, per rollout, one or two agent reconnects: the old leader
hands over, and the agents redial the new one. The Deployment rolls with
`maxSurge: 1` and `maxUnavailable: 0`, a PodDisruptionBudget with
`maxUnavailable: 1` applies when `replicas` is above 1, and the replicas are
spread over nodes softly (`operator.topologySpreadConstraints` replaces that).
A new pod is Ready only once its caches have synced, so the rollout stops an
old pod only when its replacement could take over. Set `operator.replicas: 1`
for the old shape. With `operator.leaderElect: false` the chart refuses more
than one replica and recreates the pod instead of rolling it, since without a
Lease two operators would briefly run side by side.

The ServiceMonitor scrapes every replica through the separate Service
`spawnery-operator-metrics`, so a standby's series are present too. The alert
`SpawneryNoLeader` fires when no replica holds the lease.

### Finding the leader

```bash
kubectl -n spawnery-system get lease spawnery-operator.spawnery.cloud \
  -o jsonpath='{.spec.holderIdentity}'
```

The pod name is the part before `_`. Only the leader logs reconciliation, so
`kubectl logs deployment/spawnery-operator` picks an arbitrary pod and may
show a standby's quiet log. Name the pod:

```bash
POD=$(kubectl -n spawnery-system get lease spawnery-operator.spawnery.cloud \
  -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1)
kubectl -n spawnery-system logs "$POD"
```

The leader reads its Lease every 2 s. When the Lease names another replica,
is missing, or may not be read, the leader's pod exits and Kubernetes restarts
it. A read that times out or fails on the network only leaves the lease
unconfirmed. While it is unconfirmed, the leader holds back taking a server
out of `Ready` on what the agents reported.

### Game and proxy images must not move ahead of the operator

Agents built with the 15 s keepalive ping more often than an operator from
before standby replicas accepts (it enforces a 30 s minimum and answers 15 s
pings with a GOAWAY). Upgrade the operator first. If an agent does get ahead,
grpc-java backs its keepalive off after the `too_many_pings` GOAWAY, so the
operator log is noisy; it does not loop.

## What the network metrics are

They are computed on every scrape from what the operator holds at that
moment, so a server that is gone leaves no series behind and a round-based
group does not pile up series for every server it ever had.

- **Players:** per network (on its proxies), per group, per server, per proxy.
- **Groups:** servers, ready servers, free playable seats.
- **Servers:** players, slots, effective playable slots, one-minute TPS, mean
  tick time, JVM heap in use and its maximum, and the phase
  (`spawnery_server_phase{phase=…} 1`).
- **Proxies:** players and JVM heap.

A server whose agent has not reported yet, or whose agent is gone (crashed,
restarting) or late, carries its phase and nothing else: a "lowest TPS"
panel is not dragged to 0 by a server that is still starting, and a crashed
one does not keep its last figures as a flat line. An agent older than the heap fields reports no heap, and the heap
series are then absent rather than 0.

CPU and container memory are not exported here: the kubelet's cAdvisor
series already have them per pod, and a server's `server` label is its pod
name. The dashboard joins them on `namespace` and `server` with
`label_replace(…, "server", "$1", "pod", "(.*)")`. That needs the network's
namespace on the operator's series as `namespace`, so the chart's
ServiceMonitor sets `honorLabels: true`; a hand-written scrape config needs
the equivalent, or the label arrives as `exported_namespace`.

## The dashboard

`Spawnery network`, with a data source, one network at a time (two networks
usually share group names, which would merge their rows) and a group
selector:

- **Network:** players, servers ready and total, free seats, proxies, lowest
  TPS; players per group over time.
- **Groups:** players, free seats, servers, worst TPS and MSPT per group.
- **Servers:** one row per server with group, phase, players / playable /
  slots, TPS, MSPT, heap used and max, container memory and CPU, node.
- **Health over time:** lowest TPS and highest MSPT per group, heap and
  container memory per server, proxy players and heap.

## World sync

With `worldSync.enabled`, the same switches also cover the node agents: a
PodMonitor, the `spawnery-worldsync` alerts and the `Spawnery world sync`
dashboard. See
[worlds in an object store](object-store-worlds.md#watching-it).
