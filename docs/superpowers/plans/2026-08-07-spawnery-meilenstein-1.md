# Spawnery Meilenstein 1 — Implementierungsplan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ein lauffähiger Go-Operator, der die vier Spawnery-CRDs installiert, aus einer ephemeren `ServerGroup` Paper-Pods erzeugt und jeden `Server` über die vollständige Zustandsmaschine inklusive Readiness-Verlust und Verwaisten-Abgleich führt.

**Architecture:** Ein einzelner controller-runtime-Manager mit vier Controllern (Network, ServerGroup, Server, Verwaisten-Abgleich). Die gesamte Entscheidungslogik liegt in reinen, Kubernetes-freien Paketen (`internal/phase` für die Zustandsmaschine, `internal/agent` für den Laufzeitzustand der Agents, `internal/podspec` für den Pod-Bau); die Controller sind dünne Adapter, die Eingaben einsammeln, die reine Funktion aufrufen und deren Entscheidung ausführen. Der Agent-Kanal existiert in M1 nur als Port (`agent.Registry`) — Meilenstein 2 hängt den gRPC-Dienst dahinter, ohne die Controller anzufassen.

**Tech Stack:** Go 1.26, sigs.k8s.io/controller-runtime v0.24.1, k8s.io/{api,apimachinery,client-go} v0.36.0, controller-gen 0.21.0, envtest (kube-apiserver 1.36 + etcd 3.6 aus nixpkgs), testify v1.11.1, Nix-Flake als Dev-Shell.

## Global Constraints

Diese Vorgaben gelten für **jede** Task; sie werden nicht in jeder Task wiederholt.

- **API-Gruppe:** `spawnery.cloud`, **Version:** `v1alpha1`. Alle Ressourcen sind namespaced.
- **Go-Modulpfad:** `github.com/spawnery/spawnery`.
- **Lizenz:** Apache-2.0 (permissiv, Standard im Kubernetes-Ökosystem). Kein Code aus Shulker (AGPL-3.0) — nur Architektur als Referenz.
- **Sprache:** Code, API-Feldnamen, Kommentare, Log- und Condition-Messages auf **Englisch** (Open-Source-Zielgruppe). Spec, Plan, README und Commit-Messages auf **Deutsch**.
- **Label-Präfix:** `spawnery.cloud/`. Feste Labels auf jedem erzeugten Pod: `spawnery.cloud/managed-by=spawnery-operator`, `spawnery.cloud/network`, `spawnery.cloud/group`, `spawnery.cloud/server`, `spawnery.cloud/role` (`server` oder `proxy`).
- **Minecraft-Port:** 25565, Containerport-Name `minecraft`.
- **Kern-Invariante:** Ein Pod mit Spielern wird nie gelöscht. Jede Auswahl-, Skalierungs- und Löschlogik muss dagegen getestet sein, auch bei veralteten Spielerzahlen (veraltet ⇒ belegt).
- **TDD:** Der Test entsteht vor der Implementierung. Jede Task endet mit einem Commit; Commit-Messages auf Deutsch, ohne Präfix-Konvention wie `feat:` (das Repo verwendet Klartext-Messages).
- **Alle Kommandos laufen in der Dev-Shell:** `nix develop -c <kommando>`.
- **Uhrzeit ist injizierbar:** Kein Paket ruft `time.Now()` direkt in Logik auf; die Uhr wird als `func() time.Time` übergeben. Das ist Voraussetzung für die Zeittests.

---

### Task 1: Dev-Umgebung, Go-Modul und Makefile

**Files:**
- Create: `flake.nix`
- Create: `.gitignore`
- Create: `LICENSE`
- Create: `Makefile`
- Create: `go.mod`
- Create: `hack/boilerplate.go.txt`
- Create: `internal/version/version.go`
- Test: `internal/version/version_test.go`

**Interfaces:**
- Consumes: nichts.
- Produces: die Dev-Shell (`nix develop`) mit `go`, `controller-gen`, `kustomize`, `kubectl`, `golangci-lint`, `gotestsum`, `kind`, `k3d` und der Umgebungsvariablen `KUBEBUILDER_ASSETS`, die auf ein Verzeichnis mit `kube-apiserver`, `etcd` und `kubectl` zeigt. Die Make-Ziele `manifests`, `generate`, `fmt`, `vet`, `test`, `build`, `lint`. Die Konstante `version.Version string`.

- [ ] **Step 1: Nix-Flake schreiben**

`setup-envtest` lädt vorgefertigte Binaries herunter, die auf NixOS ohne `patchelf` nicht starten. Deshalb werden die envtest-Assets aus nixpkgs zusammengesetzt.

`flake.nix`:

```nix
{
  description = "Spawnery — Kubernetes-natives Cloud-System für Minecraft-Netzwerke";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      devShells = forAllSystems (pkgs:
        let
          # envtest braucht genau diese drei Binaries in einem Verzeichnis.
          envtestAssets = pkgs.runCommand "envtest-assets" { } ''
            mkdir -p $out
            ln -s ${pkgs.kubernetes}/bin/kube-apiserver $out/kube-apiserver
            ln -s ${pkgs.etcd}/bin/etcd                 $out/etcd
            ln -s ${pkgs.kubectl}/bin/kubectl           $out/kubectl
          '';
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              gotools
              golangci-lint
              gotestsum
              kubernetes-controller-tools
              kustomize
              kubectl
              kubernetes-helm
              kind
              k3d
            ];

            env = {
              KUBEBUILDER_ASSETS = "${envtestAssets}";
            };
          };
        });
    };
}
```

- [ ] **Step 2: Flake bauen und Werkzeuge prüfen**

```bash
git add flake.nix
nix develop -c bash -c 'go version && controller-gen --version && ls "$KUBEBUILDER_ASSETS"'
```

Erwartet: `go version go1.26.x`, `Version: v0.21.0`, und die drei Symlinks `etcd`, `kube-apiserver`, `kubectl`.

`git add flake.nix` ist nötig, weil Nix nur eingecheckte Dateien eines Flakes sieht.

- [ ] **Step 3: .gitignore, LICENSE und Boilerplate anlegen**

`.gitignore`:

```
bin/
cover.out
result
result-*
.direnv/
```

`LICENSE`: den unveränderten Apache-2.0-Text einsetzen (`curl -sL https://www.apache.org/licenses/LICENSE-2.0.txt -o LICENSE`), danach prüfen, dass die Datei mit `Apache License` beginnt und rund 11 kB groß ist.

`hack/boilerplate.go.txt`:

```
/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
```

- [ ] **Step 4: Go-Modul initialisieren**

```bash
nix develop -c go mod init github.com/spawnery/spawnery
nix develop -c go get sigs.k8s.io/controller-runtime@v0.24.1
nix develop -c go get k8s.io/api@v0.36.0 k8s.io/apimachinery@v0.36.0 k8s.io/client-go@v0.36.0
nix develop -c go get github.com/stretchr/testify@v1.11.1
```

- [ ] **Step 5: Makefile schreiben**

```make
CONTROLLER_GEN ?= controller-gen

.PHONY: all
all: manifests generate fmt vet test build

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=spawnery-operator paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: manifests generate fmt vet
	go test ./... -coverprofile cover.out

.PHONY: build
build:
	go build -o bin/spawnery-operator ./cmd/spawnery-operator

.PHONY: lint
lint:
	golangci-lint run
```

- [ ] **Step 6: Den fehlschlagenden Test schreiben**

`internal/version/version_test.go`:

```go
package version

import "testing"

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
}
```

- [ ] **Step 7: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/version/...`
Expected: FAIL — `undefined: Version`.

- [ ] **Step 8: Implementieren**

`internal/version/version.go`:

```go
// Package version carries the operator build version.
package version

// Version is the operator version. Release builds override it via
// -ldflags "-X github.com/spawnery/spawnery/internal/version.Version=v1.2.3".
var Version = "dev"
```

- [ ] **Step 9: Test laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/version/...`
Expected: PASS (`ok  github.com/spawnery/spawnery/internal/version`).

- [ ] **Step 10: Commit**

```bash
git add flake.nix flake.lock .gitignore LICENSE Makefile go.mod go.sum hack internal
git commit -m "Nix-Dev-Shell, Go-Modul und Makefile"
```

---

### Task 2: API-Grundgerüst, Network-CRD und envtest-Suite

**Files:**
- Create: `api/v1alpha1/groupversion_info.go`
- Create: `api/v1alpha1/common_types.go`
- Create: `api/v1alpha1/network_types.go`
- Create: `internal/testenv/testenv.go`
- Test: `api/v1alpha1/network_envtest_test.go`

**Interfaces:**
- Consumes: nichts aus früheren Tasks.
- Produces:
  - `v1alpha1.GroupVersion` (`schema.GroupVersion{Group: "spawnery.cloud", Version: "v1alpha1"}`), `v1alpha1.AddToScheme`.
  - `v1alpha1.ObjectRef{Name string}`.
  - `v1alpha1.Scheduling{NodeSelector map[string]string, Tolerations []corev1.Toleration, Affinity *corev1.Affinity}`.
  - `v1alpha1.Defaults{MinecraftVersion string, ImagePullSecrets []corev1.LocalObjectReference, Resources *corev1.ResourceRequirements, Scheduling *Scheduling}`.
  - `v1alpha1.Mount{Name string, MountPath string, ConfigMap *corev1.ConfigMapVolumeSource, Secret *corev1.SecretVolumeSource}`.
  - `v1alpha1.Network`, `NetworkSpec{ForwardingSecretRef ObjectRef, Defaults *Defaults}`, `NetworkStatus{Conditions []metav1.Condition, ProxyGroups, ServerGroups int32, OnlinePlayers int32}`.
  - Condition-Typen als Konstanten: `ConditionAccepted = "Accepted"`, `ConditionReady = "Ready"`, `ConditionDegraded = "Degraded"`.
  - `testenv.Start(t *testing.T) (client.Client, context.Context)` — startet einmalig einen envtest-API-Server mit den CRDs aus `config/crd/bases` und gibt einen Client zurück.

- [ ] **Step 1: envtest-Helfer schreiben**

`internal/testenv/testenv.go`:

```go
// Package testenv boots a shared envtest control plane for controller tests.
package testenv

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

var (
	once sync.Once
	env  *envtest.Environment
	cfg  *rest.Config
	sch  *runtime.Scheme
	boot error
)

// crdPath walks up from the test's working directory until it finds
// config/crd/bases, so tests work from any package.
func crdPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "config", "crd", "bases")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("config/crd/bases not found — run 'make manifests' first")
		}
		dir = parent
	}
}

// Config starts the shared control plane on first use and returns its config.
func Config(t *testing.T) *rest.Config {
	t.Helper()
	once.Do(func() {
		sch = runtime.NewScheme()
		if boot = clientgoscheme.AddToScheme(sch); boot != nil {
			return
		}
		if boot = spawneryv1alpha1.AddToScheme(sch); boot != nil {
			return
		}
		env = &envtest.Environment{
			CRDDirectoryPaths:     []string{crdPath(t)},
			ErrorIfCRDPathMissing: true,
		}
		cfg, boot = env.Start()
	})
	if boot != nil {
		t.Fatalf("start envtest: %v", boot)
	}
	return cfg
}

// Scheme returns the scheme the shared control plane was started with.
func Scheme(t *testing.T) *runtime.Scheme {
	Config(t)
	return sch
}

// Client returns a client against the shared control plane, plus a context
// cancelled at the end of the test.
func Client(t *testing.T) (client.Client, context.Context) {
	t.Helper()
	c, err := client.New(Config(t), client.Options{Scheme: Scheme(t)})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return c, ctx
}

// Namespace creates a unique namespace for one test and returns its name.
// Every test gets its own, so the one-network-per-namespace rule and the
// group names never collide across tests.
func Namespace(t *testing.T, ctx context.Context, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "spawnery-test-"},
	}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

// Stop tears the control plane down. Call it from TestMain of every test
// package that uses testenv, once all tests in that package have run.
func Stop() error {
	if env == nil {
		return nil
	}
	return env.Stop()
}
```

- [ ] **Step 2: Den fehlschlagenden Test schreiben**

`api/v1alpha1/network_envtest_test.go`:

```go
package v1alpha1_test

import (
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

func TestNetworkRoundTrip(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			Defaults: &spawneryv1alpha1.Defaults{
				MinecraftVersion: "1.21.4",
			},
		},
	}
	if err := c.Create(ctx, net); err != nil {
		t.Fatalf("create Network: %v", err)
	}

	got := &spawneryv1alpha1.Network{}
	if err := c.Get(ctx, types.NamespacedName{Name: "production", Namespace: ns}, got); err != nil {
		t.Fatalf("get Network: %v", err)
	}
	if got.Spec.ForwardingSecretRef.Name != "velocity-forwarding-secret" {
		t.Errorf("forwardingSecretRef = %q, want velocity-forwarding-secret", got.Spec.ForwardingSecretRef.Name)
	}
	if got.Spec.Defaults.MinecraftVersion != "1.21.4" {
		t.Errorf("minecraftVersion = %q, want 1.21.4", got.Spec.Defaults.MinecraftVersion)
	}
}

func TestNetworkRequiresForwardingSecretRef(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: ns},
	}
	if err := c.Create(ctx, net); err == nil {
		t.Fatal("create without forwardingSecretRef succeeded, want rejection")
	}
}
```

- [ ] **Step 3: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./api/... -run TestNetwork -v`
Expected: FAIL — es kompiliert nichts: `no required module provides package github.com/spawnery/spawnery/api/v1alpha1`, gefolgt von `undefined: spawneryv1alpha1.Network`. Das ist die rote Phase; die Typen entstehen jetzt.

- [ ] **Step 4: GroupVersion anlegen**

`api/v1alpha1/groupversion_info.go` (Boilerplate-Header aus Task 1 voranstellen; das gilt für alle Go-Dateien ab hier):

```go
// Package v1alpha1 contains the Spawnery API types.
// +kubebuilder:object:generate=true
// +groupName=spawnery.cloud
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version for all Spawnery types.
	GroupVersion = schema.GroupVersion{Group: "spawnery.cloud", Version: "v1alpha1"}

	// SchemeBuilder registers the Spawnery types with a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the Spawnery types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
```

- [ ] **Step 5: Gemeinsame Typen anlegen**

`api/v1alpha1/common_types.go`:

```go
package v1alpha1

import corev1 "k8s.io/api/core/v1"

// Condition types used across all Spawnery resources.
const (
	// ConditionAccepted reports whether the operator manages this object at all.
	ConditionAccepted = "Accepted"
	// ConditionReady reports whether the object serves its purpose.
	ConditionReady = "Ready"
	// ConditionDegraded reports a persistent problem that needs attention.
	ConditionDegraded = "Degraded"
)

// Condition reasons.
const (
	ReasonDuplicateNetwork = "DuplicateNetwork"
	ReasonNetworkNotFound  = "NetworkNotFound"
	ReasonGroupNotFound    = "GroupNotFound"
	ReasonAccepted         = "Accepted"
	ReasonCrashLoopBackoff = "CrashLoopBackoff"
	ReasonNoFallback       = "NoFallbackAvailable"
	ReasonNotImplemented   = "NotImplementedInThisVersion"
	ReasonReconciling      = "Reconciling"
)

// ObjectRef names another object in the same namespace.
type ObjectRef struct {
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// Scheduling controls where pods are placed.
type Scheduling struct {
	// NodeSelector restricts pods to nodes carrying all these labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow pods onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity expresses scheduling preferences and constraints.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// Defaults are inherited by every ProxyGroup and ServerGroup of a Network.
// Each field can be overridden on the group.
type Defaults struct {
	// MinecraftVersion documents the version the images of this network carry.
	// +optional
	MinecraftVersion string `json:"minecraftVersion,omitempty"`

	// ImagePullSecrets are attached to every managed pod.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Resources are the default container resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling is the default pod placement.
	// +optional
	Scheduling *Scheduling `json:"scheduling,omitempty"`
}

// Mount is a single file mount into a managed pod. V1 supports ConfigMaps and
// Secrets only; the layered template system is a later project.
// +kubebuilder:validation:XValidation:rule="has(self.configMap) != has(self.secret)",message="exactly one of configMap or secret must be set"
type Mount struct {
	// Name of the volume inside the pod.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// MountPath is the absolute path inside the container.
	// +kubebuilder:validation:Pattern=`^/.*`
	MountPath string `json:"mountPath"`

	// ConfigMap source.
	// +optional
	ConfigMap *corev1.ConfigMapVolumeSource `json:"configMap,omitempty"`

	// Secret source.
	// +optional
	Secret *corev1.SecretVolumeSource `json:"secret,omitempty"`
}
```

- [ ] **Step 6: Network-Typ anlegen**

`api/v1alpha1/network_types.go`:

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// NetworkSpec describes one Minecraft network. Exactly one Network may exist
// per namespace; further ones are rejected with an Accepted=False condition.
type NetworkSpec struct {
	// ForwardingSecretRef names the Secret holding the Velocity modern
	// forwarding secret under the key "secret".
	ForwardingSecretRef ObjectRef `json:"forwardingSecretRef"`

	// Defaults are inherited by all groups of this network.
	// +optional
	Defaults *Defaults `json:"defaults,omitempty"`
}

// NetworkStatus is the observed state of a Network.
type NetworkStatus struct {
	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ProxyGroups is the number of ProxyGroups referencing this network.
	// +optional
	ProxyGroups int32 `json:"proxyGroups"`

	// ServerGroups is the number of ServerGroups referencing this network.
	// +optional
	ServerGroups int32 `json:"serverGroups"`

	// OnlinePlayers is the sum of players across all server groups.
	// +optional
	OnlinePlayers int32 `json:"onlinePlayers"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcnet
// +kubebuilder:printcolumn:name="Server Groups",type=integer,JSONPath=`.status.serverGroups`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.onlinePlayers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Network is the root resource of a Minecraft network.
type Network struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkSpec   `json:"spec,omitempty"`
	Status NetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetworkList contains a list of Network.
type NetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Network `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Network{}, &NetworkList{})
}
```

- [ ] **Step 7: Deepcopy und CRDs generieren**

```bash
nix develop -c make generate manifests
ls config/crd/bases
```

Expected: `spawnery.cloud_networks.yaml` existiert, und `api/v1alpha1/zz_generated.deepcopy.go` wurde erzeugt.

- [ ] **Step 8: Test laufen lassen, Erfolg prüfen**

Die Typen aus Step 4 bis 6 und die Manifeste aus Step 7 sind jetzt da; der Test aus Step 2 muss grün werden.

Run: `nix develop -c go test ./api/... -run TestNetwork -v`
Expected: PASS für `TestNetworkRoundTrip` und `TestNetworkRequiresForwardingSecretRef`.

Schlägt er mit `config/crd/bases not found` fehl, wurde `make manifests` aus Step 7 nicht ausgeführt. Schlägt er mit `unable to start control plane` fehl, ist `KUBEBUILDER_ASSETS` nicht gesetzt — dann läuft der Aufruf nicht in der Dev-Shell.

- [ ] **Step 9: Commit**

```bash
git add api internal/testenv config Makefile
git commit -m "API-Grundgerüst, Network-CRD und envtest-Suite"
```

---

### Task 3: ServerGroup-CRD mit CEL-Validierung

**Files:**
- Create: `api/v1alpha1/servergroup_types.go`
- Test: `api/v1alpha1/servergroup_envtest_test.go`

**Interfaces:**
- Consumes: `ObjectRef`, `Scheduling`, `Mount`, `Defaults`, `SchemeBuilder` aus Task 2.
- Produces:
  - `v1alpha1.ServerGroupType` mit Werten `ServerGroupEphemeral = "Ephemeral"` und `ServerGroupPersistent = "Persistent"`.
  - `v1alpha1.ServerGroup`, `ServerGroupSpec`, `ServerGroupStatus`.
  - `ScalingSpec{MinReplicas, MaxReplicas, SpareSlots int32, ScaleDownStabilizationSeconds int32}`.
  - `UpdateSpec{MaxUnavailable int32, MaxStaleSeconds int32}`.
  - `DrainSpec{TimeoutSeconds int32}`.
  - `StorageSpec{Size resource.Quantity, StorageClassName *string, AccessModes []corev1.PersistentVolumeAccessMode}`.
  - Die Methoden `(*ServerGroup) DrainTimeout() time.Duration`, `FailedRetention() time.Duration`, `DesiredReplicas() int32`, `IsEphemeral() bool`.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`api/v1alpha1/servergroup_envtest_test.go`:

```go
package v1alpha1_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func ephemeralGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: 1,
				MaxReplicas: 10,
				SpareSlots:  40,
			},
		},
	}
}

func persistentGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:       spawneryv1alpha1.ServerGroupPersistent,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 40,
			Replicas:   ptr.To[int32](1),
			Storage: &spawneryv1alpha1.StorageSpec{
				Size:             resource.MustParse("20Gi"),
				StorageClassName: ptr.To("longhorn"),
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
}

func TestServerGroupEphemeralAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	if err := c.Create(ctx, ephemeralGroup(ns, "lobby")); err != nil {
		t.Fatalf("create ephemeral group: %v", err)
	}
}

func TestServerGroupPersistentAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	if err := c.Create(ctx, persistentGroup(ns, "survival")); err != nil {
		t.Fatalf("create persistent group: %v", err)
	}
}

func TestServerGroupCELRejections(t *testing.T) {
	c, ctx := testenv.Client(t)

	cases := []struct {
		name   string
		mutate func(*spawneryv1alpha1.ServerGroup)
		base   func(ns, name string) *spawneryv1alpha1.ServerGroup
	}{
		{
			name: "ephemeral with storage",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
			},
		},
		{
			name: "ephemeral with replicas",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Replicas = ptr.To[int32](3)
			},
		},
		{
			name: "persistent with scaling",
			base: persistentGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Scaling = &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10}
			},
		},
		{
			name: "persistent with update",
			base: persistentGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1}
			},
		},
		{
			name: "persistent without storage",
			base: persistentGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Storage = nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := testenv.Namespace(t, ctx, c)
			g := tc.base(ns, "group")
			tc.mutate(g)
			if err := c.Create(ctx, g); err == nil {
				t.Fatalf("create succeeded, want CEL rejection")
			}
		})
	}
}

func TestServerGroupImmutableFields(t *testing.T) {
	c, ctx := testenv.Client(t)

	t.Run("type is immutable", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := ephemeralGroup(ns, "lobby")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
		g.Spec.Scaling = nil
		g.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update changed spec.type, want rejection")
		}
	})

	t.Run("storageClassName is immutable", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.StorageClassName = ptr.To("ceph")
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update changed storageClassName, want rejection")
		}
	})

	t.Run("storage size may grow", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.Size = resource.MustParse("30Gi")
		if err := c.Update(ctx, g); err != nil {
			t.Fatalf("growing storage.size rejected: %v", err)
		}
	})

	t.Run("storage size may not shrink", func(t *testing.T) {
		ns := testenv.Namespace(t, ctx, c)
		g := persistentGroup(ns, "survival")
		if err := c.Create(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		g.Spec.Storage.Size = resource.MustParse("10Gi")
		if err := c.Update(ctx, g); err == nil {
			t.Fatal("update shrank storage.size, want rejection")
		}
	})
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./api/... -run TestServerGroup -v`
Expected: FAIL — `undefined: spawneryv1alpha1.ServerGroup`.

- [ ] **Step 3: ServerGroup-Typ implementieren**

`api/v1alpha1/servergroup_types.go`:

```go
package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServerGroupType selects the operating mode of a group.
// +kubebuilder:validation:Enum=Ephemeral;Persistent
type ServerGroupType string

const (
	// ServerGroupEphemeral loses its state on stop: minigames and lobbies.
	ServerGroupEphemeral ServerGroupType = "Ephemeral"
	// ServerGroupPersistent keeps its world on a PVC: survival and creative.
	ServerGroupPersistent ServerGroupType = "Persistent"
)

// ScalingSpec drives slot-based scaling of ephemeral groups.
type ScalingSpec struct {
	// MinReplicas is the number of servers kept running at all times.
	// +kubebuilder:validation:Minimum=0
	MinReplicas int32 `json:"minReplicas"`

	// MaxReplicas caps the number of servers.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// SpareSlots is the number of free player slots kept available.
	// +kubebuilder:validation:Minimum=0
	SpareSlots int32 `json:"spareSlots"`

	// ScaleDownStabilizationSeconds is how long a server must be empty before
	// it is eligible for scale-down.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +optional
	ScaleDownStabilizationSeconds int32 `json:"scaleDownStabilizationSeconds,omitempty"`
}

// UpdateSpec controls the rolling update of ephemeral groups.
type UpdateSpec struct {
	// MaxUnavailable is how many servers may be draining or terminating at the
	// same time because of a generation change.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxUnavailable int32 `json:"maxUnavailable,omitempty"`

	// MaxStaleSeconds forces an active drain of stale servers after this many
	// seconds. 0 means stale servers are never actively emptied.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxStaleSeconds int32 `json:"maxStaleSeconds,omitempty"`
}

// DrainSpec bounds how long players may be moved off a server.
type DrainSpec struct {
	// TimeoutSeconds is the upper bound for the drain.
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

// StorageSpec describes the PVC of a persistent group.
type StorageSpec struct {
	// Size of the volume. May grow, never shrink; actual expansion requires
	// allowVolumeExpansion on the StorageClass.
	Size resource.Quantity `json:"size"`

	// StorageClassName is immutable once set.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// AccessModes are immutable once set.
	// +kubebuilder:default={ReadWriteOnce}
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
}

// ServerGroupSpec describes a group of Minecraft servers.
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="spec.type is immutable"
// +kubebuilder:validation:XValidation:rule="self.type != 'Ephemeral' || !has(self.storage)",message="spec.storage is not allowed for type Ephemeral"
// +kubebuilder:validation:XValidation:rule="self.type != 'Ephemeral' || !has(self.replicas)",message="spec.replicas is not allowed for type Ephemeral"
// +kubebuilder:validation:XValidation:rule="self.type != 'Ephemeral' || has(self.scaling)",message="spec.scaling is required for type Ephemeral"
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || !has(self.scaling)",message="spec.scaling is not allowed for type Persistent"
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || !has(self.update)",message="spec.update is not allowed for type Persistent"
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || has(self.storage)",message="spec.storage is required for type Persistent"
// +kubebuilder:validation:XValidation:rule="!has(self.scaling) || self.scaling.minReplicas <= self.scaling.maxReplicas",message="scaling.minReplicas must not exceed scaling.maxReplicas"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || (has(self.storage.storageClassName) == has(oldSelf.storage.storageClassName) && (!has(self.storage.storageClassName) || self.storage.storageClassName == oldSelf.storage.storageClassName))",message="storage.storageClassName is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || self.storage.accessModes == oldSelf.storage.accessModes",message="storage.accessModes is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || quantity(self.storage.size).compareTo(quantity(oldSelf.storage.size)) >= 0",message="storage.size must not shrink"
type ServerGroupSpec struct {
	// NetworkRef names the Network this group belongs to.
	NetworkRef ObjectRef `json:"networkRef"`

	// Type selects ephemeral or persistent operation. Immutable.
	Type ServerGroupType `json:"type"`

	// Image is the Paper base image. A digest reference is recommended.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// MaxPlayers is the player capacity of a single server of this group.
	// +kubebuilder:validation:Minimum=1
	MaxPlayers int32 `json:"maxPlayers"`

	// Replicas is the fixed number of persistent servers. Ephemeral groups are
	// sized by scaling instead.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources overrides Network.spec.defaults.resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling overrides Network.spec.defaults.scheduling.
	// +optional
	Scheduling *Scheduling `json:"scheduling,omitempty"`

	// Mounts are extra ConfigMap and Secret mounts.
	// +optional
	// +listType=map
	// +listMapKey=name
	Mounts []Mount `json:"mounts,omitempty"`

	// Scaling configures slot-based scaling. Ephemeral only.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`

	// Update configures the rolling update. Ephemeral only.
	// +optional
	Update *UpdateSpec `json:"update,omitempty"`

	// Storage configures the PVC. Persistent only.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	// Drain bounds how long players may be moved off a server.
	// +kubebuilder:default={timeoutSeconds:60}
	// +optional
	Drain *DrainSpec `json:"drain,omitempty"`

	// TerminationGracePeriodSeconds is the time the pod gets to save its world.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +optional
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// FailedRetentionSeconds is how long a Failed server is kept for diagnosis.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailedRetentionSeconds int32 `json:"failedRetentionSeconds,omitempty"`
}

// ServerGroupStatus is the observed state of a ServerGroup.
type ServerGroupStatus struct {
	// Phase is derived from the servers and conditions of this group.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Replicas is the number of Server objects owned by this group.
	// +optional
	Replicas int32 `json:"replicas"`

	// ReadyReplicas is the number of servers in phase Ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// OnlinePlayers is the sum of players across all ready servers.
	// +optional
	OnlinePlayers int32 `json:"onlinePlayers"`

	// FreeSlots is the sum of free slots across ready servers of the current
	// generation. Stale servers do not count.
	// +optional
	FreeSlots int32 `json:"freeSlots"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcgroup
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.onlinePlayers`
// +kubebuilder:printcolumn:name="Free Slots",type=integer,JSONPath=`.status.freeSlots`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ServerGroup is a group of interchangeable Minecraft servers.
type ServerGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerGroupSpec   `json:"spec,omitempty"`
	Status ServerGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServerGroupList contains a list of ServerGroup.
type ServerGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServerGroup `json:"items"`
}

// IsEphemeral reports whether this group is ephemeral.
func (g *ServerGroup) IsEphemeral() bool {
	return g.Spec.Type == ServerGroupEphemeral
}

// DesiredReplicas is the number of servers the group must have at minimum.
// Milestone 1 sizes ephemeral groups at their floor; slot-based scaling on top
// of this arrives in milestone 4.
func (g *ServerGroup) DesiredReplicas() int32 {
	if g.IsEphemeral() {
		if g.Spec.Scaling == nil {
			return 0
		}
		return g.Spec.Scaling.MinReplicas
	}
	if g.Spec.Replicas == nil {
		return 0
	}
	return *g.Spec.Replicas
}

// DrainTimeout is the configured drain timeout.
func (g *ServerGroup) DrainTimeout() time.Duration {
	if g.Spec.Drain == nil {
		return 60 * time.Second
	}
	return time.Duration(g.Spec.Drain.TimeoutSeconds) * time.Second
}

// FailedRetention is how long a Failed server is kept.
func (g *ServerGroup) FailedRetention() time.Duration {
	return time.Duration(g.Spec.FailedRetentionSeconds) * time.Second
}

func init() {
	SchemeBuilder.Register(&ServerGroup{}, &ServerGroupList{})
}
```

- [ ] **Step 4: Manifeste neu erzeugen und Test laufen lassen**

```bash
nix develop -c make manifests generate
nix develop -c go test ./api/... -run TestServerGroup -v
```

Expected: PASS für alle Unterfälle von `TestServerGroupCELRejections` und `TestServerGroupImmutableFields`.

Falls `quantity(...)` als unbekannte Funktion abgelehnt wird, ist der envtest-API-Server zu alt; die Regel setzt Kubernetes ≥ 1.29 voraus. Version prüfen mit `nix develop -c bash -c '$KUBEBUILDER_ASSETS/kube-apiserver --version'`.

- [ ] **Step 5: Commit**

```bash
git add api config
git commit -m "ServerGroup-CRD mit CEL-Validierung"
```

---

### Task 4: Server- und ProxyGroup-CRD

**Files:**
- Create: `api/v1alpha1/server_types.go`
- Create: `api/v1alpha1/proxygroup_types.go`
- Test: `api/v1alpha1/server_envtest_test.go`
- Test: `api/v1alpha1/proxygroup_envtest_test.go`

**Interfaces:**
- Consumes: `ObjectRef`, `Scheduling`, `DrainSpec`, `SchemeBuilder`.
- Produces:
  - `v1alpha1.Server`, `ServerSpec{GroupRef ObjectRef, Ordinal *int32, GroupGeneration int64}`, `ServerStatus`.
  - `ServerStatus`-Felder: `Phase string`, `PodName string`, `Address string`, `Players int32`, `Slots int32`, `PlayersUpdatedAt *metav1.Time`, `Registered bool`, `WasRegistered bool`, `StartedAt *metav1.Time`, `ReadySince *metav1.Time`, `DrainStartedAt *metav1.Time`, `FailedAt *metav1.Time`, `ReadinessLosses int32`, `Conditions []metav1.Condition`.
  - `v1alpha1.ProxyGroup`, `ProxyGroupSpec`, `ExposeSpec`, `ProxyGroupStatus`.
  - `ExposeType` mit `ExposeLoadBalancer`, `ExposeNodePort`, `ExposeHostPort`.

Die vier Status-Zeitstempel und `ReadinessLosses` gehen über das Beispiel in der Spec hinaus. Sie sind die Buchführung, aus der die Zustandsmaschine ihre Zeit-Eingaben (`StartupDeadlineReached`, `DrainDeadlineReached`, `FailedRetentionElapsed`, `ReadyFor`) ableitet, und müssen einen Operator-Neustart überleben — deshalb Status und nicht Speicher.

- [ ] **Step 1: Den fehlschlagenden Test für Server schreiben**

`api/v1alpha1/server_envtest_test.go`:

```go
package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestServerStatusIsASubresource(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: "lobby"},
			GroupGeneration: 7,
		},
		Status: spawneryv1alpha1.ServerStatus{Phase: "Ready"},
	}
	if err := c.Create(ctx, srv); err != nil {
		t.Fatalf("create Server: %v", err)
	}

	got := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: ns}, got); err != nil {
		t.Fatalf("get Server: %v", err)
	}
	if got.Status.Phase != "" {
		t.Errorf("status survived create, want it dropped: %q", got.Status.Phase)
	}

	got.Status.Phase = "Starting"
	got.Status.Players = 3
	got.Status.Slots = 100
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatalf("status update: %v", err)
	}

	again := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: ns}, again); err != nil {
		t.Fatalf("get after status update: %v", err)
	}
	if again.Status.Phase != "Starting" || again.Status.Players != 3 {
		t.Errorf("status = %+v, want phase Starting and 3 players", again.Status)
	}
}

