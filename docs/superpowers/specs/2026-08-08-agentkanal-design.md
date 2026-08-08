# Design: Der Agentkanal — gRPC-Dienst mit TLS und Token-Auth

**Datum:** 2026-08-08
**Status:** Entwurf zur Freigabe
**Umfang:** Meilenstein 2a — die Operator-Seite des Agentkanals, vollständig in
Go. Der gRPC-Kontrakt, der Transport, die Identität der Agents und die
Verdrahtung bis in die bestehende `agent.Registry`. Der Paper-Agent und die
Basis-Images sind Meilenstein 2b und bekommen ein eigenes Dokument.

## 1. Zweck

Meilenstein 1 hat die Zustandsmaschine fertig gebaut, aber nur eine Hälfte ihrer
Eingaben verdrahtet. Das zweistufige Ready-Gate fragt in
`server_controller.go:333-340` nach `PodReady` **und** `AgentReady`; die zweite
Bedingung ist heute immer falsch, weil niemand die Registry füllt. Dasselbe gilt
für `status.players` und `status.slots`: die Drosselung dorthin steht
(`server_controller.go:556-564`), die Quelle fehlt.

Dieses Dokument beschreibt die Quelle: einen gRPC-Dienst im Operator-Prozess, zu
dem die Agents in den Gameserver-Pods eine Session öffnen, und den Nachweis,
dass die Identität am anderen Ende die ist, die sie behauptet.

**Erfolgskriterium:** Ein Testagent, der über eine echte TLS-Verbindung mit einem
echten, per `TokenRequest` ausgestellten Token eine `ServerSession` öffnet und
`Ready` meldet, bringt einen `Server` mit grüner Pod-Readiness in Phase `Ready`
— und seine Spielerzahlen erscheinen im Status. Dieselbe Verbindung ohne
passende Audience, ohne Pod-Bindung oder mit dem Proxy-ServiceAccount wird
abgelehnt.

**Nicht in 2a:** `ProxySession` (Meilenstein 3), CA-Rotation mit
Überlappungsphase, der Kotlin-Agent, die Basis-Images.

## 2. Warum der Schnitt hier liegt

Meilenstein 2 umfasst laut Entwurf drei Bauwelten: Go, Gradle/JVM und
Container-Builds. Der Go-Teil ist der einzige, der sich hier vollständig
nachweisen lässt — envtest braucht kein Kubelet und keine Container-Laufzeit —
und er definiert den Kontrakt, gegen den 2b baut. Deshalb zuerst.

## 3. Vorbedingung: envtest auf beiden Entwicklungsrechnern

Der flake bindet `KUBEBUILDER_ASSETS` an das nixpkgs-Paket `kubernetes`, das für
`aarch64-darwin` nicht gebaut wird. Auf dem NixOS-Desktop läuft die Suite, auf
dem Mac scheitert schon die Auswertung von `nix develop`. Damit ist die
Nachweisführung dieses Meilensteins auf einem der beiden Rechner unmöglich.

Das controller-tools-Projekt veröffentlicht envtest-Binaries für alle vier
relevanten Plattformen samt SHA-512 in einer versionierten Liste, darunter
`envtest-v1.36.2-darwin-arm64.tar.gz`. Der flake holt sie künftig von dort,
plattformabhängig und mit eingecheckten Hashes — dasselbe Muster, das Spec 5 des
Hauptentwurfs für die Basis-Images vorschreibt, und eine Version, die zu den
`k8s.io/*`-Abhängigkeiten in `go.mod` (0.36) passt.

Das ist der erste Schritt des Plans, und er ist ein Tor: **solange envtest hier
nicht läuft, ist der Rest dieses Dokuments unprüfbar.**

### 3.1 Eine Annahme, die dieser Schritt mitprüft

Der Entwurf steht und fällt damit, dass envtest pod-gebundene ServiceAccount-
Tokens ausstellt und der Authentifizierer daraus die Claims
`authentication.kubernetes.io/pod-name` und `pod-uid` erzeugt. Nach Aktenlage
trägt Kubernetes 1.36 beides ohne Feature-Gate, **nachgemessen ist es hier
nicht** — anders als die `SubjectAccessReview`-Zusage aus dem E2E-Entwurf, die
vor der Umsetzung geprüft wurde.

Der erste Planschritt prüft es mit einer Sonde: Namespace, ServiceAccount und Pod
anlegen, `TokenRequest` mit Audience und `BoundObjectRef` auf den Pod, dann
`TokenReview` und die Extra-Claims ansehen. Fällt die Sonde negativ aus, ändert
sich Abschnitt 6.3 dieses Dokuments — die Pod-Identität käme dann aus einer
anderen Quelle, und das ist eine Entwurfsfrage, keine Umsetzungsfrage.

