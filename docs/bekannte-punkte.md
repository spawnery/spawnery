# Bekannte Punkte und Übernahmen für spätere Meilensteine

Stand: Abschluss von Meilenstein 2a, dem Agentkanal (2026-08-08).

Diese Liste sammelt, was während der Umsetzung und der Reviews von Meilenstein 1
und Meilenstein 2a bewusst offengelassen wurde. Sie ersetzt keine Spec — die
Entwurfsentscheidungen stehen in
`superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md` und in
`superpowers/specs/2026-08-08-agentkanal-design.md`.

## Voraussetzungen für Meilenstein 2b (Basis-Images, Kotlin-Agent)

**Der Kotlin-Agent muss überlappend neu verbinden.** Der Operator kündigt beim
Verbindungsaufbau mit `SessionDeadline{renewAfterSeconds, hardDeadlineSeconds}`
an, wann er den Stream hart schließt (heute 480/600 Sekunden). Öffnet der Agent
nicht vor `renewAfterSeconds` einen neuen Stream, während der alte noch läuft,
fällt jeder Server im Rhythmus der harten Frist aus `Ready`, meldet sich bei
den Proxies ab und sammelt einen Readiness-Verlust — ein selbstgebauter
Flap-Zähler (Entwurf, Abschnitt 7.1). `internal/agentserver` liefert dafür nur
die Operator-Hälfte (`Registry.Supersede` trägt die Bereitschaft eines
verdrängenden Streams über); ohne einen Agent, der tatsächlich vor der Frist
neu verbindet, ist das wirkungslos. Der Testagent aus 2a tut das, der
Kotlin-Agent muss es auch tun — nicht optional, sondern die Voraussetzung, unter
der `SessionDeadline` ihren Zweck erfüllt.

**`Hello{ready:false}` kann eine einmal gesetzte Bereitschaft nicht senken.**
`registry.MarkReady` wird nur bei `Hello{ready:true}` und bei der eigenständigen
`Ready`-Nachricht gerufen; `Hello{ready:false}` ist für die Registry ein
No-op. Ein Agent kann sich also nicht freiwillig als „verbunden, aber nicht
mehr bereit" melden, ohne die Verbindung zu kappen — nur ein echter Abriss
oder eine Verdrängung senkt die Bereitschaft. Das verschärft sich an einer
zweiten Stelle: bricht ein Stream tatsächlich ab, bevor sein Abbau-Pfad läuft,
und supersedet in der Zwischenzeit ein neuer, trägt `Supersede` die alte
Bereitschaft weiter — womöglich die eines inzwischen neu gestarteten
Prozesses. Für den Kotlin-Agent heißt das: „kurz nicht bereit, aber weiter
verbunden" lässt sich mit diesem Kontrakt nicht ausdrücken.

## Voraussetzungen für Meilenstein 3 (Proxy-Integration)

**Der Verwaisten-Abgleich verwirft Proxy-Agents.** `OrphanReconciler.Sweep`
listet Pods mit `spawnery.cloud/role=server` und vergisst anschließend jeden
Registry-Eintrag, der nicht in dieser Liste steht. Sobald der erste
Velocity-Agent eine Session öffnet, wird er binnen eines Sweep-Intervalls aus
der Registry entfernt. Mit dem Agentkanal aus Meilenstein 2a ist das kein
hypothetischer Pfad mehr: `ServerSession` funktioniert bereits, und sobald
`ProxySession` implementiert ist (siehe unten), trifft der Sweep sofort zu.
Der Proxy-Podspec und der geweitete Filter — nur nach
`spawnery.cloud/managed-by` listen, die Server-Existenzprüfung auf
`role=server` einschränken — müssen in derselben Änderung landen. Gehört in die
**Abnahmekriterien** von Meilenstein 3, nicht in dessen Notizen.

**`ProxySession` antwortet `Unimplemented`, und kein Bootstrap legt den
`spawnery-proxy`-ServiceAccount an.** Der Kontrakt aus Meilenstein 2a bildet
beide Sessions vollständig ab (Entwurf, Abschnitt 5), implementiert und
authentifiziert aber nur `ServerSession`. `internal/controller.Bootstrapper`
kennt bislang nur den ServiceAccount `spawnery-server`; ein Proxy-Pod bekäme
gar keinen ServiceAccount, mit dem er sich ausweisen könnte. Die
`ProxySession`-Implementierung, der Bootstrap-Eintrag für `spawnery-proxy` und
der geweitete Verwaisten-Filter oben gehören in dieselbe Änderung — keiner der
drei ergibt für sich allein einen funktionierenden Proxy-Agent.

