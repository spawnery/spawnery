# Voice chat through the proxies

**Status:** design, decided 2026-10-12
**Date:** 2026-10-12

## 1. What goes wrong today

Simple Voice Chat (SVC) is a client mod plus a server plugin. The server plugin
runs its own UDP server beside the game; the client learns where to send UDP
from a `voicechat:secret` plugin message the server sends over the game
connection, and from then on talks to it directly. Behind a proxy the client
cannot reach the backend, so SVC ships a Velocity plugin that relays.

Read from SVC's source at `2.6.24+26.3` (compatibility version 20):

- Every UDP packet is a magic byte `0xFF`, the player's UUID in cleartext, then
  an AES-128-GCM payload keyed with a per-player secret. A relay can route on
  the UUID without decrypting anything.
- SVC's Velocity plugin reads the backend's `voicechat:secret` as it passes
  through and records the backend's voice address keyed by player. It then
  rewrites the port and host in the packet to point at the proxy. That record
  lives in the memory of the proxy that carries the player's game connection
  and nowhere else (`common-proxy/.../VoiceProxyServer.java`,
  `getOrCreateBridge`).
- The backend fixes a player's UDP source address at authentication and
  drops packets from any other address. Once nothing has answered its
  keepalive for ten intervals (10 s by default), it drops the connection
  and sends a fresh secret over the game connection, and the client
  reconnects on its own (`bukkit/.../voice/server/Server.java`).

Spawnery runs several proxy replicas behind one address. A `LoadBalancer`, a
`NodePort` reached on any node, DNS round robin over `HostPort` nodes, an
external UDP gateway in front of a `ClusterIP`: each spreads a client's TCP
and UDP flows independently. The UDP flow lands on the proxy holding the game
connection about one time in `replicas`. Everywhere else SVC's plugin has no
record and drops the packets silently.

A per-pod voice address would restore the affinity, but only `HostPort` and
`NodePort` can give one cheaply. A `LoadBalancer` would need an address per
pod, and a `ClusterIP` group's entry point is outside the operator's reach.

## 2. The idea

Every proxy already knows where every player is: the `NetworkState` each agent
receives carries every player in the namespace with the backend they are on
([network boundaries](../../explanation/network-boundaries.md)). So spawnery's
own Velocity agent runs the relay, and any proxy can route any player's packet,
whichever proxy their UDP flow lands on.

SVC's Velocity plugin is therefore not used and must not be installed on a
spawnery proxy. The backend side is unchanged: operators bring SVC's Paper
plugin themselves through `extraPlugins`. Its licence is "All Rights
Reserved", so no spawnery image can ship it.

## 3. The shape

```yaml
kind: Network
spec:
  voice:                  # new, optional; present = voice on in this network
    backendPort: 24454    # default 24454, SVC's own default
```

```yaml
kind: ProxyGroup
spec:
  expose:
    type: LoadBalancer
    loadBalancer: {}
  voice:                  # new, optional
    expose:               # the same ExposeSpec type as spec.expose
      type: HostPort
      hostPort:
        port: 25565
```

`Network.spec.voice` is the one switch. When it is set, every `ProxyGroup` of
the network relays, and the policies of section 6 open the backend port.
`backendPort` is where SVC listens on the backends. The operator reads it and
writes it nowhere: the backends' `voicechat-server.properties` stays the
operator's business, and the two have to agree.

There is no opt-out per group. A server group without voice is one that does
not install the plugin: its backends never send a secret, so no packet for
their players ever arrives. The UDP port the policy opens then has nothing
listening on it. A proxy group whose relay receives nothing costs nothing.

`ProxyGroup.spec.voice` is set on a group without `Network.spec.voice` →
`Voice=False`, reason `VoiceOffInNetwork`.

*Amended 2026-10-12:* The refusals of this section and section 5 sit on a
`Voice` condition of the group, not on `Degraded`, which the controller already
rewrites on every pass. While `Voice` is `False` the group's phase is
`Degraded`. A third reason, `VoiceServiceNotOwnedByGroup`, covers a foreign
Service at `<group>-voice`, and a fourth, `VoiceServiceRejected`, an error from
the API server on the voice Service. Neither stops the game: the group keeps
reconciling and only voice is affected.

## 4. How voice reaches the proxies

**Without `voice.expose`, voice takes the game's path with the same port
number, as UDP:**

| `spec.expose.type` | what is added |
|---|---|
| `LoadBalancer` | a UDP port 25565 on the group's Service, beside the TCP one (mixed-protocol LoadBalancer Services are GA since Kubernetes 1.26) |
| `NodePort` | a UDP port on the group's Service with the same `nodePort` number as the TCP one |
| `HostPort` | a UDP `hostPort` with the same number on the proxy container |
| `ClusterIP` | a UDP port 25565 on the group's Service; the entry point in front of it has to forward UDP on the same host and port |