## 4. Bausteine

| Paket | Aufgabe |
|---|---|
| `proto/spawnery/agent/v1alpha1/agent.proto` | Der Kontrakt aus Spec 5.2 des Hauptentwurfs, vollständig — beide Services, alle Nachrichten. Zugleich das Artefakt, gegen das 2b den Kotlin-Agent baut. |
| `internal/agentpb` | Generiertes Go, eingecheckt wie `zz_generated.deepcopy.go`. Erzeugt von `make proto`. |
| `internal/certs` | CA und Serving-Zertifikat ausstellen, im Secret halten, erneuern. Reine Kryptologik plus ein Secret-Zugriff; Uhr injizierbar. |
| `internal/grpcauth` | `TokenReview` prüfen und daraus eine `Identity` machen. Kennt weder gRPC-Handler noch Registry. |
| `internal/agentserver` | Der `AgentService`. Verdrahtet Identität und Nachrichten in die `agent.Registry`. |

**Geändert:** `internal/podspec` (ServiceAccount, projizierter Token, CA-Mount,
Endpunkt), ein Namespace-Bootstrap in `internal/controller`,
`cmd/spawnery-operator` (Verdrahtung und Flags), `config/deploy/` um einen
Service und den gRPC-Port im Deployment erweitert, `config/rbac/` samt
Rechtetabelle in `internal/rbacaudit`.

**Unverändert, obwohl es zum Meilenstein gehört:** Das zweistufige Ready-Gate und
die Spielerzahl im Status stehen bereits. 2a liefert ihnen Daten, keine neue
Logik.

## 5. Der Kontrakt

Die `.proto` bildet Spec 5.2 vollständig ab, auch die Proxy-Richtung. Der
Kontrakt ist im Hauptentwurf ohnehin eingefroren, und eine Feldnummer später zu
ändern ist teurer als ein paar ungenutzte Nachrichten heute. Implementiert und
getestet wird in 2a nur `ServerSession`; `ProxySession` antwortet
`Unimplemented`.

Beide Richtungen sind Ströme von `oneof`-Umschlägen. Ein unbekannter Zweig wird
ignoriert, damit ein neuerer Agent gegen einen älteren Operator nicht abstürzt.

### 5.1 Eine Ergänzung gegenüber Spec 5.2

Neu ist `SessionDeadline{renewAfterSeconds, hardDeadlineSeconds}`, Operator →
Agent, gesendet beim Verbindungsaufbau. Die Begründung steht in 7.1. Der
Hauptentwurf kennt die Nachricht nicht; diese Ergänzung ist bewusst und
verpflichtet 2b auf überlappendes Neuverbinden.

## 6. Sicherheit

### 6.1 Transport

Der Endpunkt spricht ausschließlich TLS, auf Port 9443, erreichbar über einen
ClusterIP-Service `spawnery-operator` im Operator-Namespace. Client-Zertifikate
gibt es nicht — die Agents weisen sich über den Token aus.

### 6.2 Zertifikate

Secret `spawnery-agent-tls` im Operator-Namespace mit `ca.crt`, `ca.key`,
`tls.crt` und `tls.key`. Beide Schlüssel sind ECDSA P-256. Die CA gilt zehn
Jahre, das Serving-Zertifikat 90 Tage, mit den SANs `spawnery-operator`,
`spawnery-operator.<ns>`, `spawnery-operator.<ns>.svc` und
`spawnery-operator.<ns>.svc.cluster.local`.

Beim Start liest der Operator das Secret; fehlt es, ist es unvollständig oder
abgelaufen, stellt er neu aus. Ein stündlicher Runnable erneuert das
Serving-Zertifikat aus derselben CA, sobald weniger als ein Drittel der Laufzeit
übrig ist. Der gRPC-Server holt sein Zertifikat über `tls.Config.GetCertificate`
aus einem `atomic.Pointer`; die Erneuerung kostet weder Neustart noch
Verbindungsabriss.

Ausgestellt wird nur auf dem Leader — siehe 7.2 —, damit es kein Wettrennen um
das Secret gibt.

**Die CA-ConfigMap enthält ein Bündel**, also aneinandergehängte PEMs, obwohl 2a
immer genau eines hineinschreibt. Eine spätere CA-Rotation braucht die
Überlappungsphase alt+neu; das Format jetzt offenzuhalten kostet nichts, es
später zu ändern bricht jeden verbundenen Agent. Der Rotationspfad selbst gehört
nicht in diesen Meilenstein.

