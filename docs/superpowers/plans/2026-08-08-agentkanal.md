# Agentkanal (Meilenstein 2a) — Implementierungsplan

> **Für agentische Bearbeiter:** ERFORDERLICHES SUB-SKILL: `superpowers:subagent-driven-development` (empfohlen) oder `superpowers:executing-plans`, um diesen Plan Aufgabe für Aufgabe umzusetzen. Die Schritte sind Checkboxen (`- [ ]`).

**Ziel:** Der Operator bekommt einen gRPC-Dienst, über den die Agents in den Gameserver-Pods ihre Bereitschaft und ihre Spielerzahlen melden — TLS-verschlüsselt, mit einer Identität, die aus einem pod-gebundenen ServiceAccount-Token stammt und nicht aus der Nachricht.

**Architektur:** Vier neue Go-Pakete (`agentpb`, `certs`, `grpcauth`, `agentserver`) und ein Namespace-Bootstrap füllen die bestehende `agent.Registry`. Zustandsmaschine, Ready-Gate und Statusdrosselung existieren bereits und werden nicht angefasst — sie bekommen nur endlich Daten. Der Dienst läuft als Leader-gebundenes Runnable im Operator-Prozess.

**Tech-Stack:** Go 1.26.5, controller-runtime 0.24.1, k8s.io/* 0.36, gRPC, protobuf, envtest, Nix.

**Entwurf:** `docs/superpowers/specs/2026-08-08-agentkanal-design.md`. Abschnittsnummern unten verweisen dorthin.

## Globale Randbedingungen

- Jede neue Datei beginnt mit dem Apache-2.0-Kopf aus `hack/boilerplate.go.txt`.
- Kommentare und Bezeichner im Code sind **englisch** — in Go ebenso wie in `flake.nix`, `Makefile` und den YAML-Manifesten. Commit-Nachrichten und Dokumentation unter `docs/` und im README sind **deutsch**. Wo in einer angefassten Datei noch ein deutscher Kommentar steht, wandert er bei der Gelegenheit mit ins Englische.
- Keine neuen Testbibliotheken. Die Suite nutzt ausschließlich `testing` aus der Standardbibliothek, mit Tabellentests und `t.Errorf`-Meldungen, die den Istwert nennen.
- Der Registry-Schlüssel ist die **Pod-UID** als String, nirgends `namespace/name`.
- Die Audience heißt `spawnery-operator`, der ServiceAccount der Gameserver-Pods `spawnery-server`, das TLS-Secret `spawnery-agent-tls`, die CA-ConfigMap `spawnery-ca`, der gRPC-Port ist `9443`.
- `renewAfterSeconds: 480`, `hardDeadlineSeconds: 600`, `ReportInterval: 5s`.
- Objekte, die der Operator in fremden Namespaces anlegt, tragen `spawnery.cloud/managed-by: spawnery-operator` (`podspec.LabelManagedBy` / `podspec.ManagedByValue`).
- **Wo die gemeinsamen Namen wohnen.** `internal/podspec` ist das unterste Paket ohne eigene Abhängigkeiten und hält deshalb alle Namen, die mehr als ein Paket braucht: `AgentTokenAudience = "spawnery-operator"`, `ServerServiceAccountName = "spawnery-server"`, `ProxyServiceAccountName = "spawnery-proxy"`, `CAConfigMapName = "spawnery-ca"`, `CAConfigMapKey = "ca.crt"`. `grpcauth` und `controller` verweisen darauf und definieren sie **nicht** neu. Andersherum ginge es nicht: `grpcauth` braucht `podspec.LabelRole`, ein Gegenimport wäre ein Zyklus.
- Nach jeder Aufgabe läuft `nix develop -c make test` grün, bevor committet wird.
- Neue kubebuilder-RBAC-Marker ziehen immer einen Eintrag in `internal/rbacaudit.Required` nach sich — sonst wird der Audit rot, und das ist Absicht.

---

### Task 1: envtest-Assets, die auch auf dem Mac laufen

Ohne diesen Schritt ist auf `aarch64-darwin` kein einziger envtest-Test ausführbar: `KUBEBUILDER_ASSETS` zeigt auf das nixpkgs-Paket `kubernetes`, das für Darwin nicht gebaut wird, und schon `nix develop` bricht bei der Auswertung ab. Entwurf Abschnitt 3.

Auf Linux bleibt alles wie es ist. Die vorgebauten Linux-Binaries von controller-tools sind dynamisch gegen glibc gelinkt und liefen auf NixOS nur mit `autoPatchelfHook` — das ist unnötiges Risiko für eine Umgebung, die heute funktioniert.

**Dateien:**
- Ändern: `flake.nix:12-20` (die `envtestAssets`-Ableitung)

**Schnittstellen:**
- Liefert: `KUBEBUILDER_ASSETS` zeigt auf ein Verzeichnis mit `etcd`, `kube-apiserver` und `kubectl` — auf beiden Plattformen. Alle späteren Aufgaben setzen das voraus.

- [ ] **Schritt 1: Den Fehlschlag festhalten**

Auf einem Mac:

```bash
nix develop -c true
```

Erwartet: Abbruch mit „Refusing to evaluate package 'kubernetes-…' … hostPlatform.system = "aarch64-darwin"". Genau das behebt diese Aufgabe. (Auf Linux läuft der Befehl durch — dort ist der Ausgangszustand schon gut, und der Test dieser Aufgabe ist, dass er das bleibt.)

- [ ] **Schritt 2: Die Ableitung plattformabhängig machen**

In `flake.nix` den `let`-Block der devShell ersetzen:

```nix
      devShells = forAllSystems (pkgs:
        let
          # Linux: the nixpkgs packages, as before. envtest wants exactly these
          # three binaries in one directory.
          envtestFromNixpkgs = pkgs.runCommand "envtest-assets" { } ''
            mkdir -p $out
            ln -s ${pkgs.kubernetes}/bin/kube-apiserver $out/kube-apiserver
            ln -s ${pkgs.etcd}/bin/etcd                 $out/etcd
            ln -s ${pkgs.kubectl}/bin/kubectl           $out/kubectl
          '';

          # Darwin: nixpkgs has no kube-apiserver build there. The
          # controller-tools project publishes prebuilt darwin/arm64 binaries;
          # the hash is checked in, and the download happens only when the
          # derivation is built. The reverse does not hold for Linux: those
          # binaries are dynamically linked against glibc and would need
          # autoPatchelfHook.
          envtestVersion = "1.36.2";
          envtestFromUpstream = pkgs.stdenvNoCC.mkDerivation {
            pname = "envtest-assets";
            version = envtestVersion;
            src = pkgs.fetchurl {
              url = "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-v${envtestVersion}/envtest-v${envtestVersion}-darwin-arm64.tar.gz";
              hash = "sha256-80TnxwlhsQBHHu6k0lVQBvKCpqJ77Of0L77ed7KbiG4=";
            };
            sourceRoot = "controller-tools/envtest";
            dontConfigure = true;
            dontBuild = true;
            installPhase = ''
              mkdir -p $out
              install -m755 etcd kube-apiserver kubectl $out/
            '';
          };

          envtestAssets =
            if pkgs.stdenv.hostPlatform.isDarwin
            then envtestFromUpstream
            else envtestFromNixpkgs;
        in
```

Der Rest der Datei bleibt unverändert.

- [ ] **Schritt 3: Prüfen, dass die Shell auswertet und die Binaries da sind**

```bash
nix develop -c sh -c 'ls "$KUBEBUILDER_ASSETS" && "$KUBEBUILDER_ASSETS"/kube-apiserver --version'
```

Erwartet: die drei Dateinamen und `Kubernetes v1.36.2`.

- [ ] **Schritt 4: Einen bestehenden envtest-Test laufen lassen**

```bash
nix develop -c go test ./internal/rbacaudit/ -v
```

Erwartet: PASS für die envtest-gestützten Tests des Pakets. Das ist der Beweis, dass envtest auf dieser Plattform wirklich hochkommt und nicht nur die Binaries existieren.

Nimm hier bewusst **kein** `-run`-Muster: trifft das Muster keinen Test, endet `go test` still mit Exit 0 und „no tests to run" — ein bestandener Lauf, der nichts geprüft hat.

- [ ] **Schritt 5: Die ganze Suite**

```bash
nix develop -c make test
```

Erwartet: alle Pakete `ok`.

- [ ] **Schritt 6: Commit**

```bash
git add flake.nix
git commit -m "envtest-Assets laufen jetzt auch auf aarch64-darwin

nixpkgs baut kube-apiserver nicht für Darwin, deshalb scheiterte dort
schon die Auswertung von nix develop. Auf Darwin kommen die Binaries
jetzt aus den controller-tools-Releases, mit eingechecktem Hash; Linux
bleibt bei den nixpkgs-Paketen, weil die vorgebauten Linux-Binaries
autoPatchelfHook bräuchten."
```

---

### Task 2: Der Kontrakt — .proto und Codegenerierung

Entwurf Abschnitt 5. Die `.proto` bildet Spec 5.2 des Hauptentwurfs vollständig ab, auch die Proxy-Richtung, die erst Meilenstein 3 implementiert. Sie ist zugleich das Artefakt, gegen das Meilenstein 2b den Kotlin-Agent baut.

**Dateien:**
- Erstellen: `proto/spawnery/agent/v1alpha1/agent.proto`
- Erstellen: `internal/agentpb/agent.pb.go`, `internal/agentpb/agent_grpc.pb.go` (generiert, eingecheckt)
- Erstellen: `internal/agentpb/contract_test.go`
- Ändern: `flake.nix` (Werkzeuge), `Makefile` (Ziel `proto`), `go.mod`

**Schnittstellen:**
- Liefert: Go-Paket `github.com/spawnery/spawnery/internal/agentpb` mit `AgentServiceServer`, `UnimplementedAgentServiceServer`, `RegisterAgentServiceServer`, `NewAgentServiceClient`, den Nachrichtentypen und den `oneof`-Wrappern `ServerMessage_Hello`, `ServerMessage_Ready`, `ServerMessage_PlayerCount`, `OperatorToServer_ReportInterval`, `OperatorToServer_SessionDeadline`.

- [ ] **Schritt 1: Werkzeuge in die devShell**

In `flake.nix` in die `packages`-Liste aufnehmen, alphabetisch bei den anderen:

```nix
              protobuf
              protoc-gen-go
              protoc-gen-go-grpc
```

Prüfen:

```bash
nix develop -c sh -c 'protoc --version && protoc-gen-go --version && protoc-gen-go-grpc --version'
```

Erwartet: drei Versionszeilen, kein „command not found".

- [ ] **Schritt 2: Die .proto schreiben**

`proto/spawnery/agent/v1alpha1/agent.proto`:

```protobuf
// Copyright The Spawnery Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

syntax = "proto3";

package spawnery.agent.v1alpha1;

option go_package = "github.com/spawnery/spawnery/internal/agentpb";

// AgentService is the only channel between the operator and the in-game
// agents. The agents never read the Kubernetes API; this stream carries both
// directions instead.
service AgentService {
  // ProxySession is milestone 3. The operator answers UNIMPLEMENTED until
  // then.
  rpc ProxySession(stream ProxyMessage) returns (stream OperatorToProxy);

  // ServerSession is the Paper agent's channel.
  rpc ServerSession(stream ServerMessage) returns (stream OperatorToServer);
}

// ---- shared ----

// ReportInterval is how often the agent should report. The operator dictates
// it so both sides derive the staleness threshold from the same number.
message ReportInterval {
  int32 seconds = 1;
}

// SessionDeadline bounds the lifetime of one authenticated stream. The agent
// opens a fresh stream after renew_after_seconds — before the old one ends,
// so the operator never sees the pod as disconnected. hard_deadline_seconds
// is when the operator closes it regardless.
message SessionDeadline {
  int32 renew_after_seconds = 1;
  int32 hard_deadline_seconds = 2;
}

// Hello is the first message on every stream. It carries no identity: that
// comes from the bearer token alone.
message Hello {
  string version = 1;
  // ready is meaningful for server agents only. Readiness is a state, not an
  // event, so it is repeated on every connect.
  bool ready = 2;
}

// PlayerCount is the periodic report. Proxy agents leave slots at zero.
message PlayerCount {
  int32 players = 1;
  int32 slots = 2;
}

// ---- server direction ----

// Ready is the immediate notification that the server finished loading.
message Ready {}

message ServerMessage {
  oneof message {
    Hello hello = 1;
    Ready ready = 2;
    PlayerCount player_count = 3;
  }
}

message OperatorToServer {
  oneof message {
    ReportInterval report_interval = 1;
    SessionDeadline session_deadline = 2;
  }
}

// ---- proxy direction (milestone 3) ----

message Heartbeat {}

message PlayerJoinedServer {
  string player = 1;
  string server = 2;
}

message ProxyMessage {
  oneof message {
    Hello hello = 1;
    PlayerCount player_count = 2;
    PlayerJoinedServer player_joined_server = 3;
    Heartbeat heartbeat = 4;
  }
}

message RegisteredServer {
  string name = 1;
  string address = 2;
  string group = 3;
}

// FullSync carries exactly the registered servers, so a reconnect during a
// drain cannot undo a deregistration.
message FullSync {
  repeated RegisteredServer servers = 1;
}

message RegisterServer {
  RegisteredServer server = 1;
}

message UnregisterServer {
  string name = 1;
}

message DrainPlayers {
  string from_server = 1;
  repeated string to_groups = 2;
}

message OperatorToProxy {
  oneof message {
    FullSync full_sync = 1;
    RegisterServer register_server = 2;
    UnregisterServer unregister_server = 3;
    DrainPlayers drain_players = 4;
    ReportInterval report_interval = 5;
    SessionDeadline session_deadline = 6;
  }
}
```

- [ ] **Schritt 3: Das Makefile-Ziel**

In `Makefile` nach dem Ziel `generate` einfügen und `all` erweitern:

```makefile
.PHONY: proto
proto:
	protoc \
		--proto_path=proto \
		--go_out=. --go_opt=module=github.com/spawnery/spawnery \
		--go-grpc_out=. --go-grpc_opt=module=github.com/spawnery/spawnery \
		proto/spawnery/agent/v1alpha1/agent.proto
```

Und in der ersten Zeile `all: manifests generate fmt vet test build` das `proto` vor `manifests` setzen:

```makefile
all: proto manifests generate fmt vet test build
```

`test` hängt bewusst **nicht** von `proto` ab: der generierte Code ist eingecheckt, und ein Test soll nicht stillschweigend neu generieren.

- [ ] **Schritt 4: Generieren und die Abhängigkeiten ziehen**

```bash
nix develop -c make proto
nix develop -c go get google.golang.org/grpc@latest
nix develop -c go mod tidy
```

Erwartet: `internal/agentpb/agent.pb.go` und `internal/agentpb/agent_grpc.pb.go` existieren, `go.mod` führt `google.golang.org/grpc` als direkte Abhängigkeit.

- [ ] **Schritt 5: Den Vertragstest schreiben**

`internal/agentpb/contract_test.go` (mit Apache-Kopf):

```go
package agentpb_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// The service name is part of the wire contract: the Kotlin agent of
// milestone 2b addresses exactly this string.
func TestServiceName(t *testing.T) {
	if got := agentpb.AgentService_ServiceDesc.ServiceName; got != "spawnery.agent.v1alpha1.AgentService" {
		t.Errorf("ServiceName = %q", got)
	}
}

func TestBothStreamsAreBidirectional(t *testing.T) {
	for _, s := range agentpb.AgentService_ServiceDesc.Streams {
		if !s.ClientStreams || !s.ServerStreams {
			t.Errorf("%s is not bidirectional: client=%v server=%v",
				s.StreamName, s.ClientStreams, s.ServerStreams)
		}
	}
	if len(agentpb.AgentService_ServiceDesc.Streams) != 2 {
		t.Errorf("got %d streams, want ProxySession and ServerSession",
			len(agentpb.AgentService_ServiceDesc.Streams))
	}
}

// An unknown oneof branch must survive a round trip untouched, because a
// newer agent talking to an older operator has to keep working.
func TestServerMessageRoundTrip(t *testing.T) {
	in := &agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_PlayerCount{
			PlayerCount: &agentpb.PlayerCount{Players: 7, Slots: 100},
		},
	}
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := &agentpb.ServerMessage{}
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := out.GetMessage().(*agentpb.ServerMessage_PlayerCount)
	if !ok {
		t.Fatalf("branch = %T, want PlayerCount", out.GetMessage())
	}
	if got.PlayerCount.GetPlayers() != 7 || got.PlayerCount.GetSlots() != 100 {
		t.Errorf("got %d/%d, want 7/100", got.PlayerCount.GetPlayers(), got.PlayerCount.GetSlots())
	}
}
```

- [ ] **Schritt 6: Test laufen lassen**

```bash
nix develop -c go test ./internal/agentpb/ -v
```

Erwartet: PASS. Schlägt er mit „undefined: agentpb.AgentService_ServiceDesc" fehl, ist Schritt 4 nicht gelaufen.

- [ ] **Schritt 7: Prüfen, dass der eingecheckte Code aktuell ist**

```bash
nix develop -c make proto && git diff --exit-code internal/agentpb/
```

Erwartet: kein Diff, Exit 0. Das ist dieselbe Zusage, die `make manifests` für die CRDs gibt.

- [ ] **Schritt 8: Commit**

```bash
git add proto/ internal/agentpb/ Makefile flake.nix go.mod go.sum
git commit -m "Der gRPC-Kontrakt für die Agents

Bildet Spec 5.2 vollständig ab, auch die Proxy-Richtung aus Meilenstein
3: Feldnummern später zu ändern ist teurer als ein paar heute ungenutzte
Nachrichten, und die Datei ist zugleich das Artefakt, gegen das der
Kotlin-Agent gebaut wird. Neu gegenüber der Spec ist SessionDeadline —
die Begründung steht in Abschnitt 7.1 des Entwurfs."
```

---

### Task 3: Zertifikate ausstellen und erneuern (reine Logik)

Entwurf Abschnitt 6.2. Dieser Task enthält keine Cluster-Zugriffe, damit jede Regel über Laufzeiten und SANs mit einer gestellten Uhr prüfbar ist.

**Dateien:**
- Erstellen: `internal/certs/bundle.go`
- Erstellen: `internal/certs/bundle_test.go`

**Schnittstellen:**
- Liefert:
  - `type Bundle struct { CACertPEM, CAKeyPEM, ServingCertPEM, ServingKeyPEM []byte }`
  - `func Issue(now time.Time, dnsNames []string) (*Bundle, error)` — neue CA **und** neues Serving-Zertifikat
  - `func Reissue(now time.Time, b *Bundle, dnsNames []string) (*Bundle, error)` — neues Serving-Zertifikat aus der vorhandenen CA
  - `func (b *Bundle) TLSCertificate() (tls.Certificate, error)`
  - `func (b *Bundle) NeedsRenewal(now time.Time) bool`
  - `func (b *Bundle) Validate(now time.Time, dnsNames []string) error`
  - Konstanten `CALifetime = 10 * 365 * 24 * time.Hour`, `ServingLifetime = 90 * 24 * time.Hour`
  - `func ServingDNSNames(service, namespace string) []string`

- [ ] **Schritt 1: Die fehlschlagenden Tests schreiben**

`internal/certs/bundle_test.go` (mit Apache-Kopf):

```go
package certs

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func testDNSNames() []string { return ServingDNSNames("spawnery-operator", "spawnery-system") }

func parseServing(t *testing.T, b *Bundle) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(b.ServingCertPEM)
	if block == nil {
		t.Fatal("serving certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse serving certificate: %v", err)
	}
	return cert
}

func TestServingDNSNamesCoverEveryWayToReachTheService(t *testing.T) {
	got := ServingDNSNames("spawnery-operator", "spawnery-system")
	want := []string{
		"spawnery-operator",
		"spawnery-operator.spawnery-system",
		"spawnery-operator.spawnery-system.svc",
		"spawnery-operator.spawnery-system.svc.cluster.local",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIssueSetsTheSANsAndLifetimes(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cert := parseServing(t, b)

	if len(cert.DNSNames) != 4 {
		t.Errorf("DNSNames = %v", cert.DNSNames)
	}
	if got := cert.NotAfter.Sub(cert.NotBefore); got != ServingLifetime {
		t.Errorf("serving lifetime = %v, want %v", got, ServingLifetime)
	}
	// A minute of backdating absorbs clock skew between nodes.
	if !cert.NotBefore.Before(testNow) {
		t.Errorf("NotBefore = %v, want it backdated before %v", cert.NotBefore, testNow)
	}
}

// The whole point of the pinned CA: an agent that trusts only ca.crt must
// accept the serving certificate.
func TestServingCertificateChainsToTheCA(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CACertPEM) {
		t.Fatal("CA is not usable as a pool")
	}
	_, err = parseServing(t, b).Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: testNow.Add(24 * time.Hour),
		DNSName:     "spawnery-operator.spawnery-system.svc",
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Errorf("verify against the pinned CA: %v", err)
	}
}

func TestNeedsRenewalAtOneThirdRemaining(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"fresh", testNow, false},
		{"just above the threshold", testNow.Add(ServingLifetime*2/3 - time.Hour), false},
		{"just below the threshold", testNow.Add(ServingLifetime*2/3 + time.Hour), true},
		{"expired", testNow.Add(ServingLifetime + time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.NeedsRenewal(tc.at); got != tc.want {
				t.Errorf("NeedsRenewal(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// Renewal must not change the CA — every running agent has it pinned.
func TestReissueKeepsTheCA(t *testing.T) {
	first, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	later := testNow.Add(80 * 24 * time.Hour)
	second, err := Reissue(later, first, testDNSNames())
	if err != nil {
		t.Fatalf("Reissue: %v", err)
	}

	if string(second.CACertPEM) != string(first.CACertPEM) {
		t.Error("Reissue replaced the CA; every connected agent would break")
	}
	if string(second.ServingCertPEM) == string(first.ServingCertPEM) {
		t.Error("Reissue did not replace the serving certificate")
	}
	if second.NeedsRenewal(later) {
		t.Error("the freshly reissued certificate already wants renewal")
	}
}

func TestValidateRejectsWhatMustLeadToReissue(t *testing.T) {
	good, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(b *Bundle)
		at     time.Time
	}{
		{"garbage instead of a certificate", func(b *Bundle) { b.ServingCertPEM = []byte("nope") }, testNow},
		{"CA missing", func(b *Bundle) { b.CACertPEM = nil }, testNow},
		{"CA key missing", func(b *Bundle) { b.CAKeyPEM = nil }, testNow},
		{"expired", func(b *Bundle) {}, testNow.Add(ServingLifetime + time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bundle{
				CACertPEM: good.CACertPEM, CAKeyPEM: good.CAKeyPEM,
				ServingCertPEM: good.ServingCertPEM, ServingKeyPEM: good.ServingKeyPEM,
			}
			tc.mutate(b)
			if err := b.Validate(tc.at, testDNSNames()); err == nil {
				t.Error("Validate accepted a bundle that must be reissued")
			}
		})
	}

	if err := good.Validate(testNow, testDNSNames()); err != nil {
		t.Errorf("Validate rejected a good bundle: %v", err)
	}
}

// A service moved to another namespace changes the SANs; the old certificate
// is then useless even though it has not expired.
func TestValidateRejectsMissingSAN(t *testing.T) {
	b, err := Issue(testNow, ServingDNSNames("spawnery-operator", "spawnery-system"))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := b.Validate(testNow, ServingDNSNames("spawnery-operator", "andere-ns")); err == nil {
		t.Error("Validate accepted a certificate without the required SAN")
	}
}

func TestTLSCertificateIsUsable(t *testing.T) {
	b, err := Issue(testNow, testDNSNames())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	tlsCert, err := b.TLSCertificate()
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	if len(tlsCert.Certificate) == 0 || tlsCert.PrivateKey == nil {
		t.Error("TLSCertificate returned an empty pair")
	}
}
```

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/certs/ -v
```

Erwartet: Übersetzungsfehler „undefined: Issue", „undefined: Bundle" und so weiter.

- [ ] **Schritt 3: Implementieren**

`internal/certs/bundle.go` (mit Apache-Kopf), Paketkommentar zuerst:

```go
// Package certs issues and renews the operator's own serving certificate. The
// operator is its own CA on purpose: it creates the agent pods anyway, so it
// can pin its CA into them, and "one helm install is enough" survives without
// cert-manager.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"time"
)

const (
	// CALifetime is long because rotating the CA needs an overlap phase that
	// milestone 2a does not build.
	CALifetime = 10 * 365 * 24 * time.Hour
	// ServingLifetime is short because renewing it is automatic and costs no
	// connection.
	ServingLifetime = 90 * 24 * time.Hour

	// backdate absorbs clock skew between the operator and the agents' nodes.
	backdate = time.Minute
)

// Bundle is everything the operator keeps in its TLS secret.
type Bundle struct {
	CACertPEM      []byte
	CAKeyPEM       []byte
	ServingCertPEM []byte
	ServingKeyPEM  []byte
}

// ServingDNSNames are the names an agent may use to reach the service.
func ServingDNSNames(service, namespace string) []string {
	return []string{
		service,
		service + "." + namespace,
		service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local",
	}
}

// Issue creates a fresh CA and a serving certificate signed by it.
func Issue(now time.Time, dnsNames []string) (*Bundle, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "spawnery-agent-ca"},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("self-sign CA: %w", err)
	}
	caKeyPEM, err := encodeKey(caKey)
	if err != nil {
		return nil, err
	}

	b := &Bundle{
		CACertPEM: encodeCert(caDER),
		CAKeyPEM:  caKeyPEM,
	}
	return Reissue(now, b, dnsNames)
}

// Reissue replaces only the serving certificate. The CA stays, because every
// running agent has it pinned.
func Reissue(now time.Time, b *Bundle, dnsNames []string) (*Bundle, error) {
	caCert, caKey, err := b.parseCA()
	if err != nil {
		return nil, err
	}
	servingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate serving key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     slices.Clone(dnsNames),
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(ServingLifetime - backdate),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign serving certificate: %w", err)
	}
	keyPEM, err := encodeKey(servingKey)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		CACertPEM:      b.CACertPEM,
		CAKeyPEM:       b.CAKeyPEM,
		ServingCertPEM: encodeCert(der),
		ServingKeyPEM:  keyPEM,
	}, nil
}

// NeedsRenewal is true once less than a third of the lifetime is left.
func (b *Bundle) NeedsRenewal(now time.Time) bool {
	cert, err := b.parseServing()
	if err != nil {
		return true
	}
	total := cert.NotAfter.Sub(cert.NotBefore)
	return now.After(cert.NotAfter.Add(-total / 3))
}

// Validate reports why a bundle read back from the secret cannot be used.
func (b *Bundle) Validate(now time.Time, dnsNames []string) error {
	if len(b.CACertPEM) == 0 || len(b.CAKeyPEM) == 0 {
		return fmt.Errorf("bundle has no CA")
	}
	if _, _, err := b.parseCA(); err != nil {
		return err
	}
	cert, err := b.parseServing()
	if err != nil {
		return err
	}
	if now.After(cert.NotAfter) || now.Before(cert.NotBefore) {
		return fmt.Errorf("serving certificate is not valid at %s", now)
	}
	for _, name := range dnsNames {
		if !slices.Contains(cert.DNSNames, name) {
			return fmt.Errorf("serving certificate lacks the SAN %q", name)
		}
	}
	// Tie the leaf to the CA stored next to it. Without this a bundle whose
	// halves come from different issues — a partial write, a hand-edited
	// secret — passes validation and fails at handshake time instead, when an
	// agent that pinned this CA refuses the connection.
	if err := cert.CheckSignatureFrom(caCert); err != nil {
		return fmt.Errorf("serving certificate was not signed by the stored CA: %w", err)
	}
	if _, err := b.TLSCertificate(); err != nil {
		return err
	}
	return nil
}

// TLSCertificate is the pair the gRPC server serves.
func (b *Bundle) TLSCertificate() (tls.Certificate, error) {
	return tls.X509KeyPair(b.ServingCertPEM, b.ServingKeyPEM)
}

func (b *Bundle) parseCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(b.CACertPEM)
	keyBlock, _ := pem.Decode(b.CAKeyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, fmt.Errorf("CA is not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	// Both halves parsing is not enough: if they are not a pair, Reissue signs
	// with a key the stored CA certificate does not match, Validate rejects
	// the result, and the repair in Store.Ensure runs in circles.
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, fmt.Errorf("CA key does not belong to the CA certificate")
	}
	return cert, key, nil
}

func (b *Bundle) parseServing() (*x509.Certificate, error) {
	block, _ := pem.Decode(b.ServingCertPEM)
	if block == nil {
		return nil, fmt.Errorf("serving certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse serving certificate: %w", err)
	}
	return cert, nil
}

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("draw serial number: %w", err)
	}
	return serial, nil
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
```

- [ ] **Schritt 4: Tests laufen lassen**

```bash
nix develop -c go test ./internal/certs/ -v
```

Erwartet: PASS, alle Unterfälle.

- [ ] **Schritt 5: Commit**

```bash
git add internal/certs/
git commit -m "Der Operator stellt sein Serving-Zertifikat selbst aus

Reine Kryptologik mit gestellter Uhr: SANs, Laufzeiten, die
Erneuerungsschwelle bei einem Drittel Restlaufzeit und die Zusage, dass
eine Erneuerung die CA nie austauscht — sonst bräche jeder Agent, der
sie gepinnt hat."
```

---

### Task 4: Das Zertifikat im Secret halten

Entwurf Abschnitt 6.2. Jetzt kommt der Cluster dazu: Ablegen, Wiederfinden, Erneuern zur Laufzeit.

**Dateien:**
- Erstellen: `internal/certs/store.go`
- Erstellen: `internal/certs/store_envtest_test.go`
- Erstellen: `internal/certs/main_test.go` (nur `TestMain` mit `testenv.Stop`)

**Schnittstellen:**
- Verbraucht: `Bundle`, `Issue`, `Reissue`, `Validate`, `NeedsRenewal`, `ServingDNSNames` aus Task 3.
- Liefert:
  - `type Store struct { Client client.Client; Namespace, Name string; DNSNames []string; Clock func() time.Time }`
  - `func (s *Store) Ensure(ctx context.Context) (*Bundle, error)` — anlegen, lesen, bei Bedarf erneuern; idempotent
  - `type Provider struct { … }` mit `func NewProvider(s *Store) *Provider`, `func (p *Provider) Set(b *Bundle) error`, `func (p *Provider) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)`, `func (p *Provider) CABundle() []byte`
  - `func (p *Provider) Start(ctx context.Context) error` — das stündliche Runnable
  - `const SecretName = "spawnery-agent-tls"`, `const RenewCheckInterval = time.Hour`

- [ ] **Schritt 1: Die fehlschlagenden Tests schreiben**

`internal/certs/store_envtest_test.go` (mit Apache-Kopf):

```go
package certs_test

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/testenv"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newStore(t *testing.T) (*certs.Store, *testClock, context.Context, string) {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	clock := &testClock{now: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}
	return &certs.Store{
		Client:    c,
		Namespace: ns,
		Name:      certs.SecretName,
		DNSNames:  certs.ServingDNSNames("spawnery-operator", ns),
		Clock:     clock.Now,
	}, clock, ctx, ns
}

func TestEnsureCreatesTheSecret(t *testing.T) {
	s, _, ctx, ns := newStore(t)

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(b.CACertPEM) == 0 || len(b.ServingCertPEM) == 0 {
		t.Fatal("Ensure returned an empty bundle")
	}

	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: certs.SecretName, Namespace: ns}, secret); err != nil {
		t.Fatalf("the secret was not written: %v", err)
	}
	for _, key := range []string{"ca.crt", "ca.key", "tls.crt", "tls.key"} {
		if len(secret.Data[key]) == 0 {
			t.Errorf("secret key %q is empty", key)
		}
	}
	if secret.Type != corev1.SecretTypeTLS {
		t.Errorf("secret type = %q, want %q", secret.Type, corev1.SecretTypeTLS)
	}
}

// This is the restart: a second Ensure must not mint a new CA, or every agent
// pinned to the old one would stop trusting the operator.
func TestEnsureIsIdempotent(t *testing.T) {
	s, _, ctx, _ := newStore(t)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	if string(second.CACertPEM) != string(first.CACertPEM) {
		t.Error("the second Ensure replaced the CA")
	}
	if string(second.ServingCertPEM) != string(first.ServingCertPEM) {
		t.Error("the second Ensure replaced a still-valid serving certificate")
	}
}

func TestEnsureRenewsUnderTheThreshold(t *testing.T) {
	s, clock, ctx, _ := newStore(t)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	clock.Advance(certs.ServingLifetime*2/3 + time.Hour)
	second, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	if string(second.ServingCertPEM) == string(first.ServingCertPEM) {
		t.Error("Ensure did not renew below the threshold")
	}
	if string(second.CACertPEM) != string(first.CACertPEM) {
		t.Error("the renewal replaced the CA")
	}
}

func TestEnsureRepairsACorruptSecret(t *testing.T) {
	s, _, ctx, ns := newStore(t)

	if _, err := s.Ensure(ctx); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: certs.SecretName, Namespace: ns}, secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	secret.Data["tls.crt"] = []byte("kaputt")
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("update secret: %v", err)
	}

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure over a corrupt secret: %v", err)
	}
	if err := b.Validate(s.Clock(), s.DNSNames); err != nil {
		t.Errorf("the repaired bundle is still broken: %v", err)
	}
}

// The gRPC server asks the provider on every handshake, so a renewal takes
// effect without a restart and without dropping a connection.
func TestProviderServesTheCurrentCertificate(t *testing.T) {
	s, _, ctx, _ := newStore(t)
	p := certs.NewProvider(s)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := p.Set(first); err != nil {
		t.Fatalf("Set: %v", err)
	}

	before, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	renewed, err := certs.Reissue(s.Clock(), first, s.DNSNames)
	if err != nil {
		t.Fatalf("Reissue: %v", err)
	}
	if err := p.Set(renewed); err != nil {
		t.Fatalf("Set after renewal: %v", err)
	}

	after, err := p.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after renewal: %v", err)
	}
	if string(before.Certificate[0]) == string(after.Certificate[0]) {
		t.Error("the provider still serves the old certificate")
	}
	if string(p.CABundle()) != string(first.CACertPEM) {
		t.Error("the CA bundle changed on renewal")
	}
}

func TestGetCertificateBeforeSetFails(t *testing.T) {
	s, _, _, _ := newStore(t)
	p := certs.NewProvider(s)

	if _, err := p.GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Error("the provider handed out a certificate before it had one")
	}
}
```

`internal/certs/main_test.go` (mit Apache-Kopf):

```go
package certs_test

import (
	"os"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}
```

Hinweis: `bundle_test.go` aus Task 3 liegt in Paket `certs`, diese Datei in `certs_test`. Beides im selben Verzeichnis ist erlaubt und gewollt — die reine Logik wird von innen geprüft, die Cluster-Anbindung von außen über die öffentliche Schnittstelle.

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/certs/ -run 'TestEnsure|TestProvider|TestGetCertificate' -v
```

Erwartet: „undefined: certs.Store", „undefined: certs.NewProvider".

- [ ] **Schritt 3: Implementieren**

`internal/certs/store.go` (mit Apache-Kopf):

```go
package certs

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// SecretName holds the CA and the serving certificate in the operator's
	// own namespace.
	SecretName = "spawnery-agent-tls"
	// RenewCheckInterval is how often the provider looks at the clock. The
	// serving certificate lives 90 days, so hourly is generous.
	RenewCheckInterval = time.Hour
)

// Store reads and writes the bundle. It never caches: the manager's client
// does that, and a stale bundle would be worse than a read.
type Store struct {
	Client    client.Client
	Namespace string
	Name      string
	DNSNames  []string
	Clock     func() time.Time
}

// Ensure returns a usable bundle, creating or renewing it if needed. Safe to
// call repeatedly; only the leader ever does.
func (s *Store) Ensure(ctx context.Context) (*Bundle, error) {
	now := s.Clock()

	secret := &corev1.Secret{}
	err := s.Client.Get(ctx, types.NamespacedName{Name: s.Name, Namespace: s.Namespace}, secret)
	switch {
	case apierrors.IsNotFound(err):
		bundle, err := Issue(now, s.DNSNames)
		if err != nil {
			return nil, err
		}
		if err := s.Client.Create(ctx, s.secretFor(bundle)); err != nil {
			return nil, fmt.Errorf("create %s: %w", s.Name, err)
		}
		return bundle, nil
	case err != nil:
		return nil, fmt.Errorf("get %s: %w", s.Name, err)
	}

	bundle := &Bundle{
		CACertPEM:      secret.Data["ca.crt"],
		CAKeyPEM:       secret.Data["ca.key"],
		ServingCertPEM: secret.Data["tls.crt"],
		ServingKeyPEM:  secret.Data["tls.key"],
	}

	switch {
	case bundle.Validate(now, s.DNSNames) != nil:
		// Unusable for whatever reason — a truncated write, a hand-edited
		// secret, an expired certificate. Start over rather than guess which.
		reason := bundle.Validate(now, s.DNSNames)
		log.FromContext(ctx).Info("reissuing the TLS bundle", "reason", reason.Error())
		fresh, err := s.reissueOrIssue(now, bundle)
		if err != nil {
			return nil, err
		}
		return fresh, s.write(ctx, secret, fresh)
	case bundle.NeedsRenewal(now):
		fresh, err := Reissue(now, bundle, s.DNSNames)
		if err != nil {
			return nil, err
		}
		return fresh, s.write(ctx, secret, fresh)
	}
	return bundle, nil
}

// reissueOrIssue keeps the CA if it is still intact, so agents that pinned it
// survive a damaged serving certificate.
func (s *Store) reissueOrIssue(now time.Time, broken *Bundle) (*Bundle, error) {
	if _, _, err := broken.parseCA(); err == nil {
		return Reissue(now, broken, s.DNSNames)
	}
	return Issue(now, s.DNSNames)
}

func (s *Store) secretFor(b *Bundle) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":  b.CACertPEM,
			"ca.key":  b.CAKeyPEM,
			"tls.crt": b.ServingCertPEM,
			"tls.key": b.ServingKeyPEM,
		},
	}
}

func (s *Store) write(ctx context.Context, existing *corev1.Secret, b *Bundle) error {
	existing.Type = corev1.SecretTypeTLS
	existing.Data = s.secretFor(b).Data
	if err := s.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("update %s: %w", s.Name, err)
	}
	return nil
}

// generation is the certificate and the CA it chains to, published together.
// Two separate atomics would let a reader see a fresh certificate beside a
// stale CA — narrow, but it breaks the one promise this type makes.
type generation struct {
	cert tls.Certificate
	ca   []byte
}

// Provider hands the current certificate to the TLS stack and renews it in the
// background.
type Provider struct {
	store   *Store
	current atomic.Pointer[generation]
}

// NewProvider wires a provider to a store.
func NewProvider(s *Store) *Provider { return &Provider{store: s} }

// Set publishes a bundle. The next handshake uses it; running connections keep
// the one they negotiated.
func (p *Provider) Set(b *Bundle) error {
	cert, err := b.TLSCertificate()
	if err != nil {
		return err
	}
	p.current.Store(&generation{cert: cert, ca: b.CACertPEM})
	return nil
}

// GetCertificate is the tls.Config callback.
func (p *Provider) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	g := p.current.Load()
	if g == nil {
		return nil, fmt.Errorf("no serving certificate yet")
	}
	return &g.cert, nil
}

// CABundle is what the agents pin. It is a bundle, not a single certificate,
// so a later rotation can publish old and new side by side.
func (p *Provider) CABundle() []byte {
	g := p.current.Load()
	if g == nil {
		return nil
	}
	return g.ca
}

// Start ensures a bundle once and then checks hourly. It is a leader-bound
// Runnable: only the leader may write the secret.
func (p *Provider) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("certs")

	bundle, err := p.store.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("ensure the TLS bundle: %w", err)
	}
	if err := p.Set(bundle); err != nil {
		return err
	}
	logger.Info("serving certificate ready")

	ticker := time.NewTicker(RenewCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			bundle, err := p.store.Ensure(ctx)
			if err != nil {
				// Keep serving the old certificate; it is still valid for a
				// third of its lifetime.
				logger.Error(err, "renewal failed, keeping the current certificate")
				continue
			}
			if err := p.Set(bundle); err != nil {
				logger.Error(err, "the renewed bundle is unusable")
			}
		}
	}
}

// NeedLeaderElection makes this a leader-bound runnable.
func (p *Provider) NeedLeaderElection() bool { return true }
```

- [ ] **Schritt 4: Tests laufen lassen**

```bash
nix develop -c go test ./internal/certs/ -v
```

Erwartet: PASS, auch die Tests aus Task 3.

- [ ] **Schritt 5: Commit**

```bash
git add internal/certs/
git commit -m "Das TLS-Bündel überlebt den Neustart im Secret

Ensure legt an, liest wieder, erneuert unter der Schwelle und repariert
ein verfälschtes Secret — behält dabei aber die CA, solange sie lesbar
ist. Der Provider reicht das Zertifikat über GetCertificate heraus, also
kostet eine Erneuerung weder Neustart noch Verbindung."
```

---

### Task 5: Die Identität aus dem Token

Entwurf Abschnitt 6.3. Der Kern des Meilensteins. Die Ablehnungen sind hier die eigentliche Aussage: ein Test, der nur den guten Fall prüft, bliebe grün, wenn der Interceptor jeden durchließe.

**Dateien:**
- Erstellen: `internal/grpcauth/identity.go`
- Erstellen: `internal/grpcauth/interceptor.go`
- Erstellen: `internal/grpcauth/auth_envtest_test.go`
- Erstellen: `internal/grpcauth/main_test.go` (`TestMain` wie in Task 4)

**Schnittstellen:**
- Verbraucht: `podspec.AgentTokenAudience`, `podspec.ServerServiceAccountName`, `podspec.ProxyServiceAccountName`, `podspec.LabelRole` — diese Konstanten legt Task 8 in `podspec` an. Bis dahin definiere sie dort **jetzt schon** in einer eigenen Datei `internal/podspec/agent.go`; Task 8 baut nur darauf auf.
- Liefert:
  - `type Identity struct { Namespace, PodName, PodUID, ServiceAccount string; Role agent.Role }`
  - `func IdentityFrom(ctx context.Context) (Identity, bool)`
  - `type PodChecker interface { PodExists(ctx context.Context, namespace, name, uid string) (bool, error) }`
  - `type TokenReviewer interface { Create(ctx context.Context, tr *authnv1.TokenReview, opts metav1.CreateOptions) (*authnv1.TokenReview, error) }` — bewusst schmal, damit ein unerreichbarer API-Server ohne Cluster prüfbar ist. `authnclient.TokenReviewInterface` erfüllt sie.
  - `type Authenticator struct { Reviews TokenReviewer; Pods PodChecker; Audience string }`
  - `func (a *Authenticator) Authenticate(ctx context.Context, token string, want agent.Role) (Identity, error)`
  - `func (a *Authenticator) StreamInterceptor() grpc.StreamServerInterceptor`
  - `func RoleForMethod(fullMethod string) (agent.Role, bool)`
  - `type ClientPodChecker struct { Client client.Client }` als Umsetzung von `PodChecker` über den Manager-Cache

- [ ] **Schritt 1: Die fehlschlagenden Tests schreiben**

`internal/grpcauth/auth_envtest_test.go` (mit Apache-Kopf):

```go
package grpcauth_test

import (
	"context"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

type authFixture struct {
	t    *testing.T
	ctx  context.Context
	c    client.Client
	cs   *kubernetes.Clientset
	ns   string
	auth *grpcauth.Authenticator
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	cs, err := kubernetes.NewForConfig(testenv.Config(t))
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if err := c.Create(ctx, sa); err != nil {
			t.Fatalf("create ServiceAccount %s: %v", name, err)
		}
	}
	return &authFixture{
		t: t, ctx: ctx, c: c, cs: cs, ns: ns,
		auth: &grpcauth.Authenticator{
			Reviews:  cs.AuthenticationV1().TokenReviews(),
			Pods:     &grpcauth.ClientPodChecker{Client: c},
			Audience: podspec.AgentTokenAudience,
		},
	}
}

// pod creates a managed server pod and returns it.
func (f *authFixture) pod(name string) *corev1.Pod {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			Labels:    podspec.ServerLabels("production", "lobby", name),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ServerServiceAccountName,
			Containers:         []corev1.Container{{Name: "minecraft", Image: "example/paper:1"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		f.t.Fatalf("create pod: %v", err)
	}
	return pod
}

// token mints a token the way the kubelet would.
func (f *authFixture) token(sa string, audiences []string, boundTo *corev1.Pod) string {
	f.t.Helper()
	spec := authnv1.TokenRequestSpec{
		Audiences:         audiences,
		ExpirationSeconds: ptr.To(int64(600)),
	}
	if boundTo != nil {
		spec.BoundObjectRef = &authnv1.BoundObjectReference{
			Kind: "Pod", APIVersion: "v1", Name: boundTo.Name, UID: boundTo.UID,
		}
	}
	tr, err := f.cs.CoreV1().ServiceAccounts(f.ns).CreateToken(f.ctx, sa,
		&authnv1.TokenRequest{Spec: spec}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("TokenRequest for %s: %v", sa, err)
	}
	return tr.Status.Token
}

func TestAcceptsAPodBoundServerToken(t *testing.T) {
	f := newAuthFixture(t)
	pod := f.pod("lobby-abcd")

	id, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, pod), agent.RoleServer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if id.PodName != pod.Name {
		t.Errorf("PodName = %q, want %q", id.PodName, pod.Name)
	}
	if id.PodUID != string(pod.UID) {
		t.Errorf("PodUID = %q, want %q", id.PodUID, pod.UID)
	}
	if id.Namespace != f.ns {
		t.Errorf("Namespace = %q, want %q", id.Namespace, f.ns)
	}
	if id.Role != agent.RoleServer {
		t.Errorf("Role = %q, want %q", id.Role, agent.RoleServer)
	}
}

// Each of these must be refused. Without them the audit says nothing.
func TestRejections(t *testing.T) {
	cases := []struct {
		name    string
		token   func(f *authFixture) string
		role    agent.Role
		wantErr string
	}{
		{
			name: "token without an audience",
			token: func(f *authFixture) string {
				return f.token(podspec.ServerServiceAccountName, nil, f.pod("lobby-noaud"))
			},
			role:    agent.RoleServer,
			wantErr: "not authenticated",
		},
		{
			name: "token for another audience",
			token: func(f *authFixture) string {
				return f.token(podspec.ServerServiceAccountName, []string{"etwas-anderes"}, f.pod("lobby-otheraud"))
			},
			role:    agent.RoleServer,
			wantErr: "not authenticated",
		},
		{
			name: "token not bound to a pod",
			token: func(f *authFixture) string {
				return f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, nil)
			},
			role:    agent.RoleServer,
			wantErr: "not bound to a pod",
		},
		{
			name: "proxy token on a server session",
			token: func(f *authFixture) string {
				pod := f.pod("lobby-proxysa")
				return f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod)
			},
			role:    agent.RoleServer,
			wantErr: "service account",
		},
		{
			name:    "garbage instead of a token",
			token:   func(f *authFixture) string { return "nicht.ein.token" },
			role:    agent.RoleServer,
			wantErr: "not authenticated",
		},
		{
			name:    "empty token",
			token:   func(f *authFixture) string { return "" },
			role:    agent.RoleServer,
			wantErr: "no token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			_, err := f.auth.Authenticate(f.ctx, tc.token(f), tc.role)
			if err == nil {
				t.Fatal("Authenticate accepted the token")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// Defence in depth: a hand-built pod using the same ServiceAccount can never
// speak for another server, but it must not be able to fill the registry with
// entries that have no CR behind them either.
func TestRejectsAPodThatIsNotOurs(t *testing.T) {
	f := newAuthFixture(t)

	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fremd", Namespace: f.ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ServerServiceAccountName,
			Containers:         []corev1.Container{{Name: "c", Image: "example/x:1"}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	_, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, foreign), agent.RoleServer)
	if err == nil {
		t.Fatal("Authenticate accepted a pod without the spawnery role label")
	}
	if !strings.Contains(err.Error(), "pod") {
		t.Errorf("error = %q, want it to mention the pod", err)
	}
}

// An unreachable API server must look different from a refused token: the
// agent should back off and retry, not conclude its credentials are wrong.
// This is the one case that needs no cluster, hence the narrow TokenReviewer.
func TestTokenReviewUnavailableIsNotARejection(t *testing.T) {
	a := &grpcauth.Authenticator{
		Reviews:  failingReviewer{},
		Pods:     refusingPodChecker{},
		Audience: podspec.AgentTokenAudience,
	}

	_, err := a.Authenticate(context.Background(), "irgendein-token", agent.RoleServer)
	if err == nil {
		t.Fatal("Authenticate succeeded although the API server was unreachable")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("error = %q, want it to name the outage rather than the token", err)
	}
	// The pod checker must never be reached — the review failed first.
}

type failingReviewer struct{}

func (failingReviewer) Create(context.Context, *authnv1.TokenReview, metav1.CreateOptions) (
	*authnv1.TokenReview, error) {
	return nil, errors.New("connection refused")
}

type refusingPodChecker struct{}

func (refusingPodChecker) PodExists(context.Context, string, string, string) (bool, error) {
	return false, errors.New("the pod checker must not be reached")
}

func TestRoleForMethod(t *testing.T) {
	cases := []struct {
		method string
		want   agent.Role
		ok     bool
	}{
		{"/spawnery.agent.v1alpha1.AgentService/ServerSession", agent.RoleServer, true},
		{"/spawnery.agent.v1alpha1.AgentService/ProxySession", agent.RoleProxy, true},
		{"/spawnery.agent.v1alpha1.AgentService/Irgendwas", "", false},
	}
	for _, tc := range cases {
		got, ok := grpcauth.RoleForMethod(tc.method)
		if ok != tc.ok || got != tc.want {
			t.Errorf("RoleForMethod(%q) = %q,%v want %q,%v", tc.method, got, ok, tc.want, tc.ok)
		}
	}
}
```

Und `internal/grpcauth/main_test.go` mit demselben `TestMain` wie in Task 4, nur mit Paketnamen `grpcauth_test`.

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/grpcauth/ -v
```

Erwartet: „undefined: grpcauth.Authenticator" und weitere.

- [ ] **Schritt 3: Die Identität implementieren**

`internal/grpcauth/identity.go` (mit Apache-Kopf):

```go
// Package grpcauth turns a bearer token into the identity of exactly one pod.
//
// The identity never comes from the message. If a compromised server could
// name itself in Hello, it could report PlayerCount{0} for a full server and
// have it deleted — a direct breach of the core invariant.
package grpcauth

import (
	"context"
	"fmt"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	authnclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

// unavailableErr marks a failure to REACH the API server, as opposed to a
// refusal of the credentials. The interceptor turns the first into
// codes.Unavailable and the second into codes.Unauthenticated; an agent that
// cannot tell them apart would treat an outage as a bad token and stop
// retrying. Unexported with an Unwrap and an isUnavailable helper — only this
// package ever classifies.
type unavailableErr struct{ err error }

func (e *unavailableErr) Error() string { return e.err.Error() }
func (e *unavailableErr) Unwrap() error { return e.err }

func wrapUnavailable(err error) error { return &unavailableErr{err} }

func isUnavailable(err error) bool {
	var u *unavailableErr
	return errors.As(err, &u)
}

const (
	claimPodName = "authentication.kubernetes.io/pod-name"
	claimPodUID  = "authentication.kubernetes.io/pod-uid"

	saPrefix = "system:serviceaccount:"
)

// Identity is who is on the other end of a stream.
type Identity struct {
	Namespace      string
	PodName        string
	// PodUID is the registry key — Lookup and Forget are keyed on it.
	PodUID         string
	ServiceAccount string
	Role           agent.Role
}

// PodChecker answers whether a pod the token names is one of ours, in the
// role the caller is asking for.
type PodChecker interface {
	PodExists(ctx context.Context, namespace, name, uid string, want agent.Role) (bool, error)
}

// ClientPodChecker reads through the manager's cache.
type ClientPodChecker struct{ Client client.Client }

// PodExists implements PodChecker. It demands the same two labels the orphan
// sweep uses to decide what belongs to Spawnery — role AND managed-by — so a
// hand-built pod cannot open a session, and so the two places cannot drift
// apart on what "one of ours" means. The role must match the session being
// opened, not merely be one of the two we know.
func (c *ClientPodChecker) PodExists(
	ctx context.Context,
	namespace, name, uid string,
	want agent.Role,
) (bool, error) {
	pod := &corev1.Pod{}
	err := c.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(pod.UID) != uid {
		return false, nil
	}
	if pod.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		return false, nil
	}
	return pod.Labels[podspec.LabelRole] == roleLabelFor(want), nil
}

// roleLabelFor maps a session role to the pod label that carries it.
func roleLabelFor(role agent.Role) string {
	if role == agent.RoleProxy {
		return podspec.RoleProxy
	}
	return podspec.RoleServer
}

// Authenticator checks tokens against the real authenticator of the API
// server.
type Authenticator struct {
	Reviews  authnclient.TokenReviewInterface
	Pods     PodChecker
	Audience string
}

// serviceAccountFor is which ServiceAccount may open a session in this role.
func serviceAccountFor(role agent.Role) string {
	if role == agent.RoleProxy {
		return podspec.ProxyServiceAccountName
	}
	return podspec.ServerServiceAccountName
}

// Authenticate returns the identity behind a token, or why it is refused.
func (a *Authenticator) Authenticate(ctx context.Context, token string, want agent.Role) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("no token presented")
	}

	review, err := a.Reviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		// Marked unavailable so the interceptor can answer with
		// codes.Unavailable. An agent must back off and retry when the API
		// server is down, not conclude its credentials are wrong.
		return Identity{}, wrapUnavailable(fmt.Errorf("token review: %w", err))
	}
	if !review.Status.Authenticated {
		return Identity{}, fmt.Errorf("token not authenticated: %s", review.Status.Error)
	}
	if !containsString(review.Status.Audiences, a.Audience) {
		return Identity{}, fmt.Errorf("token not authenticated for audience %q", a.Audience)
	}

	namespace, name, ok := splitServiceAccount(review.Status.User.Username)
	if !ok {
		return Identity{}, fmt.Errorf("not a service account: %q", review.Status.User.Username)
	}
	if wantSA := serviceAccountFor(want); name != wantSA {
		return Identity{}, fmt.Errorf("service account %q may not open a %s session, %q may",
			name, want, wantSA)
	}

	podName := firstExtra(review.Status.User.Extra, claimPodName)
	podUID := firstExtra(review.Status.User.Extra, claimPodUID)
	if podName == "" || podUID == "" {
		return Identity{}, fmt.Errorf("token is not bound to a pod")
	}

	exists, err := a.Pods.PodExists(ctx, namespace, podName, podUID, want)
	if err != nil {
		return Identity{}, wrapUnavailable(fmt.Errorf("look up pod %s/%s: %w", namespace, podName, err))
	}
	if !exists {
		return Identity{}, fmt.Errorf("pod %s/%s is not a Spawnery pod", namespace, podName)
	}

	return Identity{
		Namespace:      namespace,
		PodName:        podName,
		PodUID:         podUID,
		ServiceAccount: name,
		Role:           want,
	}, nil
}

func splitServiceAccount(username string) (namespace, name string, ok bool) {
	rest, found := strings.CutPrefix(username, saPrefix)
	if !found {
		return "", "", false
	}
	namespace, name, found = strings.Cut(rest, ":")
	if !found || namespace == "" || name == "" {
		return "", "", false
	}
	return namespace, name, true
}

func firstExtra(extra map[string]authnv1.ExtraValue, key string) string {
	values, ok := extra[key]
	if !ok || len(values) == 0 {
		return ""
	}
	return values[0]
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
```

Der Import `metav1` fehlt oben absichtlich nicht — er wird für `metav1.CreateOptions{}` gebraucht; ergänze `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` in der Importliste.

- [ ] **Schritt 4: Den Interceptor implementieren**

`internal/grpcauth/interceptor.go` (mit Apache-Kopf):

```go
package grpcauth

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/agent"
)

type identityKey struct{}

// IdentityFrom reads the identity a stream was authenticated with.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// RoleForMethod maps a gRPC method to the role its caller must have.
func RoleForMethod(fullMethod string) (agent.Role, bool) {
	switch {
	case strings.HasSuffix(fullMethod, "/ServerSession"):
		return agent.RoleServer, true
	case strings.HasSuffix(fullMethod, "/ProxySession"):
		return agent.RoleProxy, true
	}
	return "", false
}

// wrappedStream carries the authenticated context into the handler.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w wrappedStream) Context() context.Context { return w.ctx }

// StreamInterceptor authenticates every stream before the handler sees it.
func (a *Authenticator) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		role, ok := RoleForMethod(info.FullMethod)
		if !ok {
			return status.Errorf(codes.Unimplemented, "unknown method")
		}

		ctx := ss.Context()
		id, err := a.Authenticate(ctx, bearerFrom(ctx), role)
		if err != nil {
			// The token itself never reaches the log.
			log.FromContext(ctx).V(1).Info("rejected an agent stream",
				"method", info.FullMethod, "reason", err.Error())
			AuthFailures.WithLabelValues(string(role)).Inc()
			code := codes.Unauthenticated
			if isUnavailable(err) {
				code = codes.Unavailable
			}
			return status.Error(code, err.Error())
		}

		return handler(srv, wrappedStream{ServerStream: ss, ctx: context.WithValue(ctx, identityKey{}, id)})
	}
}

func bearerFrom(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, value := range md.Get("authorization") {
		if token, found := strings.CutPrefix(value, "Bearer "); found {
			return token
		}
	}
	return ""
}
```

`AuthFailures` entsteht in Task 6 zusammen mit den übrigen Metriken. Damit dieser Task für sich übersetzt, lege die Metriken jetzt an, in `internal/grpcauth/metrics.go`:

```go
package grpcauth

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// AuthFailures counts refused streams. Without it a misconfigured agent is
// invisible outside the log.
var AuthFailures = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_agent_auth_failures_total",
		Help: "Refused agent streams, by role.",
	},
	[]string{"role"},
)

func init() { metrics.Registry.MustRegister(AuthFailures) }
```

- [ ] **Schritt 5: Tests laufen lassen**

```bash
nix develop -c go test ./internal/grpcauth/ -v
```

Erwartet: PASS, insbesondere alle sechs Unterfälle von `TestRejections`.

- [ ] **Schritt 6: Commit**

```bash
git add internal/grpcauth/
git commit -m "Die Identität eines Agents kommt aus dem Token, nicht aus Hello

Geprüft gegen den echten Authentifizierer in envtest, mit Tokens aus
TokenRequest: Audience, ServiceAccount-Trennung, Pod-Bindung und die
Existenz des Pods. Die sechs Ablehnungen sind der eigentliche Beweis —
ein Test, der nur den guten Fall prüft, bliebe grün, wenn der
Interceptor jeden durchließe."
```

---

### Task 6: Die ServerSession

Entwurf Abschnitt 7. Der Dienst selbst: Nachrichten in die Registry, ein Stream pro Pod, Frist mit Überlappung.

**Verdrängung darf die Bereitschaft nicht flackern lassen.** `Registry.Connect` setzt `ready = false`, und der verdrängende Stream ruft es, bevor sein `Hello` eintrifft — `phase.go:334-336` liest „verbunden, aber nicht bereit" als sofortigen Verlust. Ohne Gegenmaßnahme fiele jeder Server im Takt der Frist aus `Ready`, sich bei den Proxies abmeldend und Verluste sammelnd: genau das, was `SessionDeadline` verhindern soll. Die Registry bekommt deshalb `Supersede` — dasselbe wie `Connect`, nur behält es die Bereitschaft.

Es gehört in die Registry und nicht in den `agentserver`, weil `connected` und `ready` in **einem** kritischen Abschnitt gesetzt werden müssen; ein `Connect` gefolgt von einem Setter reißt dasselbe Fenster wieder auf, das es schließen soll. Nach einem echten Abriss gilt weiter `Connect`: der Agent-Prozess kann neu gestartet sein, und nur sein `Hello` darf das Gegenteil behaupten.

**Dateien:**
- Ändern: `internal/agent/registry.go` (`Supersede`, plus Tests)
- Erstellen: `internal/agentserver/server.go`
- Erstellen: `internal/agentserver/streams.go`
- Erstellen: `internal/agentserver/metrics.go`
- Erstellen: `internal/agentserver/server_envtest_test.go`
- Erstellen: `internal/agentserver/main_test.go` (`TestMain` wie zuvor)

**Schnittstellen:**
- Verbraucht: `agentpb` (Task 2), `certs.Provider` (Task 4), `grpcauth.Authenticator`, `grpcauth.IdentityFrom` (Task 5), `agent.Registry`.
- Liefert zusätzlich in `internal/agent`: `func (r *Registry) Supersede(key string, role Role)` — wie `Connect`, aber ohne die Bereitschaft zurückzusetzen. `Connect`, `MarkReady` und `Disconnect` bleiben unverändert.
- Liefert:
  - `type Options struct { Addr string; Provider *certs.Provider; Auth *grpcauth.Authenticator; Agents *agent.Registry; ReportInterval, RenewAfter, HardDeadline time.Duration; Clock func() time.Time }`
  - `func New(opts Options) *Server`
  - `func (s *Server) Start(ctx context.Context) error` — Runnable, `NeedLeaderElection() bool { return true }`
  - `func (s *Server) Addr() string` — die tatsächliche Adresse, für Tests mit Port 0
  - `const DefaultPort = 9443`

- [ ] **Schritt 1: Die fehlschlagenden Tests schreiben**

`internal/agentserver/server_envtest_test.go` (mit Apache-Kopf). Der Testagent ist ein echter gRPC-Client über echtes TLS:

```go
package agentserver_test

import (
	"context"
	"crypto/x509"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

// dialAgent opens a ServerSession the way a real agent would: TLS against the
// pinned CA, token in the authorization header.
func dialAgent(t *testing.T, ctx context.Context, addr string, ca []byte, token string) (
	grpc.BidiStreamingClient[agentpb.ServerMessage, agentpb.OperatorToServer], func()) {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("CA bundle unusable")
	}
	// The agent pins this CA and nothing else — that is the whole point of the
	// operator issuing its own certificate.
	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: "spawnery-operator.spawnery-system.svc",
		MinVersion: tls.VersionTLS13,
	})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	stream, err := agentpb.NewAgentServiceClient(conn).ServerSession(streamCtx)
	if err != nil {
		conn.Close()
		t.Fatalf("open ServerSession: %v", err)
	}
	return stream, func() { _ = conn.Close() }
}
```

Die Fixture gehört in dieselbe Datei und folgt dem Muster von `internal/controller/suite_test.go`. Weil der Server als Runnable läuft, startet sie ihn in einer Goroutine und wartet, bis `Addr()` belegt ist:

```go
type serverFixture struct {
	t      *testing.T
	ctx    context.Context
	c      client.Client
	cs     *kubernetes.Clientset
	ns     string
	agents *agent.Registry
	addr   string
	ca     []byte
}

func newServerFixture(t *testing.T) *serverFixture {
	return newServerFixtureWithDeadline(t, 8*time.Minute, 10*time.Minute)
}

func newServerFixtureWithDeadline(t *testing.T, renewAfter, hardDeadline time.Duration) *serverFixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	cs, err := kubernetes.NewForConfig(testenv.Config(t))
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if err := c.Create(ctx, sa); err != nil {
			t.Fatalf("create ServiceAccount %s: %v", name, err)
		}
	}

	now := func() time.Time { return time.Now() }
	store := &certs.Store{
		Client: c, Namespace: ns, Name: certs.SecretName,
		// The SANs must match what dialAgent asks for, not the test namespace.
		DNSNames: certs.ServingDNSNames("spawnery-operator", "spawnery-system"),
		Clock:    now,
	}
	provider := certs.NewProvider(store)
	bundle, err := store.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := provider.Set(bundle); err != nil {
		t.Fatalf("Set: %v", err)
	}

	registry := agent.New(now, 5*time.Second, now())
	srv := agentserver.New(agentserver.Options{
		// Port 0: the kernel picks a free one, so parallel packages do not
		// collide.
		Addr:           "127.0.0.1:0",
		Provider:       provider,
		Auth:           &grpcauth.Authenticator{Reviews: cs.AuthenticationV1().TokenReviews(), Pods: &grpcauth.ClientPodChecker{Client: c}, Audience: podspec.AgentTokenAudience},
		Agents:         registry,
		ReportInterval: 5 * time.Second,
		RenewAfter:     renewAfter,
		HardDeadline:   hardDeadline,
		Clock:          now,
	})

	serverCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		if err := srv.Start(serverCtx); err != nil {
			t.Logf("agent server stopped: %v", err)
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the agent server never bound a port")
		}
		time.Sleep(10 * time.Millisecond)
	}

	return &serverFixture{
		t: t, ctx: ctx, c: c, cs: cs, ns: ns,
		agents: registry, addr: srv.Addr(), ca: provider.CABundle(),
	}
}
```

`f.pod(name)` und `f.token(pod)` sind wortgleich mit den Hilfen aus Task 5 — kopiere sie, statt das Testpaket von Task 5 zu importieren.

Die Testfälle:

```go
func TestHelloWithReadyMarksTheAgentReady(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	defer closeConn()

	if err := stream.Send(&agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_Hello{Hello: &agentpb.Hello{Version: "0.1.0", Ready: true}},
	}); err != nil {
		t.Fatalf("send Hello: %v", err)
	}

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })
	snap := f.agents.Lookup(string(pod.UID))
	if !snap.Connected {
		t.Error("the registry does not see the stream")
	}
}

// The operator dictates the interval, so both sides derive the staleness
// threshold from the same number.
func TestOperatorSendsIntervalAndDeadlineOnConnect(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	defer closeConn()

	var gotInterval, gotDeadline bool
	for range 2 {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch m := msg.GetMessage().(type) {
		case *agentpb.OperatorToServer_ReportInterval:
			gotInterval = true
			if m.ReportInterval.GetSeconds() != 5 {
				t.Errorf("ReportInterval = %ds, want 5s", m.ReportInterval.GetSeconds())
			}
		case *agentpb.OperatorToServer_SessionDeadline:
			gotDeadline = true
			if m.SessionDeadline.GetRenewAfterSeconds() >= m.SessionDeadline.GetHardDeadlineSeconds() {
				t.Errorf("renewAfter %d must be below hardDeadline %d",
					m.SessionDeadline.GetRenewAfterSeconds(),
					m.SessionDeadline.GetHardDeadlineSeconds())
			}
		}
	}
	if !gotInterval || !gotDeadline {
		t.Errorf("interval=%v deadline=%v, want both", gotInterval, gotDeadline)
	}
}

func TestPlayerCountReachesTheRegistry(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	mustSend(t, stream, playerCount(7, 100))

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 7 })
	if got := f.agents.Lookup(string(pod.UID)).Slots; got != 100 {
		t.Errorf("Slots = %d, want 100", got)
	}
}

// Spec 5.2: discard, do not disconnect. Dropping the stream would be a
// reconnect loop the agent could trigger at will.
func TestPlayerCountAboveSlotsIsDiscardedButKeepsTheStream(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	mustSend(t, stream, playerCount(5, 100))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 5 })

	mustSend(t, stream, playerCount(4000, 100))
	mustSend(t, stream, playerCount(6, 100))

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 6 })
	if !f.agents.Lookup(string(pod.UID)).Connected {
		t.Error("the stream was dropped over a bad report")
	}
}

func TestDisconnectIsVisibleInTheRegistry(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))

	mustSend(t, stream, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	closeConn()
	waitFor(t, func() bool { return !f.agents.Lookup(string(pod.UID)).Connected })
	if f.agents.Lookup(string(pod.UID)).Ready {
		t.Error("a broken stream left the agent marked ready")
	}
}

// Make-before-break: this is what keeps a renewal from dropping the server out
// of Ready every ten minutes.
func TestASecondStreamSupersedesTheFirstWithoutLosingState(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	first, closeFirst := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	mustSend(t, first, hello(true))
	mustSend(t, first, playerCount(3, 100))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 3 })

	second, closeSecond := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	defer closeSecond()
	mustSend(t, second, hello(true))

	// Wait for the operator's first message on the new stream before dropping
	// the old one. Without this the test proves nothing: Send only fills a
	// client-side buffer, so closeFirst can land before the operator has even
	// seen the second stream — an interleaving in which Disconnect is the
	// correct behaviour. Written the naive way it fails against correct code
	// in most runs.
	awaitSession(t, second)

	// The superseded stream ends, and that must not tear down the new one.
	closeFirst()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !f.agents.Lookup(string(pod.UID)).Connected {
			t.Fatal("the superseded stream disconnected the live one")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !f.agents.Lookup(string(pod.UID)).Ready {
		t.Error("the new stream lost the ready state")
	}
}

// The hard deadline is the net under an agent that ignores renewAfter.
func TestTheHardDeadlineClosesTheStream(t *testing.T) {
	f := newServerFixtureWithDeadline(t, 300*time.Millisecond, 600*time.Millisecond)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	// Recv returns the error once the operator hangs up.
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the operator did not close the stream at the hard deadline")
	}
}

// An unknown branch must be ignored, not fatal: a newer agent against an older
// operator has to keep working.
func TestAnEmptyMessageIsIgnored(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")
	stream, closeConn := dialAgent(t, f.ctx, f.addr, f.ca, f.token(pod))
	defer closeConn()

	mustSend(t, stream, hello(true))
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Ready })

	// A ServerMessage with no branch set is what an unknown future branch
	// decodes to on this operator.
	mustSend(t, stream, &agentpb.ServerMessage{})
	mustSend(t, stream, playerCount(4, 100))

	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Players == 4 })
	if !f.agents.Lookup(string(pod.UID)).Connected {
		t.Error("an unknown message tore down the stream")
	}
}

// Milestone 3 fills ProxySession in; until then it must refuse outright rather
// than half-work.
func TestProxySessionIsRefused(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-abcd")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		t.Fatal("CA bundle unusable")
	}
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{
			RootCAs:    pool,
			ServerName: "spawnery-operator.spawnery-system.svc",
			MinVersion: tls.VersionTLS13,
		})))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx := metadata.AppendToOutgoingContext(f.ctx, "authorization", "Bearer "+f.token(pod))
	stream, err := agentpb.NewAgentServiceClient(conn).ProxySession(ctx)
	if err != nil {
		t.Fatalf("open ProxySession: %v", err)
	}
	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_Hello{Hello: &agentpb.Hello{Version: "0.1.0"}},
	}); err != nil {
		// A send may already fail once the server hung up; that is a refusal
		// too.
		return
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("ProxySession answered although milestone 3 has not happened")
	}
	// Unauthenticated (server token on a proxy session) and Unimplemented are
	// both refusals; which one wins depends on interceptor order.
	code := status.Code(err)
	if code != codes.Unauthenticated && code != codes.Unimplemented {
		t.Errorf("code = %s, want Unauthenticated or Unimplemented", code)
	}
}
```

Die Hilfen `mustSend`, `hello`, `playerCount` und `waitFor` gehören in dieselbe Datei. `waitFor` pollt in 20-Millisekunden-Schritten bis zu drei Sekunden und ruft `t.Fatal`, wenn die Bedingung nie eintritt — die Nachrichtenverarbeitung ist nebenläufig, ein direkter Vergleich wäre flatterig.

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/agentserver/ -v
```

Erwartet: „undefined: agentserver.New".

- [ ] **Schritt 3: Die Streamverwaltung implementieren**

`internal/agentserver/streams.go` (mit Apache-Kopf) — die Zuordnung Pod-UID → laufender Stream:

```go
package agentserver

import (
	"context"
	"sync"
)

// sessions tracks one live stream per pod. A second stream for the same pod
// supersedes the first: otherwise tearing down the zombie would wipe the state
// of the fresh one and the server would fall out of Ready for no reason.
type sessions struct {
	mu      sync.Mutex
	current map[string]context.CancelFunc
	// generation counts how often a pod connected, so a superseded stream can
	// tell it is no longer the current one.
	generation map[string]uint64
}

func newSessions() *sessions {
	return &sessions{
		current:    make(map[string]context.CancelFunc),
		generation: make(map[string]uint64),
	}
}

// enter registers a new stream and cancels the one it replaces. The returned
// context ends when this stream is superseded or the server shuts down.
func (s *sessions) enter(parent context.Context, podUID string) (context.Context, uint64) {
	ctx, cancel := context.WithCancel(parent)

	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.current[podUID]; ok {
		previous()
	}
	s.generation[podUID]++
	gen := s.generation[podUID]
	s.current[podUID] = cancel
	return ctx, gen
}

// leave reports whether this stream was still the current one. Only then may
// the caller mark the pod disconnected.
func (s *sessions) leave(podUID string, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation[podUID] != gen {
		return false
	}
	delete(s.current, podUID)
	return true
}
```

- [ ] **Schritt 4: Den Dienst implementieren**

`internal/agentserver/server.go` (mit Apache-Kopf). Kern von `ServerSession`:

```go
// ServerSession is the Paper agent's channel.
func (s *Server) ServerSession(stream agentpb.AgentService_ServerSessionServer) error {
	id, ok := grpcauth.IdentityFrom(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "no identity on the stream")
	}
	logger := log.FromContext(stream.Context()).WithValues("pod", id.PodName, "namespace", id.Namespace)

	ctx, gen := s.sessions.enter(stream.Context(), id.PodUID)
	defer func() {
		// Only the current stream may report the disconnect. A superseded one
		// must not, or make-before-break would break.
		if s.sessions.leave(id.PodUID, gen) {
			s.agents.Disconnect(id.PodUID)
		}
	}()

	s.agents.Connect(id.PodUID, agent.RoleServer)
	OpenStreams.WithLabelValues(string(agent.RoleServer)).Inc()
	defer OpenStreams.WithLabelValues(string(agent.RoleServer)).Dec()

	if err := stream.Send(&agentpb.OperatorToServer{
		Message: &agentpb.OperatorToServer_ReportInterval{
			ReportInterval: &agentpb.ReportInterval{Seconds: int32(s.reportInterval.Seconds())},
		},
	}); err != nil {
		return err
	}
	if err := stream.Send(&agentpb.OperatorToServer{
		Message: &agentpb.OperatorToServer_SessionDeadline{
			SessionDeadline: &agentpb.SessionDeadline{
				RenewAfterSeconds:   int32(s.renewAfter.Seconds()),
				HardDeadlineSeconds: int32(s.hardDeadline.Seconds()),
			},
		},
	}); err != nil {
		return err
	}

	deadline := time.AfterFunc(s.hardDeadline, func() {
		logger.V(1).Info("closing the stream at its hard deadline")
		s.sessions.cancel(id.PodUID, gen)
	})
	defer deadline.Stop()

	// Receiving blocks, so it runs in its own goroutine and the context does
	// the cancelling.
	received := make(chan *agentpb.ServerMessage)
	errs := make(chan error, 1)
	go func() {
		defer close(received)
		for {
			msg, err := stream.Recv()
			if err != nil {
				errs <- err
				return
			}
			select {
			case received <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
		case err := <-errs:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case msg := <-received:
			s.handle(ctx, logger, id, msg)
		}
	}
}

// handle applies one message. An unknown branch is ignored so a newer agent
// against an older operator keeps working.
func (s *Server) handle(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	msg *agentpb.ServerMessage,
) {
	switch m := msg.GetMessage().(type) {
	case *agentpb.ServerMessage_Hello:
		// Ready is a state, not an event: the agent repeats it on every
		// connect, so an operator restart cannot leave a server in Starting.
		if m.Hello.GetReady() {
			s.agents.MarkReady(id.PodUID)
		}
	case *agentpb.ServerMessage_Ready:
		s.agents.MarkReady(id.PodUID)
	case *agentpb.ServerMessage_PlayerCount:
		if err := s.agents.ReportPlayers(id.PodUID, m.PlayerCount.GetPlayers(), m.PlayerCount.GetSlots()); err != nil {
			// Discard, keep the stream. Spec 5.2.
			RejectedReports.WithLabelValues(string(agent.RoleServer)).Inc()
			logger.V(1).Info("discarded a player count", "reason", err.Error())
		}
	}
}

// ProxySession is milestone 3.
func (s *Server) ProxySession(agentpb.AgentService_ProxySessionServer) error {
	return status.Error(codes.Unimplemented, "proxy sessions arrive with milestone 3")
}
```

`Start` baut den Listener mit `tls.Config{GetCertificate: provider.GetCertificate, MinVersion: tls.VersionTLS13}`, registriert den Dienst mit `grpc.StreamInterceptor(auth.StreamInterceptor())`, merkt sich `listener.Addr().String()` für `Addr()` und stoppt bei `ctx.Done()` über `GracefulStop`. `sessions.cancel(podUID, gen)` ist die dritte Methode auf `sessions`: sie ruft die gemerkte `CancelFunc`, aber nur wenn die Generation noch passt.

`internal/agentserver/metrics.go` nach dem Muster aus Task 5, mit `OpenStreams` als `prometheus.NewGaugeVec` und `RejectedReports` als `prometheus.NewCounterVec`, beide mit dem Label `role`.

- [ ] **Schritt 5: Tests laufen lassen**

```bash
nix develop -c go test ./internal/agentserver/ -v -race
```

Erwartet: PASS. `-race` ist hier nicht optional: Streamverwaltung und Registry werden aus mehreren Goroutinen angefasst.

- [ ] **Schritt 6: Commit**

```bash
git add internal/agentserver/
git commit -m "Die ServerSession füllt die Registry

Ein Testagent über echtes TLS mit echtem Token: Hello, Ready und
PlayerCount landen in der Registry, eine Zahl über der Kapazität wird
verworfen ohne den Stream zu reißen, und ein zweiter Stream desselben
Pods verdrängt den ersten, ohne dessen Zustand mitzunehmen. Genau das
macht die Frist aus 7.1 unschädlich."
```

---

### Task 7: Namespace-Bootstrap

Entwurf Abschnitt 6.5. CA-ConfigMap und ServiceAccount in jedem Namespace, in dem Pods laufen.

**Dateien:**
- Erstellen: `internal/controller/bootstrap.go`
- Erstellen: `internal/controller/bootstrap_test.go`

**Schnittstellen:**
- Verbraucht: `podspec.CAConfigMapName`, `podspec.CAConfigMapKey`, `podspec.ServerServiceAccountName`, `podspec.LabelManagedBy`, `podspec.ManagedByValue` — angelegt in `internal/podspec/agent.go` (siehe Globale Randbedingungen und Task 5).
- Liefert:
  - `type Bootstrapper struct { Client client.Client; Reader client.Reader; CA func() []byte }`
  - `func (b *Bootstrapper) Ensure(ctx context.Context, namespace string) error`
  - `func (b *Bootstrapper) EnsureAll(ctx context.Context, namespaces []string) error`
- Verbraucht: `certs.Provider.CABundle` als `CA`.
- Wird von Task 9 im `ServerReconciler` vor dem Anlegen des Pods gerufen.

- [ ] **Schritt 1: Die fehlschlagenden Tests schreiben**

`internal/controller/bootstrap_test.go` (mit Apache-Kopf), im Paket `controller` wie die übrigen Controller-Tests:

```go
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestEnsureCreatesConfigMapAndServiceAccount(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[podspec.CAConfigMapKey] != "PEM-A" {
		t.Errorf("ca.crt = %q, want PEM-A", cm.Data[podspec.CAConfigMapKey])
	}
	if cm.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Error("the ConfigMap is unlabelled and would fall out of the restricted cache")
	}

	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.ServerServiceAccountName, Namespace: ns}, sa); err != nil {
		t.Fatalf("get ServiceAccount: %v", err)
	}
	if sa.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Error("the ServiceAccount is unlabelled")
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}

// A rotation has to reach every namespace, or agents in the ones left behind
// would stop trusting the operator.
func TestEnsureUpdatesAChangedCA(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	ca := "PEM-A"
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte(ca) }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	ca = "PEM-B"
	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure after rotation: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[podspec.CAConfigMapKey] != "PEM-B" {
		t.Errorf("ca.crt = %q, want the rotated PEM-B", cm.Data[podspec.CAConfigMapKey])
	}
}

func TestEnsureRepairsAHandEditedConfigMap(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	cm.Data[podspec.CAConfigMapKey] = "jemand-hat-daran-gedreht"
	delete(cm.Labels, podspec.LabelManagedBy)
	if err := c.Update(ctx, cm); err != nil {
		t.Fatalf("update ConfigMap: %v", err)
	}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure over the edited ConfigMap: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if cm.Data[podspec.CAConfigMapKey] != "PEM-A" {
		t.Errorf("ca.crt = %q, want the restored PEM-A", cm.Data[podspec.CAConfigMapKey])
	}
	if cm.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Error("the label was not restored; the object would stay outside the cache")
	}
}

// Ensure must not run before the provider has a CA — an empty ca.crt would be
// worse than none, because the pod would start and fail the handshake.
func TestEnsureRefusesAnEmptyCA(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return nil }}

	if err := b.Ensure(ctx, ns); err == nil {
		t.Error("Ensure wrote an empty CA bundle")
	}
}
```

Die Hilfsfunktion `newBootstrapper` oben wird von den Tests nicht gebraucht — sie ist ein Rest und muss weg, bevor der Task committet wird. (Lass sie beim Schreiben weg.)

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/controller/ -run TestEnsure -v
```

Erwartet: „undefined: Bootstrapper".

- [ ] **Schritt 3: Implementieren**

`internal/controller/bootstrap.go` (mit Apache-Kopf). Kernpunkte:

- `Ensure` bricht mit Fehler ab, wenn `b.CA()` leer ist.
- `Ensure` nutzt `controllerutil.CreateOrUpdate` mit dem gecachten `Client`; bei `AlreadyExists` — was passiert, wenn das Objekt existiert, aber sein Label verloren hat und deshalb nicht im eingeschränkten Cache steht — liest es über `b.Reader` ungecacht nach und aktualisiert direkt.
- Beide Objekte bekommen `podspec.LabelManagedBy: podspec.ManagedByValue`.
- Keine OwnerReference: die Objekte überleben den Operator bewusst, damit ein Pod-Neustart während eines Operator-Ausfalls nicht an einer fehlenden CA scheitert.

Die RBAC-Marker gehören direkt über `Ensure`:

```go
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
```

- [ ] **Schritt 4: Tests laufen lassen**

```bash
nix develop -c go test ./internal/controller/ -run TestEnsure -v
```

Erwartet: PASS, alle fünf.

- [ ] **Schritt 5: Commit**

```bash
git add internal/controller/bootstrap.go internal/controller/bootstrap_test.go
git commit -m "CA-ConfigMap und ServiceAccount je Namespace

Die CA ist öffentlich, deshalb eine ConfigMap statt eines Secrets: das
erspart dem Operator clusterweite Secret-Schreibrechte für etwas, das
nicht geheim ist. Ohne OwnerReference, damit ein Pod-Neustart während
eines Operator-Ausfalls nicht an einer fehlenden CA scheitert."
```

---

### Task 8: Token und CA in den Pod

Entwurf Abschnitt 6.4. Reine Podspec-Änderung, deshalb Unit-Tests.

**Dateien:**
- Ändern: `internal/podspec/server.go` (Konstanten oben, Volumes ab Zeile 90, Container ab 117, PodSpec ab 177)
- Ändern: `internal/podspec/server_test.go` (neue Fälle)

**Schnittstellen:**
- Ändert: `func BuildServerPod(net, group, srv)` bekommt einen vierten Parameter `agentEndpoint string`. Alle Aufrufer — heute nur `internal/controller/server_controller.go` — ziehen nach.
- Liefert zusätzlich: `const AgentVolumeName = "spawnery-agent"`, `const AgentMountPath = "/var/run/spawnery"`, `const AgentTokenPath = "token"`, `const AgentCAPath = "ca.crt"`, `const EnvOperatorEndpoint = "SPAWNERY_OPERATOR_ENDPOINT"`, `const TokenExpirationSeconds int64 = 600`.

- [ ] **Schritt 1: Die fehlschlagenden Tests schreiben**

An `internal/podspec/server_test.go` anhängen (die Hilfen der Datei wiederverwenden — sieh dir an, wie die bestehenden Tests einen Pod bauen, und folge dem Muster):

```go
// testEndpoint is what the operator would pass in; the tests below assert it
// arrives in the container unchanged.
const testEndpoint = "spawnery-operator.spawnery-system.svc:9443"

// buildTestPod renders the pod of a plain ephemeral server. testObjects is the
// helper the existing tests in this file already use — look up its exact name
// there rather than guessing.
func buildTestPod(t *testing.T) *corev1.Pod {
	t.Helper()
	net, group, srv := testObjects()
	pod, err := podspec.BuildServerPod(net, group, srv, testEndpoint)
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	return pod
}

func TestPodCarriesTheProjectedAgentToken(t *testing.T) {
	pod := buildTestPod(t)

	var projected *corev1.ProjectedVolumeSource
	for _, v := range pod.Spec.Volumes {
		if v.Name == podspec.AgentVolumeName {
			projected = v.Projected
		}
	}
	if projected == nil {
		t.Fatal("the pod has no projected agent volume")
	}

	var sawToken, sawCA bool
	for _, src := range projected.Sources {
		if sa := src.ServiceAccountToken; sa != nil {
			sawToken = true
			if sa.Audience != podspec.AgentTokenAudience {
				t.Errorf("audience = %q, want %q", sa.Audience, podspec.AgentTokenAudience)
			}
			if sa.ExpirationSeconds == nil || *sa.ExpirationSeconds != podspec.TokenExpirationSeconds {
				t.Errorf("expirationSeconds = %v, want %d", sa.ExpirationSeconds, podspec.TokenExpirationSeconds)
			}
			if sa.Path != podspec.AgentTokenPath {
				t.Errorf("token path = %q, want %q", sa.Path, podspec.AgentTokenPath)
			}
		}
		if cm := src.ConfigMap; cm != nil {
			sawCA = true
			if cm.Name != podspec.CAConfigMapName {
				t.Errorf("configmap = %q, want %q", cm.Name, podspec.CAConfigMapName)
			}
		}
	}
	if !sawToken || !sawCA {
		t.Errorf("token=%v ca=%v, want both in one volume", sawToken, sawCA)
	}
}

func TestPodUsesTheServerServiceAccountButNoAutomount(t *testing.T) {
	pod := buildTestPod(t)

	if pod.Spec.ServiceAccountName != podspec.ServerServiceAccountName {
		t.Errorf("serviceAccountName = %q, want %q",
			pod.Spec.ServiceAccountName, podspec.ServerServiceAccountName)
	}
	// The claim "these pods carry no Kubernetes credentials" only holds with
	// automount off; the projected, audience-bound token is the exception.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken is not off")
	}
}

func TestContainerKnowsWhereToReachTheOperator(t *testing.T) {
	pod := buildTestPod(t)
	c := pod.Spec.Containers[0]

	var endpoint string
	for _, e := range c.Env {
		if e.Name == podspec.EnvOperatorEndpoint {
			endpoint = e.Value
		}
	}
	if endpoint != testEndpoint {
		t.Errorf("%s = %q", podspec.EnvOperatorEndpoint, endpoint)
	}

	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == podspec.AgentVolumeName {
			mounted = true
			if m.MountPath != podspec.AgentMountPath {
				t.Errorf("mountPath = %q, want %q", m.MountPath, podspec.AgentMountPath)
			}
			if !m.ReadOnly {
				t.Error("the agent volume is writable")
			}
		}
	}
	if !mounted {
		t.Error("the agent volume is not mounted into the container")
	}
}

// A user mount must never shadow the token, and the API server's generic
// rejection is no substitute for the operator saying what is wrong.
func TestCollidingUserMountsAreRefused(t *testing.T) {
	cases := []struct {
		name  string
		mount spawneryv1alpha1.MountSpec
		want  string
	}{
		{
			name: "same volume name as the agent volume",
			mount: spawneryv1alpha1.MountSpec{
				Name:      podspec.AgentVolumeName,
				MountPath: "/irgendwo",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: podspec.AgentVolumeName,
		},
		{
			name: "mounted over the agent path",
			mount: spawneryv1alpha1.MountSpec{
				Name:      "eigenes",
				MountPath: podspec.AgentMountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: podspec.AgentMountPath,
		},
		{
			name: "mounted over /data",
			mount: spawneryv1alpha1.MountSpec{
				Name:      "eigenes",
				MountPath: podspec.DataMountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: podspec.DataMountPath,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, group, srv := testObjects() // die Hilfe der bestehenden Datei
			group.Spec.Mounts = []spawneryv1alpha1.MountSpec{tc.mount}

			_, err := podspec.BuildServerPod(net, group, srv,
				"spawnery-operator.spawnery-system.svc:9443")
			if err == nil {
				t.Fatal("BuildServerPod accepted a colliding mount")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
```

Der genaue Name der Hilfe `testObjects` und des Mount-Typs `MountSpec` steht in `internal/podspec/server_test.go` beziehungsweise `api/v1alpha1/servergroup_types.go` — lies dort nach und passe die Namen an, statt sie zu raten.

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/podspec/ -v
```

Erwartet: Übersetzungsfehler wegen der neuen Konstanten und des vierten Parameters.

- [ ] **Schritt 3: Implementieren**

In `internal/podspec/server.go`:

- Konstanten ergänzen (Werte siehe Schnittstellen oben).
- Signatur zu `BuildServerPod(net, group, srv, agentEndpoint string)` erweitern; leerer Endpunkt ist ein Fehler wie ein leeres Image.
- Das projizierte Volume anlegen:

```go
	volumes = append(volumes, corev1.Volume{
		Name: AgentVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						// The audience is what makes a standard API server
						// token worthless here, and the short expiry keeps the
						// replay window small. The kubelet rotates it.
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          podspec.AgentTokenAudience,
							ExpirationSeconds: ptr.To(TokenExpirationSeconds),
							Path:              AgentTokenPath,
						},
					},
					{
						ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: caConfigMapName},
							Items: []corev1.KeyToPath{
								{Key: AgentCAPath, Path: AgentCAPath},
							},
						},
					},
				},
			},
		},
	})
```

Alle gemeinsamen Namen stehen bereits in `internal/podspec/agent.go` (seit Task 5), `caConfigMapName` im Ausschnitt oben ist also `CAConfigMapName` aus demselben Paket. So bleibt `podspec` das unterste Paket ohne Abhängigkeiten — ein Import von `controller` oder `grpcauth` wäre ein Zyklus, weil `grpcauth` seinerseits `podspec.LabelRole` braucht.

- `ServiceAccountName: ServerServiceAccountName` in die PodSpec.
- Env-Variable `EnvOperatorEndpoint` in den Container.
- Mount des Volumes read-only unter `AgentMountPath`.
- Vor dem Anhängen der Nutzer-Mounts prüfen, ob einer `AgentVolumeName`, `DataVolumeName` oder `TmpVolumeName` heißt oder auf `AgentMountPath`, `DataMountPath` oder `TmpMountPath` zeigt, und mit klarem Fehler abbrechen.

- [ ] **Schritt 4: Aufrufer nachziehen und Tests laufen lassen**

```bash
nix develop -c go build ./... && nix develop -c go test ./internal/podspec/ ./internal/controller/ -v
```

Erwartet: PASS. Der `ServerReconciler` braucht dafür ein Feld `AgentEndpoint string`, das Task 9 dann verdrahtet — setze es in den Controller-Tests vorerst auf `"spawnery-operator.spawnery-system.svc:9443"`.

- [ ] **Schritt 5: Commit**

```bash
git add internal/podspec/ internal/controller/
git commit -m "Gameserver-Pods bekommen Token und CA

Ein projiziertes Volume mit audience-gebundenem Token und der CA aus der
ConfigMap des Namespace, dazu der Endpunkt als Umgebungsvariable.
automountServiceAccountToken bleibt aus — erst damit stimmt die Aussage,
dass diese Pods keine Kubernetes-Credentials tragen. Kollidierende
Nutzer-Mounts lehnt der Operator jetzt mit klarer Meldung ab, statt sie
dem API-Server zu überlassen."
```

---

### Task 9: Verdrahtung im Operator

Entwurf Abschnitte 7.2 und 6.1. Alles zusammenstecken, plus die Manifeste.

**Dateien:**
- Ändern: `cmd/spawnery-operator/main.go`
- Ändern: `internal/controller/setup.go`
- Ändern: `internal/controller/server_controller.go` (Bootstrap vor dem Pod, `AgentEndpoint`)
- Erstellen: `config/deploy/service.yaml`
- Ändern: `config/deploy/deployment.yaml`
- Erstellen: `internal/controller/agentchannel_envtest_test.go`

**Schnittstellen:**
- Erweitert `controller.Options` um `Bootstrapper *Bootstrapper` und `AgentEndpoint string`.
- Erweitert `ServerReconciler` um `Bootstrap *Bootstrapper` und `AgentEndpoint string`.

- [ ] **Schritt 1: Den Ende-zu-Ende-Test schreiben**

`internal/controller/agentchannel_envtest_test.go` (mit Apache-Kopf). Er verdrahtet Zertifikate, Authenticator, Dienst und den `ServerReconciler` in einem Namespace und fährt einen Server hoch — ohne die Registry von Hand anzufassen:

```go
// This is the milestone in one test: no test may call registry.MarkReady
// here. The only path to Ready is a real agent over a real TLS connection.
func TestAgentOverTheWireBringsAServerToReady(t *testing.T) {
	f := newChannelFixture(t)

	srv := f.createServer("lobby-abcd")
	f.reconcile(srv.Name)

	pod, ok := f.pod(srv.Name)
	if !ok {
		t.Fatal("no pod was created")
	}
	f.setPodRunning(srv.Name, true)

	stream, closeConn := f.dialAgentFor(pod)
	defer closeConn()
	f.sendHelloReady(stream)
	f.sendPlayerCount(stream, 12, 100)
	f.waitForRegistry(string(pod.UID))

	f.reconcile(srv.Name)
	f.reconcile(srv.Name)

	got := f.server(srv.Name)
	if got.Status.Phase != string(phase.Ready) {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.Players != 12 || got.Status.Slots != 100 {
		t.Errorf("status = %d/%d, want 12/100", got.Status.Players, got.Status.Slots)
	}
}

// The bootstrap has to have run before the pod exists, or the kubelet would
// fail to mount a ConfigMap that is not there.
func TestReconcileBootstrapsTheNamespaceBeforeCreatingThePod(t *testing.T) {
	f := newChannelFixture(t)

	srv := f.createServer("lobby-abcd")
	f.reconcile(srv.Name)

	cm := &corev1.ConfigMap{}
	if err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.CAConfigMapName, Namespace: f.ns,
	}, cm); err != nil {
		t.Fatalf("the CA ConfigMap is missing although the pod exists: %v", err)
	}
	sa := &corev1.ServiceAccount{}
	if err := f.c.Get(f.ctx, types.NamespacedName{
		Name: podspec.ServerServiceAccountName, Namespace: f.ns,
	}, sa); err != nil {
		t.Fatalf("the ServiceAccount is missing: %v", err)
	}
}
```

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/controller/ -run TestAgentOverTheWire -v
```

Erwartet: FAIL — die Fixture existiert noch nicht, und der Reconciler legt die ConfigMap nicht an.

- [ ] **Schritt 3: Den Reconciler und die Verdrahtung ändern**

In `internal/controller/server_controller.go`: vor dem Erzeugen des Pods `r.Bootstrap.Ensure(ctx, srv.Namespace)` rufen und bei Fehler mit Requeue abbrechen — ein Pod ohne CA startet sonst und scheitert am Handshake. `BuildServerPod` bekommt `r.AgentEndpoint` als vierten Parameter.

In `internal/controller/setup.go`: `Options` erweitern und an den `ServerReconciler` durchreichen.

In `cmd/spawnery-operator/main.go`:

```go
	var (
		operatorNamespace string
		agentBindAddress  string
		renewAfter        time.Duration
		hardDeadline      time.Duration
	)
	flag.StringVar(&operatorNamespace, "operator-namespace", os.Getenv("POD_NAMESPACE"),
		"namespace the operator runs in; holds the TLS secret and the agent service")
	flag.StringVar(&agentBindAddress, "agent-bind-address", ":9443",
		"address the agent gRPC endpoint binds to")
	flag.DurationVar(&renewAfter, "agent-session-renew-after", 8*time.Minute,
		"when an agent should open a fresh stream; must be below the hard deadline")
	flag.DurationVar(&hardDeadline, "agent-session-deadline", 10*time.Minute,
		"when the operator closes an agent stream regardless")
```

Danach: Cache für ConfigMaps und ServiceAccounts auf das Label einschränken, `certs.Store` und `certs.Provider` bauen, `mgr.Add(provider)`, `agentserver.New(...)` bauen und `mgr.Add(...)`, den `Bootstrapper` mit `provider.CABundle` bauen, den Endpunkt zusammensetzen und `readyz` an die Leader-Sperre koppeln:

```go
	mgrOptions.Cache.ByObject = map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}:      {Label: labels.SelectorFromSet(labels.Set{podspec.LabelManagedBy: podspec.ManagedByValue})},
		&corev1.ServiceAccount{}: {Label: labels.SelectorFromSet(labels.Set{podspec.LabelManagedBy: podspec.ManagedByValue})},
	}
```

```go
	// The agent service only runs on the leader, so a standby must not be an
	// endpoint of the Service — otherwise agents would fill a registry nobody
	// reads and their servers would never reach Ready.
	if err := mgr.AddReadyzCheck("leader", func(_ *http.Request) error {
		select {
		case <-mgr.Elected():
			return nil
		default:
			return fmt.Errorf("not the leader yet")
		}
	}); err != nil {
		setupLog.Error(err, "unable to add ready check")
		os.Exit(1)
	}
```

Der `--operator-namespace` darf nicht leer sein: ohne ihn stimmen weder die SANs noch der Endpunkt. Mit klarer Meldung abbrechen.

- [ ] **Schritt 4: Die Manifeste**

`config/deploy/service.yaml`:

```yaml
---
apiVersion: v1
kind: Service
metadata:
  name: spawnery-operator
  namespace: spawnery-system
  labels:
    app.kubernetes.io/name: spawnery
    app.kubernetes.io/component: operator
spec:
  selector:
    app.kubernetes.io/name: spawnery
    app.kubernetes.io/component: operator
  ports:
    - name: agent
      port: 9443
      targetPort: agent
      protocol: TCP
```

In `config/deploy/deployment.yaml` den Port und die Namensvariable ergänzen:

```yaml
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
          ports:
            - name: metrics
              containerPort: 8080
            - name: health
              containerPort: 8081
            - name: agent
              containerPort: 9443
```

- [ ] **Schritt 5: Tests laufen lassen**

```bash
nix develop -c make test
```

Erwartet: alles grün, insbesondere `TestAgentOverTheWireBringsAServerToReady`.

- [ ] **Schritt 6: Commit**

```bash
git add cmd/ internal/controller/ config/deploy/
git commit -m "Der Agentkanal hängt im Operator

Zertifikatsverwaltung, gRPC-Dienst und Namespace-Bootstrap laufen als
Leader-gebundene Runnables; readyz meldet erst nach Erhalt der Sperre
grün, damit ein Standby gar nicht erst Agents anzieht. Der
Ende-zu-Ende-Test fasst die Registry nicht an: der einzige Weg nach
Ready führt über eine echte TLS-Verbindung mit echtem Token."
```

---

### Task 10: Rechte teilen und den Audit nachziehen

Entwurf Abschnitt 6.6. Erstmals eine `Role` neben der `ClusterRole`.

**Dateien:**
- Ändern: `internal/rbacaudit/required.go` (Tabelle teilen)
- Ändern: `internal/rbacaudit/audit_envtest_test.go`
- Ändern: `internal/controller/setup.go:47` (Leases-Marker auf `namespace=spawnery-system`)
- Ändern: `Makefile` (controller-gen erzeugt jetzt auch eine Role)
- Erstellen: `config/rbac/role_namespaced.yaml` beziehungsweise was controller-gen erzeugt
- Erstellen: `config/deploy/rolebinding.yaml`

**Schnittstellen:**
- Liefert: `var RequiredCluster []Permission` und `var RequiredNamespaced []Permission` anstelle des heutigen `Required`. Alle Nutzer von `Required` ziehen nach.

- [ ] **Schritt 1: Die Tests anpassen und einen neuen schreiben**

Die bestehenden Tests in `internal/rbacaudit/audit_envtest_test.go` auf `RequiredCluster` umstellen und die namespace-lokale Hälfte spiegeln. Dazu neu — das schließt den ersten offenen Punkt aus `docs/bekannte-punkte.md`, Abschnitt „Zum RBAC-Audit":

```go
// Without this the SAR direction can go quietly meaningless: if a second
// binding ever widened the subject, every Allowed would still be Allowed and
// no test would notice. A permission we deliberately never grant must come
// back denied.
func TestTheAuthorizerActuallyDenies(t *testing.T) {
	c, ctx := testenv.Client(t)
	applyOperatorRoles(t, ctx, c) // die bestehende Hilfe der Datei

	denied := []authzv1.ResourceAttributes{
		{Group: "", Resource: "secrets", Verb: "get", Namespace: "irgendein-fremder-namespace"},
		{Group: "", Resource: "nodes", Verb: "delete"},
		{Group: "rbac.authorization.k8s.io", Resource: "clusterroles", Verb: "create"},
	}
	for _, attrs := range denied {
		t.Run(attrs.Resource+"/"+attrs.Verb, func(t *testing.T) {
			review := &authzv1.SubjectAccessReview{
				Spec: authzv1.SubjectAccessReviewSpec{
					User:               operatorUser, // wie in den bestehenden Tests
					ResourceAttributes: &attrs,
				},
			}
			if err := c.Create(ctx, review); err != nil {
				t.Fatalf("SubjectAccessReview: %v", err)
			}
			if review.Status.Allowed {
				t.Errorf("the authorizer allows %s/%s — either the role is too wide, "+
					"or a second binding made the SAR direction meaningless",
					attrs.Resource, attrs.Verb)
			}
		})
	}
}
```

- [ ] **Schritt 2: Zum Fehlschlagen bringen**

```bash
nix develop -c go test ./internal/rbacaudit/ -v
```

Erwartet: FAIL — `RequiredCluster` gibt es nicht, und der Audit meldet die neuen Rechte aus den Tasks 5 bis 7 als „granted but not required" beziehungsweise umgekehrt.

- [ ] **Schritt 3: Die Marker und die Tabelle**

In `internal/controller/setup.go:47` den Leases-Marker um den Namespace erweitern:

```go
// Leader election locks on a Lease in the operator's own namespace, so the
// right belongs in a namespaced Role — granting it cluster-wide would allow
// locking anywhere.
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=spawnery-system,resources=leases,verbs=create;get;update
```

In `internal/certs/store.go` über `Ensure`:

```go
// +kubebuilder:rbac:groups="",namespace=spawnery-system,resources=secrets,verbs=get;create;update
```

In `internal/grpcauth/identity.go` über `Authenticate`:

```go
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
```

`Required` in `required.go` in zwei Tabellen teilen. Neue clusterweite Einträge:

```go
	{Group: "authentication.k8s.io", Resource: "tokenreviews", Verb: "create",
		Why: "grpcauth.Authenticator.Authenticate prüft jeden Agent-Token"},

	{Group: "", Resource: "configmaps", Verb: "get", Why: "Bootstrapper.Ensure liest die CA-ConfigMap"},
	{Group: "", Resource: "configmaps", Verb: "list", Why: "eingeschränkter Cache für die CA-ConfigMaps"},
	{Group: "", Resource: "configmaps", Verb: "watch", Why: "eingeschränkter Cache für die CA-ConfigMaps"},
	{Group: "", Resource: "configmaps", Verb: "create", Why: "Bootstrapper.Ensure legt die CA-ConfigMap an"},
	{Group: "", Resource: "configmaps", Verb: "update", Why: "Bootstrapper.Ensure zieht eine geänderte CA nach"},

	{Group: "", Resource: "serviceaccounts", Verb: "get", Why: "Bootstrapper.Ensure prüft den Server-SA"},
	{Group: "", Resource: "serviceaccounts", Verb: "list", Why: "eingeschränkter Cache für die Server-SAs"},
	{Group: "", Resource: "serviceaccounts", Verb: "watch", Why: "eingeschränkter Cache für die Server-SAs"},
	{Group: "", Resource: "serviceaccounts", Verb: "create", Why: "Bootstrapper.Ensure legt den Server-SA an"},
```

Und die namespace-lokale Tabelle:

```go
// RequiredNamespaced is what the operator does in its own namespace only.
// Both of these were cluster-wide before milestone 2a: the lease because
// nobody had split the table yet, and the secret would have been a bad idea
// from the start.
var RequiredNamespaced = []Permission{
	{Group: "", Resource: "secrets", Verb: "get", Why: "certs.Store.Ensure liest das TLS-Bündel"},
	{Group: "", Resource: "secrets", Verb: "create", Why: "certs.Store.Ensure legt es beim ersten Start an"},
	{Group: "", Resource: "secrets", Verb: "update", Why: "certs.Store.Ensure erneuert das Serving-Zertifikat"},

	{Group: "coordination.k8s.io", Resource: "leases", Verb: "create", Why: "Leader-Election beim Start"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "get", Why: "Leader-Election erneuert die Sperre"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "update", Why: "Leader-Election erneuert die Sperre"},
}
```

- [ ] **Schritt 4: Manifeste erzeugen und binden**

```bash
nix develop -c make manifests
```

controller-gen legt die namespace-lokale Role in `config/rbac/` ab. Prüfe den erzeugten Dateinamen und trage die passende `RoleBinding` in `config/deploy/rolebinding.yaml` nach, gebunden an `spawnery-operator` in `spawnery-system`. Der Leases-Eintrag muss aus `config/rbac/role.yaml` verschwunden sein — falls nicht, fehlt der `namespace=`-Zusatz an einem Marker.

- [ ] **Schritt 5: Tests laufen lassen**

```bash
nix develop -c make test
```

Erwartet: grün, inklusive `TestTheAuthorizerActuallyDenies` und der beiden Richtungen für beide Tabellenhälften.

- [ ] **Schritt 6: Commit**

```bash
git add internal/ config/ Makefile
git commit -m "Secrets und Leases in eine namespace-lokale Role

Clusterweite Secret-Schreibrechte wären ausgerechnet im Meilenstein, der
Sicherheit einzieht, das falsche Signal. Weil die Trennung ohnehin
entsteht, ziehen die Leases mit um — offener Punkt aus dem
Meilenstein-1-Review, erledigt statt vertagt. Neu ist außerdem eine
Sonde, die auf einer Verweigerung besteht: ohne sie könnte die
SAR-Richtung stillschweigend bedeutungslos werden."
```

---

### Task 11: Dokumentation nachziehen

**Dateien:**
- Ändern: `README.md` (Statusabschnitt, k3d-Ablauf)
- Ändern: `docs/bekannte-punkte.md`

- [ ] **Schritt 1: README**

Im Statusabschnitt festhalten, was jetzt gilt: der Agentkanal steht, ein Spieler kann sich weiterhin nicht verbinden (Meilenstein 3), und die Basis-Images fehlen noch (Meilenstein 2b) — der Pod hängt deshalb weiter in `ErrImagePull`. Im k3d-Ablauf ergänzen, dass `--operator-namespace` gesetzt sein muss, wenn der Operator außerhalb des Clusters läuft, und dass der Agentkanal dort mangels erreichbarem Service ungenutzt bleibt.

Im Entwicklungsabschnitt erwähnen, dass `make proto` den generierten gRPC-Code neu erzeugt und dass er eingecheckt ist.

- [ ] **Schritt 2: Bekannte Punkte**

Streichen, weil erledigt: „Leases gehören in eine namespaced Role" und „Die SAR-Richtung kann stillschweigend bedeutungslos werden".

Ergänzen, unter einer neuen Überschrift für Meilenstein 2b beziehungsweise 3:

- Der Verwaisten-Abgleich verwirft weiterhin Proxy-Agents — der Punkt bleibt, wird aber jetzt konkreter, weil es einen Kanal gibt.
- `ProxySession` antwortet `Unimplemented`; der `spawnery-proxy`-ServiceAccount wird von keinem Bootstrap angelegt.
- Die CA-Rotation hat kein Verfahren: das Bündelformat steht, der Überlappungspfad nicht. Läuft die CA nach zehn Jahren ab, oder muss sie kompromittiert ersetzt werden, gibt es dafür heute nur „Secret löschen und alle Pods neu starten".
- Der Kotlin-Agent muss überlappend neu verbinden (`SessionDeadline`), sonst fällt jeder Server im Rhythmus der harten Frist aus `Ready`.
- Auf Darwin kommen die envtest-Binaries aus den controller-tools-Releases statt aus nixpkgs; eine neue Kubernetes-Version erfordert dort einen neuen Hash im flake.

- [ ] **Schritt 3: Prüfen und committen**

```bash
nix develop -c make test
git add README.md docs/bekannte-punkte.md
git commit -m "Stand nach dem Agentkanal festhalten

Zwei Punkte aus dem Meilenstein-1-Review sind erledigt und fliegen
raus; dafür kommen die Übernahmen dazu, die 2a bewusst offenlässt —
allen voran die fehlende CA-Rotation und die Pflicht des Kotlin-Agents,
überlappend neu zu verbinden."
```

---

## Abschluss

Nach Task 11 gilt das Erfolgskriterium aus Abschnitt 1 des Entwurfs als erfüllt, wenn `TestAgentOverTheWireBringsAServerToReady` grün ist **und** kein Test in `internal/controller` mehr `agents.MarkReady` von Hand ruft, außer denen, die gezielt die Zustandsmaschine ohne Kanal prüfen.

Vor dem Zusammenführen: `superpowers:requesting-code-review`, danach `superpowers:finishing-a-development-branch`.
