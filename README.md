# Spawnery

Ein Kubernetes-natives Cloud-System für Minecraft-Netzwerke.

Spawnery betreibt Paper-Gameserver hinter einer Velocity-Proxy-Schicht auf
Kubernetes — dynamisch skalierende Minigame- und Lobby-Gruppen ebenso wie
persistente Survival-Welten. Zielplattform ist RKE2 auf Bare Metal, ohne dass
andere Distributionen ausgeschlossen sind.

Server werden in Gruppen beschrieben, nicht in Pods:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
spec:
  networkRef: { name: production }
  type: Ephemeral
  maxPlayers: 100
  scaling:
    minReplicas: 1
    maxReplicas: 10
    spareSlots: 40      # so viele freie Plätze hält Spawnery vor
```

Skaliert wird nach freien Spielerplätzen statt nach CPU, und ein Server mit
Spielern wird niemals gelöscht: Vor dem Stopp werden die Spieler über den Proxy
auf einen Fallback umgezogen.

## Status

In Entwicklung. Meilenstein 1 ist umgesetzt: die vier CRDs, der Operator mit
Network-, ServerGroup- und Server-Controller, die Zustandsmaschine inklusive
Readiness-Verlust und der Verwaisten-Abgleich.

Meilenstein 2a ist umgesetzt: der Agentkanal. Ein gRPC-Dienst im
Operator-Prozess nimmt TLS-Verbindungen von Gameserver-Pods entgegen, weist
sie über einen pod-gebundenen ServiceAccount-Token zurück, und die Registry
dahinter füllt das zweistufige Ready-Gate und `status.players`, die
Meilenstein 1 unverdrahtet gelassen hatte. Der Kanal ist Ende-zu-Ende in
envtest geprüft — ein Testagent bringt einen `Server` mit grüner
Pod-Readiness bis in Phase `Ready` — aber es spricht noch kein echter Agent
mit ihm: Ein Spieler kann sich weiterhin nicht verbinden, weil zwei Dinge
fehlen: die Velocity-Proxy-Schicht (Meilenstein 3, `ProxySession` antwortet
bis dahin nur `Unimplemented`) und die Basis-Images samt Kotlin-Agent
(Meilenstein 2b) — ohne Image bleibt ein Pod weiter in `ErrImagePull`
hängen, egal wie gut der Kanal geprüft ist, der auf ihn wartet.

Details zu dem, was 2a bewusst offenlässt — CA-Rotation, die Pflicht des
Kotlin-Agents zum überlappenden Neuverbinden, der fehlende
`spawnery-proxy`-ServiceAccount — stehen in
[`docs/bekannte-punkte.md`](docs/bekannte-punkte.md).

Der Entwurf liegt unter [`docs/superpowers/specs/`](docs/superpowers/specs/),
der Plan unter [`docs/superpowers/plans/`](docs/superpowers/plans/).

Wer Meilenstein 2b beginnt, fängt bei
[`docs/uebergabe-meilenstein-2b.md`](docs/uebergabe-meilenstein-2b.md) an: dort
steht, was der Kanal aus 2a einem Agent bereitstellt, welche Pfade und Binaries
ein Basis-Image mitbringen muss, und was die Entwicklungsumgebung dafür
zusätzlich braucht.

## Entwicklung

```bash
nix develop            # Go, controller-gen, envtest-Assets, kubectl, k3d
make test              # Unit- und envtest-Tests
make build             # bin/spawnery-operator
```

`make proto` erzeugt den Go-Code unter `internal/agentpb` aus
`proto/spawnery/agent/v1alpha1/agent.proto` neu. Der generierte Code ist
eingecheckt wie `zz_generated.deepcopy.go` — nach einer Änderung an der
`.proto` `make proto` laufen lassen und den Diff mit committen, `make test`
regeneriert ihn nicht von selbst.

### Lokal gegen k3d ausprobieren

Diese Schritte brauchen eine Container-Laufzeit (Docker oder Podman) für k3d.
Die Entwicklungsumgebung, in der Meilenstein 1 gebaut wurde, hatte keine —
deshalb ist dieser Ablauf hier **nicht** in CI oder sonst irgendwo automatisiert
ausgeführt worden. Verdrahtung und Zustandsmaschine sind stattdessen mit einem
echten, laufenden Manager gegen die envtest-Kontrollebene geprüft (siehe
`internal/controller/setup_test.go`); was nur mit einem echten Kubelet
beobachtbar ist — dass der Pod mangels Basis-Image mit `ErrImagePull` hängen
bleibt — ist ungeprüft.

Der Operator läuft hier per `go run` außerhalb des Clusters, also ohne
`POD_NAMESPACE` aus der Downward API. `--operator-namespace` muss deshalb
explizit gesetzt sein — ohne das Flag verweigert der Prozess den Start (siehe
`validateAgentFlags`), weil das Serving-Zertifikat sonst die falschen SANs
trüge. Der Agentkanal selbst bleibt in diesem Ablauf ungenutzt: Der Operator
bootstrappt zwar die CA-ConfigMap und den `spawnery-server`-ServiceAccount im
Namespace, aber der Pod dialt `spawnery-operator.<ns>.svc:9443` — einen
Service, den dieser Ablauf nie anlegt, weil der Operator-Prozess nicht im
Cluster läuft und es keinen Endpunkt dorthin gäbe.

```bash
nix develop -c k3d cluster create spawnery-dev --agents 1
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false --operator-namespace minecraft &
sleep 45
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

Der erste Server kann gut eine halbe Minute auf sich warten lassen: Trifft die
ServerGroup ihr Netzwerk an, bevor der Network-Controller es angenommen hat,
versucht sie es erst nach `networkRetryInterval` (30 Sekunden) wieder. Deshalb
die 45 Sekunden oben.

Erwartet:

- `network production` mit `Accepted=True`,
- `servergroup lobby` mit `REPLICAS 1`,
- ein `server lobby-xxxx` in Phase `Pending` oder `Starting`,
- ein Pod `lobby-xxxx`, der das Image nicht ziehen kann (`ErrImagePull`) — das
  Basis-Image entsteht erst in Meilenstein 2b. Das ist unverändert der
  erwartete Endstand, auch nach Meilenstein 2a: Der Agentkanal bleibt hier
  ungenutzt (siehe oben), also ändert er nichts daran, dass der Pod nie
  startet.

Danach aufräumen:

```bash
kill %1
nix develop -c k3d cluster delete spawnery-dev
```