func TestServerRequiresGroupRef(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: ns},
	}
	if err := c.Create(ctx, srv); err == nil {
		t.Fatal("create without groupRef succeeded, want rejection")
	}
}
```

- [ ] **Step 2: Den fehlschlagenden Test für ProxyGroup schreiben**

`api/v1alpha1/proxygroup_envtest_test.go`:

```go
package v1alpha1_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

func proxyGroup(ns string, expose spawneryv1alpha1.ExposeSpec) *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.1.0",
			Expose:     expose,
			Routing: spawneryv1alpha1.RoutingSpec{
				FallbackGroups: []string{"lobby"},
			},
		},
	}
}

func TestProxyGroupExposeValidation(t *testing.T) {
	c, ctx := testenv.Client(t)

	cases := []struct {
		name    string
		expose  spawneryv1alpha1.ExposeSpec
		wantErr bool
	}{
		{
			name:   "loadbalancer without sub-block",
			expose: spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer},
		},
		{
			name: "nodeport with matching sub-block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30565},
			},
		},
		{
			name: "hostport with matching sub-block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
		},
		{
			name:    "nodeport without sub-block",
			expose:  spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort},
			wantErr: true,
		},
		{
			name:    "hostport without sub-block",
			expose:  spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeHostPort},
			wantErr: true,
		},
		{
			name: "loadbalancer with nodePort block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeLoadBalancer,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30565},
			},
			wantErr: true,
		},
		{
			name: "nodeport with hostPort block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30565},
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns := testenv.Namespace(t, ctx, c)
			err := c.Create(ctx, proxyGroup(ns, tc.expose))
			if tc.wantErr && err == nil {
				t.Fatal("create succeeded, want CEL rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("create rejected: %v", err)
			}
		})
	}
}
```

- [ ] **Step 3: Beide Tests laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./api/... -run 'TestServer|TestProxyGroup' -v`
Expected: FAIL — `undefined: spawneryv1alpha1.Server`, `undefined: spawneryv1alpha1.ProxyGroup`.

- [ ] **Step 4: Server-Typ implementieren**

`api/v1alpha1/server_types.go`:

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ServerSpec describes one running Minecraft server instance. It is created
// and owned by a ServerGroup and is not meant to be edited by hand.
type ServerSpec struct {
	// GroupRef names the owning ServerGroup.
	GroupRef ObjectRef `json:"groupRef"`

	// Ordinal is the stable index of a persistent server. Unset for ephemeral
	// servers, whose names carry a random suffix instead.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Ordinal *int32 `json:"ordinal,omitempty"`

	// GroupGeneration is the metadata.generation of the group at creation
	// time. A server whose value is behind the group's is stale.
	// +optional
	GroupGeneration int64 `json:"groupGeneration,omitempty"`
}

// ServerStatus is the observed state of a Server.
type ServerStatus struct {
	// Phase is the state machine position: Pending, Starting, Ready, Draining,
	// Terminating or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// PodName is the pod backing this server. Set once the pod was created and
	// never reused for a different pod.
	// +optional
	PodName string `json:"podName,omitempty"`

	// Address is the pod IP and port the proxies connect to.
	// +optional
	Address string `json:"address,omitempty"`

	// Players is the last reported player count, throttled to protect etcd.
	// +optional
	Players int32 `json:"players"`

	// Slots is the player capacity reported by the agent.
	// +optional
	Slots int32 `json:"slots"`

	// PlayersUpdatedAt is when Players was last reported by the agent. Counts
	// older than twice the report interval are treated as occupied.
	// +optional
	PlayersUpdatedAt *metav1.Time `json:"playersUpdatedAt,omitempty"`

	// Registered reports whether the proxies currently know this server.
	// +optional
	Registered bool `json:"registered"`

	// WasRegistered is true once this server has been registered with the
	// proxies during the life of its current pod. A server that fell out of
	// Ready is back in Starting but still has its players connected —
	// deregistering stopped new joins, it did not move anyone — so the phase
	// alone cannot tell us whether players are at risk.
	// +optional
	WasRegistered bool `json:"wasRegistered"`

	// StartedAt is when this server last began trying to become playable: the
	// pod creation, and then every entry into phase Starting. It drives the
	// startup deadline, which therefore bounds the current attempt rather than
	// the age of the pod — a long-lived server that loses readiness gets a full
	// deadline to recover in, and is failed if it does not. Do not change this
	// back to pod-creation time.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// ReadySince is when the server last entered phase Ready. Drives the reset
	// of the readiness-loss counter.
	// +optional
	ReadySince *metav1.Time `json:"readySince,omitempty"`

	// DrainStartedAt is when the server entered phase Draining. Drives the
	// drain deadline.
	// +optional
	DrainStartedAt *metav1.Time `json:"drainStartedAt,omitempty"`

	// FailedAt is when the server entered phase Failed. Drives the retention.
	// +optional
	FailedAt *metav1.Time `json:"failedAt,omitempty"`

	// ReadinessLosses counts how often this server fell out of Ready. Past the
	// threshold the server is considered broken rather than flapping.
	// +optional
	ReadinessLosses int32 `json:"readinessLosses"`

	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcsrv
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.groupRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.players`
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=`.status.slots`
// +kubebuilder:printcolumn:name="Registered",type=boolean,JSONPath=`.status.registered`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Server is a single running Minecraft server instance.
type Server struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerSpec   `json:"spec,omitempty"`
	Status ServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServerList contains a list of Server.
type ServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Server `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Server{}, &ServerList{})
}
```

- [ ] **Step 5: ProxyGroup-Typ implementieren**

Der Controller dazu kommt in Meilenstein 3; das CRD entsteht hier, weil die API-Gruppe vollständig sein muss, bevor Nutzer sie installieren.

`api/v1alpha1/proxygroup_types.go`:

```go
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ExposeType selects how the proxies are reachable from outside the cluster.
// +kubebuilder:validation:Enum=LoadBalancer;NodePort;HostPort
type ExposeType string

const (
	// ExposeLoadBalancer needs MetalLB or kube-vip on bare metal; RKE2 ships no
	// active LoadBalancer controller.
	ExposeLoadBalancer ExposeType = "LoadBalancer"
	// ExposeNodePort uses the API server's service-node-port-range.
	ExposeNodePort ExposeType = "NodePort"
	// ExposeHostPort binds a fixed port on the nodes. CNI dependent, and
	// forbidden by Pod Security restricted.
	ExposeHostPort ExposeType = "HostPort"
)

// LoadBalancerSpec configures the LoadBalancer strategy.
type LoadBalancerSpec struct {
	// Annotations are copied onto the Service, e.g. for MetalLB pool selection.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// ExternalTrafficPolicy defaults to Local so the client IP survives — bans
	// and rate limits depend on it.
	// +kubebuilder:default=Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
}

// NodePortSpec configures the NodePort strategy.
type NodePortSpec struct {
	// Port must lie inside the API server's service-node-port-range.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// HostPortSpec configures the HostPort strategy.
type HostPortSpec struct {
	// Port is bound on every node running a proxy pod. The kube-scheduler
	// keeps at most one such pod per node, so replicas are capped by nodes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// ExposeSpec selects exactly one strategy and its matching sub-block.
// +kubebuilder:validation:XValidation:rule="self.type != 'NodePort' || has(self.nodePort)",message="expose.nodePort is required for type NodePort"
// +kubebuilder:validation:XValidation:rule="self.type != 'HostPort' || has(self.hostPort)",message="expose.hostPort is required for type HostPort"
// +kubebuilder:validation:XValidation:rule="self.type == 'LoadBalancer' || !has(self.loadBalancer)",message="expose.loadBalancer is only allowed for type LoadBalancer"
// +kubebuilder:validation:XValidation:rule="self.type == 'NodePort' || !has(self.nodePort)",message="expose.nodePort is only allowed for type NodePort"
// +kubebuilder:validation:XValidation:rule="self.type == 'HostPort' || !has(self.hostPort)",message="expose.hostPort is only allowed for type HostPort"
type ExposeSpec struct {
	// Type selects the strategy.
	Type ExposeType `json:"type"`

	// LoadBalancer configures type LoadBalancer.
	// +optional
	LoadBalancer *LoadBalancerSpec `json:"loadBalancer,omitempty"`

	// NodePort configures type NodePort.
	// +optional
	NodePort *NodePortSpec `json:"nodePort,omitempty"`

	// HostPort configures type HostPort.
	// +optional
	HostPort *HostPortSpec `json:"hostPort,omitempty"`
}

// RoutingSpec configures where players land.
type RoutingSpec struct {
	// FallbackGroups is the ordered try-list on join and on drain.
	// +kubebuilder:validation:MinItems=1
	FallbackGroups []string `json:"fallbackGroups"`
}

// ProxyConfigSpec are the Velocity settings the operator renders.
type ProxyConfigSpec struct {
	// PlayerLimit is the network-wide player limit of one proxy.
	// +kubebuilder:validation:Minimum=1
	// +optional
	PlayerLimit int32 `json:"playerLimit,omitempty"`

	// Motd is shown in the server list.
	// +optional
	Motd string `json:"motd,omitempty"`
}

// ProxyGroupSpec describes the Velocity layer of a network.
type ProxyGroupSpec struct {
	// NetworkRef names the Network this group belongs to.
	NetworkRef ObjectRef `json:"networkRef"`

	// Replicas is the number of proxy pods.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// Image is the Velocity base image. A digest reference is recommended.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Resources overrides Network.spec.defaults.resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling overrides Network.spec.defaults.scheduling.
	// +optional
	Scheduling *Scheduling `json:"scheduling,omitempty"`

	// Expose makes the proxies reachable from outside the cluster.
	Expose ExposeSpec `json:"expose"`

	// Routing configures the fallback groups.
	Routing RoutingSpec `json:"routing"`

	// Drain bounds how long existing sessions may run out on proxy replacement.
	// +kubebuilder:default={timeoutSeconds:300}
	// +optional
	Drain *DrainSpec `json:"drain,omitempty"`

	// Config are the rendered Velocity settings.
	// +optional
	Config *ProxyConfigSpec `json:"config,omitempty"`
}

// ProxyGroupStatus is the observed state of a ProxyGroup.
type ProxyGroupStatus struct {
	// Phase is derived from the proxy pods and conditions.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ReadyReplicas is the number of proxies that passed the ready gate.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// Address is where players connect.
	// +optional
	Address string `json:"address,omitempty"`

	// ConnectedPlayers is the sum of players across all proxies.
	// +optional
	ConnectedPlayers int32 `json:"connectedPlayers"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcproxy
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.connectedPlayers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProxyGroup is the Velocity layer of a network.
type ProxyGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxyGroupSpec   `json:"spec,omitempty"`
	Status ProxyGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProxyGroupList contains a list of ProxyGroup.
type ProxyGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProxyGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProxyGroup{}, &ProxyGroupList{})
}
```

- [ ] **Step 6: Manifeste erzeugen und Tests laufen lassen**

```bash
nix develop -c make manifests generate
nix develop -c go test ./api/... -v
```

Expected: PASS, vier CRD-Dateien unter `config/crd/bases`.

- [ ] **Step 7: Commit**

```bash
git add api config
git commit -m "Server- und ProxyGroup-CRD"
```

---

### Task 5: Zustandsmaschine als reine Funktion

**Files:**
- Create: `internal/phase/phase.go`
- Test: `internal/phase/phase_test.go`

**Interfaces:**
- Consumes: nichts (bewusst Kubernetes-frei).
- Produces:
  - `phase.Phase` mit `Pending`, `Starting`, `Ready`, `Draining`, `Terminating`, `Failed`.
  - `phase.StreamDownGrace = 15 * time.Second`, `phase.MaxReadinessLosses int32 = 3`, `phase.FlapResetWindow = 10 * time.Minute`.
  - `phase.Inputs` (Feldliste unten) und `phase.Decision{Next Phase, Register, Deregister, StartDrain, CountReadinessLoss, ResetReadinessLosses, DeletePod bool, Reason, Message string}`.
  - `phase.Decide(current Phase, in Inputs) Decision`.
  - Reason-Konstanten: `ReasonPodPending`, `ReasonPodRunning`, `ReasonReadyGatePassed`, `ReasonReadinessLost`, `ReasonDeletionRequested`, `ReasonDrained`, `ReasonDrainTimeout`, `ReasonPodLost`, `ReasonPodTerminal`, `ReasonDrainingBeforeCleanup`, `ReasonStartupTimeout`, `ReasonFlapping`, `ReasonRetentionElapsed`, `ReasonTerminating`, `ReasonUnknownPhase`.

Das ist das Herz von Meilenstein 1. Alles, was über Registrierung und Löschung entscheidet, steht hier und nirgends sonst; die Controller führen nur aus.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/phase/phase_test.go`:

```go
package phase

import (
	"testing"
	"time"
)

// healthyReady is the input set of a server that is fine in phase Ready.
func healthyReady() Inputs {
	return Inputs{
		PodExists:      true,
		PodRunning:     true,
		PodReady:       true,
		AgentReady:     true,
		AgentConnected: true,
		Slots:          100,
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		current Phase
		in      Inputs
		want    Decision
	}{
		{
			name:    "pending stays pending without a pod",
			current: Pending,
			in:      Inputs{},
			want:    Decision{Next: Pending, Reason: ReasonPodPending},
		},
		{
			name:    "pending advances once the pod runs",
			current: Pending,
			in:      Inputs{PodExists: true, PodRunning: true},
			want:    Decision{Next: Starting, Reason: ReasonPodRunning},
		},
		{
			name:    "starting waits for the agent when only the probe is green",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, PodReady: true},
			want:    Decision{Next: Starting, Reason: ReasonPodPending},
		},
		{
			name:    "starting waits for the probe when only the agent is ready",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, AgentReady: true},
			want:    Decision{Next: Starting, Reason: ReasonPodPending},
		},
		{
			name:    "starting becomes ready on both signals and registers",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true},
			want:    Decision{Next: Ready, Register: true, Reason: ReasonReadyGatePassed},
		},
		{
			// Regression for Minor 2: the ready gate must not trust a green
			// probe and agent alone. Task 8 should never send PodReady/AgentReady
			// without PodExists/PodRunning, but this package must not depend on
			// caller discipline.
			name:    "starting does not skip to ready on contradictory inputs without the pod existing and running",
			current: Starting,
			in:      Inputs{PodExists: false, PodRunning: false, PodReady: true, AgentReady: true},
			want:    Decision{Next: Starting, Reason: ReasonPodPending},
		},
		{
			name:    "ready stays ready while healthy",
			current: Ready,
			in:      healthyReady(),
			want:    Decision{Next: Ready, Reason: ReasonReadyGatePassed},
		},
		{
			name:    "ready falls back to starting when the probe turns red",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.PodReady = false
				return in
			}(),
			want: Decision{Next: Starting, Deregister: true, CountReadinessLoss: true, Reason: ReasonReadinessLost},
		},
		{
			// A live stream that reports not-ready is the agent telling us
			// something, not us failing to hear it: no grace period applies.
			name:    "ready falls back to starting at once when a live agent reports not ready",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentReady = false
				return in
			}(),
			want: Decision{Next: Starting, Deregister: true, CountReadinessLoss: true, Reason: ReasonReadinessLost},
		},
		{
			name:    "ready falls back to starting when the agent stream is down too long",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentConnected = false
				in.AgentStreamDownFor = StreamDownGrace
				return in
			}(),
			want: Decision{Next: Starting, Deregister: true, CountReadinessLoss: true, Reason: ReasonReadinessLost},
		},
		{
			name:    "ready tolerates a short stream gap",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentConnected = false
				in.AgentStreamDownFor = StreamDownGrace - time.Millisecond
				return in
			}(),
			want: Decision{Next: Ready, Reason: ReasonReadyGatePassed},
		},
		{
			// The exact shape the agent registry emits after Disconnect: it
			// clears ready and starts the clock, so a Ready server inside the
			// grace window arrives here as neither ready nor connected. This is
			// the composition that made the StreamDownGrace clause unreachable
			// before — inside the grace only the timer may decide.
			name:    "ready tolerates a dropped stream whose agent has not reported ready since",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.AgentReady = false
				in.AgentConnected = false
				in.AgentStreamDownFor = StreamDownGrace - time.Millisecond
				return in
			}(),
			want: Decision{Next: Ready, Reason: ReasonReadyGatePassed},
		},
		{
			name:    "ready resets the flap counter after a long healthy stretch",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.ReadinessLosses = 2
				in.ReadyFor = FlapResetWindow
				return in
			}(),
			want: Decision{Next: Ready, ResetReadinessLosses: true, Reason: ReasonReadyGatePassed},
		},
		{
			name:    "flapping past the threshold fails and deregisters",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.ReadinessLosses = MaxReadinessLosses
				return in
			}(),
			want: Decision{Next: Failed, Deregister: true, Reason: ReasonFlapping},
		},
		{
			name:    "a terminal pod fails the server",
			current: Starting,
			in:      Inputs{PodExists: true, PodTerminal: true},
			want:    Decision{Next: Failed, Reason: ReasonPodTerminal},
		},
		{
			name:    "a server that never becomes ready fails on the startup deadline",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, StartupDeadlineReached: true},
			want:    Decision{Next: Failed, Reason: ReasonStartupTimeout},
		},
		{
			// A server that fell out of Ready and cannot come back must still be
			// failed, and must take its players with it. The flap counter can
			// never catch this on its own: losses are only counted on a
			// Ready -> Starting transition, which a permanently red probe never
			// produces again. The controller re-arms status.startedAt on entry
			// into Starting, so the deadline here is one full recovery window
			// after the fall-back, not the age of the pod.
			name:    "a server that cannot recover is failed and drained a deadline after falling back",
			current: Starting,
			in: Inputs{
				PodExists: true, PodRunning: true, StartupDeadlineReached: true,
				WasRegistered: true, PlayersOnline: 9, ReadinessLosses: 1,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonStartupTimeout},
		},
		{
			// Flapping is the bound for a server that was once playable, and it
			// must take its players with it: the readiness-loss fallback only
			// deregistered, it never moved anyone off.
			name:    "failing on flapping drains a server that still has players",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.ReadinessLosses = MaxReadinessLosses
				in.WasRegistered = true
				in.PlayersOnline = 12
				return in
			}(),
			want: Decision{Next: Failed, Deregister: true, StartDrain: true, Reason: ReasonFlapping},
		},
		{
			// A terminal pod means the process is already down: there is nobody
			// left to move, so draining would be pointless.
			name:    "failing on a terminal pod does not try to drain",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.PodTerminal = true
				in.WasRegistered = true
				in.PlayersOnline = 12
				return in
			}(),
			want: Decision{Next: Failed, Deregister: true, Reason: ReasonPodTerminal},
		},
		{
			name:    "a failed server with players is drained instead of cleaned up at the retention",
			current: Failed,
			in: Inputs{
				FailedRetentionElapsed: true, WasRegistered: true, PlayersOnline: 5,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonDrainingBeforeCleanup},
		},
		{
			name:    "a failed server with a stale count is drained, not cleaned up",
			current: Failed,
			in: Inputs{
				FailedRetentionElapsed: true, WasRegistered: true, PlayersStale: true,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonDrainingBeforeCleanup},
		},
		{
			// The escape hatch: one stuck player must not pin a failed server
			// forever.
			name:    "a failed server is cleaned up once its drain deadline passes",
			current: Failed,
			in: Inputs{
				FailedRetentionElapsed: true, WasRegistered: true, PlayersOnline: 5,
				DrainDeadlineReached: true,
			},
			want: Decision{Next: Terminating, DeletePod: true, Reason: ReasonRetentionElapsed},
		},
		{
			name:    "deleting an occupied failed server drains it",
			current: Failed,
			in: Inputs{
				DeletionRequested: true, WasRegistered: true, PlayersOnline: 5,
			},
			want: Decision{Next: Failed, StartDrain: true, Reason: ReasonDrainingBeforeCleanup},
		},
		{
			name:    "deleting an empty failed server terminates it",
			current: Failed,
			in:      Inputs{DeletionRequested: true, WasRegistered: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDeletionRequested},
		},
		{
			// Never registered means no session was ever routed here, so there
			// is nothing to move off.
			name:    "a failed server that was never registered is cleaned up directly",
			current: Failed,
			in:      Inputs{FailedRetentionElapsed: true, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonRetentionElapsed},
		},
		{
			name:    "deleting a ready server drains it",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.DeletionRequested = true
				in.PlayersOnline = 4
				return in
			}(),
			want: Decision{Next: Draining, Deregister: true, StartDrain: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "deleting a starting server terminates it right away",
			current: Starting,
			in:      Inputs{PodExists: true, PodRunning: true, DeletionRequested: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDeletionRequested},
		},
		{
			// Regression for the Critical finding: a Starting server that fell
			// out of Ready (WasRegistered) still has its players connected — the
			// readiness-loss fallback only deregistered to stop new joins, it did
			// not move anyone off. Deleting such a server must drain it, not
			// terminate it out from under 20 connected players. Reproduction as
			// confirmed by the reviewer, one tick after the Ready server lost its
			// probe and fell back to Starting.
			name:    "deleting a starting server that was registered before drains it instead of dropping its players",
			current: Starting,
			in: Inputs{
				PodExists: true, PodRunning: true, PodReady: false, AgentReady: true,
				PlayersOnline: 20, DeletionRequested: true, ReadinessLosses: 1,
				WasRegistered: true,
			},
			want: Decision{Next: Draining, StartDrain: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "deleting a pending server terminates it right away",
			current: Pending,
			in:      Inputs{DeletionRequested: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDeletionRequested},
		},
		{
			name:    "draining terminates once the server is empty",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 0},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrained},
		},
		{
			name:    "draining keeps waiting while players are online",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 1},
			want:    Decision{Next: Draining, Reason: ReasonDeletionRequested},
		},
		{
			name:    "draining keeps waiting when the count is stale even at zero",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 0, PlayersStale: true},
			want:    Decision{Next: Draining, Reason: ReasonDeletionRequested},
		},
		{
			name:    "draining gives up at the deadline",
			current: Draining,
			in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 3, DrainDeadlineReached: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrainTimeout},
		},
		{
			// Regression for Minor 1: a crashed pod's players are already gone
			// no matter what a stale report still claims, so draining must not
			// burn the full drain timeout waiting for players who cannot leave
			// a pod that no longer runs.
			name:    "draining terminates right away when the pod goes terminal, even with players reported online",
			current: Draining,
			in:      Inputs{PodTerminal: true, PlayersOnline: 3, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodTerminal},
		},
		{
			name:    "a lost pod terminates a ready server and deregisters it",
			current: Ready,
			in: func() Inputs {
				in := healthyReady()
				in.PodExists = false
				in.PodRunning = false
				in.PodReady = false
				in.PodLost = true
				return in
			}(),
			want: Decision{Next: Terminating, Deregister: true, DeletePod: true, Reason: ReasonPodLost},
		},
		{
			name:    "a lost pod ends a drain",
			current: Draining,
			in:      Inputs{PodLost: true, PlayersOnline: 3, PlayersStale: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodLost},
		},
		{
			name:    "failed is kept for diagnosis",
			current: Failed,
			in:      Inputs{},
			want:    Decision{Next: Failed, Reason: ReasonPodTerminal},
		},
		{
			name:    "failed is cleaned up after the retention",
			current: Failed,
			in:      Inputs{FailedRetentionElapsed: true},
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonRetentionElapsed},
		},
		{
			name:    "terminating is absorbing",
			current: Terminating,
			in:      healthyReady(),
			want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonTerminating},
		},
		{
			name:    "an unknown phase restarts at pending",
			current: Phase("Bogus"),
			in:      Inputs{},
			want:    Decision{Next: Pending, Reason: ReasonUnknownPhase},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.current, tc.in)
			got.Message = ""
			if got != tc.want {
				t.Errorf("Decide(%q, %+v)\n got  %+v\n want %+v", tc.current, tc.in, got, tc.want)
			}
		})
	}
}

