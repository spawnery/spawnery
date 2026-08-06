# Design: Kubernetes-natives Cloud-System für Minecraft-Netzwerke

**Datum:** 2026-08-07
**Status:** Entwurf zur Freigabe
**Umfang dieses Dokuments:** Projekt 1 + 2 (Operator-Kern und Proxy-Integration)

## 1. Zweck und Zielgruppe

Ein Open-Source-Cloud-System, mit dem Minecraft-Netzwerke auf Kubernetes betrieben
werden — dynamische Minigame-Server ebenso wie persistente Survival-Welten, hinter
einer Velocity-Proxy-Schicht. Zielplattform ist RKE2 auf Bare Metal, ohne dass
andere Kubernetes-Distributionen ausgeschlossen werden.

Zielgruppe sind Betreiber von Minecraft-Netzwerken, nicht Platform-Engineers. Die
Bedienoberfläche denkt in Servergruppen, nicht in Pods.

### Warum ein neues Projekt

Der Markt ist real unbesetzt. CloudNet v4 und SimpleCloud v3, die beiden
verbreiteten Cloud-Systeme, sind nicht Kubernetes-basiert. Der einzige ernsthafte
K8s-Versuch aus diesem Lager, `theSimpleCloud/SimpleCloud-Kubernetes`, wurde am
2024-04-05 unfertig archiviert.

