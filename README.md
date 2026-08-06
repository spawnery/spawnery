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

In Entwicklung — es gibt noch keinen lauffähigen Code. Der Entwurf liegt unter
[`docs/superpowers/specs/`](docs/superpowers/specs/).