// TestStreamDownGraceIsFifteenSeconds pins the value, not just the symbol.
// Every other test is written relative to the constant, so shrinking it to a
// second would break nothing and silently drop the tolerance design spec 4.4
// requires.
func TestStreamDownGraceIsFifteenSeconds(t *testing.T) {
	if StreamDownGrace != 15*time.Second {
		t.Errorf("StreamDownGrace = %v, want 15s (design spec 4.4)", StreamDownGrace)
	}
}

// TestOccupiedFailedServerIsNeverDeletedBeforeItsDrainDeadline is the Failed
// counterpart of TestOccupiedServerIsNeverDeletedWithoutDeadline: a failed
// server can still hold live sessions, so neither the retention nor a deletion
// request may remove its pod while players are on it.
func TestOccupiedFailedServerIsNeverDeletedBeforeItsDrainDeadline(t *testing.T) {
	for _, stale := range []bool{false, true} {
		for _, deleting := range []bool{false, true} {
			for _, retention := range []bool{false, true} {
				in := Inputs{
					WasRegistered:          true,
					PlayersOnline:          7,
					PlayersStale:           stale,
					DeletionRequested:      deleting,
					FailedRetentionElapsed: retention,
				}
				if got := Decide(Failed, in); got.DeletePod {
					t.Errorf("Decide(Failed, stale=%v deleting=%v retention=%v) deleted an occupied pod: %+v",
						stale, deleting, retention, got)
				}
			}
		}
	}
}

// TestNoPathBackFromDraining guards the rule that a draining server never
// serves players again, no matter how healthy it looks.
func TestNoPathBackFromDraining(t *testing.T) {
	got := Decide(Draining, healthyReady())
	if got.Next == Ready || got.Register {
		t.Fatalf("draining went back to Ready: %+v", got)
	}
}

// TestNoPathBackFromFailed guards the same rule for Failed.
func TestNoPathBackFromFailed(t *testing.T) {
	got := Decide(Failed, healthyReady())
	if got.Next == Ready || got.Register {
		t.Fatalf("failed went back to Ready: %+v", got)
	}
}

// TestOccupiedServerIsNeverDeletedWithoutDeadline is the core invariant: as
// long as players are online and no deadline has passed, no decision may
// delete the pod.
//
// Ready and Draining are always checked: a Ready server is registered with
// the proxies, and a Draining server was registered until it started
// draining. Starting is checked only with WasRegistered: true — a Starting
// server that fell out of Ready still has players connected from before it
// lost readiness, even though it deregistered to stop new joins. A
// never-registered Pending or Starting server is excluded: it was never
// registered with the proxies, so it cannot hold players — and treating its
// stale, never-reported count as "occupied" would make deletion of a server
// that never started hang until the drain deadline.
func TestOccupiedServerIsNeverDeletedWithoutDeadline(t *testing.T) {
	cases := []struct {
		phase         Phase
		wasRegistered bool
	}{
		{Ready, false},
		{Draining, false},
		{Starting, true},
	}
	for _, tc := range cases {
		for _, stale := range []bool{false, true} {
			for _, deleting := range []bool{false, true} {
				in := healthyReady()
				in.PlayersOnline = 7
				in.PlayersStale = stale
				in.DeletionRequested = deleting
				in.WasRegistered = tc.wasRegistered
				got := Decide(tc.phase, in)
				if got.DeletePod {
					t.Errorf("Decide(%q, players=7 stale=%v deleting=%v wasRegistered=%v) deleted the pod: %+v",
						tc.phase, stale, deleting, tc.wasRegistered, got)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/phase/... -v`
Expected: FAIL — `undefined: Decide`, `undefined: Inputs`.

- [ ] **Step 3: Implementieren**

`internal/phase/phase.go`:

```go
// Package phase implements the Server state machine as a pure function.
// It knows nothing about Kubernetes: the controller collects the inputs,
// calls Decide and executes the returned decision. Every rule about
// registration and deletion lives here and nowhere else.
package phase

import "time"

// Phase is the position of a Server in its lifecycle.
type Phase string

const (
	// Pending means the CR exists but the pod is not running yet.
	Pending Phase = "Pending"
	// Starting means the pod runs but at least one ready signal is missing.
	Starting Phase = "Starting"
	// Ready means the server is registered with the proxies and takes players.
	Ready Phase = "Ready"
	// Draining means the server is deregistered and players are being moved.
	// There is no way back to Ready.
	Draining Phase = "Draining"
	// Terminating means the pod is being deleted.
	Terminating Phase = "Terminating"
	// Failed means the server is broken. It is kept for diagnosis and cleaned
	// up after the group's retention.
	Failed Phase = "Failed"
)

const (
	// StreamDownGrace is how long the agent stream of a Ready server may be
	// down before the server counts as unplayable (design spec 4.4).
	StreamDownGrace = 15 * time.Second

	// FlapResetWindow is how long a server must stay Ready before its
	// readiness-loss counter is forgiven.
	FlapResetWindow = 10 * time.Minute
)

// MaxReadinessLosses is the number of readiness losses after which a server is
// considered broken rather than flapping.
const MaxReadinessLosses int32 = 3

// Reasons carried in the decision and mirrored into the CR condition.
const (
	ReasonPodPending        = "PodPending"
	ReasonPodRunning        = "PodRunning"
	ReasonReadyGatePassed   = "ReadyGatePassed"
	ReasonReadinessLost     = "ReadinessLost"
	ReasonDeletionRequested = "DeletionRequested"
	ReasonDrained           = "Drained"
	ReasonDrainTimeout      = "DrainTimeout"
	ReasonPodLost           = "PodLost"
	ReasonPodTerminal       = "PodTerminal"
	// ReasonDrainingBeforeCleanup marks a Failed server whose players are being
	// moved off before its pod is removed.
	ReasonDrainingBeforeCleanup = "DrainingBeforeCleanup"
	ReasonStartupTimeout        = "StartupTimeout"
	ReasonFlapping              = "Flapping"
	ReasonRetentionElapsed      = "RetentionElapsed"
	ReasonTerminating           = "Terminating"
	ReasonUnknownPhase          = "UnknownPhase"
)

// Inputs is everything the state machine may look at. The controller fills it
// from the CR status, the pod and the agent registry.
type Inputs struct {
	// DeletionRequested is true once the Server CR carries a deletion
	// timestamp, or the group decided to remove this server.
	DeletionRequested bool

	// PodExists is true if the pod backing this server was found.
	PodExists bool
	// PodLost is true if status.podName is set but the pod is gone. The players
	// of that pod are gone with it.
	PodLost bool
	// PodRunning is true if the pod reached phase Running.
	PodRunning bool
	// PodReady is true if the readiness probe (the SLP health check) is green.
	PodReady bool
	// PodTerminal is true if the pod reached phase Failed or Succeeded, or a
	// container is in CrashLoopBackOff past the operator's tolerance.
	PodTerminal bool

	// StartupDeadlineReached is true if the current attempt to become playable
	// has run past the operator's startup deadline. The clock is re-armed on
	// every entry into Starting, so this bounds the attempt and not the age of
	// the pod: a long-lived server that loses readiness gets a full deadline to
	// recover in, and is failed if it does not.
	StartupDeadlineReached bool

	// AgentReady is true if the in-game agent reported readiness on a live
	// stream.
	AgentReady bool
	// AgentConnected is true while the agent stream is up. It separates "the
	// agent is telling us it is not ready" from "we cannot hear the agent" —
	// the first is immediate, the second is tolerated for StreamDownGrace.
	AgentConnected bool
	// AgentStreamDownFor is how long the agent stream has been broken. Zero
	// while the stream is up.
	AgentStreamDownFor time.Duration

	// ReadinessLosses is how often this server already fell out of Ready.
	ReadinessLosses int32
	// ReadyFor is how long the server has been continuously Ready.
	ReadyFor time.Duration

	// WasRegistered is true if this server was ever registered with the proxies
	// during the life of its current pod. A Starting server that fell out of
	// Ready still has its players connected — deregistering stopped new joins,
	// it did not move anyone — so the phase alone cannot tell us whether players
	// are at risk.
	WasRegistered bool

	// PlayersOnline is the last reported player count.
	PlayersOnline int32
	// PlayersStale is true if that count is older than twice the report
	// interval. A stale count counts as occupied.
	PlayersStale bool
	// Slots is the reported capacity. Informational for the decision.
	Slots int32

	// DrainDeadlineReached is true once drain.timeoutSeconds elapsed.
	DrainDeadlineReached bool
	// FailedRetentionElapsed is true once failedRetentionSeconds elapsed.
	FailedRetentionElapsed bool
}

// Occupied reports whether the server must be treated as carrying players.
// A stale count counts as occupied: one server too many beats one kick.
func (in Inputs) Occupied() bool {
	return in.PlayersStale || in.PlayersOnline > 0
}

// Decision is what the controller has to do. Next is always set.
type Decision struct {
	// Next is the phase to write into the status.
	Next Phase
	// Register asks the proxies to take this server into their registry.
	Register bool
	// Deregister asks the proxies to drop it. Set on every exit from Ready.
	Deregister bool
	// StartDrain asks the proxies to move the players off this server.
	StartDrain bool
	// CountReadinessLoss increments status.readinessLosses.
	CountReadinessLoss bool
	// ResetReadinessLosses zeroes status.readinessLosses.
	ResetReadinessLosses bool
	// DeletePod means the pod may go: no players are at risk.
	DeletePod bool
	// Reason is the machine-readable cause, mirrored into the condition.
	Reason string
	// Message is the human-readable cause.
	Message string
}

// Decide maps the current phase and the observed inputs to the next phase.
func Decide(current Phase, in Inputs) Decision {
	switch current {
	case Terminating:
		return Decision{
			Next: Terminating, DeletePod: true,
			Reason: ReasonTerminating, Message: "pod is being deleted",
		}

	case Failed:
		if in.DeletionRequested || in.FailedRetentionElapsed {
			// A server can fail with its sessions untouched: flapping
			// readiness deregisters to stop new joins, it does not move
			// anyone off. Cleaning such a server up without draining first
			// would drop every player still on it.
			if in.Occupied() && in.WasRegistered && !in.PodLost && !in.PodTerminal &&
				!in.DrainDeadlineReached {
				return Decision{
					Next: Failed, StartDrain: true,
					Reason:  ReasonDrainingBeforeCleanup,
					Message: "moving players off a failed server before removing it",
				}
			}
			if in.DeletionRequested {
				return Decision{
					Next: Terminating, DeletePod: true,
					Reason: ReasonDeletionRequested, Message: "deletion requested for a failed server",
				}
			}
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonRetentionElapsed, Message: "failed retention elapsed",
			}
		}
		return Decision{
			Next:   Failed,
			Reason: ReasonPodTerminal, Message: "kept for diagnosis",
		}

	case Draining:
		if in.PodLost {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonPodLost, Message: "pod disappeared during drain",
			}
		}
		if in.PodTerminal {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonPodTerminal, Message: "pod reached a terminal phase during drain, its players are already gone",
			}
		}
		if !in.Occupied() {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonDrained, Message: "no players left",
			}
		}
		if in.DrainDeadlineReached {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonDrainTimeout, Message: "drain deadline reached with players online",
			}
		}
		return Decision{
			Next:   Draining,
			Reason: ReasonDeletionRequested, Message: "waiting for players to leave",
		}

	case Pending, Starting, Ready:
		// handled below
	default:
		return Decision{
			Next:   Pending,
			Reason: ReasonUnknownPhase, Message: "unknown phase, restarting the state machine",
		}
	}

	// From here on current is Pending, Starting or Ready.

	if in.PodLost {
		return Decision{
			Next: Terminating, Deregister: current == Ready, DeletePod: true,
			Reason: ReasonPodLost, Message: "pod disappeared",
		}
	}

	if in.DeletionRequested {
		// A Ready server is currently registered with the proxies. A Starting
		// server that fell out of Ready (WasRegistered) may still have players
		// connected from before: the readiness-loss fallback deregisters to stop
		// new joins, it does not move anyone off. Both cases must drain. Only a
		// server that was never registered can go straight away.
		if current == Ready || in.WasRegistered {
			return Decision{
				Next: Draining, Deregister: current == Ready, StartDrain: true,
				Reason: ReasonDeletionRequested, Message: "deletion requested, moving players off",
			}
		}
		return Decision{
			Next: Terminating, DeletePod: true,
			Reason: ReasonDeletionRequested, Message: "deletion requested before the server was ever registered",
		}
	}

	if in.PodTerminal {
		// A terminal pod is never drained: the process is already down and its
		// sessions went with it, so there is nobody left to move off.
		return Decision{
			Next: Failed, Deregister: current == Ready,
			Reason: ReasonPodTerminal, Message: "pod reached a terminal phase",
		}
	}

	// From here on the pod is neither lost nor terminal — both returned above.
	// A server that failed while it was registered still has live sessions on
	// it, so failing it has to take its players off rather than strand them.
	drainOnFailure := in.WasRegistered

	if in.ReadinessLosses >= MaxReadinessLosses {
		return Decision{
			Next: Failed, Deregister: current == Ready, StartDrain: drainOnFailure,
			Reason: ReasonFlapping, Message: "too many readiness losses",
		}
	}

	// The startup deadline bounds the current attempt to become playable. The
	// controller re-arms status.startedAt on every entry into Starting, so this
	// measures the attempt and not the age of the pod: a server that has served
	// for hours and blips once gets a fresh deadline to recover in, while one
	// that fell out of Ready and cannot come back is still failed — the flap
	// counter alone would never catch it, because losses are only counted on a
	// Ready -> Starting transition that a permanently red probe never repeats.
	if in.StartupDeadlineReached && current != Ready {
		return Decision{
			Next: Failed, StartDrain: drainOnFailure,
			Reason: ReasonStartupTimeout, Message: "server did not become ready in time",
		}
	}

	switch current {
	case Pending:
		if in.PodExists && in.PodRunning {
			return Decision{Next: Starting, Reason: ReasonPodRunning, Message: "pod is running"}
		}
		return Decision{Next: Pending, Reason: ReasonPodPending, Message: "waiting for the pod"}

	case Starting:
		if in.PodExists && in.PodRunning && in.PodReady && in.AgentReady && in.AgentStreamDownFor < StreamDownGrace {
			return Decision{
				Next: Ready, Register: true,
				Reason: ReasonReadyGatePassed, Message: "probe green and agent ready",
			}
		}
		return Decision{
			Next:   Starting,
			Reason: ReasonPodPending, Message: "waiting for both ready signals",
		}

	default: // Ready
		lost := !in.PodReady
		if !lost {
			if in.AgentConnected {
				// A live stream that reports not-ready is an immediate loss.
				lost = !in.AgentReady
			} else {
				// A broken stream is tolerated until the grace expires; the
				// player count goes stale meanwhile, so the server counts as
				// occupied and is protected from deletion either way.
				lost = in.AgentStreamDownFor >= StreamDownGrace
			}
		}
		if lost {
			return Decision{
				Next: Starting, Deregister: true, CountReadinessLoss: true,
				Reason: ReasonReadinessLost, Message: "server lost a ready signal",
			}
		}
		return Decision{
			Next:                 Ready,
			ResetReadinessLosses: in.ReadinessLosses > 0 && in.ReadyFor >= FlapResetWindow,
			Reason:               ReasonReadyGatePassed, Message: "serving players",
		}
	}
}
```

- [ ] **Step 4: Test laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/phase/... -v`
Expected: PASS, alle Unterfälle von `TestDecide` sowie die vier Invarianten-Tests.

- [ ] **Step 5: Commit**

```bash
git add internal/phase
git commit -m "Zustandsmaschine des Servers als reine Funktion"
```

---

### Task 6: Agent-Registry als Laufzeitzustand

**Files:**
- Create: `internal/agent/registry.go`
- Test: `internal/agent/registry_test.go`

**Interfaces:**
- Consumes: nichts.
- Produces:
  - `agent.Role` mit `RoleServer` und `RoleProxy`.
  - `agent.Snapshot{Known, Connected, Ready bool, Players, Slots int32, PlayersStale bool, StreamDownFor time.Duration}`.
  - `agent.Registry` mit `New(clock func() time.Time, reportInterval time.Duration, startedAt time.Time) *Registry`, `Connect(key string, role Role)`, `MarkReady(key string)`, `ReportPlayers(key string, players, slots int32) error`, `Disconnect(key string)`, `Forget(key string)`, `Lookup(key string) Snapshot`.

Der Schlüssel ist die Pod-UID als String — sie kommt in Meilenstein 2 aus den Extra-Claims des pod-gebundenen Tokens und niemals aus einer Nachricht des Agents. In Meilenstein 1 schreiben nur Tests in die Registry; das ist genau die Naht, an die Meilenstein 2 den gRPC-Dienst hängt.

`StreamDownFor` eines unbekannten Pods ist die Zeit seit `startedAt` (Operator-Start). Damit bekommen nach einem Operator-Neustart alle Agents die 15 Sekunden Kulanz aus `phase.StreamDownGrace`, bevor ihre Server deregistriert werden — das ist das in Spec 7 beschriebene Verhalten.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/agent/registry_test.go`:

```go
package agent

import (
	"testing"
	"time"
)

// fakeClock is a hand-cranked clock so the time rules are testable.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time      { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestRegistry() (*Registry, *fakeClock) {
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start}
	return New(clock.Now, 5*time.Second, start), clock
}

func TestLookupUnknownPod(t *testing.T) {
	r, clock := newTestRegistry()
	clock.Advance(3 * time.Second)

	got := r.Lookup("pod-uid-1")
	if got.Known {
		t.Error("unknown pod reported as known")
	}
	if got.Ready {
		t.Error("unknown pod reported as ready")
	}
	if !got.PlayersStale {
		t.Error("unknown pod must count as stale, i.e. occupied")
	}
	if got.StreamDownFor != 3*time.Second {
		t.Errorf("StreamDownFor = %v, want 3s since operator start", got.StreamDownFor)
	}
}

func TestConnectDoesNotImplyReady(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)

	got := r.Lookup("pod-uid-1")
	if !got.Known || !got.Connected {
		t.Fatalf("after Connect: %+v", got)
	}
	if got.Ready {
		t.Error("Connect must not mark the agent ready")
	}
	if got.StreamDownFor != 0 {
		t.Errorf("StreamDownFor = %v on a live stream, want 0", got.StreamDownFor)
	}
}

func TestMarkReadyAndReport(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 12, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	got := r.Lookup("pod-uid-1")
	if !got.Ready || got.Players != 12 || got.Slots != 100 || got.PlayersStale {
		t.Errorf("snapshot = %+v, want ready with 12/100 and fresh", got)
	}
}

func TestPlayerCountGoesStaleAtTwiceTheInterval(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	clock.Advance(9 * time.Second)
	if r.Lookup("pod-uid-1").PlayersStale {
		t.Error("count went stale before twice the report interval")
	}

	clock.Advance(1 * time.Second)
	if !r.Lookup("pod-uid-1").PlayersStale {
		t.Error("count did not go stale at twice the report interval")
	}
}

func TestReportPlayersRejectsMoreThanSlots(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 5, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	if err := r.ReportPlayers("pod-uid-1", 101, 100); err == nil {
		t.Fatal("player count above slots accepted, want rejection")
	}
	if got := r.Lookup("pod-uid-1"); got.Players != 5 {
		t.Errorf("bogus report changed the count to %d, want the previous 5", got.Players)
	}
}

func TestReportPlayersRejectsUnknownPod(t *testing.T) {
	r, _ := newTestRegistry()
	if err := r.ReportPlayers("pod-uid-1", 1, 100); err == nil {
		t.Fatal("report for an unconnected pod accepted, want rejection")
	}
}