### 6.3 Identität

Der Interceptor nimmt den Bearer-Token aus dem `authorization`-Header und
akzeptiert den Stream nur, wenn alles davon zutrifft:

1. `TokenReview` mit `spec.audiences: ["spawnery-operator"]` meldet
   `authenticated: true`, und die Audience steht in der Antwort.
2. Der Username parst als `system:serviceaccount:<ns>:<name>`; der Name ist
   `spawnery-server` für `ServerSession` und `spawnery-proxy` für
   `ProxySession`. Ohne diese Trennung könnte ein kompromittierter Gameserver
   eine Proxy-Session öffnen, per `FullSync` die Topologie lesen und über
   gefälschte Meldungen das Scaling steuern (Spec 5.2, Punkt 2).
3. Die Extra-Claims `authentication.kubernetes.io/pod-name` und `pod-uid` sind
   gesetzt. Fehlen sie, ist der Token nicht pod-gebunden und wird abgelehnt.
4. Der genannte Pod existiert im Cache des Managers, liegt in diesem Namespace,
   trägt `spawnery.cloud/role=server` und hat genau diese UID.

Die Identität des Streams ist damit `{Namespace, PodName, PodUID, Role}` und
stammt ausschließlich aus dem Token. **Aus `Hello` kommt keine Identität** — nur
Version und Ready-Flag. Käme der Podname aus der Nachricht, könnte ein
kompromittierter Server für einen fremden `PlayerCount{0}` melden und ihn
löschen lassen.

Punkt 4 ist Verteidigung in der Tiefe. Ohne ihn könnte ein selbstgebauter Pod
mit demselben ServiceAccount zwar nie für einen fremden Server sprechen — seine
Identität ist seine eigene —, wohl aber die Registry mit Einträgen füllen, zu
denen es keinen CR gibt.

Der Registry-Schlüssel ist `<namespace>/<podname>`.

### 6.4 Was in die Pods kommt

Die Gameserver-Pods behalten `automountServiceAccountToken: false` und bekommen
den ServiceAccount `spawnery-server`. Gemountet wird ein projiziertes Volume
unter `/var/run/spawnery` mit zwei Quellen:

- `token` — ein `serviceAccountToken` mit Audience `spawnery-operator` und
  `expirationSeconds: 600`. Das Kubelet rotiert ihn.
- `ca.crt` — die ConfigMap `spawnery-ca` des Namespace.

Dazu die Umgebungsvariable `SPAWNERY_OPERATOR_ENDPOINT` mit
`spawnery-operator.<operator-ns>.svc:9443`. Der Operator-Namespace kommt aus
einem Flag mit Default aus `POD_NAMESPACE`.

### 6.5 Namespace-Bootstrap

Der Operator gleicht in jedem Namespace, in dem er Pods erzeugt, zwei Objekte
ab: die ConfigMap `spawnery-ca` und den ServiceAccount `spawnery-server`. Der
Server-Controller ruft den Abgleich, bevor er einen Pod anlegt; ändert sich das
TLS-Secret, werden alle bekannten Namespaces nachgezogen.

Die CA ist öffentlich, deshalb eine ConfigMap: ein Secret verlangte dem Operator
clusterweite Secret-Schreibrechte ab, ohne dass der Inhalt geheim wäre. Ein
ServiceAccount ohne Bindung verleiht für sich genommen keine Rechte.

**Der Cache bleibt eng.** Beide Objekttypen tragen
`app.kubernetes.io/managed-by: spawnery`, und der Manager-Cache wird für
ConfigMaps und ServiceAccounts auf dieses Label eingeschränkt. Ohne die
Einschränkung hielte der Operator jede ConfigMap des Clusters im Speicher,
`kube-root-ca.crt` aus jedem Namespace eingeschlossen. Verliert ein Objekt sein
Label, sieht der Abgleich es nicht mehr und legt es neu an; das quittiert der
API-Server mit `AlreadyExists`, worauf der Abgleich ungecacht liest und das
Label zurücksetzt.

### 6.6 Rechte

Neu in der ClusterRole:

- `authentication.k8s.io/tokenreviews: create`
- `configmaps: get, list, watch, create, update` — die CA-ConfigMap je Namespace
- `serviceaccounts: get, list, watch, create` — der SA je Namespace

Neu in einer **`Role` im Operator-Namespace**, erstmals neben der ClusterRole:

- `secrets: get, create, update` — das TLS-Secret