**`Register` wird vor dem Persistieren von `WasRegistered` gesendet.**
`applyDecision` ruft den Registrar auf und schreibt erst danach
`status.wasRegistered = true`. Geht der Status-Write verloren, während bereits
Spieler joinen, nimmt eine Löschung in diesem Fenster den Zweig „nie registriert
→ sofort beenden, kein Drain". In Meilenstein 1 wirkungslos, weil der Registrar
ein No-op ist. Der richtige Fix ist, die Absicht vor dem Seiteneffekt zu
persistieren; das ist eine Verhaltensänderung und gehört mit dem Aufteilen von
`applyDecision` zusammen.

## Voraussetzungen für Meilenstein 4 (Scaling und Drain)

**Das PodDisruptionBudget hat kein Gegenstück.** Meilenstein 1 liefert den
Schutz belegter Pods, aber nicht die in Spec 5.1 und 7 vorgesehene Erkennung von
`unschedulable` gewordenen Nodes samt proaktivem Drain. Bis dahin kann der
Operator einen Node-Drain blockieren, ohne ihn selbst wieder freigeben zu
können. Beides gehört in einen Meilenstein.

**Terminierende Pods gelten als „Prozess weg".** `isOccupied` behandelt einen
Pod mit gesetztem `deletionTimestamp` als sessionsfrei, obwohl der Prozess
während der Grace Period noch läuft und Spieler noch verbunden sein können.
Dadurch sinkt `minAvailable` für die Dauer der Grace Period um eins, während das
Label des Pods noch auf den Selector passt. In envtest nicht reproduzierbar, weil
dort kein Kubelet läuft — braucht einen echten Cluster.

**Exponentieller Backoff pro Gruppe.** Spec 7 fordert ihn samt Condition
`Degraded`/`CrashLoopBackoff` und dem Einstellen weiterer Versuche. Meilenstein 1
hat stattdessen nur eine Obergrenze von einem aufbewahrten Fehlschlag pro Gruppe.

**Verwaiste `Server` ohne Pod.** Der Abgleich deckt „Pod ohne CR" und „Server
ohne Gruppe" ab, nicht aber „CR ohne Pod": das übernimmt die Zustandsmaschine
über `PodLost`, was erst greift, wenn `status.podName` geschrieben wurde. Ein
Server, der nie einen Pod bekam, bliebe in `Pending` und belegt seinen Platz.

## Voraussetzungen für Meilenstein 5 (Persistente Gruppen)

Fehlt die `ServerGroup` eines Servers, arbeitet der Server-Controller mit
ephemeren Fallback-Zeiten weiter. Für einen persistenten Server sind das die
falschen Fristen.

## Voraussetzungen für Meilenstein 6 (Helm, RBAC, E2E)

**`spawnery-system` ist in den RBAC-Markern festverdrahtet.** Die
`+kubebuilder:rbac`-Marker für das TLS-Secret (`internal/certs/store.go`) und
für die Leases (`internal/controller/setup.go`) tragen `namespace=spawnery-system`
als Literal. Läuft der Operator in einem anderen Namespace, erzeugt
`controller-gen` trotzdem eine `Role`, die in `spawnery-system` bindet — der
tatsächliche Namespace bleibt ohne Secret- und Lease-Rechte, und der Operator
scheitert beim ersten `certs.Ensure` bzw. bei der Leader-Wahl, ohne dass RBAC
selbst je meldet, wo das Problem liegt. Die Helm-Chart muss den Namespace hier
parametrisieren, nicht nur in den Objektnamen.

**Vollständigkeit der Rechtetabelle.** Der Audit in `internal/rbacaudit` fängt
Abweichungen zwischen Tabelle und Rolle. Fehlt ein Recht in beiden, bleibt er
grün — das beweist erst der Operator, der unter seinem ServiceAccount in einem
echten Cluster läuft (Ebene B des E2E-Entwurfs).

**Kein `--leader-election-namespace`.** Mit Default-Flags scheitert ein lokaler
Lauf außerhalb des Clusters; nötig ist `--leader-elect=false`.

## Zum Agentkanal (`internal/certs`, `internal/agentserver`)

**Die CA hat kein Rotationsverfahren.** Das Bündelformat der
CA-ConfigMap ist absichtlich für mehrere aneinandergehängte PEMs offen
(Entwurf, Abschnitt 6.2), damit eine spätere Rotation alt und neu
überlappend führen kann — geschrieben wird heute aber immer nur genau eines,
der Überlappungspfad selbst existiert nicht. Läuft die CA in zehn Jahren ab,
oder muss sie kompromittiert ersetzt werden, gibt es dafür nur „Secret
löschen, alle Pods neu starten". Selbst dann erreicht eine neue CA nicht jeden
Namespace sofort: `Bootstrapper.Ensure` läuft nur, bevor der
Server-Controller einen Pod anlegt. Ein bestehender Namespace ohne neue
Pod-Erzeugung behält die alte `ca.crt` in seiner ConfigMap, bis dort der
nächste Pod entsteht.