func TestDisconnectKeepsTheLastCountAndStartsTheClock(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	if err := r.ReportPlayers("pod-uid-1", 7, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	r.Disconnect("pod-uid-1")
	clock.Advance(4 * time.Second)

	got := r.Lookup("pod-uid-1")
	if got.Connected {
		t.Error("still connected after Disconnect")
	}
	if got.Ready {
		t.Error("a disconnected agent must not count as ready")
	}
	if got.Players != 7 {
		t.Errorf("Players = %d, want the last known 7", got.Players)
	}
	if got.StreamDownFor != 4*time.Second {
		t.Errorf("StreamDownFor = %v, want 4s", got.StreamDownFor)
	}
}

func TestReconnectRestoresReadiness(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.MarkReady("pod-uid-1")
	r.Disconnect("pod-uid-1")
	clock.Advance(30 * time.Second)

	r.Connect("pod-uid-1", RoleServer)
	if got := r.Lookup("pod-uid-1"); got.Ready {
		t.Error("reconnect alone must not restore readiness")
	}
	r.MarkReady("pod-uid-1")

	got := r.Lookup("pod-uid-1")
	if !got.Ready || got.StreamDownFor != 0 {
		t.Errorf("snapshot after reconnect = %+v, want ready with a live stream", got)
	}
}

func TestForgetRemovesThePod(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	r.Forget("pod-uid-1")
	if r.Lookup("pod-uid-1").Known {
		t.Error("pod still known after Forget")
	}
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/agent/... -v`
Expected: FAIL — `undefined: New`, `undefined: Registry`.

- [ ] **Step 3: Implementieren**

`internal/agent/registry.go`:

```go
// Package agent holds the runtime state the in-game agents report. It is the
// port the gRPC service of milestone 2 plugs into: the controllers only read
// snapshots from here and never talk to an agent directly.
//
// Player counts live in memory on purpose. At 200 servers, writing every
// report into etcd would be dozens of writes per second; the CR status is for
// observers, not for the control loop.
package agent

import (
	"fmt"
	"sync"
	"time"
)

// Role separates the two kinds of agents. A server agent may never act as a
// proxy agent; milestone 2 derives the role from the pod's ServiceAccount.
type Role string

const (
	// RoleServer is a Paper agent.
	RoleServer Role = "server"
	// RoleProxy is a Velocity agent.
	RoleProxy Role = "proxy"
)

// Snapshot is a consistent read of one agent's state.
type Snapshot struct {
	// Known is false if the registry never saw this pod.
	Known bool
	// Connected is true while the agent stream is up.
	Connected bool
	// Ready is true if the agent reported readiness on the current stream.
	Ready bool
	// Players is the last reported player count.
	Players int32
	// Slots is the last reported capacity.
	Slots int32
	// PlayersStale is true if the count is older than twice the report
	// interval, or if the pod is unknown. Stale counts as occupied.
	PlayersStale bool
	// StreamDownFor is how long the stream has been down. Zero while up. For
	// an unknown pod it is the time since the operator started, so agents get
	// a grace period to reconnect after an operator restart.
	StreamDownFor time.Duration
}

type entry struct {
	role           Role
	connected      bool
	ready          bool
	players        int32
	slots          int32
	lastReportAt   time.Time
	disconnectedAt time.Time
}

// Registry is the in-memory state of all connected agents. It is safe for
// concurrent use.
type Registry struct {
	mu             sync.RWMutex
	entries        map[string]*entry
	now            func() time.Time
	reportInterval time.Duration
	startedAt      time.Time
}

// New creates a registry. The clock is injectable so the staleness rules are
// testable; startedAt is when the operator process came up.
func New(clock func() time.Time, reportInterval time.Duration, startedAt time.Time) *Registry {
	return &Registry{
		entries:        make(map[string]*entry),
		now:            clock,
		reportInterval: reportInterval,
		startedAt:      startedAt,
	}
}

// Connect records a new agent stream. Readiness is not implied: the agent has
// to state it, either through MarkReady or through Hello{ready:true}.
func (r *Registry) Connect(key string, role Role) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok {
		e = &entry{}
		r.entries[key] = e
	}
	e.role = role
	e.connected = true
	e.ready = false
	e.disconnectedAt = time.Time{}
}

// MarkReady records that the agent reported readiness.
func (r *Registry) MarkReady(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[key]; ok && e.connected {
		e.ready = true
	}
}

// ReportPlayers records a player count. Counts above the reported capacity are
// rejected as defense in depth against a compromised agent.
func (r *Registry) ReportPlayers(key string, players, slots int32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if players < 0 || slots < 0 {
		return fmt.Errorf("negative report for %q: %d/%d", key, players, slots)
	}
	if players > slots {
		return fmt.Errorf("report for %q exceeds capacity: %d/%d", key, players, slots)
	}
	e.players = players
	e.slots = slots
	e.lastReportAt = r.now()
	return nil
}

// Disconnect records that the stream broke. The last player count is kept, so
// the server stays protected until the count goes stale.
func (r *Registry) Disconnect(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[key]; ok {
		e.connected = false
		e.ready = false
		e.disconnectedAt = r.now()
	}
}

// Forget drops a pod entirely. The controllers call it once a pod is gone for
// good, so the map does not grow without bound.
func (r *Registry) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

// Lookup returns a consistent snapshot of one agent.
func (r *Registry) Lookup(key string) Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.now()
	e, ok := r.entries[key]
	if !ok {
		return Snapshot{
			PlayersStale:  true,
			StreamDownFor: now.Sub(r.startedAt),
		}
	}

	snap := Snapshot{
		Known:     true,
		Connected: e.connected,
		Ready:     e.ready,
		Players:   e.players,
		Slots:     e.slots,
	}
	if !e.connected {
		snap.StreamDownFor = now.Sub(e.disconnectedAt)
	}
	snap.PlayersStale = e.lastReportAt.IsZero() ||
		now.Sub(e.lastReportAt) >= 2*r.reportInterval
	return snap
}
```

- [ ] **Step 4: Test laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/agent/... -v`
Expected: PASS für alle neun Tests.

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "Agent-Registry als Laufzeitzustand der In-Game-Agents"
```

---

### Task 7: Pod-Builder für Server-Pods

**Files:**
- Create: `internal/podspec/labels.go`
- Create: `internal/podspec/server.go`
- Test: `internal/podspec/server_test.go`

**Interfaces:**
- Consumes: `v1alpha1.Network`, `v1alpha1.ServerGroup`, `v1alpha1.Server`.
- Produces:
  - Label-Konstanten: `podspec.LabelManagedBy = "spawnery.cloud/managed-by"`, `LabelNetwork`, `LabelGroup`, `LabelServer`, `LabelRole`, `LabelOccupied` sowie `ManagedByValue = "spawnery-operator"`, `RoleServer = "server"`, `RoleProxy = "proxy"`.
  - `podspec.ServerLabels(network, group, server string) map[string]string`.
  - `podspec.ManagedSelector(network string) map[string]string`.
  - `podspec.BuildServerPod(net *v1alpha1.Network, group *v1alpha1.ServerGroup, srv *v1alpha1.Server) (*corev1.Pod, error)`.
  - `podspec.AnnotationSafeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"`.

Warum keine Deployments: das spielerbewusste Scale-Down entscheidet selbst, welcher Pod geht. Der Pod trägt deshalb den Namen des `Server` und dessen Owner-Reference.

Der Pod nutzt in Meilenstein 1 den `default`-ServiceAccount mit `automountServiceAccountToken: false` — er trägt damit keine Kubernetes-Credentials. Der dedizierte ServiceAccount samt projiziertem, audience-gebundenem Token kommt in Meilenstein 2 zusammen mit dem gRPC-Kanal.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/podspec/server_test.go`:

```go
package podspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func testNetwork() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			Defaults: &spawneryv1alpha1.Defaults{
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-credentials"}},
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
				Scheduling: &spawneryv1alpha1.Scheduling{
					NodeSelector: map[string]string{"node-role/minecraft": "true"},
				},
			},
		},
	}
}

func testGroup() *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:                          spawneryv1alpha1.ServerGroupEphemeral,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    100,
			TerminationGracePeriodSeconds: 60,
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40,
			},
		},
	}
}

func testServer() *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name: "lobby-x7k2", Namespace: "minecraft", UID: "server-uid",
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: "lobby"},
			GroupGeneration: 7,
		},
	}
}

func build(t *testing.T, mutate func(*spawneryv1alpha1.Network, *spawneryv1alpha1.ServerGroup)) *corev1.Pod {
	t.Helper()
	net, group := testNetwork(), testGroup()
	if mutate != nil {
		mutate(net, group)
	}
	pod, err := BuildServerPod(net, group, testServer())
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	return pod
}

func TestPodIdentity(t *testing.T) {
	pod := build(t, nil)

	if pod.Name != "lobby-x7k2" || pod.Namespace != "minecraft" {
		t.Errorf("pod identity = %s/%s, want minecraft/lobby-x7k2", pod.Namespace, pod.Name)
	}
	want := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   "production",
		LabelGroup:     "lobby",
		LabelServer:    "lobby-x7k2",
		LabelRole:      RoleServer,
	}
	for k, v := range want {
		if pod.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if pod.Labels[LabelOccupied] != "" {
		t.Error("the occupied label is maintained by the controller, not by the builder")
	}
	if len(pod.OwnerReferences) != 1 ||
		pod.OwnerReferences[0].Kind != "Server" ||
		pod.OwnerReferences[0].Name != "lobby-x7k2" ||
		pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller {
		t.Errorf("owner reference = %+v, want a controller ref to Server/lobby-x7k2", pod.OwnerReferences)
	}
	if pod.Annotations[AnnotationSafeToEvict] != "false" {
		t.Errorf("%s = %q, want false", AnnotationSafeToEvict, pod.Annotations[AnnotationSafeToEvict])
	}
}

func TestPodCarriesNoKubernetesCredentials(t *testing.T) {
	pod := build(t, nil)
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false")
	}
}

func TestPodIsRestrictedCompliant(t *testing.T) {
	pod := build(t, nil)

	if pod.Spec.SecurityContext == nil ||
		pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot must be true")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil ||
		pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccompProfile must be RuntimeDefault")
	}

	c := pod.Spec.Containers[0]
	if c.SecurityContext == nil {
		t.Fatal("container security context missing")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities = %+v, want drop ALL", c.SecurityContext.Capabilities)
	}
}

func TestPodWritableDirectories(t *testing.T) {
	pod := build(t, nil)

	mounts := map[string]bool{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mounts[m.MountPath] = true
	}
	for _, path := range []string{"/data", "/tmp"} {
		if !mounts[path] {
			t.Errorf("%s is not mounted; with a read-only root the server cannot write", path)
		}
	}
}

func TestPodReadinessProbeIsAnSLPCheck(t *testing.T) {
	pod := build(t, nil)

	probe := pod.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("readiness probe = %+v, want an exec probe", probe)
	}
	if probe.Exec.Command[0] != SLPHealthBinary {
		t.Errorf("probe command = %v, want %s first — a tcpSocket check turns green before the world is loaded",
			probe.Exec.Command, SLPHealthBinary)
	}
	if pod.Spec.Containers[0].LivenessProbe != nil {
		t.Error("no liveness probe: a restart would kick every player on the server")
	}
}

func TestPodPort(t *testing.T) {
	pod := build(t, nil)

	ports := pod.Spec.Containers[0].Ports
	if len(ports) != 1 || ports[0].ContainerPort != MinecraftPort || ports[0].Name != MinecraftPortName {
		t.Errorf("ports = %+v, want a single %s port %d", ports, MinecraftPortName, MinecraftPort)
	}
	if ports[0].HostPort != 0 {
		t.Error("server pods never bind a host port; only proxies are exposed")
	}
}

func TestNetworkDefaultsAreInherited(t *testing.T) {
	pod := build(t, nil)

	if pod.Spec.NodeSelector["node-role/minecraft"] != "true" {
		t.Errorf("nodeSelector = %v, want the network default", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Errorf("imagePullSecrets = %v, want the network default", pod.Spec.ImagePullSecrets)
	}
	if got := pod.Spec.Containers[0].Resources.Requests.Cpu(); got.String() != "1" {
		t.Errorf("cpu request = %s, want the network default 1", got)
	}
}

func TestGroupOverridesNetworkDefaults(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Scheduling = &spawneryv1alpha1.Scheduling{
			NodeSelector: map[string]string{"node-role/minigames": "true"},
		}
		g.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
		}
	})

	if _, ok := pod.Spec.NodeSelector["node-role/minecraft"]; ok {
		t.Error("group scheduling must replace the network default, not merge with it")
	}
	if pod.Spec.NodeSelector["node-role/minigames"] != "true" {
		t.Errorf("nodeSelector = %v, want the group override", pod.Spec.NodeSelector)
	}
	if got := pod.Spec.Containers[0].Resources.Requests.Cpu(); got.String() != "4" {
		t.Errorf("cpu request = %s, want the group override 4", got)
	}
}

func TestUserMounts(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:      "lobby-config",
			MountPath: "/data/config",
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "lobby-config"},
			},
		}}
	})

	var found bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "lobby-config" && m.MountPath == "/data/config" && m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Errorf("volumeMounts = %+v, want a read-only lobby-config at /data/config",
			pod.Spec.Containers[0].VolumeMounts)
	}

	for _, v := range pod.Spec.Volumes {
		if v.Name == "lobby-config" {
			if v.ConfigMap == nil || v.ConfigMap.Name != "lobby-config" {
				t.Errorf("volume lobby-config = %+v, want the configMap source", v)
			}
			return
		}
	}
	t.Errorf("volumes = %+v, want a lobby-config volume", pod.Spec.Volumes)
}

func TestEnvironment(t *testing.T) {
	pod := build(t, nil)

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SPAWNERY_NETWORK"] != "production" ||
		env["SPAWNERY_GROUP"] != "lobby" ||
		env["SPAWNERY_SERVER"] != "lobby-x7k2" ||
		env["SPAWNERY_MAX_PLAYERS"] != "100" {
		t.Errorf("env = %v, want network, group, server and max players", env)
	}
}

func TestTerminationGracePeriod(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.TerminationGracePeriodSeconds = 300
	})
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != 300 {
		t.Errorf("terminationGracePeriodSeconds = %v, want 300", pod.Spec.TerminationGracePeriodSeconds)
	}
}

func TestPersistentGroupMountsItsPVC(t *testing.T) {
	net := testNetwork()
	group := testGroup()
	group.Name = "survival"
	group.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
	group.Spec.Scaling = nil
	group.Spec.Replicas = ptr.To[int32](1)
	group.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("20Gi")}

	srv := testServer()
	srv.Name = "survival-0"
	srv.Spec.GroupRef.Name = "survival"
	srv.Spec.Ordinal = ptr.To[int32](0)

	pod, err := BuildServerPod(net, group, srv)
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}

	for _, v := range pod.Spec.Volumes {
		if v.Name == DataVolumeName {
			if v.PersistentVolumeClaim == nil {
				t.Fatalf("data volume = %+v, want a PVC source for a persistent group", v)
			}
			if v.PersistentVolumeClaim.ClaimName != "survival-0-data" {
				t.Errorf("claimName = %q, want survival-0-data", v.PersistentVolumeClaim.ClaimName)
			}
			return
		}
	}
	t.Errorf("volumes = %+v, want a %s volume", pod.Spec.Volumes, DataVolumeName)
}

func TestBuildRejectsAnEmptyImage(t *testing.T) {
	net, group := testNetwork(), testGroup()
	group.Spec.Image = ""
	if _, err := BuildServerPod(net, group, testServer()); err == nil {
		t.Fatal("empty image accepted, want an error")
	}
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/podspec/... -v`
Expected: FAIL — `undefined: BuildServerPod`.

- [ ] **Step 3: Labels implementieren**

`internal/podspec/labels.go`:

```go
// Package podspec turns the Spawnery API objects into pod specs. It is pure:
// no client, no cluster access, so every inheritance and override rule is
// covered by table tests.
package podspec

// Labels the operator puts on every managed pod.
const (
	// LabelManagedBy marks a pod as belonging to Spawnery.
	LabelManagedBy = "spawnery.cloud/managed-by"
	// LabelNetwork carries the Network name. NetworkPolicies select on it.
	LabelNetwork = "spawnery.cloud/network"
	// LabelGroup carries the ProxyGroup or ServerGroup name.
	LabelGroup = "spawnery.cloud/group"
	// LabelServer carries the Server name.
	LabelServer = "spawnery.cloud/server"
	// LabelRole is "server" or "proxy".
	LabelRole = "spawnery.cloud/role"
	// LabelOccupied is set to "true" while players are online. The group's
	// PodDisruptionBudget selects on it, which is what stops the eviction API
	// from removing an occupied pod. Maintained by the Server controller.
	LabelOccupied = "spawnery.cloud/occupied"
)

// Label values.
const (
	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "spawnery-operator"
	// RoleServer is the value of LabelRole for Paper pods.
	RoleServer = "server"
	// RoleProxy is the value of LabelRole for Velocity pods.
	RoleProxy = "proxy"
)

// AnnotationSafeToEvict tells the cluster autoscaler to leave the pod alone.
// It is only a hint to the autoscaler and no protection against kubectl drain
// — that is what the PodDisruptionBudget is for.
const AnnotationSafeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// ServerLabels are the labels of a Paper pod.
func ServerLabels(network, group, server string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
		LabelGroup:     group,
		LabelServer:    server,
		LabelRole:      RoleServer,
	}
}

// ManagedSelector matches every pod Spawnery manages for one network.
func ManagedSelector(network string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
	}
}
```

- [ ] **Step 4: Pod-Builder implementieren**

`internal/podspec/server.go`:

```go
package podspec

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	// MinecraftPort is the port every Paper server listens on.
	MinecraftPort int32 = 25565
	// MinecraftPortName names that port.
	MinecraftPortName = "minecraft"

	// ContainerName is the name of the Paper container.
	ContainerName = "minecraft"

	// DataVolumeName is the server's working directory: an emptyDir for
	// ephemeral groups, a PVC for persistent ones.
	DataVolumeName = "data"
	// TmpVolumeName is scratch space, needed because the root filesystem is
	// read-only.
	TmpVolumeName = "tmp"

	// DataMountPath is where DataVolumeName is mounted.
	DataMountPath = "/data"
	// TmpMountPath is where TmpVolumeName is mounted.
	TmpMountPath = "/tmp"

	// SLPHealthBinary is the Server-List-Ping tool baked into the base image.
	// Kubelet knows no SLP probe type, and a tcpSocket probe on 25565 turns
	// green before the world is loaded.
	SLPHealthBinary = "/usr/local/bin/spawnery-slp"
)

// DataClaimName is the name of the PVC of a persistent server.
func DataClaimName(server string) string {
	return server + "-" + DataVolumeName
}

// BuildServerPod renders the pod of one Server. The Server owns the pod, so
// deleting the Server cascades.
func BuildServerPod(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
) (*corev1.Pod, error) {
	if group.Spec.Image == "" {
		return nil, fmt.Errorf("server group %q has no image", group.Name)
	}

	resources := group.Spec.Resources
	if resources == nil && net.Spec.Defaults != nil {
		resources = net.Spec.Defaults.Resources
	}

	// A group's scheduling replaces the network default wholesale. Merging the
	// two would make it impossible to drop an inherited nodeSelector.
	scheduling := group.Spec.Scheduling
	if scheduling == nil && net.Spec.Defaults != nil {
		scheduling = net.Spec.Defaults.Scheduling
	}

	var pullSecrets []corev1.LocalObjectReference
	if net.Spec.Defaults != nil {
		pullSecrets = net.Spec.Defaults.ImagePullSecrets
	}

	volumes := []corev1.Volume{
		dataVolume(group, srv),
		{
			Name:         TmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: DataVolumeName, MountPath: DataMountPath},
		{Name: TmpVolumeName, MountPath: TmpMountPath},
	}

	for _, m := range group.Spec.Mounts {
		volumes = append(volumes, corev1.Volume{
			Name: m.Name,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: m.ConfigMap,
				Secret:    m.Secret,
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			ReadOnly:  true,
		})
	}

	container := corev1.Container{
		Name:  ContainerName,
		Image: group.Spec.Image,
		Ports: []corev1.ContainerPort{{
			Name:          MinecraftPortName,
			ContainerPort: MinecraftPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: []corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: "SPAWNERY_SERVER", Value: srv.Name},
			{Name: "SPAWNERY_MAX_PLAYERS", Value: strconv.FormatInt(int64(group.Spec.MaxPlayers), 10)},
		},
		VolumeMounts: mounts,
		// Readiness only. A liveness probe would restart the container and
		// kick every player on it — the state machine handles a red readiness
		// probe by deregistering instead.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{
					Command: []string{
						SLPHealthBinary,
						"--host", "127.0.0.1",
						"--port", strconv.FormatInt(int64(MinecraftPort), 10),
					},
				},
			},
			InitialDelaySeconds: 20,
			PeriodSeconds:       5,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if resources != nil {
		container.Resources = *resources
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srv.Name,
			Namespace: srv.Namespace,
			Labels:    ServerLabels(net.Name, group.Name, srv.Name),
			Annotations: map[string]string{
				AnnotationSafeToEvict: "false",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spawneryv1alpha1.GroupVersion.String(),
				Kind:               "Server",
				Name:               srv.Name,
				UID:                srv.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{container},
			Volumes:       volumes,
			RestartPolicy: corev1.RestartPolicyAlways,
			// The pods carry no Kubernetes credentials. Milestone 2 mounts a
			// projected, audience-bound token for the gRPC channel instead.
			AutomountServiceAccountToken: ptr.To(false),
			ImagePullSecrets:             pullSecrets,
			TerminationGracePeriodSeconds: ptr.To(group.Spec.TerminationGracePeriodSeconds),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		},
	}

	if scheduling != nil {
		pod.Spec.NodeSelector = scheduling.NodeSelector
		pod.Spec.Tolerations = scheduling.Tolerations
		pod.Spec.Affinity = scheduling.Affinity
	}

	return pod, nil
}

func dataVolume(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) corev1.Volume {
	if group.Spec.Type == spawneryv1alpha1.ServerGroupPersistent {
		return corev1.Volume{
			Name: DataVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: DataClaimName(srv.Name),
				},
			},
		}
	}
	return corev1.Volume{
		Name:         DataVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}
```

- [ ] **Step 5: Test laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/podspec/... -v`
Expected: PASS für alle 13 Tests.

- [ ] **Step 6: Commit**

```bash
git add internal/podspec
git commit -m "Pod-Builder für Server-Pods"
```

---

### Task 8: Server-Controller

**Files:**
- Create: `internal/controller/registrar.go`
- Create: `internal/controller/server_controller.go`
- Test: `internal/controller/server_controller_test.go`
- Test: `internal/controller/suite_test.go`

**Interfaces:**
- Consumes: `phase.Decide`, `phase.Inputs`, `agent.Registry`, `podspec.BuildServerPod`, `podspec.Label*`, alle API-Typen.
- Produces:
  - `controller.Registrar` mit `Register(ctx context.Context, srv *v1alpha1.Server) error`, `Deregister(ctx context.Context, srv *v1alpha1.Server) error`, `Drain(ctx context.Context, srv *v1alpha1.Server) error`.
  - `controller.NoopRegistrar` — die Meilenstein-1-Implementierung; Meilenstein 3 ersetzt sie durch den Proxy-Broadcast.
  - `controller.ServerFinalizer = "spawnery.cloud/drain"`.
  - `controller.MaxContainerRestarts int32 = 3`.
  - `controller.ReasonPodNameConflict = "PodNameConflict"`.
  - `controller.ServerReconciler{Client, Scheme, Recorder, Agents, Clock, StartupDeadline, PlayerStatusInterval, Registrar}` mit `Reconcile` und `SetupWithManager`.

Der Controller ist bewusst dünn: Eingaben sammeln, `phase.Decide` fragen, Entscheidung ausführen. Er trifft keine eigene Entscheidung darüber, ob ein Pod gelöscht werden darf.

Gruppe und Netzwerk werden nur für den Podbau gebraucht. Fehlt eines von beiden, läuft die Zustandsmaschine mit den CRD-Vorgaben weiter (`fallbackGroup`), der Podbau entfällt, und der Grund steht als `Accepted`-Condition am Server — sonst hinge ein Server, dessen Gruppe gelöscht wurde, für immer mit Pod und Finalizer fest und der Verwaisten-Abgleich aus Task 11 liefe in eine Blockade.

Findet der Controller einen Pod, während `status.podName` leer ist, übernimmt er ihn, sofern die Owner-Referenz auf genau diesen Server zeigt — das ist die Erholung von einem Statusschreibvorgang, der zwischen `Create(pod)` und `Status().Update` verloren ging. Gehört der Pod jemandem anders, bleibt er unangetastet.

- [ ] **Step 1: Registrar-Port schreiben**

`internal/controller/registrar.go`:

```go
package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// Registrar is how the Server controller reaches the proxies. Milestone 3
// implements it on top of the gRPC broadcast; milestone 1 wires the no-op, so
// the state machine can be exercised end to end without a proxy.
type Registrar interface {
	// Register tells every connected proxy to accept players for this server.
	Register(ctx context.Context, srv *spawneryv1alpha1.Server) error
	// Deregister tells every proxy to stop sending players here.
	Deregister(ctx context.Context, srv *spawneryv1alpha1.Server) error
	// Drain tells every proxy to move the players off this server.
	Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error
}

// NoopRegistrar logs what it would have sent.
type NoopRegistrar struct{}

// Register implements Registrar.
func (NoopRegistrar) Register(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	log.FromContext(ctx).V(1).Info("would register server with the proxies", "server", srv.Name)
	return nil
}

// Deregister implements Registrar.
func (NoopRegistrar) Deregister(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	log.FromContext(ctx).V(1).Info("would deregister server from the proxies", "server", srv.Name)
	return nil
}

// Drain implements Registrar.
func (NoopRegistrar) Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	log.FromContext(ctx).V(1).Info("would drain players off the server", "server", srv.Name)
	return nil
}
```

- [ ] **Step 2: Den fehlschlagenden Test schreiben**

`internal/controller/suite_test.go`:

