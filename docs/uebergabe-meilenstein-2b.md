# Übergabe an Meilenstein 2b

Stand: Abschluss von Meilenstein 2a (2026-08-08), Merge-Commit `f0705b2`.

Dieses Dokument ist keine Spec. Es sagt, wo 2a aufgehört hat und was 2b beim
Start bereits vorfindet — damit die Arbeit auf einem anderen Rechner ohne
Rückfragen weitergehen kann. Die Entwurfsentscheidungen stehen in
`superpowers/specs/2026-08-08-agentkanal-design.md`, die offenen Punkte in
`bekannte-punkte.md`.

## Wo wir stehen

Der Operator hat einen abgesicherten Kanal zu den Agents: gRPC über TLS auf
Port 9443, Serving-Zertifikat aus einer selbst ausgestellten CA, Identität aus
einem pod-gebundenen ServiceAccount-Token. Ein `Server` erreicht Phase `Ready`,
sobald die Readiness-Probe grün ist **und** ein Agent über diesen Kanal seine
Bereitschaft gemeldet hat; Spielerzahlen landen gedrosselt im Status.

Was weiterhin fehlt, damit ein Spieler sich verbinden kann: die Basis-Images
(2b) und die Proxy-Schicht (3). Ohne Basis-Image bleibt der Pod in
`ErrImagePull` hängen — das ist der erwartete Endstand.

## Was 2b umfasst

Aus der Meilensteintabelle des Hauptentwurfs, abzüglich dessen, was 2a schon
erledigt hat:

- **Basis-Image für Paper**, versioniert, mit reproduzierbarem Build. Kein
  Download zur Laufzeit; Paper-Jar und Agent-Plugin werden beim Build gegen
  eingecheckte SHA-256-Hashes geprüft, die **nicht** aus derselben Quelle wie
  das Artefakt stammen.
- **SLP-Health-Tool** im Image, das die Readiness-Probe aufruft.
- **Paper-Agent** als Kotlin-Plugin, das den Kanal aus 2a bedient.

Das Velocity-Image gehört zu Meilenstein 3, nicht hierher.

## Der Kontrakt, gegen den 2b baut

**Die `.proto` ist eingefroren.** `proto/spawnery/agent/v1alpha1/agent.proto`
enthält beide Richtungen, auch die Proxy-Nachrichten aus Meilenstein 3.
Feldnummern nicht ändern — der generierte Go-Code liegt eingecheckt in
`internal/agentpb` und `make proto` erzeugt ihn neu.

**Drei Pflichten des Agents** stehen ausführlich in `bekannte-punkte.md`,
Abschnitt „Voraussetzungen für Meilenstein 2b", und sind hier nur benannt,
damit sie beim Planen nicht untergehen:

1. überlappend neu verbinden, bevor `renewAfterSeconds` abläuft,
2. den Header `Bearer ` zeichengenau setzen,
3. `Hello{ready:false}` senkt keine einmal gemeldete Bereitschaft.

**Was der Pod dem Agent zur Verfügung stellt** (aus `internal/podspec`):

| | |
|---|---|
| Token | `/var/run/spawnery/token`, Audience `spawnery-operator`, 600 s, vom Kubelet rotiert |
| CA | `/var/run/spawnery/ca.crt` — ausschließlich dagegen validieren |
| Endpunkt | Umgebungsvariable `SPAWNERY_OPERATOR_ENDPOINT` |
| Kontext | `SPAWNERY_NETWORK`, `SPAWNERY_GROUP`, `SPAWNERY_SERVER`, `SPAWNERY_MAX_PLAYERS` |

## Was das Basis-Image erfüllen muss

Die Podspec verdrahtet das heute schon. Ein Image, das davon abweicht, startet
nicht oder wird nie bereit:

- **`/usr/local/bin/spawnery-slp`** muss existieren und ausführbar sein. Die
  Readiness-Probe ruft es als `spawnery-slp --host 127.0.0.1 --port 25565` auf,
  erstmals nach 20 Sekunden, danach alle 5 Sekunden, Zeitlimit 5 Sekunden, drei
  Fehlversuche bis rot. Es muss einen echten Server-List-Ping sprechen — eine
  Portprüfung würde grün, bevor die Welt geladen ist.
- **Port 25565**, TCP.
- **Arbeitsverzeichnis `/data`**, Scratch unter `/tmp`. Beides ist gemountet;
  alles andere im Dateisystem ist **schreibgeschützt** (`readOnlyRootFilesystem`).
- **Kein Root**: `runAsNonRoot: true`, alle Capabilities entfernt, kein
  Privilege-Escalation, Seccomp `RuntimeDefault`. Das Image muss also einen
  numerischen Nutzer setzen.
- **Kein Liveness-Probe-Verhalten einplanen** — es gibt bewusst keine. Ein
  Neustart würde jeden Spieler auf dem Server werfen.
- Nutzer-Mounts dürfen sich nicht in `/var/run/spawnery` schachteln; `/data/config`
  ist das dokumentierte Muster und bleibt erlaubt.

Die gRPC-Bibliotheken im Plugin **müssen geshadet und relocated werden** —
Protobuf-Classpath-Konflikte sind bei Paper-Plugins ein bekannter Fallstrick
(Hauptentwurf, Abschnitt 5.2).

## Umgebung auf dem neuen Rechner

```bash
git clone git@github.com:spawnery/spawnery.git
cd spawnery
nix develop        # Go, controller-gen, protoc, envtest-Assets, kubectl, k3d
make test          # muss grün sein, bevor irgendetwas angefasst wird
```

`nix develop` funktioniert auf `x86_64-linux`, `aarch64-linux` und
`aarch64-darwin`. Auf Darwin kommen die envtest-Binaries aus den
controller-tools-Releases mit einem in `flake.nix` gepinnten Hash; eine neue
Kubernetes-Version verlangt dort einen neuen Hash.

**Neu für 2b: es braucht eine Container-Laufzeit.** Docker oder Podman, für den
Image-Build und für k3d. Der Rechner, auf dem 2a entstanden ist, hatte keine —
deshalb ist der k3d-Ablauf in der README bis heute nirgends ausgeführt worden.
Wer 2b beginnt, sollte das als Erstes nachholen: der erwartete Endstand von 2a
ist ein Pod in `ErrImagePull`, und genau den kann 2b als Ausgangspunkt
verwenden.

Für den Kotlin-Teil kommt eine JVM-Toolchain dazu (Gradle, JDK). Die ist in
`flake.nix` noch nicht enthalten und gehört in den ersten Planschritt von 2b.

## Erster Schritt

2b hat noch keine Spec. Der Weg ist derselbe wie bei 2a: erst brainstormen,
dann die Spec schreiben und freigeben lassen, dann den Plan, dann umsetzen.

Zwei Fragen, die das Brainstorming früh klären sollte, weil alles andere daran
hängt:

- **Ein Meilenstein oder zwei?** Image-Build und Kotlin-Plugin sind zwei
  Bauwelten mit wenig Überschneidung, so wie 2a und 2b es waren.
- **Nix oder Dockerfile für den Image-Build?** Die Spec verlangt
  Reproduzierbarkeit und eingecheckte Hashes; das Projekt benutzt bereits
  Flakes. Als geprüfte Alternative ist im Hauptentwurf dokumentiert,
  `itzg`-Images per Digest gepinnt als Basis-Layer zu verwenden und den Agent
  als eigenen Layer darüberzulegen.