**`controller-gen` ignoriert einen `+kubebuilder:rbac`-Marker in einem
Doc-Kommentar stillschweigend — keine Regel, kein Fehler.** Der Marker muss
unmittelbar vor der Deklaration stehen, für die er gilt; steht er weiter oben
als Teil ihres Kommentarblocks, entsteht in `config/rbac/role.yaml` einfach
keine Zeile. Task 10 ist so zweimal hineingelaufen — der erste Versuch, Rechte
für `secrets` und `tokenreviews` zu ergänzen, erzeugte kommentarlos gar keine
Regel. Wer einen neuen Marker ergänzt, sollte danach `config/rbac/role.yaml`
diffen, nicht nur `make manifests` grün sehen.

**Auf Darwin kommen die envtest-Binaries aus den controller-tools-Releases,
nicht aus nixpkgs**, mit einem Hash je Version fest im `flake.nix`
eingecheckt (Entwurf, Abschnitt 3). Eine neue Kubernetes-Version im
nixpkgs-Kanal — der Linux-Pfad — bringt diesen Hash nicht automatisch mit; er
muss für Darwin separat nachgezogen werden, sonst laufen die beiden
Entwicklungsumgebungen mit unterschiedlichen `kube-apiserver`-Versionen gegen
dieselbe Suite, ohne dass etwas das anzeigt.

## Zum RBAC-Audit (`internal/rbacaudit`)

Der Audit prüft die ClusterRole und die namespace-lokale Role in zwei
Richtungen: eine dateibasiert gegen die handgepflegte Tabelle, eine über
`SubjectAccessReview` gegen den echten Authorizer in envtest. Die Redundanz ist
Absicht — die folgenden Punkte betreffen jeweils nur eine der beiden Hälften.

- **`apply()` verschleiert Fehlerquellen.** Es toleriert `AlreadyExists`, damit
  sich die Tests die clusterweiten Objekte teilen können. Dadurch ist der
  Aufruf im Manifest-Test faktisch wirkungslos, weil der Rechte-Test zuerst
  läuft und die Objekte anlegt. Wer eine *geänderte* ClusterRole anwendet,
  bekommt still die alte.
- **`ExpandRules` ignoriert `rule.ResourceNames`.** Eine namensbeschränkte Regel
  faltet zu einem unbeschränkten Recht auf. controller-gen erzeugt so etwas nie,
  und die SAR-Richtung finge es ab — deshalb bewusst offengelassen.
- **Die Flags im Deployment sind ungeprüft.** `sigs.k8s.io/yaml` ist nicht
  strikt, ein vertippter Schlüssel verschwindet lautlos. Die Spec verlangt
  `--startup-deadline=20s` für Ebene B; kein Test bewacht das bisher.
- **Nichts erzwingt, dass `Why` gefüllt und `Required` duplikatfrei ist.**
  `Compare` sammelt Duplikate ein, der letzte gewinnt.

## Kleinigkeiten

- `ObjectRef` ist ein Nicht-Pointer-Struct ohne `omitempty`, deshalb greift ein
  `required`-Marker auf Feldern dieses Typs nie; die Ablehnung kommt faktisch von
  `MinLength=1` auf dem Namen.
- `BuildServerPod` lehnt seit Meilenstein 2a einen Nutzer-Mount ab, der `/data`
  oder `/tmp` exakt trifft oder sich unter dem Agent-Mount-Pfad verschachtelt
  (`checkMountCollision`). Zwei Nutzer-Mounts mit demselben Namen prüft es
  weiterhin nicht — das fängt der API-Server ab, aber mit generischer Meldung
  statt klarem Operator-Fehler.
- „Ältesten Fehlschlag behalten" trägt nicht bei gleichem `creationTimestamp`
  (Sekundenauflösung); der Tiebreak fällt auf den Zufallssuffix statt auf
  `status.failedAt`.
- Der Status eines abgelehnten `Network` friert ein und meldet weiter alte
  Zahlen.
- Nach dem Löschen des gewinnenden `Network` dauert die Erholung bis zu rund 90
  Sekunden, weil der Verlierer im Minutentakt und die Gruppe im 30-Sekunden-Takt
  nachfragt. Eine Watch-Zuordnung `ServerGroup → Network` würde beides lösen.
- `NetworkReconciler.Recorder` und `Clock` sind ungenutzt; bei einer Ablehnung
  gibt es kein Kubernetes-Event, nur eine Condition.
- Der `deletionTimestamp`-Skip in `Sweep` ist durch keinen Test gedeckt; er
  betrifft nur einen bereits löschenden verwaisten Pod, wo ein zweites `Delete`
  folgenlos ist.