```go
package controller

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

// containsString is the test-side spelling of the finalizer check.
func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// testClock is a hand-cranked clock shared by the controller tests.
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// recordingRegistrar remembers the calls the controller made.
type recordingRegistrar struct {
	registered   []string
	deregistered []string
	drained      []string
}

func (r *recordingRegistrar) Register(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.registered = append(r.registered, s.Name)
	return nil
}

func (r *recordingRegistrar) Deregister(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.deregistered = append(r.deregistered, s.Name)
	return nil
}

func (r *recordingRegistrar) Drain(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.drained = append(r.drained, s.Name)
	return nil
}

// fixture is one isolated namespace with a network, a group and the wired
// Server reconciler.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	c         client.Client
	ns        string
	clock     *testClock
	agents    *agent.Registry
	registrar *recordingRegistrar
	reconc    *ServerReconciler
	network   *spawneryv1alpha1.Network
	group     *spawneryv1alpha1.ServerGroup
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}
	agents := agent.New(clock.Now, 5*time.Second, start)
	registrar := &recordingRegistrar{}

	f := &fixture{
		t: t, ctx: ctx, c: c, ns: ns,
		clock: clock, agents: agents, registrar: registrar,
		reconc: &ServerReconciler{
			Client:               c,
			Scheme:               testenv.Scheme(t),
			Recorder:             record.NewFakeRecorder(100),
			Agents:               agents,
			Clock:                clock.Now,
			StartupDeadline:      5 * time.Minute,
			PlayerStatusInterval: 30 * time.Second,
			Registrar:            registrar,
		},
	}

	f.network = &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := c.Create(ctx, f.network); err != nil {
		t.Fatalf("create Network: %v", err)
	}

	f.group = &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:                          spawneryv1alpha1.ServerGroupEphemeral,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    100,
			TerminationGracePeriodSeconds: 60,
			FailedRetentionSeconds:        3600,
			Drain:                         &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 60},
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40,
			},
		},
	}
	if err := c.Create(ctx, f.group); err != nil {
		t.Fatalf("create ServerGroup: %v", err)
	}

	return f
}

// createServer adds one Server owned by the fixture's group.
func (f *fixture) createServer(name string) *spawneryv1alpha1.Server {
	f.t.Helper()
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       f.group.Name,
				UID:        f.group.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			GroupGeneration: f.group.Generation,
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		f.t.Fatalf("create Server: %v", err)
	}
	return srv
}

// reconcile runs the Server reconciler once.
func (f *fixture) reconcile(name string) {
	f.t.Helper()
	_, err := f.reconc.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	})
	if err != nil {
		f.t.Fatalf("reconcile %s: %v", name, err)
	}
}

// server re-reads a Server.
func (f *fixture) server(name string) *spawneryv1alpha1.Server {
	f.t.Helper()
	srv := &spawneryv1alpha1.Server{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, srv); err != nil {
		f.t.Fatalf("get Server %s: %v", name, err)
	}
	return srv
}

// pod re-reads the pod of a server. found is false once it is gone — which
// includes a pod that only carries a deletion timestamp, because envtest runs
// no kubelet to finish the job.
func (f *fixture) pod(name string) (*corev1.Pod, bool) {
	f.t.Helper()
	pod := &corev1.Pod{}
	err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, pod)
	if err != nil {
		return nil, false
	}
	if !pod.DeletionTimestamp.IsZero() {
		return pod, false
	}
	return pod, true
}

// setPodRunning fakes what a kubelet would do: envtest runs no nodes.
func (f *fixture) setPodRunning(name string, ready bool) {
	f.t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		f.t.Fatalf("pod %s not found", name)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.42.3.17"
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: cond,
		LastTransitionTime: metav1.NewTime(f.clock.Now()),
	}}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		f.t.Fatalf("update pod status: %v", err)
	}
}
```

`internal/controller/server_controller_test.go`:

```go
package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// bringUpReady walks a fresh server all the way into phase Ready and returns
// the pod UID the agent registry is keyed on.
func bringUpReady(t *testing.T, f *fixture, name string) string {
	t.Helper()
	f.createServer(name)
	f.reconcile(name)

	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("reconcile did not create the pod for %s", name)
	}
	uid := string(pod.UID)

	f.setPodRunning(name, false)
	f.reconcile(name)

	f.setPodRunning(name, true)
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(name)

	if got := f.server(name).Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q, want Ready", got)
	}
	return uid
}

// driveToFailed flaps the server past MaxReadinessLosses so it ends up in phase
// Failed with its players still connected, and returns once it is there.
func driveToFailed(t *testing.T, f *fixture, name string) {
	t.Helper()
	for i := int32(0); i < phase.MaxReadinessLosses; i++ {
		f.setPodRunning(name, false)
		f.reconcile(name)
		if i < phase.MaxReadinessLosses-1 {
			f.setPodRunning(name, true)
			f.reconcile(name)
		}
	}
	f.reconcile(name)
	if got := f.server(name).Status.Phase; got != string(phase.Failed) {
		t.Fatalf("phase = %q after %d readiness losses, want Failed", got, phase.MaxReadinessLosses)
	}
}

// pods lists every pod in the fixture namespace that still exists.
func (f *fixture) pods() []corev1.Pod {
	f.t.Helper()
	list := &corev1.PodList{}
	if err := f.c.List(f.ctx, list, client.InNamespace(f.ns)); err != nil {
		f.t.Fatalf("list pods: %v", err)
	}
	return list.Items
}

// TestLongLivedReadyServerSurvivesAReadinessBlip is the regression test for the
// stale startup deadline. status.startedAt is written once at pod creation and
// never refreshed, so StartupDeadlineReached is true for every server older than
// the deadline. Before the fix, one probe blip on a server that had been serving
// for hours put it in Starting and the very next reconcile failed it — and the
// Failed retention then deleted its pod with everyone still on board.
func TestLongLivedReadyServerSurvivesAReadinessBlip(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 40, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	// Far past the 5 minute startup deadline.
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 40, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q after two hours of healthy service, want Ready", got)
	}

	// One probe blip.
	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")
	blipped := f.server("lobby-x7k2")
	if got := blipped.Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q after the blip, want Starting", got)
	}
	// The mechanism that makes this safe: entering Starting re-arms the
	// startup deadline, so the recovery attempt gets a full window.
	if blipped.Status.StartedAt == nil || !blipped.Status.StartedAt.Time.Equal(f.clock.Now()) {
		var got any
		if blipped.Status.StartedAt != nil {
			got = blipped.Status.StartedAt.Time.UTC()
		}
		t.Fatalf("status.startedAt = %v, want it re-armed to %v on entry into Starting",
			got, f.clock.Now().UTC())
	}

	// The reconcile that used to fail it: still Starting, still past the
	// startup deadline, but this server was playable once.
	f.reconcile("lobby-x7k2")
	srv := f.server("lobby-x7k2")
	if srv.Status.Phase == string(phase.Failed) {
		t.Fatal("a long-lived server was failed by its stale startup deadline")
	}
	if srv.Status.Phase != string(phase.Starting) {
		t.Fatalf("phase = %q, want Starting", srv.Status.Phase)
	}

	// And it recovers.
	f.setPodRunning("lobby-x7k2", true)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Errorf("phase = %q after recovery, want Ready", got)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while 40 players were online — core invariant broken")
	}
}

// TestServerThatNeverBecomesPlayableFailsAtTheDeadline is the other side of the
// re-armed startup deadline: a server that was never playable must still be
// failed when the deadline passes, exactly as before.
func TestServerThatNeverBecomesPlayableFailsAtTheDeadline(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	// The pod runs but its probe never turns green.
	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q, want Starting", got)
	}

	f.clock.Advance(6 * time.Minute) // the fixture's deadline is 5 minutes
	f.reconcile("lobby-x7k2")

	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Failed) {
		t.Errorf("phase = %q past the startup deadline, want Failed", got)
	}
	if len(f.registrar.drained) != 0 {
		t.Errorf("drained = %v, want none — this server never took a player", f.registrar.drained)
	}
}

// TestServerThatCannotRecoverIsFailedAndDrained is the zombie the first attempt
// at the startup-deadline fix created. Exempting a once-registered server from
// the deadline meant a server that fell out of Ready with a permanently red
// probe was never failed at all: the flap counter cannot catch it either,
// because losses are only counted on a Ready -> Starting transition that a
// permanently red probe never produces again. Its players sat on a server that
// fails its own health check, forever. Re-arming the clock instead of exempting
// the server fails it one deadline after the fall-back, and drains it.
func TestServerThatCannotRecoverIsFailedAndDrained(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	// The probe goes red and never comes back.
	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q after the fall-back, want Starting", got)
	}

	f.clock.Advance(200 * time.Minute)
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Failed) {
		t.Fatalf("phase = %q after 200 minutes with a red probe, want Failed — the server is a zombie",
			srv.Status.Phase)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want exactly one drain — 9 players were left on a dead server",
			f.registrar.drained)
	}
	if srv.Status.DrainStartedAt == nil {
		t.Error("status.drainStartedAt not set, so the drain would never time out")
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted with 9 players still on it — core invariant broken")
	}
}

// TestZombieIsCaughtUnderAContinuousReconcileLoop drives the reconciler the way
// the operator actually runs it — once per resync interval — instead of jumping
// the clock and reconciling once. That difference is the whole point: the
// startup deadline is re-armed only on *entry* into Starting, and a re-arm on
// every pass would push the deadline out forever under a real loop while
// leaving a single-reconcile test perfectly green. The zombie would be back,
// with its players still on board.
func TestZombieIsCaughtUnderAContinuousReconcileLoop(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	// The probe goes red and never comes back.
	f.setPodRunning("lobby-x7k2", false)

	const ticks = 200 // 200 * 5s resync is far past the 5 minute deadline
	failedAfter := -1
	for i := 0; i < ticks; i++ {
		f.clock.Advance(resyncInterval)
		if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
		if f.server("lobby-x7k2").Status.Phase == string(phase.Failed) {
			failedAfter = i
			break
		}
	}

	srv := f.server("lobby-x7k2")
	if failedAfter < 0 {
		t.Fatalf("still %q with 9 players after %d reconciles over %v — the startup deadline never fired",
			srv.Status.Phase, ticks, time.Duration(ticks)*resyncInterval)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want exactly one drain — 9 players were left on a dead server",
			f.registrar.drained)
	}
	if srv.Status.DrainStartedAt == nil {
		t.Error("status.drainStartedAt not set, so the drain would never time out")
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted with 9 players still on it — core invariant broken")
	}
}

// TestFailedServerDrainsBeforeItsPodIsDeleted covers the second half of the
// same hole: a server can reach Failed with its sessions untouched, and the
// retention path must not delete that pod without moving the players off first.
func TestFailedServerDrainsBeforeItsPodIsDeleted(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	driveToFailed(t, f, "lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want a drain when the server failed with players on it", f.registrar.drained)
	}
	if srv.Status.DrainStartedAt == nil {
		t.Error("status.drainStartedAt not set on a failed server that is being drained")
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod of a failed server deleted while players were online")
	}

	// While the drain runs the pod is kept, the drain clock is not pushed out by
	// the repeated decisions, and the command is not re-broadcast on every pass
	// — the Failed branch returns StartDrain each time, but the real registrar
	// fans out to every proxy.
	drainStarted := srv.Status.DrainStartedAt.DeepCopy()
	for i := 0; i < 3; i++ {
		f.clock.Advance(10 * time.Second)
		if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
		if _, ok := f.pod("lobby-x7k2"); !ok {
			t.Fatalf("pod deleted while 6 players were still online (tick %d)", i)
		}
	}
	if got := f.server("lobby-x7k2").Status.DrainStartedAt; !got.Equal(drainStarted) {
		t.Errorf("drainStartedAt rewritten: %v, want the original %v", got, drainStarted)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v after four reconciles of a draining failed server, want exactly one broadcast",
			f.registrar.drained)
	}

	// Once it runs empty and the retention has passed, it goes.
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("pod of an empty failed server survived its retention")
	}
}

// TestFailedServerIsCleanedUpOnceItsDrainDeadlinePasses documents the escape
// hatch deliberately: the drain of a failed server is bounded, so one stuck
// player cannot pin a broken server forever. The group's drain timeout is 60s
// and its failed retention an hour, so by the time the retention elapses the
// drain deadline has always passed — this is the intended end of that path,
// not an accident.
func TestFailedServerIsCleanedUpOnceItsDrainDeadlinePasses(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	driveToFailed(t, f, "lobby-x7k2")

	// Past both the drain deadline and the retention, with players still on.
	f.clock.Advance(2 * time.Hour)
	if err := f.agents.ReportPlayers(uid, 6, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Error("a failed server pinned by players survived its drain deadline")
	}
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Terminating) {
		t.Errorf("phase = %q, want Terminating", got)
	}
}

// TestDeletingAFailedServerDrainsThenReleasesIt pins that a Failed server
// honours a deletion request: it drains first and releases its finalizer after.
func TestDeletingAFailedServerDrainsThenReleasesIt(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	driveToFailed(t, f, "lobby-x7k2")

	if err := f.c.Delete(f.ctx, f.server("lobby-x7k2")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("deleting a failed server dropped its players")
	}
	if len(f.registrar.drained) == 0 {
		t.Error("deleting a failed server issued no drain")
	}

	// This is the window where the Failed branch really does return StartDrain
	// on every single pass: occupied, once registered, deletion pending, drain
	// deadline not yet reached. The clock stays put so the state holds. The
	// command must still go out exactly once — the real registrar broadcasts to
	// every proxy, and this loop would otherwise be eleven fan-outs.
	for i := 0; i < 10; i++ {
		f.reconcile("lobby-x7k2")
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v after ten further reconciles of a deleted, occupied failed server, want exactly one broadcast",
			f.registrar.drained)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were still online — core invariant broken")
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod still there after the failed server was emptied")
	}

	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer not released on a deleted failed server: %v", err)
	}
}

// TestServerOutlivingItsGroupStillDrainsAndReleasesItself pins that a missing
// ServerGroup does not freeze the controller. Before the fix both the group and
// the network lookup returned before the finalizer, the drain and the label
// sync, so such a Server kept its pod and its finalizer forever and the orphan
// sweep of Task 11 would deadlock on it.
func TestServerOutlivingItsGroupStillDrainsAndReleasesItself(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if err := f.c.Delete(f.ctx, f.group); err != nil {
		t.Fatalf("delete ServerGroup: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	accepted := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonGroupNotFound {
		t.Errorf("Accepted condition = %+v, want False/GroupNotFound", accepted)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod dropped when the group disappeared")
	}

	// It must still drain and still let go.
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Draining) {
		t.Errorf("phase = %q for a groupless server being deleted, want Draining", got)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod leaked: still there after the drain finished")
	}

	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer never released on a groupless server: %v", err)
	}
}

// TestServerInAPersistentGroupStillDrainsAndReleasesItself pins that "not
// implemented yet" bounds the pod, not the object's lifecycle. Persistent
// groups arrive in milestone 5, so no pod is built for them — but an early
// return on that check would skip the finalizer, the drain and the release
// exactly as the missing-group case used to, and such a Server could never be
// deleted at all.
func TestServerInAPersistentGroupStillDrainsAndReleasesItself(t *testing.T) {
	f := newFixture(t)

	persistent := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:                          spawneryv1alpha1.ServerGroupPersistent,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    50,
			Replicas:                      ptr.To(int32(1)),
			TerminationGracePeriodSeconds: 60,
			FailedRetentionSeconds:        3600,
			Drain:                         &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 60},
			Storage:                       &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("10Gi")},
		},
	}
	if err := f.c.Create(f.ctx, persistent); err != nil {
		t.Fatalf("create persistent ServerGroup: %v", err)
	}

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "survival-0",
			Namespace: f.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       persistent.Name,
				UID:        persistent.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: persistent.Name},
			Ordinal:  ptr.To(int32(0)),
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create Server: %v", err)
	}

	// No pod is built, but the object is managed: finalizer on, reason stated.
	f.reconcile("survival-0")
	srv = f.server("survival-0")
	if _, ok := f.pod("survival-0"); ok {
		t.Fatal("a pod was built for a persistent group")
	}
	if !containsString(srv.Finalizers, ServerFinalizer) {
		t.Fatalf("finalizers = %v, want %s even for a persistent group", srv.Finalizers, ServerFinalizer)
	}
	accepted := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason != spawneryv1alpha1.ReasonNotImplemented {
		t.Errorf("Accepted condition = %+v, want False/%s", accepted, spawneryv1alpha1.ReasonNotImplemented)
	}

	// Milestone 5 will create this pod; stand in for it so the drain path is
	// exercised rather than merely the empty release.
	built, err := podspec.BuildServerPod(f.network, persistent, srv)
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	if err := f.c.Create(f.ctx, built); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	f.reconcile("survival-0")
	if got := f.server("survival-0").Status.PodName; got != "survival-0" {
		t.Fatalf("status.podName = %q, want the pod adopted", got)
	}

	pod, ok := f.pod("survival-0")
	if !ok {
		t.Fatal("pod vanished")
	}
	uid := string(pod.UID)
	f.setPodRunning("survival-0", true)
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	if err := f.agents.ReportPlayers(uid, 5, 50); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("survival-0") // Pending -> Starting
	f.reconcile("survival-0") // Starting -> Ready
	if got := f.server("survival-0").Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q, want Ready", got)
	}

	// The point of the whole test: it must still drain and still let go.
	if err := f.c.Delete(f.ctx, f.server("survival-0")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("survival-0")
	if got := f.server("survival-0").Status.Phase; got != string(phase.Draining) {
		t.Errorf("phase = %q for a deleted persistent server, want Draining", got)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want one drain command", f.registrar.drained)
	}
	if _, ok := f.pod("survival-0"); !ok {
		t.Fatal("pod deleted while 5 players were online — core invariant broken")
	}

	if err := f.agents.ReportPlayers(uid, 0, 50); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("survival-0")
	if _, ok := f.pod("survival-0"); ok {
		t.Fatal("pod leaked: still there after the drain finished")
	}

	f.reconcile("survival-0")
	err = f.c.Get(f.ctx, types.NamespacedName{Name: "survival-0", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("finalizer never released on a persistent-group server: %v", err)
	}
}

// TestPodIsAdoptedAfterALostStatusWrite covers a crash between Create(pod) and
// the status update. fetchPod falls back to the server name and finds the pod,
// so without adoption the creation branch is skipped forever while podName and
// startedAt stay empty — the startup deadline could never fire and PodLost
// could never be detected.
func TestPodIsAdoptedAfterALostStatusWrite(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod created")
	}
	originalUID := pod.UID

	// Simulate the lost write: the pod exists, the status does not know it.
	srv := f.server("lobby-x7k2")
	srv.Status.PodName = ""
	srv.Status.StartedAt = nil
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("clear status: %v", err)
	}

	f.reconcile("lobby-x7k2")

	srv = f.server("lobby-x7k2")
	if srv.Status.PodName != "lobby-x7k2" {
		t.Errorf("status.podName = %q, want the existing pod adopted", srv.Status.PodName)
	}
	if srv.Status.StartedAt == nil {
		t.Error("status.startedAt not restored from the pod; the startup deadline needs it")
	}
	if got := f.pods(); len(got) != 1 {
		t.Errorf("%d pods in the namespace, want exactly 1 — a second pod was created", len(got))
	}
	if again, ok := f.pod("lobby-x7k2"); !ok || again.UID != originalUID {
		t.Error("the original pod was replaced instead of adopted")
	}
}

// TestForeignPodWithTheSameNameIsNotAdopted is the other half of adoption: the
// owner reference has to be verified, or a Server would take charge of a
// workload it never created.
func TestForeignPodWithTheSameNameIsNotAdopted(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")

	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-x7k2",
			Namespace: f.ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       f.group.Name,
				UID:        f.group.UID,
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "not-ours", Image: "busybox"}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create foreign pod: %v", err)
	}

	f.reconcile("lobby-x7k2")

	if got := f.server("lobby-x7k2").Status.PodName; got != "" {
		t.Errorf("status.podName = %q, want empty — a foreign pod was adopted", got)
	}
	got, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("the foreign pod was deleted")
	}
	if got.UID != foreign.UID {
		t.Error("the foreign pod was replaced")
	}
	if _, labelled := got.Labels[podspec.LabelOccupied]; labelled {
		t.Error("the controller labelled a pod it does not own")
	}
	if len(f.pods()) != 1 {
		t.Errorf("%d pods, want 1 — the controller created a second pod over the conflict", len(f.pods()))
	}
}

func TestReconcileCreatesThePod(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	pod, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("no pod created")
	}
	if pod.Labels[podspec.LabelGroup] != "lobby" {
		t.Errorf("pod labels = %v, want the group label", pod.Labels)
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != "Server" {
		t.Errorf("owner references = %+v, want a Server controller ref", pod.OwnerReferences)
	}

	srv := f.server("lobby-x7k2")
	if srv.Status.PodName != "lobby-x7k2" {
		t.Errorf("status.podName = %q, want lobby-x7k2", srv.Status.PodName)
	}
	if srv.Status.Phase != string(phase.Pending) {
		t.Errorf("phase = %q, want Pending until the pod runs", srv.Status.Phase)
	}
	if srv.Status.StartedAt == nil {
		t.Error("status.startedAt not set; the startup deadline needs it")
	}
	if !containsString(srv.Finalizers, ServerFinalizer) {
		t.Errorf("finalizers = %v, want %s", srv.Finalizers, ServerFinalizer)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")
	first, _ := f.pod("lobby-x7k2")

	f.reconcile("lobby-x7k2")
	second, ok := f.pod("lobby-x7k2")
	if !ok {
		t.Fatal("pod disappeared on the second reconcile")
	}
	if first.UID != second.UID {
		t.Error("second reconcile replaced the pod")
	}
}

func TestReadyGateNeedsBothSignals(t *testing.T) {
	f := newFixture(t)
	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	pod, _ := f.pod("lobby-x7k2")
	uid := string(pod.UID)

	// Only the probe is green.
	f.setPodRunning("lobby-x7k2", true)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Fatalf("phase = %q with a green probe alone, want Starting", got)
	}
	if len(f.registrar.registered) != 0 {
		t.Errorf("registered = %v, want no registration before the agent is ready", f.registrar.registered)
	}

	// Now the agent as well.
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Ready) {
		t.Fatalf("phase = %q with both signals, want Ready", srv.Status.Phase)
	}
	if !srv.Status.Registered {
		t.Error("status.registered = false, want true")
	}
	if srv.Status.Address != "10.42.3.17:25565" {
		t.Errorf("status.address = %q, want 10.42.3.17:25565", srv.Status.Address)
	}
	if len(f.registrar.registered) != 1 {
		t.Errorf("registered = %v, want exactly one registration", f.registrar.registered)
	}
}

func TestReadinessLossDeregistersImmediately(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")

	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Starting) {
		t.Errorf("phase = %q after readiness loss, want Starting", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("status.registered = true after readiness loss, want false")
	}
	if srv.Status.ReadinessLosses != 1 {
		t.Errorf("readinessLosses = %d, want 1", srv.Status.ReadinessLosses)
	}
	if len(f.registrar.deregistered) != 1 {
		t.Errorf("deregistered = %v, want exactly one deregistration", f.registrar.deregistered)
	}
}

func TestStreamLossDeregistersAfterTheGracePeriod(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")

	f.agents.Disconnect(uid)
	f.clock.Advance(phase.StreamDownGrace - time.Second)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Ready) {
		t.Fatalf("phase = %q inside the grace period, want Ready", got)
	}

	f.clock.Advance(2 * time.Second)
	f.reconcile("lobby-x7k2")
	if got := f.server("lobby-x7k2").Status.Phase; got != string(phase.Starting) {
		t.Errorf("phase = %q past the grace period, want Starting", got)
	}
}

func TestOccupiedLabelTracksThePlayerCount(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")

	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	pod, _ := f.pod("lobby-x7k2")
	if pod.Labels[podspec.LabelOccupied] != "true" {
		t.Errorf("occupied label = %q with 3 players, want true", pod.Labels[podspec.LabelOccupied])
	}

	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	pod, _ = f.pod("lobby-x7k2")
	if _, ok := pod.Labels[podspec.LabelOccupied]; ok {
		t.Errorf("occupied label still set with 0 players: %v", pod.Labels)
	}
}

func TestStalePlayerCountKeepsThePodOccupied(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	f.clock.Advance(11 * time.Second) // past twice the 5s report interval
	f.reconcile("lobby-x7k2")

	pod, _ := f.pod("lobby-x7k2")
	if pod.Labels[podspec.LabelOccupied] != "true" {
		t.Errorf("occupied label = %q on a stale count, want true — stale means occupied",
			pod.Labels[podspec.LabelOccupied])
	}
}

func TestDeletionDrainsBeforeThePodIsDeleted(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}

	// First reconcile after deletion: drain starts, pod survives.
	f.reconcile("lobby-x7k2")
	srv = f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Draining) {
		t.Fatalf("phase = %q, want Draining", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("a draining server must not stay registered")
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want one drain command", f.registrar.drained)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}

	// Players keep it alive.
	f.clock.Advance(time.Second)
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}

	// Now the server runs empty.
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod still there after the drain finished")
	}

	// The finalizer goes once the pod is gone.
	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Server still present after the drain: %v", err)
	}
}

// TestDeletionAfterAReadinessLossStillDrains covers the case the phase alone
// cannot describe: the server reached Ready, was registered, then lost a ready
// signal and fell back to Starting — deregistered, but with its players still
// connected, because deregistering only stops new joins. Deleting it now must
// drain, not terminate.
func TestDeletionAfterAReadinessLossStillDrains(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	f.setPodRunning("lobby-x7k2", false)
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Starting) {
		t.Fatalf("phase = %q after the readiness loss, want Starting", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("status.registered = true after the readiness loss, want false")
	}
	if !srv.Status.WasRegistered {
		t.Fatal("status.wasRegistered = false, want true — the server was registered once")
	}

	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv = f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Draining) {
		t.Errorf("phase = %q, want Draining — a once-registered server still holds its players",
			srv.Status.Phase)
	}
	if len(f.registrar.drained) != 1 {
		t.Errorf("drained = %v, want one drain command", f.registrar.drained)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("pod deleted while players were online — core invariant broken")
	}
}

func TestLostPodTerminatesTheServer(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")

	pod, _ := f.pod("lobby-x7k2")
	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	f.reconcile("lobby-x7k2")

	srv := f.server("lobby-x7k2")
	if srv.Status.Phase != string(phase.Terminating) {
		t.Errorf("phase = %q after the pod vanished, want Terminating", srv.Status.Phase)
	}
	if srv.Status.Registered {
		t.Error("a server whose pod is gone must be deregistered")
	}
}

func TestDrainTimeoutTerminatesLoudly(t *testing.T) {
	f := newFixture(t)
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")

	if err := f.c.Delete(f.ctx, f.server("lobby-x7k2")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")

	// Keep reporting players so the drain can never finish on its own.
	for i := 0; i < 13; i++ {
		f.clock.Advance(5 * time.Second)
		if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile("lobby-x7k2")
	}

	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod survived the drain timeout")
	}
}

// TestCrashLoopingOnlyLooksAtTheMinecraftContainer pins the scope of the
// crash-loop check: PodTerminal aborts a running drain, so a crash-looping
// sidecar must never be able to cut short the drain of a healthy server.
func TestCrashLoopingOnlyLooksAtTheMinecraftContainer(t *testing.T) {
	backoff := corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}

	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "metrics-sidecar",
			RestartCount: MaxContainerRestarts + 5,
			State:        backoff,
		}},
	}}
	if crashLooping(pod) {
		t.Error("a crash-looping sidecar counted as terminal; only the Minecraft container may")
	}

	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
		Name:         podspec.ContainerName,
		RestartCount: MaxContainerRestarts,
		State:        backoff,
	})
	if !crashLooping(pod) {
		t.Error("a crash-looping Minecraft container was not detected")
	}
}
```

