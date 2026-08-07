# Design: Reproduzierbares E2E-Testcluster und RBAC-Nachweis

**Datum:** 2026-08-07
**Status:** Entwurf zur Freigabe
**Umfang:** Testinfrastruktur für Spawnery in zwei Ebenen — eine Rechtetabelle
in envtest, die bei jedem Commit läuft, und ein NixOS-VM-Test mit RKE2, der den
Operator unter seinem ServiceAccount arbeiten sieht. Jede Ebene bekommt einen
eigenen Implementierungsplan; Ebene A zuerst.

## 1. Zweck

Nach Meilenstein 1 gibt es eine Lücke: Die Controller-Tests sprechen mit dem
Admin-Kubeconfig von envtest, also läuft in ihnen kein Vorgang unter dem
ServiceAccount des Operators. Ein fehlendes Verb in der generierten ClusterRole
fällt dort nicht auf — es zeigt sich erst, wenn der Operator zum ersten Mal in
einem echten Cluster unter seinen eigenen Rechten läuft. Die Schlussdurchsicht
von Meilenstein 1 hat das als unprüfbar markiert und zugleich sieben überflüssig
gewährte Verben gefunden, die niemand bemerkt hätte.

**Korrektur einer früheren Annahme.** Ursprünglich stand hier, envtest laufe mit
Adminrechten und könne deshalb über Rechte gar nichts aussagen. Das gilt für den
*Client* der Controller-Tests, nicht für den *Authorizer*: envtest startet den
API-Server mit `--authorization-mode=RBAC`, und `SubjectAccessReview` liefert
dort echte Antworten. Empirisch geprüft — ohne Rolle verweigert, nach Bindung
erlaubt, ein nicht gewährtes Verb verweigert, in unter zwei Sekunden.

Daraus folgt der Zuschnitt dieses Dokuments in zwei Ebenen.

### 1.1 Zwei Ebenen

**Ebene A — Rechtetabelle in envtest.** Die Prüfung, ob die ClusterRole alles
gewährt was der Code braucht und nichts darüber hinaus, braucht kein Cluster.
Sie läuft in der bestehenden envtest-Suite bei **jedem Commit**, in Sekunden.
Ein fehlendes oder überflüssiges Verb fällt damit auf, während es entsteht.

**Ebene B — der Operator im echten Cluster.** Was envtest nicht kann: dass ein
Operator-Prozess unter seinem ServiceAccount gegen einen echten API-Server
spricht, dabei seine Codepfade durchläuft und kein `Forbidden` erzeugt. Dafür
die VM.

Die Ebenen werden getrennt geplant und umgesetzt; Ebene A zuerst, weil sie den
Nutzen sofort liefert und Ebene B kleiner macht.

**Erfolgskriterium Ebene A:** `make test` schlägt fehl, sobald ein Verb in der
ClusterRole fehlt oder eines zu viel gewährt wird.

**Erfolgskriterium Ebene B:** Ein Lauf von `make e2e` zeigt den Operator im
Cluster arbeitend, ohne eine einzige abgelehnte Anfrage.

### Was dieser Zuschnitt nicht ist

Kein Ersatz für die E2E-Szenarien aus Spec §10 des Operator-Entwurfs. Die setzen
alle echte Paper- und Velocity-Prozesse voraus und beginnen mit Meilenstein 3.
Diese Infrastruktur ist der Unterbau, in den sie später hineinwachsen.

## 2. Warum eine VM und nicht ein lokales Cluster

`kind` und `k3d` brauchen eine Container-Laufzeit. Auf der Entwicklungsmaschine
(NixOS) gibt es keine, und eine einzurichten hieße, die Systemkonfiguration zu
ändern und Zustand aufzubauen, der bei der nächsten Person anders aussieht.

`pkgs.testers.runNixOSTest` bootet stattdessen eine VM unter QEMU/KVM, deren
gesamter Inhalt über `flake.lock` gepinnt ist. Derselbe Aufruf erzeugt bei jedem
Entwickler und in CI dasselbe Cluster. Kein Daemon, keine Änderung an der
Systemkonfiguration, kein veränderlicher Zustand zwischen Läufen.

**RKE2 statt k3s**, obwohl der RBAC-Nachweis auf beiden identisch wäre: RKE2 ist
die Zielplattform aus dem Operator-Entwurf, und die dort beschriebenen
Eigenheiten (CIS-Profil mit clusterweitem Pod-Security-`restricted`,
CNI-Abhängigkeit der HostPort-Strategie) fallen so früher auf statt erst in
Meilenstein 6. Der Preis ist Bootzeit und Speicher.

## 3. Bestandteile

Ebene A braucht nur die Deployment-Manifeste aus 3.1 und einen gewöhnlichen
Go-Test. Alles Übrige in diesem Abschnitt und in Abschnitt 4 gehört zu Ebene B.

Drei neue Flake-Outputs:

