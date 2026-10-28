# Voice chat through the proxies

Simple Voice Chat (SVC) sends audio over UDP straight to a plugin on the
backend, which a proxy in front cannot pass along, and with several proxy
replicas SVC's own Velocity plugin only works for the one that holds the game
connection. Spawnery's Velocity agent relays voice itself, so any proxy routes
any player's packets, and you bring only SVC's Paper plugin.

Turn it on for the network, and every `ProxyGroup` in it relays:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft
spec:
  voice:
    backendPort: 24454   # where SVC listens on the backends; this is the default
```

`backendPort` is read, not written. Set the same number in the backends'
`voicechat-server.properties`. Switching voice on rolls the network's proxies
once; backend pods do not change.

Proxies need an image that carries this agent release. The `Voice` condition
is computed from what the proxies report about their relay, so a proxy on an
older image cannot confirm it: the condition reads `Unknown`, not `Relaying`,
until the proxies are rolled to an image that carries this release.

If a group's load balancer cannot carry UDP or mixed protocols, set
`spec.voice.expose` on that group before you turn voice on in the network,
because turning it on adds a UDP port to the group's LoadBalancer Service.

Add a `spec.voice.expose` to a group only when its game entry point cannot
carry UDP, see below:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  expose:
    type: LoadBalancer
    loadBalancer: {}
  voice:
    expose:
      type: HostPort
      hostPort:
        port: 25565
```

## How voice leaves the group

Without `voice.expose`, voice takes the game's path with the same port number,
as UDP:

| `spec.expose.type` | What is added |
|---|---|
| `LoadBalancer` | a UDP port 25565 on the group's Service, beside the TCP one (mixed-protocol Services are GA since Kubernetes 1.26) |
| `NodePort` | a UDP port on the group's Service with the same `nodePort` number as the TCP one |
| `HostPort` | a UDP `hostPort` with the same number on the proxy container |
| `ClusterIP` | a UDP port 25565 on the group's Service; the entry point in front of it has to forward UDP on the same host and port |

With `voice.expose`, voice gets its own way out: a Service `<group>-voice`
carrying only the UDP port, or for `HostPort` a UDP `hostPort` and no Service.
Use it for a cloud load balancer without UDP, a TCP-only tunnel, or an ingress
controller with only a TCP entry point. An entry point that carries only TCP
carries no voice, and this is the way around it. A `<group>-voice` Service the
group does not own is never touched.

## The two plugins

Bring the SVC Paper plugin to the backends through `extraPlugins`. Do not
install SVC's Velocity plugin: the group reports `Voice=False` with reason
`VoicePluginConflict` and the relay stays off. Its licence forbids shipping it
in an image, which is why you bring it.

## Who receives a player's voice

A player's replies go to one UDP source at a time. When two sources send for
the same player UUID, the first one keeps the replies until it has been silent
for 3 seconds, and then the other takes them.

Where the game connection and the voice packets reach the proxy with the
client's own source address, the operator tells the proxy so, and the relay
prefers the source whose IP is the IP of the player's game connection (an
IPv4-mapped IPv6 address counts as its IPv4 address). That source takes the
replies at once, and a source elsewhere takes them back only after the matching
one has been silent for 15 seconds. The operator decides by strategy:

| Strategy | Client IP kept |
|---|---|
| `LoadBalancer` with `externalTrafficPolicy` `Local` (the default) | yes |
| `LoadBalancer` with `externalTrafficPolicy` `Cluster` | no |
| `NodePort` (always `Local`) | yes |
| `HostPort` | yes (assumes the CNI keeps the source address of a host port) |
| `ClusterIP` | no, something else in front |

With `spec.voice.expose`, both the game's strategy and voice's have to keep the
client IP. The rule is a preference where it applies, and the first sender
wins elsewhere: under `Cluster` policy and behind `ClusterIP`; for a sender on
the same public IP as the player (household, campus, CGNAT, VPN exit); and for
a player whose game connection is on another proxy replica, which has no
address to compare. An operator older than this release does not tell the
proxy, so there the first sender always wins. [Known
issues](../reference/known-issues.md) has the details.

## Checking it

```bash
kubectl get proxygroup -n minecraft -o wide
```

The `VOICE` column shows `status.voiceAddress`, which stays empty until a proxy
is ready. The `Voice` condition says why a group is not relaying:

| Reason | Meaning |
|---|---|
| `Relaying` | every proxy of the group reports its relay bound and running (`True`) |
| `RelayNotReported` | `Unknown`: a proxy has not reported a running relay, because its image is older than this release or its relay has not started yet, or no proxy has reported yet; the group's phase is not `Degraded` |
| `RelayBindFailed` | a proxy last reported that it could not bind the relay's UDP port 25565; it retries while its stream to the operator is up, and the condition follows its next report |
| `VoiceOffInNetwork` | the group has `spec.voice`, the Network does not |
| `VoicePluginConflict` | a proxy has SVC's Velocity plugin loaded |
| `VoiceServiceNotOwnedByGroup` | something else holds the name `<group>-voice` |
| `VoiceServiceRejected` | the API server refused the voice Service |

While the condition is `False` the group's phase is `Degraded`. Players can
still join, since only voice is affected.

While people talk, `spawnery_voice_packets_total{namespace,group,reason="relayed"}` rises.
The other reasons count what the relay dropped: `not_svc`, `unknown_player`,
`unknown_server`, `unknown_version` and `ping`.

Every field is in the [custom resource reference](../reference/crds.md#proxygroup).