- [ ] **Step 3: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/controller/... -v`
Expected: FAIL — `undefined: ServerReconciler`, `undefined: ServerFinalizer`, `undefined: MaxContainerRestarts`, `undefined: crashLooping`.

- [ ] **Step 4: Server-Controller implementieren**

`internal/controller/server_controller.go`:

```go
package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// ServerFinalizer keeps the Server object around until its players are safe
// and its pod is gone.
const ServerFinalizer = "spawnery.cloud/drain"

// MaxContainerRestarts is how often the Paper container may restart before the
// server counts as broken rather than flaky.
const MaxContainerRestarts int32 = 3

// ReasonPodNameConflict marks a Server whose pod name is taken by a pod it does
// not control.
const ReasonPodNameConflict = "PodNameConflict"

// defaultDrainTimeoutSeconds and defaultFailedRetentionSeconds mirror the
// kubebuilder defaults on ServerGroupSpec. They are what a Server falls back to
// when its group is gone, so drain and cleanup keep sane timings.
const (
	defaultDrainTimeoutSeconds    int32 = 60
	defaultFailedRetentionSeconds int32 = 3600
)

// resyncInterval is how often a Server is re-examined even without an event.
// The state machine has time-driven transitions (startup deadline, drain
// deadline, stream grace period) that no watch reports.
const resyncInterval = 5 * time.Second

// ServerReconciler drives one Server through the state machine.
type ServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Agents is the runtime state reported by the in-game agents.
	Agents *agent.Registry
	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
	// StartupDeadline is how long a server may take to reach Ready.
	StartupDeadline time.Duration
	// PlayerStatusInterval throttles player-count writes into etcd.
	PlayerStatusInterval time.Duration
	// Registrar reaches the proxies.
	Registrar Registrar
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servers/finalizers,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile collects the inputs, asks the state machine and executes the
// decision. It contains no rule of its own about deleting an occupied pod.
func (r *ServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	srv := &spawneryv1alpha1.Server{}
	if err := r.Get(ctx, req.NamespacedName, srv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only pod creation needs the group and the network. Everything else — the
	// finalizer, the drain, the occupied label, releasing the object — has to
	// keep running without them, or a Server whose group was deleted would stay
	// Ready forever with its pod alive and its finalizer held, and the orphan
	// sweep of Task 11 would deadlock on that finalizer.
	group := &spawneryv1alpha1.ServerGroup{}
	groupKey := types.NamespacedName{Name: srv.Spec.GroupRef.Name, Namespace: srv.Namespace}
	groupFound := true
	if err := r.Get(ctx, groupKey, group); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		groupFound = false
	}

	// Persistent groups need a PVC and an ordered shutdown; that is milestone 5,
	// so no pod is built for them. That must not stop anything else: an early
	// return here would skip the finalizer, the drain and the release, and a
	// Server in a persistent group could then never be deleted — the same
	// deadlock as a Server whose group is gone.
	persistentUnsupported := groupFound && !group.IsEphemeral()
	if persistentUnsupported {
		logger.Info("persistent groups are not implemented yet", "group", group.Name)
	}

	network := &spawneryv1alpha1.Network{}
	networkFound := false
	if groupFound {
		networkKey := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: srv.Namespace}
		if err := r.Get(ctx, networkKey, network); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else {
			networkFound = true
		}
	}

	// The finalizer must sit on the object before the pod exists, otherwise a
	// deletion between creation and the next reconcile skips the drain. This
	// has to happen before anything is written onto srv.Status: Update returns
	// the persisted object, whose status the API server does not take from us
	// because status is a subresource, so it overwrites every status change
	// made before it. On a first reconcile that status is empty.
	if srv.DeletionTimestamp.IsZero() && !slices.Contains(srv.Finalizers, ServerFinalizer) {
		srv.Finalizers = append(srv.Finalizers, ServerFinalizer)
		if err := r.Update(ctx, srv); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch {
	case !groupFound:
		logger.Info("server group not found, running on the CRD defaults", "group", srv.Spec.GroupRef.Name)
		setAccepted(srv, false, spawneryv1alpha1.ReasonGroupNotFound,
			fmt.Sprintf("server group %q not found; draining and cleanup continue on the default timings", srv.Spec.GroupRef.Name))
		group = fallbackGroup(srv)
	case persistentUnsupported:
		setAccepted(srv, false, spawneryv1alpha1.ReasonNotImplemented,
			fmt.Sprintf("server group %q is persistent; persistent groups arrive in milestone 5, so no pod is created for this server", group.Name))
	case !networkFound:
		logger.Info("network not found, running on the CRD defaults", "network", group.Spec.NetworkRef.Name)
		setAccepted(srv, false, spawneryv1alpha1.ReasonNetworkNotFound,
			fmt.Sprintf("network %q not found; no pod can be created for this server", group.Spec.NetworkRef.Name))
	default:
		setAccepted(srv, true, spawneryv1alpha1.ReasonAccepted, "group and network resolved")
	}

	pod, podFound, err := r.fetchPod(ctx, srv)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Recover from a status write lost between Create(pod) and Status().Update:
	// the pod is there but status.podName is empty, so without adoption the
	// creation branch would be skipped forever while the startup deadline could
	// never fire and PodLost could never be detected.
	nameConflict := false
	if podFound && srv.Status.PodName == "" {
		if metav1.IsControlledBy(pod, srv) {
			srv.Status.PodName = pod.Name
			if srv.Status.StartedAt == nil {
				started := pod.CreationTimestamp
				srv.Status.StartedAt = &started
			}
			r.Recorder.Eventf(srv, corev1.EventTypeNormal, "PodAdopted",
				"adopted existing pod %s after a lost status write", pod.Name)
		} else {
			// Someone else's pod holds this name. Adopting it would put this
			// Server in charge of a workload it never created, and deleting it
			// is not ours to do. Stand off and say so.
			r.Recorder.Eventf(srv, corev1.EventTypeWarning, "PodNameConflict",
				"pod %s exists but is not controlled by this Server", pod.Name)
			setAccepted(srv, false, ReasonPodNameConflict,
				fmt.Sprintf("pod %q exists but is not controlled by this Server", pod.Name))
			pod, podFound, nameConflict = nil, false, true
		}
	}

	// Create the pod once, and only for a server that has not been asked to go
	// away. status.podName is the record that a pod once existed; it is never
	// reused for a different pod, which is what makes PodLost detectable.
	if groupFound && networkFound && !persistentUnsupported && !nameConflict &&
		!podFound && srv.Status.PodName == "" && srv.DeletionTimestamp.IsZero() {
		built, err := podspec.BuildServerPod(network, group, srv)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, built); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(srv, corev1.EventTypeNormal, "PodCreated", "created pod %s", built.Name)

		srv.Status.PodName = built.Name
		now := metav1.NewTime(r.Clock())
		srv.Status.StartedAt = &now
		// A fresh pod has never been registered and carries no flap history.
		srv.Status.WasRegistered = false
		srv.Status.ReadinessLosses = 0
		if srv.Status.Phase == "" {
			srv.Status.Phase = string(phase.Pending)
		}
		if err := r.Status().Update(ctx, srv); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}

	in := r.collectInputs(srv, group, pod, podFound)
	current := phase.Phase(srv.Status.Phase)
	if current == "" {
		current = phase.Pending
	}
	decision := phase.Decide(current, in)

	if err := r.applyDecision(ctx, srv, group, pod, podFound, current, decision); err != nil {
		return ctrl.Result{}, err
	}

	// Once the pod is gone and deletion was requested, let the object go.
	if decision.Next == phase.Terminating && !podFound {
		if !srv.DeletionTimestamp.IsZero() {
			srv.Finalizers = slices.DeleteFunc(srv.Finalizers, func(f string) bool { return f == ServerFinalizer })
			if err := r.Update(ctx, srv); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		// Terminating without a deletion request means the state machine
		// decided the server is finished. Remove the object so the group
		// creates a replacement.
		if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// fetchPod returns the pod of a server. A pod carrying a deletion timestamp
// counts as gone: it is on its way out, its players are leaving with it, and
// nothing we could decide would bring it back. Without this rule the Server
// object would wait for a pod that only the kubelet can finally remove — and
// in envtest, where no kubelet runs, it would wait forever.
func (r *ServerReconciler) fetchPod(ctx context.Context, srv *spawneryv1alpha1.Server) (*corev1.Pod, bool, error) {
	name := srv.Status.PodName
	if name == "" {
		name = srv.Name
	}
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: srv.Namespace}, pod)
	switch {
	case err == nil:
		if !pod.DeletionTimestamp.IsZero() {
			return pod, false, nil
		}
		return pod, true, nil
	case apierrors.IsNotFound(err):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

// collectInputs is the only place that reads Kubernetes state into the pure
// state machine.
func (r *ServerReconciler) collectInputs(
	srv *spawneryv1alpha1.Server,
	group *spawneryv1alpha1.ServerGroup,
	pod *corev1.Pod,
	podFound bool,
) phase.Inputs {
	now := r.Clock()

	in := phase.Inputs{
		DeletionRequested: !srv.DeletionTimestamp.IsZero(),
		PodExists:         podFound,
		PodLost:           !podFound && srv.Status.PodName != "",
		ReadinessLosses:   srv.Status.ReadinessLosses,
		// Whether the server was ever registered is recorded state, not
		// something to re-derive here: a Starting server that fell out of Ready
		// may still have players connected from before the readiness loss (the
		// fallback deregisters to stop new joins, it does not move anyone off),
		// and only status.wasRegistered still knows that. The controller writes
		// it wherever it registers and resets it when it creates a fresh pod.
		WasRegistered: srv.Status.WasRegistered,
	}

	if podFound {
		in.PodRunning = pod.Status.Phase == corev1.PodRunning
		in.PodTerminal = pod.Status.Phase == corev1.PodFailed ||
			pod.Status.Phase == corev1.PodSucceeded ||
			crashLooping(pod)
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady {
				in.PodReady = c.Status == corev1.ConditionTrue
			}
		}
	}

	snap := r.Agents.Lookup(podUID(pod, podFound))
	in.AgentReady = snap.Ready
	in.AgentConnected = snap.Connected
	in.AgentStreamDownFor = snap.StreamDownFor
	in.PlayersOnline = snap.Players
	in.PlayersStale = snap.PlayersStale
	in.Slots = snap.Slots

	if srv.Status.StartedAt != nil {
		in.StartupDeadlineReached = now.Sub(srv.Status.StartedAt.Time) > r.StartupDeadline
	}
	if srv.Status.ReadySince != nil {
		in.ReadyFor = now.Sub(srv.Status.ReadySince.Time)
	}
	if srv.Status.DrainStartedAt != nil {
		in.DrainDeadlineReached = now.Sub(srv.Status.DrainStartedAt.Time) >= group.DrainTimeout()
	}
	if srv.Status.FailedAt != nil {
		in.FailedRetentionElapsed = now.Sub(srv.Status.FailedAt.Time) >= group.FailedRetention()
	}

	return in
}

// fallbackGroup stands in for a ServerGroup that is gone. It carries the CRD
// defaults, so a Server that outlives its group still drains and cleans up on
// sane timings instead of freezing. It is never used to build a pod.
func fallbackGroup(srv *spawneryv1alpha1.Server) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      srv.Spec.GroupRef.Name,
			Namespace: srv.Namespace,
		},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:                   spawneryv1alpha1.ServerGroupEphemeral,
			Drain:                  &spawneryv1alpha1.DrainSpec{TimeoutSeconds: defaultDrainTimeoutSeconds},
			FailedRetentionSeconds: defaultFailedRetentionSeconds,
		},
	}
}

// setAccepted records whether the operator can fully manage this Server. It is
// written onto the object; applyDecision persists it with the rest of the
// status in a single update.
func setAccepted(srv *spawneryv1alpha1.Server, ok bool, reason, message string) {
	meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  conditionStatus(ok),
		Reason:  reason,
		Message: message,
	})
}

func podUID(pod *corev1.Pod, found bool) string {
	if !found {
		return ""
	}
	return string(pod.UID)
}

// crashLooping reports whether the Minecraft container is stuck restarting.
// The check is deliberately scoped to that one container: PodTerminal aborts a
// running drain, so a crash-looping sidecar must never be able to cut short the
// drain of a healthy server that still has players on it.
func crashLooping(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != podspec.ContainerName {
			continue
		}
		if cs.RestartCount >= MaxContainerRestarts &&
			cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

// applyDecision executes the decision and writes the status.
func (r *ServerReconciler) applyDecision(
	ctx context.Context,
	srv *spawneryv1alpha1.Server,
	group *spawneryv1alpha1.ServerGroup,
	pod *corev1.Pod,
	podFound bool,
	current phase.Phase,
	d phase.Decision,
) error {
	now := metav1.NewTime(r.Clock())

	if d.Deregister {
		if err := r.Registrar.Deregister(ctx, srv); err != nil {
			return fmt.Errorf("deregister %s: %w", srv.Name, err)
		}
		srv.Status.Registered = false
	}
	if d.Register {
		if err := r.Registrar.Register(ctx, srv); err != nil {
			return fmt.Errorf("register %s: %w", srv.Name, err)
		}
		srv.Status.Registered = true
		// Remembered for the life of this pod: from here on a deletion has to
		// drain, even if the server falls back out of Ready first.
		srv.Status.WasRegistered = true
	}
	// The drain clock starts with the drain, not with phase Draining: a Failed
	// server is drained while staying Failed, and without this its deadline
	// would never be reached. Both the clock and the broadcast happen exactly
	// once — the Failed branch repeats StartDrain on every pass, and re-sending
	// the command to every proxy each resync would be pure noise. A proxy that
	// reconnects is re-synced from the phase in the CR status.
	if d.StartDrain && srv.Status.DrainStartedAt == nil {
		if err := r.Registrar.Drain(ctx, srv); err != nil {
			return fmt.Errorf("drain %s: %w", srv.Name, err)
		}
		srv.Status.DrainStartedAt = &now
	}

	if d.CountReadinessLoss {
		srv.Status.ReadinessLosses++
		r.Recorder.Eventf(srv, corev1.EventTypeWarning, phase.ReasonReadinessLost,
			"%s (loss %d of %d)", d.Message, srv.Status.ReadinessLosses, phase.MaxReadinessLosses)
	}
	if d.ResetReadinessLosses {
		srv.Status.ReadinessLosses = 0
	}

	// Phase bookkeeping. These timestamps are what the time-driven inputs are
	// derived from, so they have to survive an operator restart.
	if d.Next != current {
		r.Recorder.Eventf(srv, corev1.EventTypeNormal, d.Reason,
			"phase %s -> %s: %s", current, d.Next, d.Message)
	}
	switch d.Next {
	case phase.Ready:
		if current != phase.Ready || srv.Status.ReadySince == nil {
			srv.Status.ReadySince = &now
		}
	case phase.Starting:
		// Re-arm the startup deadline. It bounds the current attempt to become
		// playable, not the age of the pod: entering Starting from Pending arms
		// it, and entering it from Ready after a readiness loss re-arms it for
		// the recovery attempt. Without this a server older than the deadline
		// would be failed by the first blip; with it, one that cannot recover is
		// still failed a deadline later.
		if current != phase.Starting {
			srv.Status.StartedAt = &now
		}
		srv.Status.ReadySince = nil
	case phase.Draining:
		if srv.Status.DrainStartedAt == nil {
			srv.Status.DrainStartedAt = &now
		}
		srv.Status.ReadySince = nil
	case phase.Failed:
		if srv.Status.FailedAt == nil {
			srv.Status.FailedAt = &now
		}
		srv.Status.ReadySince = nil
	default:
		srv.Status.ReadySince = nil
	}
	srv.Status.Phase = string(d.Next)

	if podFound {
		srv.Status.Address = ""
		if pod.Status.PodIP != "" {
			srv.Status.Address = fmt.Sprintf("%s:%d", pod.Status.PodIP, podspec.MinecraftPort)
		}
	}

	snap := r.Agents.Lookup(podUID(pod, podFound))
	r.mirrorPlayerCount(srv, snap, now)

	if podFound {
		if err := r.syncOccupiedLabel(ctx, pod, snap); err != nil {
			return err
		}
	}

	if d.DeletePod && podFound && pod.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.Recorder.Eventf(srv, corev1.EventTypeNormal, "PodDeleted",
			"deleted pod %s: %s", pod.Name, d.Message)
	}

	meta.SetStatusCondition(&srv.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionReady,
		Status:  conditionStatus(d.Next == phase.Ready),
		Reason:  d.Reason,
		Message: d.Message,
	})

	return r.Status().Update(ctx, srv)
}

// mirrorPlayerCount writes the in-memory count into the status, throttled.
// The control loop reads memory; the CR status is for observers.
func (r *ServerReconciler) mirrorPlayerCount(
	srv *spawneryv1alpha1.Server,
	snap agent.Snapshot,
	now metav1.Time,
) {
	if !snap.Known {
		return
	}
	significant := snap.Players != srv.Status.Players || snap.Slots != srv.Status.Slots
	overdue := srv.Status.PlayersUpdatedAt == nil ||
		now.Time.Sub(srv.Status.PlayersUpdatedAt.Time) >= r.PlayerStatusInterval
	if !significant && !overdue {
		return
	}
	srv.Status.Players = snap.Players
	srv.Status.Slots = snap.Slots
	srv.Status.PlayersUpdatedAt = &now
}

// syncOccupiedLabel keeps the label the group's PodDisruptionBudget selects on
// in step with reality. A stale count counts as occupied.
func (r *ServerReconciler) syncOccupiedLabel(ctx context.Context, pod *corev1.Pod, snap agent.Snapshot) error {
	occupied := snap.PlayersStale || snap.Players > 0
	_, labelled := pod.Labels[podspec.LabelOccupied]
	if occupied == labelled {
		return nil
	}

	patched := pod.DeepCopy()
	if occupied {
		if patched.Labels == nil {
			patched.Labels = map[string]string{}
		}
		patched.Labels[podspec.LabelOccupied] = "true"
	} else {
		delete(patched.Labels, podspec.LabelOccupied)
	}
	return r.Patch(ctx, patched, client.MergeFrom(pod))
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// SetupWithManager registers the controller.
func (r *ServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.Server{}).
		Owns(&corev1.Pod{}).
		Named("server").
		Complete(r)
}
```

Für `meta.SetStatusCondition` gehört `"k8s.io/apimachinery/pkg/api/meta"` in die Importe.

- [ ] **Step 5: Abhängigkeiten aufräumen**

Der Controller zieht `sigs.k8s.io/controller-runtime` als Manager-Paket herein; damit kommen `fsnotify`, `golang.org/x/sync` und `gomodules.xyz/jsonpatch/v2` neu in den Modulgraphen. Keine Version ändert sich:

```bash
nix develop -c go mod tidy
```

- [ ] **Step 6: Tests laufen lassen, Erfolg prüfen**

```bash
nix develop -c make manifests generate
nix develop -c go test ./internal/controller/... -v
```

Expected: PASS für alle zwölf Tests, insbesondere `TestDeletionDrainsBeforeThePodIsDeleted`, `TestDeletionAfterAReadinessLossStillDrains` und `TestStalePlayerCountKeepsThePodOccupied`.

- [ ] **Step 7: Commit**

```bash
git add internal/controller config go.mod go.sum
git commit -m "Server-Controller mit Zustandsmaschine, Drain und Occupied-Label"
```

Das Statusfeld `wasRegistered` samt neu generiertem CRD gehört in einen eigenen, vorangehenden Commit — es ist eine API-Änderung und keine Controller-Änderung.

**Nachtrag aus Task 9: `syncOccupiedLabel` bekommt eine engere Regel.**

Der Block oben setzt `occupied := snap.PlayersStale || snap.Players > 0`. Das
Label bedeutet aber „auf diesem Pod können Spieler sein", und das ist enger als
„der Zählerstand ist veraltet". Ein Failed-Server behält seinen Pod die ganze
Retention über; sein Zählerstand veraltet dabei zwangsläufig, und die alte Regel
markiert damit einen toten Pod als belegt. Das daraus gebaute
PodDisruptionBudget gibt ihn nicht mehr frei — bei `currentHealthy <
desiredHealthy` rührt die Eviction-API auch einen unhealthy Pod nicht an —, und
ein `kubectl drain` läuft auf diesem Knoten nie zu Ende.

Umgesetzt ist deshalb:

```go
occupied := snap.Players > 0 ||
	(snap.PlayersStale && srv.Status.WasRegistered && !podTerminal(pod))
```

Zwei Ausnahmen, sonst nichts: der Server muss im Leben dieses Pods einmal bei
den Proxies registriert gewesen sein — zu einem anderen wird niemand geroutet —,
und sein Pod muss noch leben. Für jeden anderen Fall gilt „veraltet heisst
belegt" unverändert weiter; genau das schützt einen laufenden Server, dessen
Agent verstummt ist. Die Terminal-Prüfung liegt jetzt in `podTerminal(pod)`, das
sich `collectInputs` und `syncOccupiedLabel` teilen, damit es die Frage nur
einmal gibt. `syncOccupiedLabel` nimmt dafür zusätzlich den `srv` entgegen.

`ServerView.Occupied()` in Task 9 spiegelt diese Regel eins zu eins — wer eine
der beiden ändert, muss die andere mitändern.

---

### Task 9: ServerGroup-Controller

**Files:**
- Create: `internal/controller/candidates.go`
- Create: `internal/controller/servergroup_controller.go`
- Test: `internal/controller/candidates_test.go`
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `v1alpha1.ServerGroup.DesiredReplicas()`, `agent.Registry`, `podspec.Label*`, `phase.Phase`.
- Produces:
  - `controller.ServerView{Name string, Phase phase.Phase, Players int32, Slots int32, Stale bool, Generation int64, CreatedAt time.Time}`.
  - `controller.SelectDeletionCandidates(views []ServerView, count int) []string` — wählt ausschließlich unbelegte Server, jüngste zuerst.
  - `controller.AggregateGroup(views []ServerView, generation int64) GroupTotals` mit `GroupTotals{Replicas, ReadyReplicas, OnlinePlayers, FreeSlots int32}`.
  - `controller.ServerGroupReconciler{Client, Scheme, Recorder, Agents, Clock}` mit `Reconcile` und `SetupWithManager`.
  - `controller.NewServerName(group string) string`.

Meilenstein 1 hält die Gruppe auf ihrer Untergrenze `scaling.minReplicas`. Slot-basiertes Hochskalieren, Rolling Updates und die Stabilisierungsfenster kommen in Meilenstein 4 — sie bauen auf `AggregateGroup` und `SelectDeletionCandidates` auf, die hier bereits die volle Invariante tragen.

- [ ] **Step 1: Den fehlschlagenden Test für die reine Auswahl schreiben**

`internal/controller/candidates_test.go`:

```go
package controller