| Output | Inhalt |
|---|---|
| `packages.operator-image` | `dockerTools.buildLayeredImage` über das vorhandene Operator-Binary. Kein Daemon, bitreproduzierbar. |
| `packages.e2e-probe` | Go-Binary mit den Assertions. Importiert dieselben Konstanten wie der Operator. |
| `checks.e2e-rbac` | `testers.runNixOSTest` — die VM, die beides hineinreicht und ausführt. |

Ausgelagert nach `nix/operator-image.nix` und `nix/e2e-rbac.nix`, damit
`flake.nix` lesbar bleibt.

### 3.1 Deployment-Manifeste

Meilenstein 1 erzeugt nur die ClusterRole. ServiceAccount, ClusterRoleBinding
und Deployment gehören zur Helm-Chart aus Meilenstein 6 und existieren noch
nicht — ohne sie läuft der Operator gar nicht unter einem ServiceAccount, und
RBAC ist prinzipiell nicht prüfbar.

Dieser Zuschnitt zieht deshalb eine dünne Scheibe aus Meilenstein 6 vor: vier
Manifeste unter `config/deploy/` — Namespace `spawnery-system`, ServiceAccount
`spawnery-operator` darin, ClusterRoleBinding auf die generierte ClusterRole,
und ein Deployment mit einer Replica. Kein verlorener Aufwand — die Chart wird
später genau diese Objekte templaten.

Das Deployment setzt `--startup-deadline=20s`, damit der Fehlerpfad innerhalb
eines Testlaufs erreichbar ist (siehe 5.2, Szenario 6).

### 3.2 Testmanifest statt Beispielmanifest

Der Test wendet **nicht** `config/samples/network.yaml` an, sondern ein eigenes
Manifest unter `test/e2e/manifests/`. Grund: Szenario 6 braucht
`failedRetentionSeconds: 30`, und das Beispielmanifest soll ein realistischer
Einstieg für Nutzer bleiben statt für einen Testlauf verbogen zu werden.

Beide benutzen den Namespace `minecraft` und dieselbe Struktur — ein Netzwerk,
eine ephemere Gruppe. Ein eigener Testfall prüft zusätzlich, dass
`config/samples/network.yaml` vom API-Server angenommen wird, damit das
Beispiel nicht unbemerkt verrottet.

## 4. Ablauf des VM-Tests

Das testScript beschränkt sich auf Klempnerei; die Aussagen trifft die Probe.

1. Auf `rke2-server.service` warten.
2. Auf einen `Ready`-Node warten.
3. Das Operator-Image in den containerd-Namespace `k8s.io` importieren.
4. CRDs, Deployment-Manifeste und das Testmanifest anwenden.
5. Warten, bis das Operator-Deployment `Available` meldet.
6. `machine.succeed("/bin/e2e-probe")`.

**Keine feste Wartezeit an irgendeiner Stelle.** Jede Wartestelle ist an eine
Bedingung mit Frist geknüpft. Ein VM-Test, der auf `sleep` baut, wird unter Last
flakig, und ein flakiger E2E-Test wird binnen Wochen ignoriert.

Schlägt die Probe fehl, gibt das testScript Operator-Logs,
`kubectl get networks,servergroups,servers,pods -A` und die Events aus.

## 5. Die Prüfungen

Beide Ebenen fragen über `SubjectAccessReview` nach den Rechten eines *fremden*
Subjekts — des Operator-ServiceAccounts. Damit braucht der Prüfer kein
ServiceAccount-Token und kann zugleich Logs und Events lesen, was mit den
Rechten des Operators nicht ginge. (`SelfSubjectAccessReview` prüft die Rechte
des Aufrufers und wäre hier falsch.)

### 5.1 Ebene A: die Rechtetabelle, in beide Richtungen

Läuft in der envtest-Suite, nicht in der VM. Der Test wendet die generierte
ClusterRole und die Manifeste aus `config/deploy/` in envtest an und leitet das
zu prüfende Subjekt **aus dem ClusterRoleBinding und dem Deployment** ab, statt
es zu wiederholen. Damit deckt Ebene A auch ab, dass die Bindung auf die
richtige Rolle zeigt und das Deployment den richtigen ServiceAccount benutzt —
drei Fehlerquellen statt einer.

```go
type Permission struct {
    Group, Resource, Subresource, Verb string
    Why string   // welche Codestelle das braucht
}
```

Die Tabelle wird **von Hand gepflegt** und nicht aus den kubebuilder-Markern
abgeleitet. Eine abgeleitete Tabelle prüfte nur, dass die Rolle gewährt, was die
Rolle gewährt. Von Hand gepflegt ist sie die unabhängige Aussage „das braucht
der Code".

Das Feld `Why` benennt die Aufrufstelle, damit beim Entfernen eines Codepfads
auffällt, dass der Eintrag mitgeht.

**Nichts fehlt.** Für jeden Eintrag ein `SubjectAccessReview` auf
`system:serviceaccount:spawnery-system:spawnery-operator`. Namespaced
Ressourcen werden im Namespace `minecraft` geprüft, cluster-scoped ohne
Namespace. Jede Ablehnung ist ein Fehlschlag, der das Tripel im Klartext nennt.

