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