import (
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

func view(name string, p phase.Phase, players, slots int32, stale bool, gen int64, ageSeconds int) ServerView {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	return ServerView{
		Name:       name,
		Phase:      p,
		Players:    players,
		Slots:      slots,
		Stale:      stale,
		Generation: gen,
		CreatedAt:  base.Add(time.Duration(ageSeconds) * time.Second),
	}
}

func TestSelectDeletionCandidates(t *testing.T) {
	cases := []struct {
		name  string
		views []ServerView
		count int
		want  []string
	}{
		{
			name: "picks the youngest empty server",
			views: []ServerView{
				view("old", phase.Ready, 0, 100, false, 1, 0),
				view("young", phase.Ready, 0, 100, false, 1, 60),
			},
			count: 1,
			want:  []string{"young"},
		},
		{
			name: "never picks an occupied server",
			views: []ServerView{
				view("busy", phase.Ready, 3, 100, false, 1, 60),
				view("empty", phase.Ready, 0, 100, false, 1, 0),
			},
			count: 1,
			want:  []string{"empty"},
		},
		{
			name: "treats a stale count as occupied",
			views: []ServerView{
				view("stale", phase.Ready, 0, 100, true, 1, 60),
				view("fresh", phase.Ready, 0, 100, false, 1, 0),
			},
			count: 1,
			want:  []string{"fresh"},
		},
		{
			name: "returns fewer than asked when nothing else is free",
			views: []ServerView{
				view("busy-a", phase.Ready, 1, 100, false, 1, 0),
				view("busy-b", phase.Ready, 2, 100, false, 1, 60),
			},
			count: 2,
			want:  nil,
		},
		{
			name: "prefers servers that never took players over ready ones",
			views: []ServerView{
				view("ready", phase.Ready, 0, 100, false, 1, 60),
				view("pending", phase.Pending, 0, 0, true, 1, 0),
			},
			count: 1,
			want:  []string{"pending"},
		},
		{
			name: "ignores servers that are already going away",
			views: []ServerView{
				view("draining", phase.Draining, 0, 100, false, 1, 0),
				view("terminating", phase.Terminating, 0, 100, false, 1, 0),
				view("ready", phase.Ready, 0, 100, false, 1, 60),
			},
			count: 3,
			want:  []string{"ready"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectDeletionCandidates(tc.views, tc.count)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestSelectNeverReturnsAnOccupiedServer is the core invariant at the
// selection layer: no combination of inputs may nominate a server carrying
// players, or one whose count we cannot trust.
func TestSelectNeverReturnsAnOccupiedServer(t *testing.T) {
	views := []ServerView{
		view("a", phase.Ready, 1, 100, false, 1, 0),
		view("b", phase.Ready, 0, 100, true, 1, 10),
		view("c", phase.Starting, 0, 100, true, 1, 20),
		view("d", phase.Ready, 99, 100, false, 1, 30),
	}
	occupied := map[string]bool{"a": true, "b": true, "d": true}

	for count := 0; count <= len(views)+2; count++ {
		for _, name := range SelectDeletionCandidates(views, count) {
			if occupied[name] {
				t.Errorf("count=%d nominated the occupied server %q", count, name)
			}
		}
	}
}

func TestAggregateGroup(t *testing.T) {
	views := []ServerView{
		view("current-a", phase.Ready, 20, 100, false, 7, 0),
		view("current-b", phase.Ready, 10, 100, false, 7, 10),
		view("stale-gen", phase.Ready, 40, 100, false, 6, 20),
		view("starting", phase.Starting, 0, 100, true, 7, 30),
		view("draining", phase.Draining, 5, 100, false, 7, 40),
	}

	got := AggregateGroup(views, 7)

	if got.Replicas != 5 {
		t.Errorf("Replicas = %d, want 5", got.Replicas)
	}
	if got.ReadyReplicas != 3 {
		t.Errorf("ReadyReplicas = %d, want 3 (two current plus the stale generation)", got.ReadyReplicas)
	}
	if got.OnlinePlayers != 75 {
		t.Errorf("OnlinePlayers = %d, want 75 across every server that has players", got.OnlinePlayers)
	}
	// 150 free on the two current-generation servers. The old generation does
	// not count, otherwise a rolling update would never create replacements.
	if got.FreeSlots != 150 {
		t.Errorf("FreeSlots = %d, want 150 from the current generation only", got.FreeSlots)
	}
}

func TestAggregateIgnoresStaleCountsForFreeSlots(t *testing.T) {
	views := []ServerView{
		view("fresh", phase.Ready, 0, 100, false, 7, 0),
		view("stale", phase.Ready, 0, 100, true, 7, 10),
	}
	if got := AggregateGroup(views, 7).FreeSlots; got != 100 {
		t.Errorf("FreeSlots = %d, want 100 — a server we cannot measure offers no free slots", got)
	}
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/controller/... -run 'TestSelect|TestAggregate' -v`
Expected: FAIL — `undefined: ServerView`, `undefined: SelectDeletionCandidates`.

- [ ] **Step 3: Reine Auswahl- und Aggregationslogik implementieren**

`internal/controller/candidates.go`:

```go
package controller

import (
	"sort"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// ServerView is everything the group logic needs about one server. It is a
// value type on purpose: the selection rules are pure and table-tested.
type ServerView struct {
	// Name of the Server object.
	Name string
	// Phase is its current state machine position.
	Phase phase.Phase
	// Players is the last reported count.
	Players int32
	// Slots is the reported capacity.
	Slots int32
	// Stale is true if the count cannot be trusted. Stale counts as occupied.
	Stale bool
	// Generation is the group generation this server was created from.
	Generation int64
	// CreatedAt is the creation timestamp of the Server object.
	CreatedAt time.Time
}

// Occupied reports whether the server must be treated as carrying players.
func (v ServerView) Occupied() bool {
	return v.Stale || v.Players > 0
}

// leaving reports whether the server is already on its way out, so the group
// must not count it as a candidate again.
func (v ServerView) leaving() bool {
	return v.Phase == phase.Draining || v.Phase == phase.Terminating
}

// tookPlayers reports whether the server was ever able to hold players. Only a
// Ready server is registered with the proxies.
func (v ServerView) tookPlayers() bool {
	return v.Phase == phase.Ready
}

// SelectDeletionCandidates nominates up to count servers for removal.
//
// It never nominates an occupied server, and a stale player count counts as
// occupied — one server too many beats one kicked player. Servers that never
// took players go first, then the youngest, so that long-lived sessions on
// older instances are disturbed last.
func SelectDeletionCandidates(views []ServerView, count int) []string {
	if count <= 0 {
		return nil
	}

	eligible := make([]ServerView, 0, len(views))
	for _, v := range views {
		if v.leaving() || v.Occupied() {
			continue
		}
		eligible = append(eligible, v)
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].tookPlayers() != eligible[j].tookPlayers() {
			return !eligible[i].tookPlayers()
		}
		if !eligible[i].CreatedAt.Equal(eligible[j].CreatedAt) {
			return eligible[i].CreatedAt.After(eligible[j].CreatedAt)
		}
		return eligible[i].Name < eligible[j].Name
	})

	if count > len(eligible) {
		count = len(eligible)
	}
	if count == 0 {
		return nil
	}

	names := make([]string, 0, count)
	for _, v := range eligible[:count] {
		names = append(names, v.Name)
	}
	return names
}

// GroupTotals is the aggregated status of a group.
type GroupTotals struct {
	// Replicas is the number of Server objects.
	Replicas int32
	// ReadyReplicas is how many are in phase Ready.
	ReadyReplicas int32
	// OnlinePlayers is the sum of players, whatever their generation.
	OnlinePlayers int32
	// FreeSlots counts only Ready servers of the current generation with a
	// fresh player count. Stale generations are excluded on purpose: without
	// that, a rolling update would never create replacements, because the old
	// servers' free slots would satisfy the scaler forever.
	FreeSlots int32
}

// AggregateGroup sums the views up for the group status.
func AggregateGroup(views []ServerView, generation int64) GroupTotals {
	var t GroupTotals
	for _, v := range views {
		t.Replicas++
		if v.Phase == phase.Ready {
			t.ReadyReplicas++
		}
		if !v.Stale {
			t.OnlinePlayers += v.Players
		}
		if v.Phase == phase.Ready && v.Generation == generation && !v.Stale {
			free := v.Slots - v.Players
			if free > 0 {
				t.FreeSlots += free
			}
		}
	}
	return t
}
```

- [ ] **Step 4: Test laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/controller/... -run 'TestSelect|TestAggregate' -v`
Expected: PASS.

Der Fall `draining` in `TestAggregateGroup` trägt fünf Spieler und ist nicht stale, zählt also in `OnlinePlayers` mit, aber nicht in `FreeSlots` — genau so ist es gewollt.

- [ ] **Step 5: Den fehlschlagenden Controller-Test schreiben**

`internal/controller/servergroup_controller_test.go`:

```go
package controller

import (
	"strings"
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// groupReconciler wires a ServerGroup reconciler onto an existing fixture.
func groupReconciler(f *fixture) *ServerGroupReconciler {
	return &ServerGroupReconciler{
		Client:   f.c,
		Scheme:   f.reconc.Scheme,
		Recorder: record.NewFakeRecorder(100),
		Agents:   f.agents,
		Clock:    f.clock.Now,
	}
}

func (f *fixture) reconcileGroup(t *testing.T, r *ServerGroupReconciler) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.group.Name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile group: %v", err)
	}
}

func (f *fixture) listServers(t *testing.T) []spawneryv1alpha1.Server {
	t.Helper()
	list := &spawneryv1alpha1.ServerList{}
	if err := f.c.List(f.ctx, list, ctrlclientInNamespace(f.ns)); err != nil {
		t.Fatalf("list servers: %v", err)
	}
	return list.Items
}

func TestGroupCreatesItsFloor(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want minReplicas = 1", len(servers))
	}
	srv := servers[0]
	if !strings.HasPrefix(srv.Name, "lobby-") {
		t.Errorf("server name = %q, want the group prefix", srv.Name)
	}
	if srv.Spec.GroupRef.Name != "lobby" {
		t.Errorf("groupRef = %q, want lobby", srv.Spec.GroupRef.Name)
	}
	if srv.Spec.GroupGeneration != f.group.Generation {
		t.Errorf("groupGeneration = %d, want %d", srv.Spec.GroupGeneration, f.group.Generation)
	}
	if len(srv.OwnerReferences) != 1 ||
		srv.OwnerReferences[0].Kind != "ServerGroup" ||
		srv.OwnerReferences[0].Controller == nil || !*srv.OwnerReferences[0].Controller {
		t.Errorf("owner references = %+v, want a ServerGroup controller ref", srv.OwnerReferences)
	}
}

func TestGroupScalesUpToTheFloor(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.group.Spec.Scaling.MinReplicas = 3
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 3 {
		t.Fatalf("got %d servers, want 3", got)
	}

	// Names must be unique, or the pods would collide.
	names := map[string]bool{}
	for _, s := range f.listServers(t) {
		if names[s.Name] {
			t.Fatalf("duplicate server name %q", s.Name)
		}
		names[s.Name] = true
	}
}

func TestGroupDeletesOnlyEmptySurplus(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.group.Spec.Scaling.MinReplicas = 2
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(servers))
	}

	// Give both a pod and make one of them busy.
	busy := servers[0].Name
	for _, s := range servers {
		f.reconcile(s.Name)
	}
	for _, s := range servers {
		pod, ok := f.pod(s.Name)
		if !ok {
			t.Fatalf("no pod for %s", s.Name)
		}
		f.setPodRunning(s.Name, true)
		f.agents.Connect(string(pod.UID), agentRoleServer())
		f.agents.MarkReady(string(pod.UID))
		players := int32(0)
		if s.Name == busy {
			players = 5
		}
		if err := f.agents.ReportPlayers(string(pod.UID), players, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcile(s.Name)
	}

	// Shrink the floor to 1 — exactly one server must go, and it must be the
	// empty one.
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MinReplicas = 1
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	for _, s := range f.listServers(t) {
		if s.Name == busy && !s.DeletionTimestamp.IsZero() {
			t.Fatal("the occupied server was marked for deletion — core invariant broken")
		}
	}
}

func TestGroupAggregatesStatus(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)

	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 12, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if group.Status.Replicas != 1 || group.Status.ReadyReplicas != 1 {
		t.Errorf("replicas = %d/%d, want 1/1", group.Status.ReadyReplicas, group.Status.Replicas)
	}
	if group.Status.OnlinePlayers != 12 {
		t.Errorf("onlinePlayers = %d, want 12", group.Status.OnlinePlayers)
	}
	if group.Status.FreeSlots != 88 {
		t.Errorf("freeSlots = %d, want 88", group.Status.FreeSlots)
	}
	if group.Status.Phase != string(phase.Ready) {
		t.Errorf("phase = %q, want Ready", group.Status.Phase)
	}
}

func TestGroupMaintainsAPodDisruptionBudget(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)

	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 4, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, r)

	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, pdb); err != nil {
		t.Fatalf("get PDB: %v", err)
	}
	if pdb.Spec.MaxUnavailable != nil {
		t.Error("maxUnavailable is not allowed for pods without a scale subresource")
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.Type != intstrInt {
		t.Fatalf("minAvailable = %+v, want an absolute integer", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %d, want 1 occupied pod", pdb.Spec.MinAvailable.IntValue())
	}
	if pdb.Spec.Selector.MatchLabels[podspec.LabelOccupied] != "true" {
		t.Errorf("selector = %v, want it to match the occupied label", pdb.Spec.Selector.MatchLabels)
	}
	if pdb.Spec.Selector.MatchLabels[podspec.LabelGroup] != "lobby" {
		t.Errorf("selector = %v, want it scoped to the group", pdb.Spec.Selector.MatchLabels)
	}
}

func TestGroupWithoutItsNetworkIsNotAccepted(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	orphan := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "nowhere", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "missing"},
			Type:       spawneryv1alpha1.ServerGroupEphemeral,
			Image:      "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers: 100,
			Scaling:    &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 10},
		},
	}
	if err := f.c.Create(f.ctx, orphan); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "nowhere", Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "nowhere", Namespace: f.ns}, got); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted, metav1.ConditionFalse, spawneryv1alpha1.ReasonNetworkNotFound) {
		t.Errorf("conditions = %+v, want Accepted=False/NetworkNotFound", got.Status.Conditions)
	}
	if len(f.listServers(t)) != 0 {
		t.Error("a group without a network must not create servers")
	}
}
```

Die kleinen Test-Helfer gehören nach `suite_test.go`:

```go
// ctrlclientInNamespace is a shorthand for the list option.
func ctrlclientInNamespace(ns string) client.ListOption { return client.InNamespace(ns) }

// agentRoleServer avoids importing the agent package into every test file.
func agentRoleServer() agent.Role { return agent.RoleServer }

// intstrInt is the IntOrString kind an absolute PDB value must have.
var intstrInt = intstr.Int

// hasCondition reports whether the list carries the given condition.
func hasCondition(conds []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == status && c.Reason == reason {
			return true
		}
	}
	return false
}

// bringUpNamed walks an already-created server into Ready.
func bringUpNamed(t *testing.T, f *fixture, name string) string {
	t.Helper()
	f.reconcile(name)
	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("no pod for %s", name)
	}
	uid := string(pod.UID)
	f.setPodRunning(name, true)
	f.agents.Connect(uid, agent.RoleServer)
	f.agents.MarkReady(uid)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(name)
	return uid
}
```

Dafür `"k8s.io/apimachinery/pkg/util/intstr"` in die Importe von `suite_test.go` aufnehmen. `bringUpReady` aus Task 8 wird zu `f.createServer(name)` gefolgt von `bringUpNamed(t, f, name)` vereinfacht, damit es nur eine Aufbaufunktion gibt.

- [ ] **Step 6: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/controller/... -run TestGroup -v`
Expected: FAIL — `undefined: ServerGroupReconciler`.

- [ ] **Step 7: ServerGroup-Controller implementieren**

`internal/controller/servergroup_controller.go`:

```go
package controller

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// nameSuffixAlphabet avoids characters that are easy to misread in a terminal.
const nameSuffixAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// NewServerName builds a unique ephemeral server name below the group prefix.
func NewServerName(group string) string {
	buf := make([]byte, 4)
	// crypto/rand.Read never fails on the platforms we support.
	_, _ = rand.Read(buf)
	suffix := make([]byte, len(buf))
	for i, b := range buf {
		suffix[i] = nameSuffixAlphabet[int(b)%len(nameSuffixAlphabet)]
	}
	return fmt.Sprintf("%s-%s", group, suffix)
}

// ServerGroupReconciler keeps a group at its desired size and publishes its
// aggregated status.
type ServerGroupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Agents is the runtime state reported by the in-game agents.
	Agents *agent.Registry
	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=servergroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

// Reconcile sizes the group and updates its status.
func (r *ServerGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// The Server objects are owned by the group; Kubernetes garbage
		// collection cascades, and each Server drains through its finalizer.
		return ctrl.Result{}, nil
	}

	network := &spawneryv1alpha1.Network{}
	networkKey := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: group.Namespace}
	if err := r.Get(ctx, networkKey, network); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNetworkNotFound,
			Message: fmt.Sprintf("network %q does not exist in this namespace", group.Spec.NetworkRef.Name),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, group)
	}

	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonAccepted,
		Message: fmt.Sprintf("managed as part of network %q", network.Name),
	})

	if !group.IsEphemeral() {
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNotImplemented,
			Message: "persistent groups arrive in milestone 5",
		})
		return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, group)
	}

	views, servers, err := r.collectViews(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}

	desired := group.DesiredReplicas()
	alive := int32(0)
	for _, v := range views {
		if v.Phase != phase.Draining && v.Phase != phase.Terminating {
			alive++
		}
	}

	switch {
	case alive < desired:
		for i := alive; i < desired; i++ {
			if err := r.createServer(ctx, group); err != nil {
				return ctrl.Result{}, err
			}
		}
	case alive > desired:
		surplus := int(alive - desired)
		names := SelectDeletionCandidates(views, surplus)
		if len(names) < surplus {
			logger.Info("fewer free servers than the surplus, trying again later",
				"group", group.Name, "surplus", surplus, "free", len(names))
		}
		for _, name := range names {
			if err := r.deleteServer(ctx, group, servers, name); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if err := r.reconcilePDB(ctx, group, views); err != nil {
		return ctrl.Result{}, err
	}

	totals := AggregateGroup(views, group.Generation)
	group.Status.Replicas = totals.Replicas
	group.Status.ReadyReplicas = totals.ReadyReplicas
	group.Status.OnlinePlayers = totals.OnlinePlayers
	group.Status.FreeSlots = totals.FreeSlots
	group.Status.ObservedGeneration = group.Generation
	group.Status.Phase = derivePhase(group, totals)

	return ctrl.Result{RequeueAfter: resyncInterval}, r.Status().Update(ctx, group)
}

// derivePhase maps the totals and conditions onto the group phase.
func derivePhase(group *spawneryv1alpha1.ServerGroup, totals GroupTotals) string {
	if meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		return "Degraded"
	}
	if totals.ReadyReplicas >= group.DesiredReplicas() && totals.ReadyReplicas > 0 {
		return string(phase.Ready)
	}
	return string(phase.Pending)
}

// collectViews reads every Server of the group plus its live player count.
func (r *ServerGroupReconciler) collectViews(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
) ([]ServerView, map[string]*spawneryv1alpha1.Server, error) {
	list := &spawneryv1alpha1.ServerList{}
	if err := r.List(ctx, list, client.InNamespace(group.Namespace)); err != nil {
		return nil, nil, err
	}

	views := make([]ServerView, 0, len(list.Items))
	byName := make(map[string]*spawneryv1alpha1.Server, len(list.Items))

	for i := range list.Items {
		srv := &list.Items[i]
		if srv.Spec.GroupRef.Name != group.Name {
			continue
		}
		byName[srv.Name] = srv

		// The live count comes from the registry, not from the throttled
		// status: the control loop must decide on fresh data.
		snap := r.Agents.Lookup(r.podUIDFor(ctx, srv))
		v := ServerView{
			Name:       srv.Name,
			Phase:      phase.Phase(srv.Status.Phase),
			Players:    snap.Players,
			Slots:      snap.Slots,
			Stale:      snap.PlayersStale,
			Generation: srv.Spec.GroupGeneration,
			CreatedAt:  srv.CreationTimestamp.Time,
		}
		if v.Phase == "" {
			v.Phase = phase.Pending
		}
		views = append(views, v)
	}
	return views, byName, nil
}

// podUIDFor resolves the registry key of a server. An unresolvable pod yields
// the empty key, whose snapshot is "unknown, therefore stale" — the
// conservative answer.
func (r *ServerGroupReconciler) podUIDFor(ctx context.Context, srv *spawneryv1alpha1.Server) string {
	if srv.Status.PodName == "" {
		return ""
	}
	pod := &corev1.Pod{}
	key := types.NamespacedName{Name: srv.Status.PodName, Namespace: srv.Namespace}
	if err := r.Get(ctx, key, pod); err != nil {
		return ""
	}
	return string(pod.UID)
}

func (r *ServerGroupReconciler) createServer(ctx context.Context, group *spawneryv1alpha1.ServerGroup) error {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NewServerName(group.Name),
			Namespace: group.Namespace,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   group.Spec.NetworkRef.Name,
				podspec.LabelGroup:     group.Name,
			},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: group.Name},
			GroupGeneration: group.Generation,
		},
	}
	if err := controllerutil.SetControllerReference(group, srv, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, srv); err != nil {
		return err
	}
	r.Recorder.Eventf(group, corev1.EventTypeNormal, "ServerCreated", "created server %s", srv.Name)
	return nil
}

func (r *ServerGroupReconciler) deleteServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	servers map[string]*spawneryv1alpha1.Server,
	name string,
) error {
	srv, ok := servers[name]
	if !ok {
		return nil
	}
	// Deleting the object is the request; the Server controller's finalizer
	// runs the drain before the object actually goes away.
	if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	r.Recorder.Eventf(group, corev1.EventTypeNormal, "ServerRemoved", "removing server %s", name)
	return nil
}

// reconcilePDB keeps the group's PodDisruptionBudget in step with the number
// of occupied pods.
//
// For pods without a controller carrying a scale subresource, Kubernetes
// allows neither maxUnavailable nor percentages in a PDB. The absolute number
// of occupied pods is the only formulation that works — and it makes the
// eviction API refuse to evict any of them.
func (r *ServerGroupReconciler) reconcilePDB(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
) error {
	occupied := 0
	for _, v := range views {
		if v.Occupied() && !v.leaving() {
			occupied++
		}
	}
	minAvailable := intstr.FromInt32(int32(occupied))

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: group.Name, Namespace: group.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.MinAvailable = &minAvailable
		pdb.Spec.MaxUnavailable = nil
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelGroup:     group.Name,
				podspec.LabelOccupied:  "true",
			},
		}
		return controllerutil.SetControllerReference(group, pdb, r.Scheme)
	})
	return err
}