**With `voice.expose`, voice gets its own way out:** a Service
`<group>-voice` carrying only the UDP port, of the given type, or for
`HostPort` a UDP `hostPort` and no Service. This is for an entry point that
cannot carry UDP: a cloud load balancer without UDP, a TCP-only tunnel, an
ingress controller with only a TCP entry point. `voice.expose.hostPort.port`
passes `HostPortRefusal` like the game's, and `NodePort` keeps
`externalTrafficPolicy: Local`.

Inside the pod the relay listens on UDP 25565, a container port named
`voice`. Velocity's own query feature listens on UDP too. While voice is on,
the renderer refuses a `velocity.toml` overlay that enables query on that port.

**What the client is told** is the voice endpoint. The operator computes it
per group, sends it to the group's proxies over the agent channel (a new field
in what `proxyreg` sends, so a load balancer address that arrives late reaches
running proxies without a roll), and publishes it as `status.voiceAddress`.
It takes one of three forms:

- **Port only.** This is the form whenever voice takes the game's path. The
  client takes the host it typed to join. That holds under DNS round robin over
  `HostPort` nodes as well, since whichever proxy the name resolves to can
  route the player.
- **Own node, plus a port.** For a `NodePort` or `HostPort` override. The
  proxy holding the game connection puts in its own node's IP (downward API
  `status.hostIP`, as `SPAWNERY_HOST_IP`), because under `Local` that node is
  the one certain to have a proxy behind the port.
- **A fixed host and port.** For a `LoadBalancer` override, the voice Service's
  first ingress IP or hostname. For a `ClusterIP` override,
  `voice.expose.clusterIP.address`, echoed as written like the game's.

`status.voiceAddress` follows `status.address`'s rule: empty until a proxy is
ready and the strategy has produced an endpoint. For the port-only form it
shows the game address's host with the voice port, so an observer reads
something dialable. A `Voice` printcolumn is added at `priority=1`, which
`kubectl get -o wide` shows.

**Rolling.** The voice port, the `hostPort`, and `SPAWNERY_HOST_IP` enter the
proxy pod only while voice is on. The proxy hash golden stays where it is, so
upgrading the operator rolls no proxy. Switching voice on rolls the network's
proxies once. Backend pods do not change at all: the relay reaches a backend
by pod IP, which needs no declared container port.

## 5. The relay

Part of the Velocity agent, started only when the operator's sync says voice
is on.

**Refusals at start.** If Velocity has a plugin with id `voicechat` loaded,
the relay does not start. The agent reports it, and the group's `Voice`
condition goes `False` with reason `VoicePluginConflict`. Two relays on one
port would fail with nothing to point at the cause.

**A packet from a client:**

1. The packet is dropped if it is shorter than 17 bytes or its first byte is
   not `0xFF`.
2. If the UUID is SVC's fixed ping UUID, the packet is dropped and counted as
   `ping` (amended 2026-10-12). The reply format is not verified, and a player
   does not need the answer to talk.
3. Otherwise the proxy picks the backend:
   - If the player is connected through this proxy, it uses their current
     server as Velocity sees it. This is correct the moment a server switch
     happens.
   - Otherwise it uses the player's server from `NetworkState`.
   - The address is that server's registered host (its pod IP) with
     `backendPort`.
   - An unknown player or an unknown server means the packet is dropped and
     counted.

The relay binds dual-stack when its address is the wildcard (amended
2026-10-12).

**A bridge per player.** Each player gets one ephemeral UDP socket towards the
backend, so the backend sees a stable source per player. Replies arriving on
that socket go back to the client's address through the relay's public
socket. The sockets are non-blocking with one selector, not one thread per
player as in SVC's proxy.

- *The backend changes.* The bridge is closed and a new one opened. The new
  backend demands a fresh authentication anyway.
- *The client's source address changes.* The bridge adopts the new address
  only once the old one has been silent for 3 s. Until then, packets from the
  new address are still forwarded upstream, but replies keep going to the old
  one. The client echoes a keepalive every second, so someone who knows a
  player's UUID (which is public) cannot take the reply path of a player who
  is active. A NAT rebind costs 3 s instead of the backend's 10-second
  timeout.
- *Idle.* After 15 s without traffic in either direction the bridge is
  removed. That is longer than SVC's 10 s timeout, so a bridge never goes away
  under a connection the backend still considers alive.

**Rewriting the secret.** This is done by the proxy holding the game
connection, the only one that sees the packet:

- It records the client's compatibility version from
  `voicechat:request_secret`.