Clusterweite Secret-Schreibrechte wären ausgerechnet in dem Meilenstein, der
Sicherheit einzieht, das falsche Signal. Weil die Trennung ohnehin entsteht,
ziehen die Leases mit um: sie stehen heute clusterweit, obwohl Leader-Election
nur im eigenen Namespace sperrt. `docs/bekannte-punkte.md` führt das als offenen
Punkt für Meilenstein 6 — er ist mit 2a erledigt.

`internal/rbacaudit` teilt seine Tabelle entsprechend in eine clusterweite und
eine namespace-lokale Hälfte; beide Hälften werden weiter in beide Richtungen
geprüft, dateibasiert und per `SubjectAccessReview`.

## 7. Der Lebenszyklus einer Session

### 7.1 Make-before-break

Eine geprüfte Identität gilt nicht unbegrenzt: nach `hardDeadlineSeconds`
schließt der Operator den Stream, der Agent verbindet mit frischem Token neu.
Ein abgefangener Token nützt damit höchstens bis zu seinem Ablauf.

Würde der Operator schlicht schließen, hätte jeder Server alle zehn Minuten ein
15-Sekunden-Fenster (`phase.go:341`), in dem er aus `Ready` fällt, sich bei den
Proxies abmeldet und einen Readiness-Verlust gezählt bekommt. Das wäre ein
selbstgebauter Flap-Zähler.

Deshalb kündigt der Operator die Frist an. Der Agent öffnet nach
`renewAfterSeconds` einen neuen Stream, **bevor** der alte endet. Weil ein
zweiter Stream desselben Pods den ersten verdrängt, ohne `Disconnect` zu rufen
(siehe 7.3), wird `Connected` dabei nie falsch. Der harte Schluss bleibt das Netz
für einen Agent, der sich nicht daran hält.

Werte: `renewAfterSeconds: 480`, `hardDeadlineSeconds: 600`, passend zur
Token-Lebensdauer.

### 7.2 Nur auf dem Leader

Der gRPC-Dienst läuft als Runnable mit Leader-Election-Bindung, denn die
Registry nützt genau dort, wo die Controller lesen. Ein Agent, der auf einem
Standby landet, füllte eine Registry, die niemand liest — der Server käme nie in
`Ready`.

Damit der Service einen Standby gar nicht erst als Endpunkt führt, meldet dessen
`/readyz` erst nach Erhalt der Leader-Sperre grün. In V1 mit einer Replica
folgenlos; ohne diese Kopplung gingen Mehrfach-Replicas später still kaputt.

### 7.3 Ein Stream pro Pod

Öffnet derselbe Pod einen zweiten Stream, verdrängt der neue den alten, und der
alte wird geschlossen, **ohne** `registry.Disconnect` zu rufen. Sonst löschte der
Abbau des verdrängten Streams den Zustand des frischen, und der Server fiele
grundlos aus `Ready`.

### 7.4 Nachrichten

Beim Verbindungsaufbau sendet der Operator `ReportInterval{5s}` und
`SessionDeadline`. Danach:

| Vom Agent | Wirkung |
|---|---|
| `Hello{version, ready}` | `registry.Connect`, bei `ready:true` zusätzlich `MarkReady`. Ready ist ein Zustand, kein Ereignis — sonst hinge ein Server nach einem Operator-Neustart dauerhaft in `Starting`. |
| `Ready` | `registry.MarkReady` als Sofort-Benachrichtigung. |
| `PlayerCount{n, slots}` | `registry.ReportPlayers`. |
| Stream endet | `registry.Disconnect`, außer bei Verdrängung nach 7.3. |

## 8. Fehlerbehandlung

| Fall | Verhalten |
|---|---|
| `TokenReview` nicht erreichbar | Ablehnung mit `Unavailable`; der Agent versucht es mit Backoff erneut. Kein Positiv-Cache: bei 200 Servern und einer Prüfung je Stream und zehn Minuten sind das rund 0,3 Prüfungen pro Sekunde. |
| Token abgelehnt | Ablehnung mit `Unauthenticated`, Zähler hoch, Log mit Namespace und ServiceAccount — nie mit dem Token. |
| `PlayerCount` über `slots` | Verworfen, geloggt, Zähler hoch, Stream bleibt offen. So steht es in Spec 5.2; ein Abbruch wäre eine Reconnect-Schleife auf Zuruf des Agents. |
| Unbekannter `oneof`-Zweig | Ignoriert. |
| Operator-Neustart | Die Agents verbinden neu und schicken `Hello{ready:true}`. Für unbekannte Pods misst die Registry `StreamDownFor` ab Prozessstart, die Karenz aus `phase.go` greift also. Bereits so gebaut. |
| Pod verschwindet | Der pod-gebundene Token wird ungültig, der Stream stirbt. `Forget` bleibt Sache des Verwaisten-Abgleichs. |
| CA-ConfigMap gelöscht oder verfälscht | Der Bootstrap-Abgleich stellt sie wieder her. Laufende Streams stört das nicht; ein neu startender Pod wartet, bis die Datei wieder stimmt. |

