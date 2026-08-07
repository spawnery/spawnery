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
Readiness-Verlust und der Verwaisten-Abgleich. Es gibt noch keine Basis-Images
und keine Proxy-Schicht — ein Spieler kann sich also noch nicht verbinden; das
ist Meilenstein 3.

Der Entwurf liegt unter [`docs/superpowers/specs/`](docs/superpowers/specs/),
der Plan unter [`docs/superpowers/plans/`](docs/superpowers/plans/).

## Entwicklung

```bash
nix develop            # Go, controller-gen, envtest-Assets, kubectl, k3d
make test              # Unit- und envtest-Tests
make build             # bin/spawnery-operator
```

### Lokal gegen k3d ausprobieren

Diese Schritte brauchen eine Container-Laufzeit (Docker oder Podman) für k3d.
Die Entwicklungsumgebung, in der Meilenstein 1 gebaut wurde, hatte keine —
deshalb ist dieser Ablauf hier **nicht** in CI oder sonst irgendwo automatisiert
ausgeführt worden. Verdrahtung und Zustandsmaschine sind stattdessen mit einem
echten, laufenden Manager gegen die envtest-Kontrollebene geprüft (siehe
`internal/controller/setup_test.go`); was nur mit einem echten Kubelet
beobachtbar ist — dass der Pod mangels Basis-Image mit `ErrImagePull` hängen
bleibt — ist ungeprüft.

```bash
nix develop -c k3d cluster create spawnery-dev --agents 1
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false &
sleep 20
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

Erwartet:

- `network production` mit `Accepted=True`,
- `servergroup lobby` mit `REPLICAS 1`,
- ein `server lobby-xxxx` in Phase `Pending` oder `Starting`,
- ein Pod `lobby-xxxx`, der das Image nicht ziehen kann (`ErrImagePull`) — das
  Basis-Image entsteht erst in Meilenstein 2. Genau das ist der erwartete
  Endstand von Meilenstein 1.

Danach aufräumen:

```bash
kill %1
nix develop -c k3d cluster delete spawnery-dev
```
