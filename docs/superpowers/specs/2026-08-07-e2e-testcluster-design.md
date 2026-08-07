# Design: Reproduzierbares E2E-Testcluster und RBAC-Nachweis

**Datum:** 2026-08-07
**Status:** Entwurf zur Freigabe
**Umfang:** Testinfrastruktur für Spawnery — ein NixOS-VM-Test mit RKE2, der die
Rechte des Operators im Cluster nachweist.

## 1. Zweck

Nach Meilenstein 1 gibt es eine Lücke, die keine der vorhandenen Testebenen
schließen kann: **envtest läuft mit Adminrechten.** Ein fehlendes Verb in der
generierten ClusterRole fällt dort strukturell nie auf — es zeigt sich erst,
wenn der Operator zum ersten Mal unter seinem eigenen ServiceAccount in einem
echten Cluster läuft. Die Schlussdurchsicht von Meilenstein 1 hat das
ausdrücklich als unprüfbar markiert und zugleich sieben überflüssig gewährte
Verben gefunden, die niemand bemerkt hätte.

Dieses Dokument beschreibt die Testinfrastruktur, die diese Lücke schließt, und
den ersten Test, der darauf läuft.

**Erfolgskriterium:** Ein Lauf von `make e2e` beweist, dass die ClusterRole des
Operators alles gewährt, was sein Code braucht — und nichts darüber hinaus.

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

## 5. Was die Probe prüft

Die Probe läuft **innerhalb** der VM mit Adminrechten. Sie fragt über
`SubjectAccessReview` nach den Rechten eines *fremden* Subjekts — des
Operator-ServiceAccounts. Damit braucht sie kein ServiceAccount-Token und kann
zugleich Logs und Events lesen, was mit den Rechten des Operators nicht ginge.
(`SelfSubjectAccessReview` prüft die Rechte des Aufrufers und wäre hier falsch.)

### 5.1 Die Rechtetabelle, in beide Richtungen

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

### 5.2 Getriebene Szenarien

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

### 5.3 Was ungeprüft bleibt

Das Patchen des Occupied-Labels. Es setzt voraus, dass ein Server einmal `Ready`
war, und das braucht ein Image mit dem SLP-Health-Tool aus Meilenstein 2. Die
Tabellenprüfung deckt das Verb ab, das Szenario nicht.

## 6. Der Prüfer wird selbst geprüft

Das Auffalten der ClusterRole-Regeln und der Abgleich gegen die Tabelle sind
reine Funktionen und bekommen gewöhnliche Go-Unit-Tests ohne Cluster.

**Abnahmekriterium ist Mutation, nicht ein grüner Lauf.** Drei Mutationen müssen
die Probe umwerfen:

- ein Verb aus den Markern entfernen → Fehlschlag mit genau diesem Tripel,
- ein überflüssiges Verb ergänzen → Fehlschlag in der Gegenrichtung,
- den Verwaisten-Abgleich brechen → Szenario 4 fällt um.

Ein Test, der bloß grün ist, hat in diesem Projekt dreimal nichts bewiesen.

## 7. Einbettung

`make e2e` ruft `nix build .#checks.x86_64-linux.e2e-rbac -L`.

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