- In the backend's `voicechat:secret` it rewrites `serverPort` (an int at
  offset 16) and `voiceHost` (a string at offset 54, a VarInt length followed
  by UTF-8, followed by the one-byte `allowRecording`) to the voice endpoint.
  The port-only form writes an empty `voiceHost` and the port into
  `serverPort`.
- The layout is taken from `bukkit/.../net/SecretPacket.java` at
  compatibility version 20, and a test holds that version's bytes.
- For any other version the packet is patched only if it parses exactly to
  that layout: the string fits, and exactly one byte of 0 or 1 follows it.
  Otherwise it passes through untouched. Either way an unknown version is
  counted and logged once per player, so that a SVC update on the backends
  shows up before players report it.

**What failures cost:**

- *A proxy rolls or dies.* The UDP flow moves to another proxy, which can
  route at once. The backend sees a new source and drops it until its 10 s
  timeout sends a fresh secret, and voice comes back without anyone acting.
- *A server switch while the UDP flow is on a proxy other than the game
  connection's.* That proxy routes to the old backend until `NetworkState`
  shows the move. The old backend ignores the packets, and SVC's timeout
  recovers in the worst case. How long the update takes to reach the other
  proxy is measured on a real cluster (section 7) and recorded here, not
  assumed.

**Counting.** The agent's report carries relayed and dropped packets by
reason (`relayed`, `not_svc`, `unknown_player`, `unknown_server`,
`unknown_version`, `ping`). The operator exports them as
`spawnery_voice_packets_total{group,reason}`. No label carries a player.

## 6. NetworkPolicies

- The per-`Network` backend policy (`BuildNetworkPolicy`) admits UDP
  `backendPort` from the network's proxies, beside 25565/TCP.
- Each group's proxy egress policy (`BuildProxyNetworkPolicy`) admits UDP
  `backendPort` to the network's backends.
- Ingress to proxies is unchanged: no policy selects proxy pods, so UDP 25565
  arrives with no rule added.

Both additions exist only while `Network.spec.voice` is set.

## 7. Testing

**Kotlin (agent):**
- Packet framing.
- The secret patch, against bytes built field by field in version 20's layout,
  citing SVC's `SecretPacket`. This covers the shape check on a packet with a
  longer tail and on a truncated one.
- Routing over real UDP sockets to two fake backends: a player on this proxy,
  a player known only from `NetworkState`, and a backend switch.
- The 3 s rule and the idle removal, under a fake clock.
- The refusal when a `voicechat` plugin is loaded.

**envtest:**
- The Service ports for all four strategies, with and without
  `voice.expose`. This includes a TCP and a UDP port sharing one `nodePort`
  number, which the API server must accept.
- The policies, `status.voiceAddress`, and both `Degraded` reasons.
- The proxy hash golden unchanged for a group without voice.

**Cross-proxy proof (amended 2026-10-12):** `make e2e` starts no game image,
so this runs as a phase of `hack/agent-test.sh` against a real Velocity image
(`AGENT_TEST_ONLY=voice` runs it alone):
- A real Velocity proxy that holds no player, and `spawnery-stubop`, whose
  `NetworkState` puts one player on a backend.
- `spawnery-voiceprobe` sends a frame carrying that player's UUID to the proxy
  and checks the echo from a stub backend. The claim holds when it comes back:
  a proxy routes a player it only knows from the mirror.

**On a real cluster**, which no test here can cover:
- Two proxies behind a `LoadBalancer`, the SVC plugin on a lobby group, and
  two clients with the SVC mod standing near each other.
- To measure: after a server switch, how long the other proxy keeps routing to
  the old backend. The figure goes into section 5.
- To see:
  - both hear each other;
  - after `/server` to a second group with the plugin, they hear each other
    there as well;
  - during a proxy roll, voice is gone for at most about 10 s and comes back
    without a rejoin;
  - `spawnery_voice_packets_total{reason="relayed"}` rises on both proxies
    whenever the flows were spread across them.

## 8. Docs and release

`docs/guides/voice-chat.md`, short, carrying:
- the two YAML blocks and the table from section 4;
- that the SVC jar is brought along and SVC's Velocity plugin stays off;
- the limit: an entry point that carries only TCP carries no voice, and
  `voice.expose` is the way around it.

The reference pages are regenerated by `make manifests`.

New CRD fields make this a **minor** step. `imageVersion` moves (agent),
`operatorVersion` moves, and so does the chart.

## 9. Not in scope

- Writing SVC's configuration on the backends.
- A per-group opt-out (section 3 says why none is needed).
- A TCP fallback for voice: SVC has none.
- BungeeCord or Waterfall proxies.