// SetupWithManager registers the controller.
func (r *ServerGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.ServerGroup{}).
		Owns(&spawneryv1alpha1.Server{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("servergroup").
		Complete(r)
}
```

- [ ] **Step 8: Tests laufen lassen, Erfolg prüfen**

```bash
nix develop -c make manifests generate
nix develop -c go test ./internal/controller/... -v
```

Expected: PASS für alle Tests aus Task 8 und Task 9.

- [ ] **Step 9: Commit**

```bash
git add internal/controller config
git commit -m "ServerGroup-Controller mit Kandidatenauswahl und PodDisruptionBudget"
```

**Abweichungen vom obigen Code, wie umgesetzt:**

1. **Ein Failed-Server zählt nicht zur Gruppengröße.** Der Block oben zählt alles
   ausser `Draining` und `Terminating`. Ein `Failed`-Server ist von den Proxies
   abgemeldet und wird laut Task 8 eine Stunde lang (`failedRetentionSeconds`)
   zur Diagnose aufgehoben — die Gruppe stünde diese Stunde ohne einen Server da,
   den ein Spieler betreten kann. `ServerView.countsTowardSize()` schliesst
   deshalb `Failed` mit aus, die Gruppe legt sofort Ersatz an, und der
   Failed-Server bleibt bis zum Ablauf seiner Retention liegen.
   `SelectDeletionCandidates` nominiert ihn ebenfalls nie: ihn zu löschen würde
   die Gruppe nicht verkleinern.
2. **Belegung für die Auswahl und Belegung für das PDB sind zwei Fragen.**
   `Occupied()` bleibt die breite Regel `Stale || Players > 0` — exakt das, womit
   der Server-Controller `spawnery.cloud/occupied` setzt, und damit die richtige
   Zahl für `minAvailable`. Die Kandidatenauswahl fragt stattdessen
   `mayHavePlayers()`: ein veralteter Zählerstand verbirgt nur auf einem bei den
   Proxies registrierten Server Spieler. Sonst wäre jeder `Pending`-Server
   dauerhaft unlöschbar, und der Testfall „prefers servers that never took
   players over ready ones" aus Step 1 könnte nie erfüllt werden.
3. **Das PDB zählt auch die abfliessenden Server.** Der Block oben nimmt
   `!v.leaving()` in die Zählung auf. Ein `Draining`-Pod trägt das
   Occupied-Label aber weiter, bis der letzte Spieler weg ist; ihn nicht zu
   zählen setzt `minAvailable` unter die Zahl der vom Selektor getroffenen Pods,
   und genau diese Differenz ist eine Räumung, die die Eviction-API auf einem Pod
   mit Spielern erlauben würde. `occupiedPods()` zählt daher jede Phase.
4. **Eine fehlende Network blockiert nur, was von ihr abhängt.** Statt früh
   zurückzukehren setzt der Controller `Accepted=False/NetworkNotFound`,
   überspringt ausschliesslich das Anlegen von Servern und pflegt PDB und Status
   weiter. Sonst blieben ausgerechnet die Pods einer Gruppe, deren Network
   gelöscht wurde, ungeschützt. Gleiches gilt für persistente Gruppen.
5. **`FreeSlots` in `TestAggregateGroup` ist 170, nicht 150.** Die Fixture hat
   zwei Ready-Server der aktuellen Generation mit 100 Slots und 20 bzw. 10
   Spielern; 80 + 90 = 170. Die 150 im Block oben sind ein Rechenfehler, die
   Implementierung stimmt mit der Feldbeschreibung im CRD überein.
6. **`bringUpNamed` braucht drei Durchläufe, nicht zwei.** Der erste legt den Pod
   an, der zweite sieht ihn laufen und geht nach `Starting`, erst der dritte
   passiert das Ready-Gate. Mit der Fassung oben endete der Server in `Starting`,
   und `TestGroupAggregatesStatus` (`readyReplicas = 1`) hätte nie bestehen
   können.
5b. **`ServerView` trägt `WasRegistered` und `SessionsGone`.** Beides kommt aus
   dem Server-Status bzw. dem Pod und ersetzt die Phase als Behelfsantwort.
   `tookPlayers()` liest `WasRegistered` statt `Phase == Ready`: ein Server, der
   seine Bereitschaft verloren hat, steht in `Starting` und hat seine Spieler
   noch, weil das Abmelden nur neue Verbindungen stoppt. `mayHavePlayers()` ist
   `Players > 0 || (Stale && WasRegistered)`, `Occupied()` zusätzlich
   `&& !SessionsGone` — Letzteres spiegelt die Label-Regel aus dem Nachtrag zu
   Task 8 und gibt einen toten Pod aus dem Budget frei.
5c. **Obergrenze für aufgehobene Fehlschläge.** `maxRetainedFailures = 1` plus
   `selectFailedForPruning()`: die Gruppe behält den **ältesten** Fehlschlag —
   der erste nach einer Änderung ist der aussagekräftige — und löscht die
   übrigen. Ohne das erreicht ein kaputtes Image über `MaxContainerRestarts` und
   Kubelet-Backoff in ein bis zwei Minuten `Failed`, die Gruppe legt beim
   nächsten Fünf-Sekunden-Durchlauf Ersatz an, und über eine Retention-Stunde
   sammeln sich pro Sockel-Replika Dutzende Server samt Pods an. Exponentielles
   Backoff, eine `Degraded`-Bedingung mit `CrashLoopBackoff` und das Aufgeben
   nach wiederholten Fehlschlägen bleiben Meilenstein 4. `pruneFailed()` hängt
   nicht an der Network und läuft deshalb auch ohne sie.
5d. **`deleteServer` löscht nicht zweimal.** Ein Server behält seine Phase,
   während er abfliesst, kann also erneut nominiert werden; ohne die Prüfung auf
   den Deletion-Timestamp käme dasselbe Event jeden Resync neu.
7. **Zusätzliche Tests.** `TestOccupiedServerSurvivesAContinuousScaleDown` und
   `TestGroupHoldsItsFloorWithoutChurn` fahren die Invariante über 60 Durchläufe
   im `resyncInterval`-Takt statt in einem einzelnen Durchlauf;
   `TestGroupReplacesAFailedServer`, `TestPodDisruptionBudgetTracksThePlayerCount`,
   `TestGroupWithoutItsNetworkStillProtectsItsPlayers`, `TestCountsTowardSize` und
   `TestOccupiedPodsCountsEveryProtectedPod` sichern die Punkte 1 bis 4 ab.

---

### Task 10: Network-Controller

**Files:**
- Create: `internal/controller/network_controller.go`
- Test: `internal/controller/network_controller_test.go`

**Interfaces:**
- Consumes: `v1alpha1.Network`, `v1alpha1.ServerGroup`, `v1alpha1.ProxyGroup`, die Condition-Konstanten.
- Produces: `controller.NetworkReconciler{Client, Scheme, Recorder, Clock}` mit `Reconcile` und `SetupWithManager`.

Ein Namespace, ein Netzwerk. Der Grund ist die Netz-Isolation: NetworkPolicies selektieren über Labels, und ein zweites Netzwerk im selben Namespace unterläuft die Annahme. Der Gewinner ist das älteste `Network`; bei gleichem Zeitstempel entscheidet der Name, damit die Wahl deterministisch ist und nicht bei jedem Reconcile kippt.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/controller/network_controller_test.go`:

```go
package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func networkReconciler(f *fixture) *NetworkReconciler {
	return &NetworkReconciler{
		Client:   f.c,
		Scheme:   f.reconc.Scheme,
		Recorder: record.NewFakeRecorder(100),
		Clock:    f.clock.Now,
	}
}

func (f *fixture) reconcileNetwork(t *testing.T, r *NetworkReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile network %s: %v", name, err)
	}
}

func (f *fixture) network(t *testing.T, name string) *spawneryv1alpha1.Network {
	t.Helper()
	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network %s: %v", name, err)
	}
	return net
}

func TestFirstNetworkIsAccepted(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	f.reconcileNetwork(t, r, "production")

	got := f.network(t, "production")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Errorf("conditions = %+v, want Accepted=True", got.Status.Conditions)
	}
}

func TestSecondNetworkInTheSameNamespaceIsRejected(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	// The fixture's network already exists. Create a younger one.
	f.clock.Advance(time.Minute)
	second := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "other-secret"},
		},
	}
	if err := f.c.Create(f.ctx, second); err != nil {
		t.Fatalf("create second network: %v", err)
	}

	f.reconcileNetwork(t, r, "production")
	f.reconcileNetwork(t, r, "staging")

	if !hasCondition(f.network(t, "production").Status.Conditions,
		spawneryv1alpha1.ConditionAccepted, metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Error("the older network must stay accepted")
	}
	if !hasCondition(f.network(t, "staging").Status.Conditions,
		spawneryv1alpha1.ConditionAccepted, metav1.ConditionFalse, spawneryv1alpha1.ReasonDuplicateNetwork) {
		t.Errorf("conditions = %+v, want Accepted=False/DuplicateNetwork",
			f.network(t, "staging").Status.Conditions)
	}
}

func TestNetworkCountsItsGroups(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	gr := groupReconciler(f)

	f.reconcileGroup(t, gr)
	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, gr)
	f.reconcileNetwork(t, r, "production")

	got := f.network(t, "production")
	if got.Status.ServerGroups != 1 {
		t.Errorf("serverGroups = %d, want 1", got.Status.ServerGroups)
	}
	if got.Status.ProxyGroups != 0 {
		t.Errorf("proxyGroups = %d, want 0", got.Status.ProxyGroups)
	}
	if got.Status.OnlinePlayers != 9 {
		t.Errorf("onlinePlayers = %d, want 9", got.Status.OnlinePlayers)
	}
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/controller/... -run TestNetwork -v` (bzw. `TestFirstNetwork`, `TestSecondNetwork`)
Expected: FAIL — `undefined: NetworkReconciler`.

- [ ] **Step 3: Implementieren**

`internal/controller/network_controller.go`:

```go
package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// NetworkReconciler enforces one network per namespace and publishes the
// aggregated network status.
type NetworkReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=networks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups,verbs=get;list;watch

// Reconcile decides whether this network is the one that owns its namespace
// and, if so, sums up its groups.
func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	network := &spawneryv1alpha1.Network{}
	if err := r.Get(ctx, req.NamespacedName, network); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !network.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	owner, err := r.namespaceOwner(ctx, network.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if owner != network.Name {
		meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
			Type:   spawneryv1alpha1.ConditionAccepted,
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonDuplicateNetwork,
			Message: fmt.Sprintf(
				"namespace %q is already served by network %q; put staging and production in separate namespaces",
				network.Namespace, owner),
		})
		return ctrl.Result{RequeueAfter: time.Minute}, r.Status().Update(ctx, network)
	}

	meta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonAccepted,
		Message: "this network owns its namespace",
	})

	serverGroups := &spawneryv1alpha1.ServerGroupList{}
	if err := r.List(ctx, serverGroups, client.InNamespace(network.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	proxyGroups := &spawneryv1alpha1.ProxyGroupList{}
	if err := r.List(ctx, proxyGroups, client.InNamespace(network.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	var serverGroupCount, players int32
	for _, g := range serverGroups.Items {
		if g.Spec.NetworkRef.Name != network.Name {
			continue
		}
		serverGroupCount++
		players += g.Status.OnlinePlayers
	}
	var proxyGroupCount int32
	for _, g := range proxyGroups.Items {
		if g.Spec.NetworkRef.Name == network.Name {
			proxyGroupCount++
		}
	}

	network.Status.ServerGroups = serverGroupCount
	network.Status.ProxyGroups = proxyGroupCount
	network.Status.OnlinePlayers = players

	return ctrl.Result{RequeueAfter: resyncInterval}, r.Status().Update(ctx, network)
}

// namespaceOwner picks the network that owns the namespace: the oldest one,
// with the name as the tiebreaker so the choice is stable across reconciles.
func (r *NetworkReconciler) namespaceOwner(ctx context.Context, namespace string) (string, error) {
	list := &spawneryv1alpha1.NetworkList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", err
	}

	owner := ""
	var ownerCreated metav1.Time
	for i := range list.Items {
		n := &list.Items[i]
		if !n.DeletionTimestamp.IsZero() {
			continue
		}
		switch {
		case owner == "",
			n.CreationTimestamp.Before(&ownerCreated),
			n.CreationTimestamp.Equal(&ownerCreated) && n.Name < owner:
			owner, ownerCreated = n.Name, n.CreationTimestamp
		}
	}
	return owner, nil
}

// SetupWithManager registers the controller.
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.Network{}).
		Owns(&spawneryv1alpha1.ServerGroup{}).
		Named("network").
		Complete(r)
}
```

- [ ] **Step 4: Tests laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/controller/... -v`
Expected: PASS.

Falls `TestSecondNetworkInTheSameNamespaceIsRejected` scheitert, weil beide Netzwerke denselben `creationTimestamp` auf Sekundengenauigkeit tragen: das ist gewollt abgedeckt, der Namensvergleich entscheidet dann — `production` gewinnt gegen `staging`.

- [ ] **Step 5: Commit**

```bash
git add internal/controller config
git commit -m "Network-Controller mit Ein-Netzwerk-pro-Namespace-Regel"
```

---

### Task 11: Verwaisten-Abgleich

**Files:**
- Create: `internal/controller/orphan.go`
- Test: `internal/controller/orphan_test.go`

**Interfaces:**
- Consumes: `podspec.Label*`, alle API-Typen.
- Produces: `controller.OrphanReconciler{Client, Recorder, Agents, Interval}` mit `Sweep(ctx context.Context) error` und `Start(ctx context.Context) error` (erfüllt `manager.Runnable`).

Jeder erzeugte Pod trägt Owner-Reference und Labels. Der periodische Abgleich korrigiert beide Richtungen und ist die Antwort auf Node-Ausfälle, verpasste Events und einen Operator, der während einer Löschung neu startete.

Drei Fälle:

1. Ein verwalteter Pod ohne zugehörigen `Server` wird gelöscht.
2. Ein `Server`, dessen `ServerGroup` nicht mehr existiert, wird gelöscht.
3. Agents in der Registry, deren Pod verschwunden ist, werden vergessen, damit die Map nicht unbegrenzt wächst.

Der Fall „Server ohne Pod" braucht hier nichts: die Zustandsmaschine erkennt ihn über `PodLost` und beendet den Server, woraufhin die Gruppe Ersatz erzeugt.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/controller/orphan_test.go`:

```go
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

func orphanReconciler(f *fixture) *OrphanReconciler {
	return &OrphanReconciler{
		Client:   f.c,
		Recorder: record.NewFakeRecorder(100),
		Agents:   f.agents,
	}
}

func TestSweepDeletesAPodWithoutItsServer(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	stray := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-ghost",
			Namespace: f.ns,
			Labels:    podspec.ServerLabels("production", "lobby", "lobby-ghost"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "minecraft", Image: "paper"}},
		},
	}
	if err := f.c.Create(f.ctx, stray); err != nil {
		t.Fatalf("create stray pod: %v", err)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-ghost", Namespace: f.ns}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("stray pod survived the sweep: %v", err)
	}
}

func TestSweepKeepsAPodThatHasItsServer(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	f.createServer("lobby-x7k2")
	f.reconcile("lobby-x7k2")

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, ok := f.pod("lobby-x7k2"); !ok {
		t.Fatal("the sweep deleted a pod that has its Server")
	}
}

func TestSweepIgnoresForeignPods(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "someone-elses-pod",
			Namespace: f.ns,
			Labels:    map[string]string{"app": "postgres"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "db", Image: "postgres"}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create foreign pod: %v", err)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "someone-elses-pod", Namespace: f.ns}, &corev1.Pod{}); err != nil {
		t.Fatalf("the sweep touched a pod it does not manage: %v", err)
	}
}

func TestSweepDeletesAServerWithoutItsGroup(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "gone-x1", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "does-not-exist"},
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got := &spawneryv1alpha1.Server{}
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "gone-x1", Namespace: f.ns}, got)
	if err == nil && got.DeletionTimestamp.IsZero() {
		t.Fatal("the server of a deleted group was not removed")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get server: %v", err)
	}
}

func TestSweepForgetsAgentsOfVanishedPods(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)

	f.agents.Connect("pod-uid-that-never-existed", agent.RoleServer)
	if !f.agents.Lookup("pod-uid-that-never-existed").Known {
		t.Fatal("precondition: the agent must be known")
	}

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if f.agents.Lookup("pod-uid-that-never-existed").Known {
		t.Error("the registry still knows an agent whose pod does not exist")
	}
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/controller/... -run TestSweep -v`
Expected: FAIL — `undefined: OrphanReconciler`.

- [ ] **Step 3: Implementieren**

Die Registry braucht dafür eine Methode, die alle bekannten Schlüssel liefert. In `internal/agent/registry.go` ergänzen:

```go
// Keys returns every pod key the registry currently knows.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0, len(r.entries))
	for k := range r.entries {
		keys = append(keys, k)
	}
	return keys
}
```

und in `internal/agent/registry_test.go` dazu:

```go
func TestKeysListsEveryKnownPod(t *testing.T) {
	r, _ := newTestRegistry()
	r.Connect("a", RoleServer)
	r.Connect("b", RoleProxy)
	r.Disconnect("b")

	keys := r.Keys()
	if len(keys) != 2 {
		t.Fatalf("Keys() = %v, want both a and b — a disconnected agent is still known", keys)
	}
}
```

`internal/controller/orphan.go`:

```go
package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

// DefaultOrphanInterval is how often the sweep runs.
const DefaultOrphanInterval = time.Minute

// OrphanReconciler reconciles the two directions no watch covers: a managed
// pod whose Server is gone, and a Server whose group is gone. It also drops
// registry entries of pods that no longer exist, so the map stays bounded.
//
// It is a manager Runnable rather than a controller, because it is a periodic
// full comparison and not a reaction to an event.
type OrphanReconciler struct {
	client.Client
	Recorder record.EventRecorder

	// Agents is the runtime state to be pruned.
	Agents *agent.Registry
	// Interval is how often Start runs the sweep. Zero means the default.
	Interval time.Duration
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=list;delete

// Start runs the sweep until the context is cancelled. It implements
// manager.Runnable.
func (r *OrphanReconciler) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultOrphanInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Sweep(ctx); err != nil {
				log.FromContext(ctx).Error(err, "orphan sweep failed")
			}
		}
	}
}

// Sweep runs one full comparison.
func (r *OrphanReconciler) Sweep(ctx context.Context) error {
	logger := log.FromContext(ctx)

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleServer,
	}); err != nil {
		return err
	}

	liveUIDs := make(map[string]bool, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		liveUIDs[string(pod.UID)] = true

		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		serverName := pod.Labels[podspec.LabelServer]
		if serverName == "" {
			continue
		}

		srv := &spawneryv1alpha1.Server{}
		key := types.NamespacedName{Name: serverName, Namespace: pod.Namespace}
		err := r.Get(ctx, key, srv)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		// No Server object: nobody owns this pod, so nobody would ever drain
		// it. Deleting it is the only way the count converges.
		logger.Info("deleting a managed pod whose Server is gone", "pod", pod.Name, "namespace", pod.Namespace)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	servers := &spawneryv1alpha1.ServerList{}
	if err := r.List(ctx, servers); err != nil {
		return err
	}
	for i := range servers.Items {
		srv := &servers.Items[i]
		if !srv.DeletionTimestamp.IsZero() {
			continue
		}

		group := &spawneryv1alpha1.ServerGroup{}
		key := types.NamespacedName{Name: srv.Spec.GroupRef.Name, Namespace: srv.Namespace}
		err := r.Get(ctx, key, group)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}

		logger.Info("deleting a Server whose group is gone", "server", srv.Name, "namespace", srv.Namespace)
		if err := r.Delete(ctx, srv); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	for _, key := range r.Agents.Keys() {
		if !liveUIDs[key] {
			r.Agents.Forget(key)
		}
	}

	return nil
}
```

Der Aufruf `r.Agents.Forget` für Proxy-Agents wäre falsch, sobald Meilenstein 3 Proxy-Pods erzeugt: die Pod-Liste filtert auf `role=server`. Meilenstein 3 erweitert den Filter auf beide Rollen, indem der Label-Filter auf `LabelManagedBy` allein reduziert und die Server-Prüfung auf `role=server` beschränkt wird. Bis dahin dürfen keine Proxy-Agents in der Registry stehen.

- [ ] **Step 4: Tests laufen lassen, Erfolg prüfen**

```bash
nix develop -c make manifests generate
nix develop -c go test ./internal/agent/... ./internal/controller/... -v
```

Expected: PASS, inklusive `TestKeysListsEveryKnownPod` und der fünf Sweep-Tests.

- [ ] **Step 5: Commit**

```bash
git add internal/agent internal/controller config
git commit -m "Verwaisten-Abgleich für Pods, Server und Registry-Einträge"
```

---

### Task 12: Manager-Verdrahtung, RBAC und Beispiel-Manifest

**Files:**
- Create: `internal/controller/setup.go`
- Create: `cmd/spawnery-operator/main.go`
- Create: `config/samples/network.yaml`
- Test: `internal/controller/setup_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `NetworkReconciler`, `ServerGroupReconciler`, `ServerReconciler`, `OrphanReconciler`, `agent.Registry`.
- Produces:
  - `controller.Options{Agents *agent.Registry, Clock func() time.Time, StartupDeadline, PlayerStatusInterval, OrphanInterval time.Duration, Registrar Registrar}`.
  - `controller.SetupAll(mgr ctrl.Manager, opts Options) error` — registriert alle Controller und den Verwaisten-Abgleich.
  - Das Binary `bin/spawnery-operator`.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/controller/setup_test.go`:

```go
package controller

import (
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestSetupAllRegistersEveryController(t *testing.T) {
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}

	mgr, err := ctrl.NewManager(testenv.Config(t), manager.Options{
		Scheme:         testenv.Scheme(t),
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	opts := Options{
		Agents:               agent.New(clock.Now, 5*time.Second, start),
		Clock:                clock.Now,
		StartupDeadline:      5 * time.Minute,
		PlayerStatusInterval: 30 * time.Second,
		OrphanInterval:       time.Minute,
		Registrar:            NoopRegistrar{},
	}
	if err := SetupAll(mgr, opts); err != nil {
		t.Fatalf("SetupAll: %v", err)
	}

	// Registering the same controllers twice must fail: controller-runtime
	// rejects duplicate names. That proves SetupAll really registered them.
	if err := SetupAll(mgr, opts); err == nil {
		t.Fatal("SetupAll succeeded twice, so it registered nothing the first time")
	}
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/controller/... -run TestSetupAll -v`
Expected: FAIL — `undefined: Options`, `undefined: SetupAll`.

- [ ] **Step 3: Verdrahtung implementieren**

`internal/controller/setup.go`:

```go
package controller

import (
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/spawnery/spawnery/internal/agent"
)

// Options are the knobs the operator binary passes to the controllers.
type Options struct {
	// Agents is the shared runtime state of all connected agents.
	Agents *agent.Registry
	// Clock is the time source. Injectable for tests.
	Clock func() time.Time
	// StartupDeadline is how long a server may take to reach Ready.
	StartupDeadline time.Duration
	// PlayerStatusInterval throttles player-count writes into etcd.
	PlayerStatusInterval time.Duration
	// OrphanInterval is how often the orphan sweep runs.
	OrphanInterval time.Duration
	// Registrar reaches the proxies. Milestone 1 wires the no-op.
	Registrar Registrar
}

// SetupAll registers every controller and the orphan sweep with the manager.
func SetupAll(mgr ctrl.Manager, opts Options) error {
	if err := (&NetworkReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("network"),
		Clock:    opts.Clock,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup network controller: %w", err)
	}

	if err := (&ServerGroupReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("servergroup"),
		Agents:   opts.Agents,
		Clock:    opts.Clock,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server group controller: %w", err)
	}

	if err := (&ServerReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Recorder:             mgr.GetEventRecorderFor("server"),
		Agents:               opts.Agents,
		Clock:                opts.Clock,
		StartupDeadline:      opts.StartupDeadline,
		PlayerStatusInterval: opts.PlayerStatusInterval,
		Registrar:            opts.Registrar,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server controller: %w", err)
	}

	if err := mgr.Add(&OrphanReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("orphan"),
		Agents:   opts.Agents,
		Interval: opts.OrphanInterval,
	}); err != nil {
		return fmt.Errorf("add orphan sweep: %w", err)
	}

	return nil
}
```

- [ ] **Step 4: Test laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/controller/... -run TestSetupAll -v`
Expected: PASS.

- [ ] **Step 5: Operator-Binary schreiben**

`cmd/spawnery-operator/main.go`:

```go
// Command spawnery-operator runs the Spawnery controllers.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/version"
)

var scheme = runtime.NewScheme()

func init() {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		leaderElect          bool
		watchNamespace       string
		reportInterval       time.Duration
		startupDeadline      time.Duration
		playerStatusInterval time.Duration
		orphanInterval       time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"run leader election; on from the start so extra replicas are not an architecture change later")
	flag.StringVar(&watchNamespace, "namespace", "",
		"namespace to watch; empty means all namespaces")
	flag.DurationVar(&reportInterval, "report-interval", 5*time.Second,
		"how often agents report; a count older than twice this counts as stale")
	flag.DurationVar(&startupDeadline, "startup-deadline", 5*time.Minute,
		"how long a server may take to reach Ready before it counts as failed")
	flag.DurationVar(&playerStatusInterval, "player-status-interval", 30*time.Second,
		"how often unchanged player counts are written into the CR status")
	flag.DurationVar(&orphanInterval, "orphan-interval", controller.DefaultOrphanInterval,
		"how often the orphan sweep runs")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting spawnery-operator", "version", version.Version)

	mgrOptions := manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "spawnery-operator.spawnery.cloud",
	}
	if watchNamespace != "" {
		mgrOptions.Cache.DefaultNamespaces = map[string]cache.Config{watchNamespace: {}}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	started := time.Now()
	registry := agent.New(time.Now, reportInterval, started)

	if err := controller.SetupAll(mgr, controller.Options{
		Agents:               registry,
		Clock:                time.Now,
		StartupDeadline:      startupDeadline,
		PlayerStatusInterval: playerStatusInterval,
		OrphanInterval:       orphanInterval,
		// Milestone 3 replaces this with the proxy broadcast.
		Registrar: controller.NoopRegistrar{},
	}); err != nil {
		setupLog.Error(err, "unable to set up controllers")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with an error")
		os.Exit(1)
	}
}
```

`"sigs.k8s.io/controller-runtime/pkg/cache"` gehört in die Importe, weil `mgrOptions.Cache.DefaultNamespaces` den Typ `cache.Config` verwendet.

- [ ] **Step 6: Binary bauen und Hilfe prüfen**

```bash
nix develop -c make build
nix develop -c ./bin/spawnery-operator --help
```

Expected: Der Aufruf listet alle acht Flags und beendet sich mit Status 0 (bzw. 2, wie Go's `flag` es bei `--help` tut). Kein Panic.

- [ ] **Step 7: Beispiel-Manifest schreiben**

`config/samples/network.yaml`:

```yaml
# A minimal Spawnery network: one lobby group of ephemeral Paper servers.
# The proxy layer arrives in milestone 3 — until then the servers come up and
# reach phase Starting, and the ready gate closes once the agent of milestone 2
# reports in.
apiVersion: v1
kind: Namespace
metadata:
  name: minecraft
---
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  # Replace this before you expose anything: it authenticates the proxy to the
  # backends. Generate one with: head -c 32 /dev/urandom | base64
  secret: change-me
---
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret
  defaults:
    minecraftVersion: "1.21.4"
    resources:
      requests:
        cpu: "1"
        memory: 2Gi
      limits:
        memory: 2Gi
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Ephemeral
  image: ghcr.io/spawnery/paper:1.21.4-0.1.0
  maxPlayers: 100
  drain:
    timeoutSeconds: 60
  scaling:
    minReplicas: 1
    maxReplicas: 10
    spareSlots: 40
```

- [ ] **Step 8: Gegen einen echten Cluster prüfen**

```bash
nix develop -c k3d cluster create spawnery-dev --agents 1
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false &
sleep 20
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

Expected:
- `network production` mit `Accepted=True`,
- `servergroup lobby` mit `REPLICAS 1`,
- ein `server lobby-xxxx` in Phase `Pending` oder `Starting`,
- ein Pod `lobby-xxxx`, der das Image nicht ziehen kann (`ErrImagePull`) — das Basis-Image entsteht erst in Meilenstein 2. Genau das ist der erwartete Endstand von Meilenstein 1.

Danach aufräumen:

```bash
kill %1
nix develop -c k3d cluster delete spawnery-dev
```

- [ ] **Step 9: README aktualisieren**

Den Status-Abschnitt in `README.md` ersetzen:

```markdown
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
```

- [ ] **Step 10: Vollständigen Testlauf und Commit**

```bash
nix develop -c make all
git add cmd internal config README.md
git commit -m "Manager-Verdrahtung, Operator-Binary und Beispiel-Manifest"
```

Expected: `make all` läuft `manifests generate fmt vet test build` fehlerfrei durch.

---

## Abnahme von Meilenstein 1

Nach Task 12 muss Folgendes gelten — das ist die Prüfliste, gegen die der Meilenstein abgenommen wird:

- [ ] `nix develop -c make all` ist grün.
- [ ] `kubectl apply -f config/crd/bases` installiert vier CRDs: `networks`, `proxygroups`, `servergroups`, `servers` in der Gruppe `spawnery.cloud`.
- [ ] Eine `ServerGroup` vom Typ `Ephemeral` erzeugt `scaling.minReplicas` viele `Server`-Objekte und je einen Pod.
- [ ] Ein `Server` durchläuft `Pending → Starting` und erreicht `Ready` erst, wenn Readiness-Probe **und** Agent-Bereitschaft vorliegen.
- [ ] Der Verlust eines der beiden Signale führt sofort zurück nach `Starting` samt Deregistrierung; nach `phase.MaxReadinessLosses` Verlusten nach `Failed`.
- [ ] Das Löschen eines belegten `Server` löscht den Pod nicht, bevor er leer ist oder `drain.timeoutSeconds` abläuft.
- [ ] Das PodDisruptionBudget der Gruppe trägt eine absolute `minAvailable`-Zahl in Höhe der belegten Pods und selektiert auf `spawnery.cloud/occupied=true`.
- [ ] Ein verwalteter Pod ohne `Server`-Objekt und ein `Server` ohne `ServerGroup` verschwinden beim Verwaisten-Abgleich.
- [ ] Ein zweites `Network` im selben Namespace bekommt `Accepted=False` mit Reason `DuplicateNetwork`.
- [ ] CEL lehnt `storage` bei `Ephemeral`, `scaling` bei `Persistent`, einen Typwechsel und ein Schrumpfen von `storage.size` ab.

## Was Meilenstein 1 bewusst offenlässt

Damit beim Review niemand nach Fehlendem sucht, das keiner vergessen hat:

- **Basis-Images und gRPC-Dienst** (Meilenstein 2). Die `agent.Registry` ist der Port dafür; in Meilenstein 1 schreibt nur der Test hinein. `NoopRegistrar` ist der zweite Port.
- **Proxy-Schicht** (Meilenstein 3). Das `ProxyGroup`-CRD existiert bereits, sein Controller nicht.
- **Slot-basiertes Hochskalieren und Rolling Updates** (Meilenstein 4). `AggregateGroup` liefert `FreeSlots` schon generationsbewusst, `SelectDeletionCandidates` trägt bereits die Invariante — Meilenstein 4 ruft beides mit anderen Zielgrößen auf.
- **Persistente Gruppen** (Meilenstein 5). Das CRD samt CEL-Regeln steht, der `podspec` legt das PVC-Volume schon an; der Controller lehnt persistente Gruppen bislang mit einer Condition ab.
- **Expose-Strategien, NetworkPolicies, Helm-Chart, RKE2-E2E** (Meilenstein 6). Meilenstein 1 wird lokal mit `go run` gegen k3d betrieben.
- **Dedizierte ServiceAccounts und projizierte Token** (Meilenstein 2). Die Pods laufen mit `automountServiceAccountToken: false` und tragen damit heute schon keine Kubernetes-Credentials.
- **Node-Drain-Erkennung** (Meilenstein 4), weil sie den Drain-Pfad aus 6.2 voraussetzt, den erst die Proxy-Schicht vollständig macht.