**Nichts zu viel.** Die ClusterRole aus dem Cluster lesen, ihre Regeln zu
Tripeln auffalten und prüfen, dass jedes in der Tabelle steht. Ein `*` in
Gruppe, Ressource oder Verb gilt per Definition als Übergewährung und schlägt
fehl.

Wer einen Marker ergänzt, ohne die Tabelle zu pflegen, bekommt einen roten Test.
Das ist beabsichtigt.

**Was Ebene A nicht kann.** Sie prüft, dass Rolle und Tabelle übereinstimmen —
nicht, dass die Tabelle vollständig ist. Fehlt ein Recht in *beiden*, bleibt der
Test grün und der Operator läuft trotzdem in ein `Forbidden`. Genau das ist der
Grund, warum Ebene B nötig bleibt: Dort spricht ein echter Prozess unter seinem
ServiceAccount, und jede Lücke meldet sich von selbst. Ebene A verhindert das
Abdriften, Ebene B beweist die Vollständigkeit.

### 5.2 Ebene B: getriebene Szenarien im Cluster

Erreichbar ohne Paper-Image:

1. Testmanifest anwenden → Netzwerk akzeptiert, Gruppe akzeptiert, ein
   `Server`, ein Pod. Der Pod bleibt in `ErrImagePull` — das ist der erwartete
   Endstand von Meilenstein 1, kein Fehler.
2. `minReplicas` hochsetzen → weitere `Server` und Pods entstehen.
3. `minReplicas` senken → überzählige `Server` verschwinden.
4. Einen Fremdpod mit den verwalteten Labels, aber ohne `Server`-Objekt
   unterschieben → der Verwaisten-Abgleich löscht ihn.
5. Den `Server` löschen → Finalizer wird freigegeben, Objekt verschwindet.
6. Mit `--startup-deadline=20s` am Deployment und `failedRetentionSeconds: 30`
   im Manifest läuft ein Server binnen einer Minute über `Failed` nach
   `Terminating`. Damit ist auch das Löschen eines Pods durch den Operator
   erreichbar.

Danach die Operator-Logs über die API lesen und bei jedem `forbidden`
fehlschlagen, mit der Zeile im Klartext.

### 5.3 Was auch dann ungeprüft bleibt

Das Patchen des Occupied-Labels. Es setzt voraus, dass ein Server einmal `Ready`
war, und das braucht ein Image mit dem SLP-Health-Tool aus Meilenstein 2. Die
Tabellenprüfung deckt das Verb ab, das Szenario nicht.

## 6. Der Prüfer wird selbst geprüft

Das Auffalten der ClusterRole-Regeln und der Abgleich gegen die Tabelle sind
reine Funktionen und bekommen gewöhnliche Go-Unit-Tests ohne Cluster.

**Abnahmekriterium ist Mutation, nicht ein grüner Lauf.** Für Ebene A:

- ein Verb aus den Markern entfernen → Fehlschlag mit genau diesem Tripel,
- ein überflüssiges Verb ergänzen → Fehlschlag in der Gegenrichtung,
- das ClusterRoleBinding auf einen falschen ServiceAccount zeigen lassen →
  Fehlschlag, weil das abgeleitete Subjekt nichts mehr darf.

Für Ebene B: den Verwaisten-Abgleich brechen → Szenario 4 fällt um.

Ein Test, der bloß grün ist, hat in diesem Projekt dreimal nichts bewiesen.

## 7. Einbettung

Ebene A läuft als gewöhnlicher Go-Test in `make test` mit — sie kostet Sekunden.

Ebene B: `make e2e` ruft `nix build .#checks.x86_64-linux.e2e-rbac -L`.
Ausdrücklich **nicht** in `make test` oder `make all`: die Commit-Schleife bleibt
bei rund 25 Sekunden. Die CI-Verdrahtung gehört zu Meilenstein 6; dieser
Zuschnitt achtet nur darauf, sie nicht zu verbauen.

**Kosten:** Der erste Build lädt mehrere Gigabyte (NixOS-Image, RKE2,
containerd). Jeder Lauf bootet eine VM mit etwa 4 GB RAM; rechne mit einigen
Minuten. Das ist der Preis dafür, dass der Lauf bei jedem Entwickler und in CI
derselbe ist.

## 8. Offene Punkte für später

- **CI braucht KVM.** Ohne `/dev/kvm` läuft der Test unter QEMU-Emulation und
  wird um ein Vielfaches langsamer. Vor der CI-Verdrahtung in Meilenstein 6 ist
  zu prüfen, ob der gewählte Runner Nested Virtualization bietet.
- **Die Szenarien aus Spec §10** wachsen ab Meilenstein 3 in dieselbe VM hinein.
  Der Test ist so zu schneiden, dass weitere Prüfungen danebentreten können,
  ohne die bestehenden anzufassen.
- **Pod Security.** RKE2 mit CIS-Profil erzwingt clusterweit `restricted`. Diese
  Spec aktiviert das Profil nicht; sobald Meilenstein 6 die HostPort-Strategie
  prüft, muss der Test es einschalten und die dort beschriebene
  Namespace-Ausnahme mitprüfen.
