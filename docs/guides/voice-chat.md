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

## Checking it

```bash
kubectl get proxygroup -n minecraft -o wide
```

The `VOICE` column shows `status.voiceAddress`, which stays empty until a proxy
is ready. The `Voice` condition says why a group is not relaying:

| Reason | Meaning |
|---|---|
| `Relaying` | the group's proxies relay voice |
| `VoiceOffInNetwork` | the group has `spec.voice`, the Network does not |
| `VoicePluginConflict` | a proxy has SVC's Velocity plugin loaded |
| `VoiceServiceNotOwnedByGroup` | something else holds the name `<group>-voice` |
| `VoiceServiceRejected` | the API server refused the voice Service |

While the condition is `False` the group's phase is `Degraded`. Players can
still join, since only voice is affected.

While people talk, `spawnery_voice_packets_total{group,reason="relayed"}` rises.
The other reasons count what the relay dropped: `not_svc`, `unknown_player`,
`unknown_server`, `unknown_version` and `ping`.

Every field is in the [custom resource reference](../reference/crds.md#proxygroup).