Das einzige reife Prior Art ist [Shulker](https://github.com/jeremylvln/Shulker):
ein Rust-Operator auf Agones-Basis mit dem CRD-Dreieck MinecraftCluster →
ProxyFleet → MinecraftServerFleet. Architektonisch lehrreich, aber:
MinecraftServerFleets sind ausdrücklich ephemer ohne persistenten Storage, es gibt
kein Konzept für Survival-Welten oder Lobbys mit Daten; das Projekt ist seit
Release v0.13.0 (2025-04-05) feature-stagnant mit einem einzigen Maintainer; und es
steht unter AGPL-3.0.

Die Differenzierung dieses Projekts ist damit konkret:

1. Persistente und ephemere Server als gleichwertige First-Class-Konzepte.
2. Keine Agones-Abhängigkeit — ein Operator statt zwei, kein SDK-Zwang im
   Serverprozess, kein enges Kubernetes-Versionsfenster.
3. Die vertraute Gruppen-UX der etablierten Systeme, auf Kubernetes übersetzt.
4. Permissive Lizenz und aktive Pflege.

**Lizenz-Hygiene:** Shulker ist AGPL-3.0. Sein Code wird nicht übernommen, nur
seine Architektur als Referenz gelesen. Gleiches gilt für andere Fremdprojekte:
Lizenz vor jeder Wiederverwendung prüfen.

### Warum kein Agones

Agones' Kernnutzen ist dynamisches hostPort-Brokering, damit Clients sich direkt
mit einem zugewiesenen Gameserver verbinden. Hinter einem Velocity-Proxy entfällt
das fast vollständig — nur die Proxies brauchen externe Erreichbarkeit. Was
tatsächlich gebraucht wird (Velocity-Registry, Spieler-Routing, PVC-Persistenz,
Template-Provisioning), liefert Agones nicht.

Dazu kommt: Persistente Server passen strukturell nicht ins Fleet-Modell, dessen
Rolling Updates GameServer ersetzen. Das Kern-Differenzierungsmerkmal würde also
ohnehin am Agones-Modell vorbei entstehen. Kostenseite wären ein zweiter Operator,
verpflichtende Admission-Webhooks, ein SDK-Sidecar und ein Drei-Minor-Versionsfenster
ohne Bare-Metal-Testabdeckung.

Übernommen werden trotzdem die guten Ideen: die explizite Zustandsmaschine als
Status-Subresource, der Schutz belegter Server vor Eviction, Health über
Standard-Probes.

### Warum nicht Control-Plane-first

Eine eigene API mit Datenbank als Source of Truth und Kubernetes nur als
Ausführungsschicht wäre die CloudNet-Architektur. Sie verschenkt genau das, was
Kubernetes-nativ wertvoll macht — deklarative CRDs, GitOps, kubectl, Reconciliation,
RBAC — und schafft ein zweites Konsistenzproblem zwischen Datenbank und
Clusterzustand. CLI, Dashboard und REST-API entstehen später als dünne Schicht über
den CRDs.

## 2. Projektschnitt

Der Gesamtumfang zerfällt in vier Projekte mit je eigener Spec:

| # | Projekt | Inhalt |
|---|---------|--------|
| 1 | Operator-Kern | CRDs, Reconciliation, Pod-Lifecycle, Ready-Gates, spielerbewusstes Drain, Expose-Strategien |
| 2 | Proxy-Integration | Velocity-Agent, Paper-Agent, gRPC-Kanal, Modern Forwarding, Fallback-Routing |
| 3 | Templates & Provisioning | Geschichtete Datei-Overlays, OCI-/S3-Quellen, Plugin-Downloads mit gepinnten Checksums, Backups, Image-Build-Pipeline |
| 4 | CLI & Dashboard | `mcctl`, Web-UI, Metrik-Pfad |

**Dieses Dokument spezifiziert Projekt 1 und 2 gemeinsam.** Sie sind nicht sinnvoll
trennbar: ohne Proxy-Integration ist der Operator nicht spielbar, ohne Operator hat
der Agent nichts zu registrieren.

### Erfolgskriterium

*Ein Spieler verbindet sich mit dem Netzwerk, landet auf einer Lobby, und beim
Skalieren, beim Rolling Update und beim Neustart einzelner Backend-Server verliert
niemand seine Verbindung.*

Die Zusicherung gilt ausdrücklich für den **Backend-Lifecycle**. Ein Proxy-Neustart
trennt bestehende Verbindungen nach Ablauf seines Drain-Fensters prinzipbedingt —
die Client-Verbindung endet am Proxy, und eine Session-Übergabe zwischen Proxies
setzt den auf später verschobenen proxyübergreifenden Spieler-State voraus.

### Nicht in dieser Version

Ausdrücklich verschoben, nicht vergessen: das geschichtete Template-System
(V1 nutzt einfache ConfigMap-, Secret- und PVC-Mounts), CLI und Dashboard, Redis und
proxyübergreifender Spieler-State, Matchmaking, Signs und NPCs, Permissions-Sync,
MOTD- und Tablist-Sync, Multi-Cluster, BungeeCord, Fabric und Forge, automatische
Backups, ein eigener Transfer-Befehl (`/play`), automatisch orchestrierte
Forwarding-Secret-Rotation, Laufzeit-Downloads von Plugins.

## 3. Architekturüberblick

```
                    ┌──────────────────────────────┐
   kubectl / GitOps │  CRs: Network, ProxyGroup,   │
   ────────────────▶│       ServerGroup, Server    │  (etcd = Source of Truth)
                    └───────────────┬──────────────┘
                                    │ watch / reconcile
                            ┌───────▼────────┐
                            │    Operator    │  Go, controller-runtime
                            │  (1 Replica,   │
                            │   Leader-Elect)│
                            └───┬────────┬───┘
                    Pods erzeugen│        │ gRPC über TLS (bidirektional)
                 ┌───────────────┘        └──────────────┐
                 │                                       │
        ┌────────▼────────┐                     ┌────────▼────────┐
        │  Proxy-Pods     │  Registry-Updates   │  Server-Pods    │
        │  Velocity       │◀───────────────────▶│  Paper          │
        │  + Velocity-    │                     │  + Paper-Agent  │
        │    Agent        │   Minecraft-Traffic │                 │
        │                 │────────────────────▶│                 │
        └────────▲────────┘                     └─────────────────┘
                 │ Expose: LoadBalancer | NodePort | HostPort
            Spieler
```

Der Operator ist ein einzelner Go-Prozess. Er reconciliert CRs zu Pods und hostet
den gRPC-Dienst, mit dem die In-Game-Agents sprechen. Es gibt keine zweite
Datenhaltung: hochfrequente Laufzeitdaten (Spielerzahlen) leben im Speicher des
Operators und werden nur gedrosselt in den CR-Status geschrieben.

## 4. API-Modell

**Arbeitsname:** `cloudsystem`. **API-Gruppe:** `minecraft.cloudsystem.dev`,
**Version:** `v1alpha1`. Alle Ressourcen sind namespaced.

Die Gruppenbezeichnung ist bewusst früh festgelegt, weil eine spätere Änderung
Konvertierungs-Webhooks erfordert. Eine Umbenennung des Projekts ist bis zum ersten
`v1alpha1`-Release möglich und zieht dann die Gruppe mit.

**Ein Namespace, ein Netzwerk.** Staging und Produktion gehören in getrennte
Namespaces. Der Grund ist die Netz-Isolation: NetworkPolicies selektieren über
Labels, und zwei Netzwerke im selben Namespace ließen sich zwar per Network-Label
trennen, aber jeder zusätzliche unverwaltete Pod im Namespace unterläuft die
Annahme. Der Operator lehnt ein zweites `Network` im selben Namespace mit einer
Condition ab.

### 4.1 Network

Die Wurzel-Ressource. Trägt das Forwarding-Secret und die Defaults, die
untergeordnete Ressourcen erben.

```yaml
apiVersion: minecraft.cloudsystem.dev/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret   # Key: "secret"
  defaults:
    minecraftVersion: "1.21.4"
    imagePullSecrets:
      - name: registry-credentials
    resources:
      requests: { cpu: "1", memory: "2Gi" }
      limits:   { memory: "2Gi" }
    scheduling:
      nodeSelector: { node-role/minecraft: "true" }
      tolerations: []
      affinity: {}
status:
  conditions: [...]
  proxyGroups: 1
  serverGroups: 3
  onlinePlayers: 42
```

`ProxyGroup` und `ServerGroup` referenzieren ihr Netzwerk über
`spec.networkRef.name` und erben `defaults`, können aber jedes Feld überschreiben.
Ressourcen ohne gültige Referenz werden nicht reconciled und melden das als
Condition.

### 4.2 ProxyGroup

Die Velocity-Schicht und der einzige von außen erreichbare Teil des Systems.

```yaml
apiVersion: minecraft.cloudsystem.dev/v1alpha1
kind: ProxyGroup
metadata:
  name: edge
  namespace: minecraft
spec:
  networkRef: { name: production }
  replicas: 2
  image: ghcr.io/<org>/velocity:3.4.0-<agentversion>
  resources: {...}
  scheduling: {...}                 # überschreibt Network.defaults.scheduling
  expose:
    type: LoadBalancer              # LoadBalancer | NodePort | HostPort
    loadBalancer:
      annotations: {}               # z.B. MetalLB-Pool-Auswahl
      externalTrafficPolicy: Local
    # nodePort: { port: 30565 }
    # hostPort: { port: 25565 }
  routing:
    fallbackGroups: ["lobby"]       # geordnete Try-Liste beim Join
  drain:
    timeoutSeconds: 300             # wie lange bestehende Sessions auslaufen dürfen
  config:
    playerLimit: 500
    motd: "§bMein Netzwerk"
status:
  phase: Ready
  readyReplicas: 2
  address: "203.0.113.10:25565"
  connectedPlayers: 42
  conditions: [...]
```

**Expose-Strategien.** Genau eine der drei ist aktiv; der Operator validiert per
CEL-Regel, dass der passende Unterblock gesetzt ist.

- `LoadBalancer` erzeugt einen Service `type: LoadBalancer`. Auf Bare Metal
  erfordert das MetalLB oder kube-vip; RKE2 bringt keinen aktiven
  LoadBalancer-Controller mit (ServiceLB ist opt-in und sollte deaktiviert
  bleiben). Default für `externalTrafficPolicy` ist `Local`, damit die Client-IP
  erhalten bleibt — Bans und Rate-Limits hängen daran.
- `NodePort` erzeugt einen Service `type: NodePort`. Ports außerhalb des
  Standardbereichs 30000–32767 erfordern eine Anpassung von
  `service-node-port-range` am API-Server; das dokumentieren wir, prüfen es aber
  nicht.
- `HostPort` bindet einen **festen** Port direkt auf den Nodes. Es gibt keinen
  eigenen Port-Allokator: die Konfliktvermeidung übernimmt der kube-scheduler
  nativ, der pro Node höchstens einen Pod mit demselben hostPort platziert.
  Faktisch läuft also höchstens eine Proxy-Replica pro Node; bleiben Replicas
  mangels freier Nodes `Pending`, meldet der Operator das als Condition. Die
  Funktionsfähigkeit hängt am CNI: Canal funktioniert, Cilium nur mit
  `kubeProxyReplacement` oder portmap-Chaining. Auf CIS-gehärteten RKE2-Clustern
  verbietet Pod Security `restricted` HostPorts — der Namespace braucht dann eine
  Ausnahme.

`hostNetwork` wird nicht angeboten. Der Latenzgewinn liegt im Bereich einer halben
Millisekunde und kostet Netzwerk-Isolation.

**Spielerinitiierte Wechsel zwischen Gruppen.** V1 bringt keinen eigenen
Transfer-Befehl mit. Velocitys eingebautes `/server` (Permission
`velocity.command.server`, standardmäßig für alle Spieler aktiv) ist damit der
einzige spielerinitiierte Weg zwischen Gruppen und bleibt bewusst offen. Drainende
Server sind darüber nicht erreichbar, weil sie vor dem Drain deregistriert werden,
und volle Server lehnt Paper selbst ab. Der Preis: Spieler können gezielt einzelne
Instanzen ansteuern und so die Lastverteilung umgehen. Ein `/play <gruppe>` mit
Gruppen-Policy im Velocity-Agent folgt in einem späteren Projekt.

### 4.3 ServerGroup

Die Abstraktion, in der Netzwerk-Admins denken. Das Feld `type` unterscheidet die
beiden Betriebsarten.

**Ephemer** — Minigames und Lobbys, Zustand geht beim Stopp verloren:

```yaml
apiVersion: minecraft.cloudsystem.dev/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft
spec:
  networkRef: { name: production }
  type: Ephemeral
  image: ghcr.io/<org>/paper:1.21.4-<agentversion>
  maxPlayers: 100
  resources: {...}
  scheduling: {...}
  mounts:                          # V1: einfache Mounts, kein Overlay-System
    - name: lobby-config
      configMap: { name: lobby-config }
      mountPath: /data/config
  scaling:
    minReplicas: 1
    maxReplicas: 10
    spareSlots: 40                 # freie Spielerplätze, die vorgehalten werden
    scaleDownStabilizationSeconds: 300
  update:
    maxUnavailable: 1              # gleichzeitig ersetzte veraltete Server
    maxStaleSeconds: 0             # 0 = veraltete Server nie aktiv leeren
  drain:
    timeoutSeconds: 60
  failedRetentionSeconds: 3600
status:
  phase: Ready                     # abgeleitet: Pending | Ready | Degraded
  replicas: 2
  readyReplicas: 2
  onlinePlayers: 42
  freeSlots: 158
  conditions: [...]
```

**Persistent** — Survival, Creative, Build-Server; die Welt überlebt Neustarts:

```yaml
spec:
  type: Persistent
  replicas: 1                      # meist 1, stabile Namen survival-0, survival-1, ...
  storage:
    size: 20Gi
    storageClassName: longhorn
    accessModes: [ReadWriteOnce]
  drain:
    timeoutSeconds: 120
  terminationGracePeriodSeconds: 300   # Zeit für den Weltspeicher
```

**Validierung per CEL** (`x-kubernetes-validations` in den CRDs, keine
Admission-Webhooks): Bei `Persistent` sind `scaling` und `update` verboten, bei
`Ephemeral` `storage` und `replicas`. Zusätzlich sind `spec.type` sowie bei
Persistent `storage.storageClassName` und `storage.accessModes` per
Transition-Rule unveränderlich (`self.type == oldSelf.type`); `storage.size` darf
nur wachsen, wobei die tatsächliche PVC-Vergrößerung `allowVolumeExpansion` der
StorageClass voraussetzt. Ein Typwechsel an einer bestehenden Gruppe erfordert
Löschen und Neuanlegen. Alle diese Regeln sind reine Cross-Field-Checks auf
demselben Objekt und damit ohne Webhook umsetzbar — was die Installation auf ein
`helm install` ohne Zertifikatsverwaltung beschränkt.

**`Degraded`** ist eine Condition mit Reason (etwa `CrashLoopBackoff`,
`NoFallbackAvailable`). Die Phase einer Gruppe wird daraus abgeleitet und zeigt
`Degraded`, solange die Condition `True` ist.

### 4.4 Server

Eine laufende Instanz, im Besitz ihrer ServerGroup. Ein eigenes Objekt pro Instanz
ist bewusst gewählt: `kubectl get servers` zeigt den Netzwerkzustand, Events landen
an der richtigen Stelle, und CLI und Dashboard brauchen später keine eigene
Datenquelle.

```yaml
apiVersion: minecraft.cloudsystem.dev/v1alpha1
kind: Server
metadata:
  name: lobby-x7k2
  ownerReferences: [{ kind: ServerGroup, name: lobby, controller: true }]
spec:
  groupRef: { name: lobby }
  ordinal: 0                       # nur bei Persistent gesetzt
  groupGeneration: 7               # Snapshot der Gruppen-Generation bei Erstellung
status:
  phase: Ready
  podName: lobby-x7k2
  address: "10.42.3.17:25565"
  players: 12
  slots: 100
  playersUpdatedAt: "2026-08-07T12:34:56Z"
  registered: true                 # aktuell bei den Proxies registriert
  conditions: [...]
```

#### Zustandsmaschine

Die Übergänge sind die einzige Stelle, an der über Registrierung und Löschung
entschieden wird.

```
              ┌───────────── Readiness-Verlust ──────────────┐
              ▼                                              │
Pending ──▶ Starting ──▶ Ready ──▶ Draining ──▶ Terminating ─┴─▶ (gelöscht)
   │            │          │                        ▲
   └────────────┴──────────┴──── Failed ────────────┘
```

- **Pending** — CR existiert, Pod noch nicht erstellt oder noch nicht gescheduled.
- **Starting** — Pod läuft, aber mindestens ein Ready-Signal fehlt.
- **Ready** — bei den Proxies registriert, nimmt Spieler an.
- **Draining** — deregistriert, Spieler werden umgezogen. Kein Rückweg nach Ready.
- **Terminating** — leer oder Drain-Timeout erreicht, Pod wird gelöscht.
- **Failed** — Startfehler oder wiederholter Absturz. Kein Rückweg in den Betrieb;
  beim Eintritt aus `Ready` wird sofort deregistriert. Das Objekt bleibt für die
  Diagnose bestehen und wird nach `failedRetentionSeconds` über `Terminating`
  aufgeräumt (ohne Drain, da keine Spieler mehr).

**Readiness-Verlust (`Ready → Starting`)** ist der wichtigste Zusatz gegenüber dem
naiven Modell: Ein Container-Restart oder eine rot werdende Readiness-Probe macht
einen Server unspielbar, ohne dass er die Phase verlässt — Spieler würden weiter
auf einen bootenden Server geroutet. Der Übergang wird ausgelöst, wenn die
Readiness-Probe rot wird **oder** der Agent-Stream länger als 15 Sekunden
abgerissen ist; der Operator deregistriert dann sofort. Der Rückweg nach `Ready`
verlangt erneut **beide** Signale (siehe 6.1) — das Ready-Gate gilt für jeden
Eintritt in `Ready`, nicht nur für den Erststart. Mehrfacher Readiness-Verlust in
kurzer Folge zählt auf denselben Zähler wie ein wiederholter Absturz und führt nach
Überschreiten der Schwelle nach `Failed`, damit ein flappender Server nicht endlos
registriert und deregistriert wird.

#### Rolling Updates

Ändert sich die Gruppen-Spec, steigt deren `generation`. Server mit veraltetem
`groupGeneration` sind *stale*.

Bei **ephemeren** Gruppen läuft der Wechsel surge-first und ohne Kick:

1. Stale Server zählen **nicht** mehr in die freien Slots der Gruppe (siehe 6.3).
   Der Scaler erzeugt dadurch von selbst Ersatz der neuen Generation.
2. Sobald genügend Ready-Kapazität der neuen Generation existiert — mindestens ein
   Ready-Server, bei Fallback-Gruppen zwingend — geht ein stale Server in den
   **Soft-Drain**: Er wird deregistriert und nimmt keine neuen Joins mehr an, aber
   seine Spieler bleiben ungestört, bis er von allein leer ist.
3. `update.maxUnavailable` (Default 1) begrenzt, wie viele Server der Gruppe sich
   gleichzeitig wegen eines Generationswechsels in `Draining` oder `Terminating`
   befinden.
4. `update.maxStaleSeconds` (Default 0 = unbegrenzt) erzwingt nach Ablauf den
   aktiven Drain aus 6.2, wenn ein Server nicht von allein leer wird.

Ohne Schritt 1 würde das Update nie terminieren: Eine Lobby, die als Fallback-Ziel
praktisch nie leer wird, bliebe registriert, und ihre freien Slots würden die
Erzeugung neuer Server verhindern.

**Persistente** Server nutzen `Recreate` mit vorgeschaltetem Drain, da das PVC nur
einmal gebunden werden kann.

## 5. Komponenten

Vier Artefakte, jedes mit einer Aufgabe:

**Operator** (Go, kubebuilder/controller-runtime). Vier Controller — je einer pro
CRD — plus der gRPC-Server im selben Prozess. In V1 läuft er mit einer Replica;
Leader-Election ist von Beginn an eingebaut, damit Mehrfach-Replicas später keine
Architekturänderung sind.

**Velocity-Agent** (Kotlin-Plugin). Öffnet beim Start einen bidirektionalen
gRPC-Stream zum Operator: empfängt Registry-Updates und Drain-Befehle, meldet
Spielerzahlen und Join-Events zurück. Bedient außerdem den Readiness-Endpunkt des
Proxy-Pods (siehe 6.6).

**Paper-Agent** (Kotlin-Plugin). Meldet die Bereitschaft des Servers und danach
periodisch Spielerzahl und Slots. Er führt keine Drain-Befehle aus — das Umziehen
von Spielern ist ausschließlich Sache der Proxies (siehe 5.2).

**Basis-Images** für Paper und Velocity, versioniert, mit vorinstalliertem Agent
und einem SLP-Health-Tool für die Readiness-Probe. Das ist eine bewusste Abkehr von
den `itzg`-Images: bei Shulker sind kaputte Upstream-Images und fehlgeschlagene
Plugin-Downloads eine wiederkehrende Fehlerquelle in den Issues.

Das Provisioning läuft **ausschließlich beim Image-Build**: Paper-Jar, Velocity-Jar
und Agent-Plugin werden gegen SHA-256-Hashes geprüft, die im Repository eingecheckt
sind und niemals von der Download-Quelle mitgeladen werden — eine Checksum aus
derselben Quelle wie das Artefakt sichert nur den Transport, nicht den Upstream.
Zur Laufzeit finden keine Downloads statt; konfigurierbare Plugin- und
Template-Downloads sind Projekt 3.

**V1 unterstützt genau eine Paper-/Minecraft-Version und eine Velocity-Version.**
Die Versionsmatrix wird bewusst klein gehalten: Der Image-Tag `1.21.4-<agentversion>`
erzeugt sonst einen Pflegeaufwand (neue Minecraft-Releases binnen Tagen,
CVE-Rebuilds), der als Dauerposten in Projekt 3 gehört und nicht in die erste
Version.

### 5.1 Pod-Verwaltung ohne Deployments

Der Operator erzeugt Pods direkt — keine Deployments, keine StatefulSets, für beide
Server-Typen. Der Grund ist das spielerbewusste Scale-Down: ein Deployment
entscheidet selbst, welchen Pod es beendet. Gegen einen Controller mit anderen
Vorstellungen anzukämpfen ist teurer, als Ordinale und PVCs selbst zu verwalten.
Ein Codepfad statt zwei.

**Schutz belegter Pods vor Eviction.** Der Operator pflegt auf Pods mit Spielern
das Label `minecraft.cloudsystem.dev/occupied="true"` und hält pro Gruppe ein
PodDisruptionBudget, dessen Selector auf dieses Label zeigt und dessen
`minAvailable` als **absolute Zahl** der aktuell belegten Pods nachgeführt wird.
Für Pods ohne Controller mit Scale-Subresource lässt Kubernetes weder
`maxUnavailable` noch Prozentwerte in einem PDB zu — die absolute Zahl ist die
einzige Variante, die trägt. Damit blockiert die Eviction-API jede Eviction eines
belegten Pods.

Die Annotation `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` wird
zusätzlich gesetzt, ist aber ausdrücklich **nur** ein Signal an den
Cluster-Autoscaler und kein Schutz gegen `kubectl drain`.

Damit ein Node-Drain nicht unbegrenzt hängt, beobachtet der Operator, ob ein Node
auf `unschedulable` gesetzt wurde, und stößt für die betroffenen Server proaktiv
den Drain-Ablauf aus 6.2 an. Der Node-Drain terminiert dann, sobald die Spieler
umgezogen sind.

### 5.2 gRPC-Kontrakt

```protobuf
service AgentService {
  rpc ProxySession(stream ProxyMessage)   returns (stream OperatorToProxy);
  rpc ServerSession(stream ServerMessage) returns (stream OperatorToServer);
}
```

**Operator → Proxy:** `FullSync{servers[]}` (beim Verbindungsaufbau),
`RegisterServer{name, address, group}`, `UnregisterServer{name}`,
`DrainPlayers{fromServer, toGroups[]}`, `ReportInterval{seconds}`.

**Proxy → Operator:** `Hello{version}`, `PlayerCount{n}`,
`PlayerJoinedServer{player, server}`, `Heartbeat`.

**Operator → Server:** `ReportInterval{seconds}`.

**Server → Operator:** `Hello{version, ready}`, `Ready`, `PlayerCount{n, slots}`.

Zwei Festlegungen, die aus Fehlerfällen folgen:

**`FullSync` enthält genau die registrierten Server**, also die in Phase `Ready`.
Andernfalls würde ein Proxy-Reconnect während eines Drains die Deregistrierung
rückgängig machen, und der drainende Server bekäme wieder Joins. Direkt nach jedem
`FullSync` sendet der Operator für alle Server in Phase `Draining` erneut
`DrainPlayers` an diesen Proxy. Beide Nachrichten sind idempotent und werden allein
aus dem CR-Status abgeleitet, sodass ein Reconnect oder Operator-Neustart während
eines Drains denselben Zustand deterministisch rekonstruiert.

**Ready ist ein Zustand, kein Ereignis.** Der Paper-Agent setzt sein internes
Ready-Flag bei `ServerLoadEvent(STARTUP)` und meldet es bei **jedem** Connect im
`Hello` mit. Die separate `Ready`-Nachricht bleibt als Sofort-Benachrichtigung
bestehen, ist aber nicht die einzige Quelle — sonst hinge ein Server dauerhaft in
`Starting` fest, wenn der Operator genau im Fenster zwischen Probe-grün und
Ready-Empfang neu startet.

**Melde-Intervall:** Der Operator teilt es beim Verbindungsaufbau per
`ReportInterval` mit; Default sind 5 Sekunden, für Spielerzahlen wie für den
Proxy-Heartbeat. Beide Seiten verwenden damit garantiert denselben Wert, und die
Staleness-Schwelle aus 6.3 (das Doppelte, also 10 Sekunden) ist eindeutig
abgeleitet.

**Drain ist proxy-getrieben.** Es gibt bewusst keine Drain-Nachricht an den Server:
Das Umziehen der Spieler kann nur der Proxy, und der Operator erkennt die Leere am
`PlayerCount{n=0}` des Server-Agents. Ein zweiter, serverseitiger Drain-Pfad wäre
eine zweite Wahrheit über denselben Vorgang.

#### Authentifizierung und Transport

Der gRPC-Endpunkt spricht **ausschließlich TLS**. Der Operator stellt sich beim
Start selbst ein Serving-Zertifikat aus, legt CA und Zertifikat in einem Secret im
eigenen Namespace ab und rotiert rechtzeitig vor Ablauf. Da er die Agent-Pods
selbst erzeugt, mountet er das CA-Bundle in jeden Pod; die Agents validieren
ausschließlich gegen diese gepinnte CA. Kein cert-manager, keine
Webhook-caBundle-Mechanik — der Operator verwaltet sein Serving-Zertifikat selbst,
und das Ziel „ein `helm install` reicht" bleibt unberührt.

Die Authentifizierung nutzt einen projizierten ServiceAccount-Token, den der
Operator per `TokenReview` prüft. Drei Festlegungen machen daraus eine echte
Identität:

1. **Dedizierte Audience.** Der Token wird mit der Audience
   `cloudsystem-operator` und kurzer Lebensdauer (`expirationSeconds: 600`, das
   Kubelet rotiert automatisch) ausgestellt; der Operator setzt genau diese
   Audience in `spec.audiences` des TokenReview. Standard-API-Server-Tokens sind
   beim Operator damit wertlos, und das Replay-Fenster eines abgefangenen Tokens
   ist kurz.
2. **Getrennte ServiceAccounts für Proxy- und Server-Pods.** `ProxySession` wird
   nur für Proxy-SA-Tokens autorisiert, `ServerSession` nur für Server-SA-Tokens.
   Ohne diese Trennung könnte ein kompromittierter Backend-Pod eine ProxySession
   öffnen, per `FullSync` die komplette interne Topologie lesen und über gefälschte
   Meldungen das Scaling manipulieren.
3. **Pod-Identität aus dem Token, nicht aus der Nachricht.** Die Identität des
   Streams stammt ausschließlich aus den Extra-Claims des pod-gebundenen Tokens
   (`authentication.kubernetes.io/pod-name` und `pod-uid`) im TokenReview-Ergebnis.
   Alle Meldungen eines Streams gelten nur für genau diesen Pod. Käme der Podname
   aus der `Hello`-Nachricht, könnte ein kompromittierter Server für einen fremden
   Server `PlayerCount{0}` melden und damit einen vollen Server löschen lassen —
   ein direkter Bruch der Kern-Invariante.

Zusätzlich gilt auf allen Gameserver- und Proxy-Pods
`automountServiceAccountToken: false`; gemountet wird ausschließlich der
projizierte, audience-gebundene Token. Erst damit stimmt die Aussage, dass die
Pods keine Kubernetes-Credentials tragen.

Als Defense-in-Depth verwirft der Operator `PlayerCount`-Meldungen größer als
`slots` und dämpft Scale-up bei sprunghaften Meldungen.

**Agent-Abhängigkeiten müssen geshadet und relocated werden.** Shulker hat einen
dokumentierten Protobuf-Classpath-Konflikt auf BungeeCord — dieselbe Falle gilt für
gRPC-Bibliotheken in jedem Plugin-Classpath.

### 5.3 Warum die Agents nicht selbst die Kubernetes-API lesen

Das naheliegende Alternativmuster (Kuvel) lässt jeden Proxy per Informer selbst
Pods entdecken. Dagegen spricht: Wir brauchen den Rückkanal für Spielerzahlen und
Drain-Kommandos ohnehin, ein Kanal ist besser als zwei, und die Proxy-Pods bleiben
ohne Kubernetes-Credentials (siehe die Audience- und Automount-Festlegungen in
5.2).

Der Preis ist, dass der Operator zur Laufzeit relevant wird. Das wird abgefedert:
Bricht der Stream ab, behält der Proxy seine letzte bekannte Serverliste und
reconnected mit Backoff. Ein Operator-Neustart wirft niemanden aus dem Spiel; nur
Scaling und neue Registrierungen pausieren.

### 5.4 Konfigurations-Rendering

Der Operator rendert pro Gruppe eine ConfigMap mit den aus `spec.config` und dem
`Network` abgeleiteten Werten (MOTD, Player-Limit, Forwarding-Modus). Das
Entrypoint-Skript des Basis-Images merged sie beim Start in `velocity.toml`
beziehungsweise `paper-global.yml`. Nutzer-Mounts aus `mounts` haben Vorrang vor
Defaults, aber nicht vor betriebskritischen Feldern (Forwarding-Modus,
`online-mode`, Ports).

Für Velocity zeigt `forwarding-secret-file` direkt auf den Secret-Mount. Paper
kennt keine Datei-Referenz für das Secret, deshalb injiziert das Entrypoint es beim
Start aus dem Mount in `paper-global.yml`.

Konfigurationsänderungen wirken erst beim Neustart des Pods — konsistent mit dem
Update-durch-Abnutzung-Modell aus 4.4.

## 6. Datenflüsse

### 6.1 Ein Server geht online

1. Der ServerGroup-Controller stellt fest, dass die freien Slots unter
   `spareSlots` liegen, und erzeugt einen `Server`-CR.
2. Der Server-Controller erzeugt den Pod (bei `Persistent` zuvor das PVC).
   Phase: `Starting`.
3. Die Readiness-Probe des Pods wird grün. Sie ist eine **exec-Probe**, die das im
   Basis-Image mitgelieferte SLP-Health-Tool aufruft — ein echter
   Server-List-Ping, kein reiner Port-Check. Kubelet kennt keinen SLP-Probe-Typ,
   und eine `tcpSocket`-Probe auf 25565 würde bereits vor dem Weltenladen grün.
4. Der Paper-Agent meldet `Ready` (beziehungsweise `Hello{ready: true}` nach einem
   Reconnect) über den gRPC-Stream.
5. **Erst wenn beide Signale vorliegen**, geht die Phase auf `Ready`. Das gilt für
   jeden Eintritt in `Ready`, auch nach einem Readiness-Verlust.
6. Der Operator sendet `RegisterServer` an alle verbundenen Proxies.

Das Ready-Gate ist zweistufig, weil auch ein erfolgreicher SLP antwortet, bevor
Plugins vollständig geladen sind. Ein Spieler, der in diesem Fenster geroutet wird,
landet auf einem halb geladenen Server. Log-Parsing auf `Done (x.xs)!` wäre die
dritte Möglichkeit und ist für ein Produkt zu fragil.

### 6.2 Ein Server geht offline

Die Reihenfolge ist die eigentliche Substanz des Systems:

1. Phase auf `Draining`; `UnregisterServer` an alle Proxies — ab jetzt keine neuen
   Joins.
2. `DrainPlayers` an die Proxies: Spieler werden per `createConnectionRequest` auf
   die Fallback-Gruppen umgezogen. `KickedFromServerEvent`-Redirect fängt ab, was
   dabei scheitert.
3. Warten, bis der Server-Agent `PlayerCount{n=0}` meldet oder
   `drain.timeoutSeconds` abläuft.
4. Phase `Terminating`, Pod löschen. Bei `Persistent` läuft dann die
   `terminationGracePeriodSeconds` für den Weltspeicher.

Beim Soft-Drain eines veralteten Servers (siehe 4.4) entfällt Schritt 2: Der Server
wird nur deregistriert und läuft leer, ohne dass jemand umgezogen wird.

**Die tragende Invariante: ein Pod mit Spielern wird nie gelöscht.** Sie gilt für
Scale-Down, Rolling Updates und Node-Drains gleichermaßen und ist der Grund, warum
kein HorizontalPodAutoscaler eingesetzt wird — CPU-basiertes Scaling verfehlt diese
Domäne.

**Proxy-Ersatz** läuft rollierend, einer nach dem anderen: Der neue Proxy wird
Ready (Gate siehe 6.6), dann geht der alte auf `NotReady` und verschwindet aus den
LoadBalancer-Endpoints. Bestehende Verbindungen laufen aus, bis der Proxy leer ist
oder `drain.timeoutSeconds` abläuft; verbleibende Spieler werden dann getrennt.
Anders als beim Server-Drain gibt es kein aktives Umziehen — die Client-Verbindung
endet am Proxy.

### 6.3 Slot-basiertes Scaling

Freie Slots sind die Summe über alle `Ready`-Server **der aktuellen Generation**
von `slots - players`. Stale Server zählen nicht mit; ohne diese Einschränkung
würde ein Rolling Update nie Ersatz erzeugen (siehe 4.4).

- **Hochskalieren**, sobald `freeSlots < spareSlots`: so viele Server erzeugen wie
  nötig, um die Lücke zu decken, begrenzt durch `maxReplicas`.
- **Herunterskalieren** nur, wenn nach dem Entfernen eines Servers noch
  `freeSlots >= spareSlots` bliebe, `replicas > minReplicas` gilt und ein
  **leerer** Server für `scaleDownStabilizationSeconds` durchgehend leer war.

Sind die Spielerzahlen eines Servers älter als 10 Sekunden (das Doppelte des
Melde-Intervalls aus 5.2), gilt er als belegt. Lieber ein Server zu viel als ein
Kick.

### 6.4 Spielerzahlen und etcd

Die Agents melden Spielerzahlen alle 5 Sekunden. Der Operator hält sie im Speicher
— dort trifft die Scaling-Logik ihre Entscheidungen — und schreibt sie nur alle 30
Sekunden oder bei signifikanter Änderung in `Server.status`. Bei 200 Servern wären
ungedrosselte Updates dutzende etcd-Writes pro Sekunde. Der CR-Status ist für
Beobachter da, nicht für den Regelkreis.

### 6.5 Player-Forwarding

Ausschließlich Modern Forwarding. Das Secret liegt als Kubernetes-Secret vor und
wird in beide Ebenen gemountet; das Rendering beschreibt 5.4. Backends laufen mit
`online-mode=false` und `enforce-secure-profile=false` und sind ausschließlich über
die vom Operator erzeugte NetworkPolicy vom Proxy aus erreichbar — ohne diese Sperre
wäre jeder Backend-Server frei betretbar.

**Secret-Rotation ist in V1 ein manueller Wartungsvorgang.** Der Operator erkennt
die Änderung (Secret-Watch mit Hash-Vergleich) und meldet sie als Condition
`ForwardingSecretRotationPending` samt Kubernetes-Event; die Neustarts folgen einem
dokumentierten Runbook: erst alle Backend-Gruppen rollen, dann alle Proxy-Gruppen.

Der Grund für diese Ehrlichkeit: Weder Velocity noch Paper akzeptieren zwei
Forwarding-Secrets gleichzeitig. Es entsteht zwingend ein Fenster, in dem Joins und
Transfers zwischen bereits und noch nicht rotierten Ebenen mit „Unable to verify
player details" fehlschlagen — und genau in diesem Fenster würde ein automatischer
Drain seine Spieler auf Fallback-Server umziehen wollen, die er nicht mehr erreicht.
Eine automatisch orchestrierte Rotation müsste die Registrierung
generationsbewusst machen (Backends nur an Proxies derselben Secret-Generation
registrieren) und Drains puffern, bis die Generationen übereinstimmen. Das ist ein
netzweit koordinierter Rollout mit eigener Fehlerbehandlung, für das
Erfolgskriterium nicht nötig und deshalb auf ein späteres Projekt verschoben.

### 6.6 Ein Proxy geht online

Ein Velocity-Pod, der Traffic bekommt, bevor sein Agent die Serverliste hat, trennt
jeden Spieler mit „no available server". Deshalb bedient der Velocity-Agent selbst
die Readiness-Probe des Proxy-Pods: Sie wird erst grün, nachdem der Agent den
gRPC-Stream aufgebaut und den `FullSync` verarbeitet hat. Ein reiner TCP-Check auf
25565 genügt nicht. Erst dann zählt der Pod für `status.readyReplicas` und erhält
LoadBalancer-Traffic.

Das Gate betrifft nur den Start. Ein bereits bereiter Proxy bleibt bei einem
Stream-Abriss ready und bedient weiter seine letzte bekannte Serverliste.

## 7. Fehlerbehandlung

**Server startet nicht** (CrashLoop, fehlerhafte Konfiguration): Nach einem Timeout
geht der `Server` auf `Failed`, mit sprechendem Grund im Status und einem
Kubernetes-Event. Ersatz wird erzeugt, aber mit exponentiellem Backoff pro Gruppe —
sonst dreht eine kaputte Konfiguration eine Endlosschleife aus Pod-Erstellungen.
Nach mehreren aufeinanderfolgenden Fehlschlägen setzt die Gruppe die Condition
`Degraded` (Reason `CrashLoopBackoff`) und stellt weitere Versuche ein.

**Operator fällt aus:** Server und Proxies laufen weiter, Spieler merken nichts, die
Proxies behalten ihre Serverliste. Beim Neustart rekonstruiert der Operator seinen
Zustand vollständig aus CRs und laufenden Pods — er verlässt sich auf nichts, was
nur im Speicher stand. Spielerzahlen kommen mit den Agent-Reconnects zurück; bis
dahin gelten alle Server als belegt, was Scale-Down bis zur Wiederherstellung
verhindert.

**Agent-Verbindung reißt ab:** Der Agent reconnected mit Backoff und erhält einen
`FullSync` samt Wiederholung offener `DrainPlayers`. Der Operator markiert die
Spielerzahl als veraltet und behandelt den Server konservativ als belegt. Reißt der
Stream länger als 15 Sekunden ab, löst das zusätzlich den Übergang
`Ready → Starting` samt Deregistrierung aus (siehe 4.4).

**Node stirbt:** Die Pods sind weg, die Spieler ebenfalls — dagegen hilft nichts.
Der Operator räumt verwaiste `Server`-CRs auf und stellt die Sollstärke wieder her.
Bei persistenten Servern hängt das am PVC: bei `ReadWriteOnce` muss das Volume erst
freigegeben werden, was hängen bleiben kann. Der Operator erkennt das und meldet es
als Condition, statt still zu warten.

**Node wird gedraint:** Der Operator erkennt `unschedulable` und stößt für die
betroffenen Server den Drain aus 6.2 an, damit `kubectl drain` nicht am PDB
belegter Pods hängen bleibt.

**Drain-Timeout:** Sind nach Ablauf noch Spieler online (Fallback voll oder selbst
ausgefallen), wird bei einem angeforderten Löschen trotzdem beendet — aber laut, mit
Event und Metrik. Ein Scale-Down bricht in diesem Fall stattdessen ab und versucht
es später erneut.

**Proxy-Fallback nicht verfügbar:** Ist keine Fallback-Gruppe `Ready`, verweigert
der Operator den Drain und setzt die Condition `Degraded` (Reason
`NoFallbackAvailable`) an der ServerGroup, deren Server gedraint werden sollte.
Spieler ins Leere zu schicken ist schlimmer, als einen Server länger laufen zu
lassen.

**Verwaiste Pods:** Jeder erzeugte Pod trägt Owner-Reference und Labels. Ein
periodischer Abgleich findet Pods ohne CR und CRs ohne Pod und korrigiert beide
Richtungen.

## 8. Sicherheit und RKE2-Spezifika

Der Operator selbst läuft strikt `restricted`-konform: non-root, read-only
Root-Dateisystem, `RuntimeDefault`-Seccomp, keine Privilege Escalation, alle
Capabilities entfernt.

**RBAC.** Namespace-scoped braucht der Operator: die eigenen CRDs, Pods, PVCs,
Services, Events, PodDisruptionBudgets, NetworkPolicies sowie Secrets — Letztere
nur mit `get`/`watch` und per `resourceNames` auf die in Netzwerken referenzierten
Secrets beschränkt. Cluster-scoped braucht er ein separates, minimales ClusterRole
mit `create` auf `tokenreviews.authentication.k8s.io` (Agent-Authentifizierung) und
`get`/`list` auf `nodes` (Adressermittlung bei HostPort).

Die ServiceAccounts der Gameserver- und Proxy-Pods tragen **keinerlei** Role- oder
ClusterRoleBindings und haben `automountServiceAccountToken: false`. Das ist keine
Kosmetik: Wer Pods erstellen darf, darf ihnen jede ServiceAccount des Namespaces
zuweisen — ohne diese Regel wäre das `pods/create`-Recht des Operators ein
Sprungbrett zu jeder SA im Namespace.

**NetworkPolicies.** Die Helm-Chart liefert die netzwerkunabhängigen Policies:
Default-Deny-Ingress für alle verwalteten Pods und Agent → Operator auf dem
gRPC-Port. Die Policy Proxy → Server auf 25565 erzeugt der **Operator** pro
`Network`, mit dem Label `minecraft.cloudsystem.dev/network=<name>` in `podSelector`
und Ingress-Regel. Auf einem gehärteten Cluster ohne diese Policies bricht die
interne Kommunikation; ohne sie wären die offline-mode-Backends offen.

**Pod Security:** Das RKE2-CIS-Profil erzwingt clusterweit `restricted`, was
HostPort und hostNetwork verbietet. Wer die HostPort-Strategie nutzt, braucht für
den Gameserver-Namespace ein `baseline`-Label oder eine Ausnahme. Die Helm-Chart
dokumentiert das und weist beim Rendern darauf hin, wenn `expose.type: HostPort` mit
einem `restricted`-Namespace kombiniert wird.

**CNI-Abhängigkeit:** HostPort funktioniert mit Canal, mit Cilium nur bei aktivem
`kubeProxyReplacement` oder portmap-Chaining; es gibt dokumentierte Regressionen
nach RKE2-Upgrades. Deshalb gehört ein Erreichbarkeitstest pro Expose-Strategie in
die CI und nicht in die Fehlerberichte der ersten Nutzer.

**Bei HostPort** setzt RKE2 ohne explizites `node-external-ip` die interne IP als
Node-Adresse — der Operator würde dann eine unerreichbare Adresse melden. Das wird
erkannt und als Warnung ausgegeben.

**Image-Referenzen** in den mitgelieferten Manifests sind per Digest gepinnt; Tags
sind mutabel. Die `image`-Felder der CRs akzeptieren Digest-Referenzen, und die
Dokumentation empfiehlt sie.

## 9. Installation

Ein `helm install` muss reichen, und danach muss etwas Spielbares dastehen. Die
Setup-Reibung ist der meistgenannte Schmerzpunkt der etablierten Systeme; eine
jahrelange RC-Phase wie bei CloudNet v4 ist ein Negativbeispiel.

Die Chart installiert CRDs, den Operator, RBAC, ServiceAccounts und die
netzwerkunabhängigen NetworkPolicies. Ein mitgeliefertes Beispiel-Manifest erzeugt
ein Netzwerk mit einem Proxy und einer Lobby-Gruppe, das ohne weitere Konfiguration
funktioniert. Keine Admission-Webhooks, damit keine externe Zertifikatsverwaltung
nötig ist — die Validierung läuft über CEL-Regeln in den CRDs, und sein
gRPC-Serving-Zertifikat verwaltet der Operator selbst.

## 10. Teststrategie

Entwickelt wird testgetrieben; der Test entsteht vor der Implementierung.

**Unit-Tests** decken die Entscheidungslogik ohne Kubernetes ab: den
Scaling-Algorithmus, die Drain-Kandidatenauswahl und die Update-Reihenfolge,
tabellengetrieben. Hier lebt die Prüfung der zentralen Invariante — kein Kandidat
mit Spielern wird je zum Löschen ausgewählt, auch nicht bei veralteten Daten.
Ebenso die Zustandsmaschine: jeder erlaubte und jeder verbotene Übergang,
einschließlich `Ready → Starting` bei Readiness-Verlust.

**Controller-Tests mit envtest** fahren einen echten API-Server hoch und prüfen die
Reconciliation gegen echte CRDs: Erzeugt eine ServerGroup die richtigen Pods? Führt
Pod-Ready plus Agent-Ready zum Phasenwechsel, Pod-Ready allein nicht? Wird das PDB
bei Belegungsänderung nachgeführt? Greifen die CEL-Validierungen, auch die
Transition-Rules mit `oldSelf`? Schnell genug für jeden Commit.

**End-to-End auf k3d und RKE2** mit echten Paper- und Velocity-Prozessen, getrieben
von einem headless Minecraft-Client, der tatsächlich joint. Nur diese Ebene beweist,
dass Forwarding, Registrierung und Drain wirklich funktionieren. Kernszenarien:

- Join landet auf der Lobby.
- Scale-Down mit einem Spieler online zieht ihn um, statt ihn zu kicken.
- Rolling Update einer belegten Lobby-Gruppe terminiert und verliert niemanden.
- Ein persistenter Server behält seine Welt über den Neustart.
- Operator-Neustart im Fenster zwischen Probe-grün und Ready-Empfang führt nach
  Agent-Reconnect trotzdem zum Phasenwechsel.
- Proxy-Scale-Up während ein Client joint erzeugt keinen
  „no available server"-Disconnect.
- Erreichbarkeitstest je Expose-Strategie.

## 11. Meilensteine

| M | Ergebnis |
|---|----------|
| 1 | CRDs und Operator-Gerüst; ServerGroup erzeugt ephemere Pods; Zustandsmaschine inkl. Readiness-Verlust; Verwaisten-Abgleich |
| 2 | Basis-Images mit reproduzierbarem Build; gRPC-Dienst mit TLS und Token-Auth; Paper-Agent; zweistufiges Ready-Gate; Spielerzahl-Meldung |
| 3 | ProxyGroup, Velocity-Agent, Registrierung, Proxy-Ready-Gate, Modern Forwarding, Fallback-Routing — **ein Spieler kann joinen** |
| 4 | Slot-basiertes Scaling, spielerbewusstes Drain, PDB-Schutz, Rolling Update ephemerer Gruppen (Abnutzung, `maxUnavailable`) |
| 5 | Persistente Gruppen mit PVC, geordnetem Shutdown und Recreate-Update; Erkennung der Secret-Rotation samt Runbook |
| 6 | Alle drei Expose-Strategien, NetworkPolicies, Helm-Chart, RKE2-E2E in CI |

Meilenstein 3 ist der Punkt, ab dem das System vorführbar ist.

## 12. Offene Punkte für spätere Projekte

Bewusst offengelassen, mit Auswirkung auf spätere Specs:

- Das Format der geschichteten Templates (Projekt 3) muss zu den `mounts` aus V1
  abwärtskompatibel sein.
- Die automatisierte Image-Build-Pipeline — neue Paper- und Velocity-Releases
  binnen Tagen nachziehen, CVE-Rebuilds des Base-Layers — gehört zu Projekt 3. Als
  geprüfte Alternative dokumentiert: `itzg`-Images per Digest gepinnt als
  Basis-Layer mit eigenem Agent-Layer obendrauf.
- Der Metrik-Pfad für das Dashboard (Projekt 4) nutzt voraussichtlich
  Prometheus-Metriken aus dem Operator statt einer eigenen Aggregation.
- Automatisch orchestrierte Forwarding-Secret-Rotation erfordert
  generationsbewusste Registrierung (siehe 6.5).
- Ein `/play <gruppe>`-Befehl mit Gruppen-Policy im Velocity-Agent
  (`ServerPreConnectEvent`).
- Die endgültige Projektbenennung zieht die API-Gruppe mit und sollte vor dem
  ersten öffentlichen `v1alpha1`-Release feststehen.
