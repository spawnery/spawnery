# Bekannte Punkte und Übernahmen für spätere Meilensteine

Stand: Abschluss von Meilenstein 1 (2026-08-07).

Diese Liste sammelt, was während der Umsetzung und der Reviews von Meilenstein 1
bewusst offengelassen wurde. Sie ersetzt keine Spec — die Entwurfsentscheidungen
stehen in `superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`.

## Voraussetzungen für Meilenstein 3 (Proxy-Integration)

**Der Verwaisten-Abgleich verwirft Proxy-Agents.** `OrphanReconciler.Sweep`
listet Pods mit `spawnery.cloud/role=server` und vergisst anschließend jeden
Registry-Eintrag, der nicht in dieser Liste steht. Sobald der erste
Velocity-Agent eine Session öffnet, wird er binnen eines Sweep-Intervalls aus
der Registry entfernt. Der Proxy-Podspec und der geweitete Filter — nur nach
`spawnery.cloud/managed-by` listen, die Server-Existenzprüfung auf
`role=server` einschränken — müssen in derselben Änderung landen. Gehört in die
**Abnahmekriterien** von Meilenstein 3, nicht in dessen Notizen.

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

**Vollständigkeit der Rechtetabelle.** Der Audit in `internal/rbacaudit` fängt
Abweichungen zwischen Tabelle und Rolle. Fehlt ein Recht in beiden, bleibt er
grün — das beweist erst der Operator, der unter seinem ServiceAccount in einem
echten Cluster läuft (Ebene B des E2E-Entwurfs).

**Kein `--leader-election-namespace`.** Mit Default-Flags scheitert ein lokaler
Lauf außerhalb des Clusters; nötig ist `--leader-elect=false`.

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
- `BuildServerPod` prüft nicht, ob ein Nutzer-Mount mit `/data` oder `/tmp`
  kollidiert oder zwei Mounts denselben Namen tragen. Der API-Server lehnt ab,
  aber mit generischer Meldung statt klarem Operator-Fehler.
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