Drei Prometheus-Metriken, weil abgelehnte Streams sonst nur im Log auftauchen:
offene Streams als Gauge, abgelehnte Authentifizierungen und verworfene
Spielerzahlen als Zähler.

## 9. Tests

Alles hier läuft in `make test`, ohne Kubelet und ohne Container-Laufzeit.

**Sonde (Planschritt 1).** envtest stellt einen pod-gebundenen Token mit Audience
aus, und `TokenReview` liefert `pod-name` und `pod-uid`. Siehe 3.1 — dieser Test
trägt die Annahme, auf der 6.3 steht.

**`internal/certs`, Unit.** Ausstellung setzt die erwarteten SANs; Erneuerung
greift unter der Schwelle und nicht darüber; ein verfälschtes Secret führt zur
Neuausstellung statt zum Absturz. Uhr injiziert.

**`internal/certs`, envtest.** Das Secret übersteht einen Neustart: der zweite
Start verwendet dieselbe CA weiter.

**`internal/grpcauth`, envtest.** Gegen den echten Authentifizierer, mit Tokens
aus `TokenRequest`:

- angenommen: pod-gebundener Token des `spawnery-server`-SA auf `ServerSession`,
  und die Identität nennt Podname und UID des richtigen Pods;
- abgelehnt: falsche Audience, Token ohne Pod-Bindung, Token des
  `spawnery-proxy`-SA auf `ServerSession`, Token für einen nicht existierenden
  Pod, abgelaufener Token, fehlender Header.

Die Ablehnungen sind der eigentliche Beweis — ein Test, der nur Annahmen prüft,
bliebe auch dann grün, wenn der Interceptor jeden durchließe.

**`internal/agentserver`, envtest.** Ein Go-Testagent über eine echte
TLS-Verbindung gegen einen echten Listener: `Hello`, `Ready` und `PlayerCount`
landen in der Registry; der Abriss setzt `Connected` auf falsch; ein zweiter
Stream verdrängt den ersten, ohne den Zustand zu löschen; der harte Schluss
greift; überlappendes Neuverbinden hält `Connected` durchgehend wahr.

**`internal/controller`, envtest.** Mit laufendem Manager und gRPC-Dienst: ein
`Server`, dessen Pod-Readiness gesetzt ist und dessen Agent `Ready` meldet,
erreicht Phase `Ready` — und seine Spielerzahlen erscheinen in `status.players`.
Die Pod-Readiness setzen die Tests weiterhin selbst; ein Kubelet gibt es nicht.

**`internal/podspec`, Unit.** ServiceAccount, projiziertes Volume mit Audience
und Ablauf, CA-Pfad, Endpunkt-Variable.

**Namespace-Bootstrap, envtest.** ConfigMap und ServiceAccount entstehen; eine
CA-Änderung wird in alle bekannten Namespaces nachgezogen; eine fremde Änderung
an der ConfigMap wird korrigiert.

**`internal/rbacaudit`, envtest.** Die Tabelle deckt die neuen Rechte ab und ist
in eine clusterweite und eine namespace-lokale Hälfte geteilt. Zusätzlich eine
Sonde, die ein bekannt **nicht** gewährtes Recht abfragt und auf Ablehnung
besteht — damit ist der erste offene Punkt aus `docs/bekannte-punkte.md`,
Abschnitt „Zum RBAC-Audit", erledigt.

## 10. Was 2a offenlässt

- **`ProxySession`** antwortet `Unimplemented`. Meilenstein 3 füllt sie, und
  zwar zusammen mit dem geweiteten Filter im Verwaisten-Abgleich — sonst wirft
  `OrphanReconciler.Sweep` jeden Proxy-Agent binnen eines Intervalls aus der
  Registry (`docs/bekannte-punkte.md`).
- **CA-Rotation** mit Überlappungsphase. Das Bündelformat steht, der Pfad nicht.
- **Der Agent selbst.** Bis 2b spricht nur der Testagent mit dem Dienst; damit
  ist der Kontrakt geprüft, aber keine Zeile Kotlin.
- **Der `spawnery-proxy`-ServiceAccount** wird von 2a nicht angelegt. Der
  Bootstrap kennt ihn erst, wenn es Proxy-Pods gibt.
