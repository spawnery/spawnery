# Voice Chat Relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Simple Voice Chat works on a spawnery network with any number of proxy replicas and with every expose strategy, because spawnery's Velocity agent relays voice UDP itself and any proxy can route any player.

**Architecture:** `Network.spec.voice` switches voice on. The operator adds a UDP port (25565) to every proxy pod and to the way out the group already has (or to a `<group>-voice` Service / hostPort when `spec.voice.expose` overrides it), opens UDP `backendPort` between proxies and backends in the NetworkPolicies, computes a voice endpoint per group, and sends it to the proxies as a `VoiceConfig` message. The Velocity agent runs a UDP relay that reads the cleartext UUID of each SVC packet, resolves the player's backend (Velocity first, then the `NetworkState` mirror), bridges through one socket per player, and rewrites `serverPort`/`voiceHost` in the backend's `voicechat:secret` plugin message.

**Tech Stack:** Go (controller-runtime, envtest), protobuf/gRPC, Kotlin on Velocity (java.nio `DatagramChannel` + `Selector`), JUnit 5, bash harness `hack/agent-test.sh` with podman.

**Spec:** `docs/superpowers/specs/2026-10-12-voice-chat-relay-design.md`

## Global Constraints

- All new text in files is English. Copyright header on new files: `Copyright paul_wtf.` (copy the Apache header from a neighbouring file).
- Comments: default none. Only what a careful reader cannot derive from the code beside it (foreign-system behaviour, an absence, an arbitrary-looking number, why the obvious way was not taken). No fix histories.
- Every Go/Make command runs in the devshell: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c <cmd>`. Never `cd` before `nix develop`. Below this is abbreviated `$NIX`.
- Machine: check `hostname` first. On `dev` (2 CPUs, 12 GB) run envtest packages with `-p 1` and one package at a time; `internal/controller` takes ~85 s. On `paul-desktop` no throttling.
- Nix builds read the git index: `git add` every new file before `make agent` or any image build.
- Generated files are committed: after API/marker/proto changes run `$NIX make manifests generate proto` and commit `config/crd/bases/`, `config/rbac/role.yaml`, `charts/spawnery/templates/{crds,rbac}.yaml`, `zz_generated.deepcopy.go`, `internal/agentpb/`, `agent/common/src/proto/java/`, `docs/reference/{crds,chart-values,metrics-and-alerts}.md`.
- Commits: Conventional Commits with scope (`feat(api): …`), body wrapped at 72, ending with
  `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_014okRcEmshaQbMTqmuHRDiH`. Commits are gpg-signed; on `dev` probe first with `echo probe | gpg --clearsign --pinentry-mode error -u "$(git config user.signingkey)" >/dev/null` and ask the user to unlock if it fails. Never disable signing.
- Exact values from the spec: backend port default `24454`; relay port inside the pod `25565/UDP`, container port name `voice`; client-address takeover after `3 s` of silence; bridge idle removal after `15 s`; secret layout of compatibility version `20`: `serverPort` int at offset 16, `voiceHost` VarInt-prefixed UTF-8 at offset 54, then one `allowRecording` byte; UDP frame `0xFF` + 16-byte UUID (msb, lsb big-endian) + payload; minimum frame length 17.
- Metric: `spawnery_voice_packets_total{group,reason}`, reasons exactly `relayed`, `not_svc`, `unknown_player`, `unknown_server`, `unknown_version`, `ping`. No player label.
- No change to the proxy hash golden in `internal/podspec/hash_golden_test.go` for a group in a network without voice. Backend pods do not change at all.
- SVC jars are never added to any image or test fixture (licence "All Rights Reserved").

## Decisions taken while planning (differ from the spec's wording)

1. The voice problems go on a new condition type `Voice`, not on `Degraded`: `reportBlockedProxies` writes `Degraded` every pass (`ProxyPodsAdmitted`) and a second writer would flap it. `setStatus` turns the phase `Degraded` when `Voice` is `False`, which is what the spec asked for. A third reason, `VoiceServiceNotOwnedByGroup`, covers a foreign Service at `<group>-voice`.
2. Ping packets (the fixed UUID) are dropped and counted as `ping` rather than answered: the reply format is not in the spec and not verified. Task 13 amends the spec.
3. `make e2e` starts no game image (its manifests use `e2e-no-such-tag`), so the cross-proxy proof runs in `hack/agent-test.sh` against a real Velocity image and `spawnery-stubop`, and the server-switch propagation time is measured on a real cluster. Task 13 amends spec §7.

## Review Focus

1. A network with voice off: no proxy rolls on operator upgrade, no Service gains a port, no policy changes. Pinned in Tasks 3, 4, 5 by explicit "voice off renders exactly what it rendered before" tests.
2. Switching `spec.voice.expose` on, off, and between types: the `<group>-voice` Service is created, retyped, and deleted only when it is ours. Pinned in Task 5.
3. A garbage UDP flood on 25565 (short packets, wrong magic, random UUIDs): no bridge is created, nothing is resolved, counters rise. Pinned in Task 10.
4. A backend secret packet that is truncated or has a longer tail (a future SVC): passed through untouched, never a half-patched packet. Pinned in Task 9.
5. An agent that reports invented counter reasons or a decreasing total (restart): unknown reasons are discarded, a decrease is read as a restart, never a negative increment. Pinned in Task 7.

---

### Task 1: API fields, reasons, condition

**Files:**
- Modify: `api/v1alpha1/network_types.go`
- Modify: `api/v1alpha1/proxygroup_types.go`
- Modify: `api/v1alpha1/common_types.go`
- Create: `api/v1alpha1/voice_test.go`
- Regenerate: everything `make manifests generate` writes

**Interfaces:**
- Produces:
  - `type NetworkVoiceSpec struct { BackendPort int32 }`, field `NetworkSpec.Voice *NetworkVoiceSpec`
  - `const DefaultVoiceBackendPort int32 = 24454`
  - `func (n *Network) VoiceBackendPort() (int32, bool)` — `(0,false)` when voice is off
  - `type ProxyVoiceSpec struct { Expose *ExposeSpec }`, field `ProxyGroupSpec.Voice *ProxyVoiceSpec`
  - `ProxyGroupStatus.VoiceAddress string`
  - `func (g *ProxyGroup) VoiceExpose() (ExposeSpec, bool)` — the override and `true`, or the game's `spec.expose` and `false`
  - `ConditionVoice = "Voice"`; reasons `ReasonVoiceRelaying = "Relaying"`, `ReasonVoiceOffInNetwork = "VoiceOffInNetwork"`, `ReasonVoicePluginConflict = "VoicePluginConflict"`, `ReasonVoiceServiceNotOurs = "VoiceServiceNotOwnedByGroup"`

- [ ] **Step 1: Write the failing test**

`api/v1alpha1/voice_test.go` (plain unit test, no envtest):

```go
package v1alpha1

import "testing"

func TestVoiceBackendPortIsOffWithoutTheBlock(t *testing.T) {
	n := &Network{}
	if port, on := n.VoiceBackendPort(); on || port != 0 {
		t.Fatalf("VoiceBackendPort() = %d, %v; want 0, false", port, on)
	}
}

func TestVoiceBackendPortDefaultsForAnObjectThatSkippedDefaulting(t *testing.T) {
	n := &Network{Spec: NetworkSpec{Voice: &NetworkVoiceSpec{}}}
	if port, on := n.VoiceBackendPort(); !on || port != DefaultVoiceBackendPort {
		t.Fatalf("VoiceBackendPort() = %d, %v; want %d, true", port, on, DefaultVoiceBackendPort)
	}
}

func TestVoiceBackendPortReadsTheSpec(t *testing.T) {
	n := &Network{Spec: NetworkSpec{Voice: &NetworkVoiceSpec{BackendPort: 24500}}}
	if port, on := n.VoiceBackendPort(); !on || port != 24500 {
		t.Fatalf("VoiceBackendPort() = %d, %v; want 24500, true", port, on)
	}
}

func TestVoiceExposeFollowsTheGameWithoutAnOverride(t *testing.T) {
	g := &ProxyGroup{Spec: ProxyGroupSpec{Expose: ExposeSpec{Type: ExposeLoadBalancer}}}
	got, own := g.VoiceExpose()
	if own || got.Type != ExposeLoadBalancer {
		t.Fatalf("VoiceExpose() = %+v, %v; want the game's LoadBalancer, false", got, own)
	}
	g.Spec.Voice = &ProxyVoiceSpec{}
	if got, own = g.VoiceExpose(); own || got.Type != ExposeLoadBalancer {
		t.Fatalf("an empty voice block must still follow the game; got %+v, %v", got, own)
	}
}

func TestVoiceExposeTakesTheOverride(t *testing.T) {
	g := &ProxyGroup{Spec: ProxyGroupSpec{
		Expose: ExposeSpec{Type: ExposeLoadBalancer},
		Voice: &ProxyVoiceSpec{Expose: &ExposeSpec{
			Type: ExposeHostPort, HostPort: &HostPortSpec{Port: 25565},
		}},
	}}
	got, own := g.VoiceExpose()
	if !own || got.Type != ExposeHostPort || got.HostPort.Port != 25565 {
		t.Fatalf("VoiceExpose() = %+v, %v; want the HostPort override, true", got, own)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `$NIX go test ./api/v1alpha1/ -run 'TestVoice' -count=1`
Expected: compile failure, `undefined: NetworkVoiceSpec` and the other new names.

- [ ] **Step 3: Implement**

In `network_types.go`, add to `NetworkSpec` after `Commands`:

```go
	// Voice relays Simple Voice Chat through this network's proxies. Present
	// means on: every ProxyGroup relays, and the backends' policy admits UDP
	// backendPort from the proxies. The SVC plugin itself is not installed by
	// the operator, and SVC's own Velocity plugin must not be.
	// +optional
	Voice *NetworkVoiceSpec `json:"voice,omitempty"`
```

and below `NetworkCommands`:

```go
// DefaultVoiceBackendPort is Simple Voice Chat's own default for port in
// voicechat-server.properties.
const DefaultVoiceBackendPort int32 = 24454

// NetworkVoiceSpec says where the voice server listens on every backend.
type NetworkVoiceSpec struct {
	// BackendPort is the UDP port SVC listens on in each backend pod. The
	// operator writes it nowhere; it has to match the plugin's own
	// configuration.
	// +kubebuilder:default=24454
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	BackendPort int32 `json:"backendPort,omitempty"`
}

// VoiceBackendPort is spec.voice.backendPort and whether voice is on.
func (n *Network) VoiceBackendPort() (int32, bool) {
	if n.Spec.Voice == nil {
		return 0, false
	}
	if n.Spec.Voice.BackendPort == 0 {
		return DefaultVoiceBackendPort, true
	}
	return n.Spec.Voice.BackendPort, true
}
```

In `proxygroup_types.go`, add to `ProxyGroupSpec` after `Substitution`:

```go
	// Voice changes how voice chat leaves this group. Without it, voice takes
	// the game's way out with the same port number, as UDP. Only read while
	// the Network has spec.voice.
	// +optional
	Voice *ProxyVoiceSpec `json:"voice,omitempty"`
```

and the type plus helper (near `ExposeSpec`):

```go
// ProxyVoiceSpec overrides how voice chat reaches this group's proxies.
type ProxyVoiceSpec struct {
	// Expose gives voice a way out of its own, for an entry point that carries
	// no UDP. HostPort binds a UDP host port; the other types get a Service
	// named <group>-voice with only the UDP port.
	// +optional
	Expose *ExposeSpec `json:"expose,omitempty"`
}

// VoiceExpose is the strategy voice leaves by, and whether it is its own.
func (g *ProxyGroup) VoiceExpose() (ExposeSpec, bool) {
	if g.Spec.Voice != nil && g.Spec.Voice.Expose != nil {
		return *g.Spec.Voice.Expose, true
	}
	return g.Spec.Expose, false
}
```

Add to `ProxyGroupStatus` after `Address`:

```go
	// VoiceAddress is where voice chat reaches this group, empty while voice
	// is off or no proxy is ready. When voice takes the game's way out it is
	// the game address's host with the UDP port; clients are told the port
	// only and keep the host they typed.
	// +optional
	VoiceAddress string `json:"voiceAddress,omitempty"`
```

Add the printcolumn after the `Address` one:

```go
// +kubebuilder:printcolumn:name="Voice",type=string,JSONPath=`.status.voiceAddress`,priority=1
```

In `common_types.go` add `ConditionVoice` next to the other condition types and the four reasons in the reasons block:

```go
	// ConditionVoice is True while the group relays voice chat; absent while
	// neither the Network nor the group asks for voice.
	ConditionVoice = "Voice"
```

```go
	// The Voice reasons on a ProxyGroup.
	ReasonVoiceRelaying       = "Relaying"
	ReasonVoiceOffInNetwork   = "VoiceOffInNetwork"
	ReasonVoicePluginConflict = "VoicePluginConflict"
	ReasonVoiceServiceNotOurs = "VoiceServiceNotOwnedByGroup"
```

- [ ] **Step 4: Regenerate and run**

Run: `$NIX make manifests generate`
Run: `$NIX go test ./api/v1alpha1/ -run 'TestVoice' -count=1`
Expected: PASS. `git diff --stat` shows the CRDs, the chart's `crds.yaml`, `zz_generated.deepcopy.go`, `docs/reference/crds.md`.

- [ ] **Step 5: CEL reuse check**

`spec.voice.expose` carries `ExposeSpec`'s own `XValidation` rules. Add to the existing envtest validation tests in `api/v1alpha1` (find the file that creates ProxyGroups against the API server: `git grep -ln "expose.nodePort is required" api/v1alpha1`) one case: a ProxyGroup with `spec.voice.expose.type: NodePort` and no `nodePort` is refused with the message `expose.nodePort is required for type NodePort`. Run that package's test with `-run <the new test> -count=1` (`-p 1` on dev). Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add api/ config/ charts/ docs/reference/
git commit  # feat(api): voice chat fields on Network and ProxyGroup
```

---

### Task 2: The voice endpoint, as a pure package

**Files:**
- Create: `internal/voice/voice.go`
- Create: `internal/voice/voice_test.go`

**Interfaces:**
- Consumes: Task 1 (`VoiceExpose`, `VoiceBackendPort`, `Status.VoiceAddress`).
- Produces:
  - `type Endpoint struct { Host string; Port int32; OwnNode bool }` — `Host == "" && !OwnNode` is the port-only form
  - `func PublicPort(group *spawneryv1alpha1.ProxyGroup) int32` — the UDP port clients dial, 0 when the strategy has none yet
  - `func Announce(net *spawneryv1alpha1.Network, group *spawneryv1alpha1.ProxyGroup) (Endpoint, bool)` — false while voice is off or the endpoint is not known yet
  - `const RelayPort int32 = 25565` — the UDP port inside the proxy pod

- [ ] **Step 1: Write the failing test**

`internal/voice/voice_test.go`:

```go
package voice

import (
	"testing"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func voiceNetwork() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{Spec: spawneryv1alpha1.NetworkSpec{
		Voice: &spawneryv1alpha1.NetworkVoiceSpec{BackendPort: 24454},
	}}
}

func group(game spawneryv1alpha1.ExposeSpec, override *spawneryv1alpha1.ExposeSpec, voiceAddress string) *spawneryv1alpha1.ProxyGroup {
	g := &spawneryv1alpha1.ProxyGroup{}
	g.Spec.Expose = game
	if override != nil {
		g.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: override}
	}
	g.Status.VoiceAddress = voiceAddress
	return g
}

var (
	lb       = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer}
	nodePort = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765}}
	hostPort = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25577}}
	clusterIP = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "voice.example.test:24000"}}
)

func TestAnnounce(t *testing.T) {
	cases := []struct {
		name  string
		g     *spawneryv1alpha1.ProxyGroup
		want  Endpoint
		known bool
	}{
		{"LoadBalancer, game's way: port only", group(lb, nil, "203.0.113.7:25565"), Endpoint{Port: 25565}, true},
		{"NodePort, game's way: the node port, port only", group(nodePort, nil, "192.0.2.1:30765"), Endpoint{Port: 30765}, true},
		{"HostPort, game's way: the host port, port only", group(hostPort, nil, "192.0.2.1:25577"), Endpoint{Port: 25577}, true},
		{"ClusterIP, game's way: 25565, port only", group(clusterIP, nil, "voice.example.test:25565"), Endpoint{Port: 25565}, true},
		{"HostPort override: own node", group(lb, &hostPort, "192.0.2.1:25577"), Endpoint{Port: 25577, OwnNode: true}, true},
		{"NodePort override: own node", group(lb, &nodePort, "192.0.2.1:30765"), Endpoint{Port: 30765, OwnNode: true}, true},
		{"LoadBalancer override: the voice Service's ingress", group(nodePort, &lb, "198.51.100.4:25565"), Endpoint{Host: "198.51.100.4", Port: 25565}, true},
		{"LoadBalancer override, no ingress yet: unknown", group(nodePort, &lb, ""), Endpoint{}, false},
		{"ClusterIP override: the address as written", group(lb, &clusterIP, "voice.example.test:24000"), Endpoint{Host: "voice.example.test", Port: 24000}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, known := Announce(voiceNetwork(), c.g)
			if known != c.known || got != c.want {
				t.Fatalf("Announce() = %+v, %v; want %+v, %v", got, known, c.want, c.known)
			}
		})
	}
}

func TestAnnounceIsOffWithoutNetworkVoice(t *testing.T) {
	if _, known := Announce(&spawneryv1alpha1.Network{}, group(lb, nil, "203.0.113.7:25565")); known {
		t.Fatal("a network without spec.voice must announce nothing")
	}
}

func TestAClusterIPOverrideWithoutAPortMeans25565(t *testing.T) {
	bare := spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "voice.example.test"}}
	got, known := Announce(voiceNetwork(), group(lb, &bare, "voice.example.test:25565"))
	if !known || got != (Endpoint{Host: "voice.example.test", Port: 25565}) {
		t.Fatalf("Announce() = %+v, %v", got, known)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `$NIX go test ./internal/voice/ -count=1`
Expected: compile failure (package has no non-test files).

- [ ] **Step 3: Implement**

`internal/voice/voice.go` (with the copyright header):

```go
// Package voice decides what a proxy tells a Simple Voice Chat client about
// where to send its UDP. The controller publishes the result on the group's
// status and proxyreg sends it to the group's proxies, so both read it here.
package voice

import (
	"net"
	"strconv"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// RelayPort is the UDP port the relay binds inside every proxy pod.
const RelayPort int32 = 25565

// Endpoint is what the secret packet is rewritten to. With no Host and no
// OwnNode, the client keeps the host it joined through.
type Endpoint struct {
	Host    string
	Port    int32
	OwnNode bool
}

// PublicPort is the UDP port clients dial, by the strategy voice leaves by.
func PublicPort(group *spawneryv1alpha1.ProxyGroup) int32 {
	spec, _ := group.VoiceExpose()
	switch spec.Type {
	case spawneryv1alpha1.ExposeNodePort:
		if spec.NodePort != nil {
			return spec.NodePort.Port
		}
	case spawneryv1alpha1.ExposeHostPort:
		if spec.HostPort != nil {
			return spec.HostPort.Port
		}
	case spawneryv1alpha1.ExposeClusterIP:
		if spec.ClusterIP != nil {
			if _, port, ok := splitAddress(spec.ClusterIP.Address); ok {
				return port
			}
		}
		return RelayPort
	case spawneryv1alpha1.ExposeLoadBalancer:
		return RelayPort
	}
	return 0
}

// Announce is the endpoint for this group's clients, and false while voice is
// off or the endpoint is not known yet (a LoadBalancer without an ingress).
func Announce(network *spawneryv1alpha1.Network, group *spawneryv1alpha1.ProxyGroup) (Endpoint, bool) {
	if _, on := network.VoiceBackendPort(); !on {
		return Endpoint{}, false
	}
	spec, own := group.VoiceExpose()
	port := PublicPort(group)
	if port == 0 {
		return Endpoint{}, false
	}
	if !own {
		return Endpoint{Port: port}, true
	}
	switch spec.Type {
	case spawneryv1alpha1.ExposeHostPort, spawneryv1alpha1.ExposeNodePort:
		return Endpoint{Port: port, OwnNode: true}, true
	case spawneryv1alpha1.ExposeClusterIP:
		host, _, _ := splitAddress(spec.ClusterIP.Address)
		return Endpoint{Host: host, Port: port}, true
	case spawneryv1alpha1.ExposeLoadBalancer:
		host, _, ok := splitAddress(group.Status.VoiceAddress)
		if !ok || host == "" {
			return Endpoint{}, false
		}
		return Endpoint{Host: host, Port: port}, true
	}
	return Endpoint{}, false
}

// splitAddress reads "host" or "host:port"; a bare host means 25565.
func splitAddress(address string) (string, int32, bool) {
	if address == "" {
		return "", 0, false
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return address, RelayPort, true
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return host, int32(port), true
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `$NIX go test ./internal/voice/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/voice/
git commit  # feat(voice): the endpoint a group's voice clients are told
```

---

### Task 3: The proxy pod carries the relay port

**Files:**
- Modify: `internal/podspec/proxy.go`
- Modify: `internal/podspec/proxy_test.go`
- Test: `internal/podspec/hash_golden_test.go` (must stay unchanged and green)

**Interfaces:**
- Consumes: Task 1, Task 2 (`voice.RelayPort`).
- Produces: constants `VoicePortName = "voice"`, `EnvHostIP = "SPAWNERY_HOST_IP"`, `EnvVoice = "SPAWNERY_VOICE"` in `podspec`. The proxy container gets, only while `net.VoiceBackendPort()` is on: a UDP container port `voice` 25565 (with `HostPort` = the voice HostPort when voice leaves by HostPort, its own or the game's), `SPAWNERY_VOICE=1`, and `SPAWNERY_HOST_IP` from the downward API `status.hostIP`.

- [ ] **Step 1: Write the failing tests** in `proxy_test.go` (use the file's `testNetwork()`, `testProxyGroup()` and `BuildProxyPod`):

```go
func voicePort(pod *corev1.Pod) *corev1.ContainerPort {
	for i, p := range pod.Spec.Containers[0].Ports {
		if p.Name == VoicePortName {
			return &pod.Spec.Containers[0].Ports[i]
		}
	}
	return nil
}

func TestAProxyWithoutVoiceHasNoVoicePortAndNoHostIP(t *testing.T) {
	pod := buildProxy(t)
	if p := voicePort(pod); p != nil {
		t.Fatalf("voice port present without Network.spec.voice: %+v", p)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvHostIP || e.Name == EnvVoice {
			t.Fatalf("%s set without Network.spec.voice", e.Name)
		}
	}
}

func TestAProxyInAVoiceNetworkListensOnUDP25565(t *testing.T) {
	net := testNetwork()
	net.Spec.Voice = &spawneryv1alpha1.NetworkVoiceSpec{BackendPort: 24454}
	pod, err := BuildProxyPod(net, testProxyGroup(), "gateway-abcd", "operator:9443", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := voicePort(pod)
	if p == nil || p.ContainerPort != 25565 || p.Protocol != corev1.ProtocolUDP || p.HostPort != 0 {
		t.Fatalf("voice port = %+v; want UDP 25565 without a host port", p)
	}
	var hostIP *corev1.EnvVar
	for i, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvHostIP {
			hostIP = &pod.Spec.Containers[0].Env[i]
		}
	}
	if hostIP == nil || hostIP.ValueFrom == nil || hostIP.ValueFrom.FieldRef == nil ||
		hostIP.ValueFrom.FieldRef.FieldPath != "status.hostIP" {
		t.Fatalf("SPAWNERY_HOST_IP = %+v; want the downward API status.hostIP", hostIP)
	}
}

func TestVoiceTakesTheGamesHostPortAsUDP(t *testing.T) {
	net := testNetwork()
	net.Spec.Voice = &spawneryv1alpha1.NetworkVoiceSpec{}
	g := testProxyGroup()
	g.Spec.Expose = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25577}}
	pod, err := BuildProxyPod(net, g, "gateway-abcd", "operator:9443", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := voicePort(pod); p == nil || p.HostPort != 25577 {
		t.Fatalf("voice port = %+v; want host port 25577 over UDP", p)
	}
}

func TestAHostPortVoiceOverrideBindsItsOwnPort(t *testing.T) {
	net := testNetwork()
	net.Spec.Voice = &spawneryv1alpha1.NetworkVoiceSpec{}
	g := testProxyGroup()
	g.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: &spawneryv1alpha1.ExposeSpec{
		Type: spawneryv1alpha1.ExposeHostPort, HostPort: &spawneryv1alpha1.HostPortSpec{Port: 24500}}}
	pod, err := BuildProxyPod(net, g, "gateway-abcd", "operator:9443", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := voicePort(pod); p == nil || p.HostPort != 24500 {
		t.Fatalf("voice port = %+v; want host port 24500", p)
	}
	for _, p := range pod.Spec.Containers[0].Ports {
		if p.Name == MinecraftPortName && p.HostPort != 0 {
			t.Fatalf("the game port gained a host port from the voice override: %+v", p)
		}
	}
}

func TestVoiceInTheNetworkMovesTheProxyHash(t *testing.T) {
	g := testProxyGroup()
	off, err := DesiredProxyHash(testNetwork(), g, "operator:9443", nil)
	if err != nil {
		t.Fatal(err)
	}
	net := testNetwork()
	net.Spec.Voice = &spawneryv1alpha1.NetworkVoiceSpec{}
	on, err := DesiredProxyHash(net, g, "operator:9443", nil)
	if err != nil {
		t.Fatal(err)
	}
	if on == off {
		t.Fatal("switching voice on must roll the proxies once")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `$NIX go test ./internal/podspec/ -run 'Voice|HostIP' -count=1` (envtest package: `-p 1` on dev)
Expected: compile failure on `VoicePortName`, `EnvHostIP`, `EnvVoice`.

- [ ] **Step 3: Implement** in `proxy.go`:

Constants next to the others:

```go
	VoicePortName = "voice"
	// EnvVoice tells the agent to start the relay; EnvHostIP is the node
	// address it announces when voice leaves by HostPort or NodePort.
	EnvVoice  = "SPAWNERY_VOICE"
	EnvHostIP = "SPAWNERY_HOST_IP"
```

In `renderProxyPod`, after the `minecraft` port is built and before `container`:

```go
	ports := []corev1.ContainerPort{
		minecraft,
		{Name: ProxyReadyPortName, ContainerPort: ProxyReadyPort, Protocol: corev1.ProtocolTCP},
	}
	var voiceEnv []corev1.EnvVar
	if _, on := net.VoiceBackendPort(); on {
		relay := corev1.ContainerPort{
			Name:          VoicePortName,
			ContainerPort: voice.RelayPort,
			Protocol:      corev1.ProtocolUDP,
		}
		if spec, _ := group.VoiceExpose(); spec.Type == spawneryv1alpha1.ExposeHostPort && spec.HostPort != nil {
			relay.HostPort = spec.HostPort.Port
		}
		ports = append(ports, relay)
		voiceEnv = []corev1.EnvVar{
			{Name: EnvVoice, Value: "1"},
			{Name: EnvHostIP, ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
			}},
		}
	}
```

and use `Ports: ports` in the container, and append `voiceEnv...` right after `transferEnv(group)...` in the env chain (before substitution and the user's env, so the user's env still comes last). Import `github.com/spawnery/spawnery/internal/voice`.

The game's HostPort must keep coming only from `spec.expose`: leave the existing `minecraft.HostPort` block as it is.

- [ ] **Step 4: Run**

Run: `$NIX go test ./internal/podspec/ -count=1` (`-p 1` on dev)
Expected: PASS, including `hash_golden_test.go` unchanged (no network in its fixtures has voice). If a golden fails, the env/port is leaking into the off case: fix the code, never the golden.

- [ ] **Step 5: Commit**

```bash
git add internal/podspec/proxy.go internal/podspec/proxy_test.go
git commit  # feat(podspec): proxies in a voice network listen on UDP 25565
```

---

### Task 4: NetworkPolicies admit the backend voice port

**Files:**
- Modify: `internal/podspec/netpol.go`
- Modify: `internal/podspec/netpol_test.go`
- Modify: `internal/controller/proxygroup_controller.go` (the one caller of `BuildProxyNetworkPolicy`, `reconcileNetworkPolicy`)

**Interfaces:**
- Consumes: Task 1 (`VoiceBackendPort`).
- Produces: `BuildProxyNetworkPolicy(networkName string, group *ProxyGroup, operatorNamespace string, onlineMode bool, voiceBackendPort int32)`; 0 means no voice rule. `BuildNetworkPolicy` reads the network itself.

- [ ] **Step 1: Write the failing tests** in `netpol_test.go`:

```go
func udpPorts(ports []networkingv1.NetworkPolicyPort) []int32 {
	var out []int32
	for _, p := range ports {
		if p.Protocol != nil && *p.Protocol == corev1.ProtocolUDP && p.Port != nil {
			out = append(out, p.Port.IntVal)
		}
	}
	return out
}

func TestBackendsAdmitNoUDPWithoutVoice(t *testing.T) {
	pol := BuildNetworkPolicy(testNetwork(), "spawnery-system")
	for _, rule := range pol.Spec.Ingress {
		if got := udpPorts(rule.Ports); len(got) > 0 {
			t.Fatalf("ingress admits UDP %v without voice", got)
		}
	}
}

func TestBackendsAdmitTheVoicePortFromTheirProxies(t *testing.T) {
	net := testNetwork()
	net.Spec.Voice = &spawneryv1alpha1.NetworkVoiceSpec{BackendPort: 24454}
	pol := BuildNetworkPolicy(net, "spawnery-system")
	if len(pol.Spec.Ingress) != 1 {
		t.Fatalf("want one ingress rule (from this network's proxies), got %d", len(pol.Spec.Ingress))
	}
	if got := udpPorts(pol.Spec.Ingress[0].Ports); len(got) != 1 || got[0] != 24454 {
		t.Fatalf("ingress UDP ports = %v; want [24454] in the proxies' rule", got)
	}
}

func TestAProxyReachesTheBackendVoicePortOnlyWithVoice(t *testing.T) {
	g := testProxyGroup()
	off := BuildProxyNetworkPolicy("production", g, "spawnery-system", false, 0)
	on := BuildProxyNetworkPolicy("production", g, "spawnery-system", false, 24454)
	count := func(p *networkingv1.NetworkPolicy) int {
		n := 0
		for _, r := range p.Spec.Egress {
			for _, port := range udpPorts(r.Ports) {
				if port == 24454 {
					n++
				}
			}
		}
		return n
	}
	if count(off) != 0 || count(on) != 1 {
		t.Fatalf("UDP 24454 egress rules: off=%d on=%d; want 0 and 1", count(off), count(on))
	}
}
```

Use the test file's existing network/group fixtures if their names differ from `testNetwork()`/`testProxyGroup()` (both exist in `proxy_test.go` in the same package). Update every existing call of `BuildProxyNetworkPolicy` in tests to pass `0`.

- [ ] **Step 2: Run to verify they fail**

Run: `$NIX go test ./internal/podspec/ -run 'Voice|UDP' -count=1`
Expected: compile failure (argument count), then the backend test fails once it compiles.

- [ ] **Step 3: Implement**

In `BuildNetworkPolicy`, build the ingress ports first:

```go
	ingressPorts := []networkingv1.NetworkPolicyPort{{
		Protocol: &tcp,
		Port:     ptr.To(intstr.FromInt32(MinecraftPort)),
	}}
	if port, on := network.VoiceBackendPort(); on {
		ingressPorts = append(ingressPorts, networkingv1.NetworkPolicyPort{
			Protocol: &udp,
			Port:     ptr.To(intstr.FromInt32(port)),
		})
	}
```

and use `Ports: ingressPorts` in the single ingress rule. In `BuildProxyNetworkPolicy` add the `voiceBackendPort int32` parameter and, in the backends rule, append `{Protocol: &udp, Port: ptr.To(intstr.FromInt32(voiceBackendPort))}` when it is non-zero. Update the doc comment's parameter mention only if it lists parameters.

In `proxygroup_controller.go`, `reconcileNetworkPolicy` needs the network: change its signature to `reconcileNetworkPolicy(ctx, network, group)` and its call in `reconcileObserved` accordingly, and pass:

```go
	voicePort, _ := network.VoiceBackendPort()
	desired := podspec.BuildProxyNetworkPolicy(group.Spec.NetworkRef.Name, group, r.OperatorNamespace,
		online != nil && *online, voicePort)
```

The per-Network policy is rebuilt by the Network controller from the Network object, so the backends' rule follows `spec.voice` without further wiring. Confirm that with `git grep -n BuildNetworkPolicy internal/controller/network_controller.go`.

- [ ] **Step 4: Run**

Run: `$NIX go test ./internal/podspec/ -count=1` then `$NIX go build ./...`
Expected: PASS, build clean.

- [ ] **Step 5: Commit**

```bash
git add internal/podspec/netpol.go internal/podspec/netpol_test.go internal/controller/proxygroup_controller.go
git commit  # feat(podspec): policies admit the voice port between proxies and backends
```

---

### Task 5: Services, voice status and the Voice condition in the ProxyGroup controller

**Files:**
- Modify: `internal/controller/proxygroup_controller.go`
- Create: `internal/controller/voice.go` (the voice-only reconcile helpers, so the 1500-line controller does not grow by another feature's worth)
- Create: `internal/controller/voice_test.go` (pure tests of `voiceAddress`)
- Create: `internal/controller/voice_envtest_test.go`

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces:
  - `func VoiceServiceName(group string) string` → `group + "-voice"` (in `podspec`, next to `ProxyNetworkPolicyName`)
  - `func (r *ProxyGroupReconciler) reconcileVoiceService(ctx, network, group) (*corev1.Service, error)`; returns `errForeignVoiceService` for a Service at that name controlled by someone else
  - `func voiceAddress(network *Network, group *ProxyGroup, pods []corev1.Pod, gameSvc, voiceSvc *corev1.Service) string`
  - `func setVoiceCondition(group *ProxyGroup, network *Network, conflict bool, foreign bool)`
  - `reconcileService` gains the network parameter and adds the UDP port when voice is on and has no override.
  - `proxyObservation` gains `voiceSvc *corev1.Service`; `setStatus` gains `network` and `voiceSvc`.

Rules to implement (from spec §3–4):

| Network voice | Override | Game Service | `<group>-voice` | HostPort |
|---|---|---|---|---|
| off | any | TCP only | deleted if ours | — |
| on | none | TCP + UDP `voice` (same `port`, same `nodePort` for NodePort, `targetPort: voice`) | deleted if ours | voice port carries the game's host port (Task 3) |
| on | LoadBalancer / NodePort / ClusterIP | TCP only | UDP only, of that type; LB keeps annotations and `externalTrafficPolicy` from the override's `loadBalancer`; NodePort is `Local` with the override's `nodePort.port` | — |
| on | HostPort | TCP only | deleted if ours | voice port carries the override's host port (Task 3); `HostPortRefusal` applies to it |

`voiceAddress` (status):
- voice off → `""`.
- game's way → the host of `status.address` (as `proxyAddress` computed it this pass) joined with `voice.PublicPort(group)`; `""` when `status.address` is empty.
- override HostPort → `readyHostIPBindingPort(pods, port)` + port; NodePort → `readyHostIP(pods)` + the voice Service's allocated UDP node port; LoadBalancer → first ingress IP/hostname of the voice Service + 25565, gated on `readyHostIP(pods) != ""`; ClusterIP → `override.clusterIP.address` gated on a ready pod and an existing voice Service.

`Voice` condition:
- Network voice off and `spec.voice` unset → remove the condition (`meta.RemoveStatusCondition`).
- Network voice off and `spec.voice` set → `False`, `VoiceOffInNetwork`, message `network "<n>" has no spec.voice; this group's spec.voice is ignored`.
- foreign `<group>-voice` Service → `False`, `VoiceServiceNotOwnedByGroup`, message naming the Service.
- any connected proxy pod of the group reports the SVC Velocity plugin (Task 7 adds `Snapshot.VoicePluginLoaded`; until Task 7 lands, pass `false`) → `False`, `VoicePluginConflict`, message `proxy <pod> has Simple Voice Chat's own Velocity plugin loaded; remove it, spawnery relays voice itself`.
- otherwise `True`, `Relaying`.
- `setStatus`: phase is `Degraded` when `Degraded` is true **or** `Voice` is `False`.

`HostPortRefusal` for the override runs in `Reconcile` beside the existing game check, with the same event/condition pattern and `ReasonHostPortNotAllowed`.

- [ ] **Step 1: Write the failing pure test** `voice_test.go`, mirroring `proxyaddress_test.go` (reuse its `readyProxyPod`, `nodePortService`, `loadBalancerService` helpers):

```go
func TestVoiceAddress(t *testing.T) {
	voiceNet := &spawneryv1alpha1.Network{Spec: spawneryv1alpha1.NetworkSpec{
		Voice: &spawneryv1alpha1.NetworkVoiceSpec{}}}
	lb := spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer}
	nodePort := spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765}}

	withStatus := func(spec spawneryv1alpha1.ExposeSpec, override *spawneryv1alpha1.ExposeSpec, address string) *spawneryv1alpha1.ProxyGroup {
		g := groupExposing(spec)
		if override != nil {
			g.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: override}
		}
		g.Status.Address = address
		return g
	}

	cases := []struct {
		name     string
		net      *spawneryv1alpha1.Network
		g        *spawneryv1alpha1.ProxyGroup
		pods     []corev1.Pod
		voiceSvc *corev1.Service
		want     string
	}{
		{"voice off", &spawneryv1alpha1.Network{}, withStatus(lb, nil, "203.0.113.7:25565"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, ""},
		{"game's LoadBalancer", voiceNet, withStatus(lb, nil, "203.0.113.7:25565"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, "203.0.113.7:25565"},
		{"game's NodePort: same node port", voiceNet, withStatus(nodePort, nil, "192.168.1.10:30765"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, "192.168.1.10:30765"},
		{"no game address yet: nothing", voiceNet, withStatus(lb, nil, ""),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, ""},
		{"LoadBalancer override: the voice Service's ingress", voiceNet, withStatus(nodePort, &lb, "192.168.1.10:30765"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, loadBalancerService("198.51.100.4", ""), "198.51.100.4:25565"},
		{"LoadBalancer override without ingress", voiceNet, withStatus(nodePort, &lb, "192.168.1.10:30765"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, loadBalancerService("", ""), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := voiceAddress(c.net, c.g, c.pods, nil, c.voiceSvc); got != c.want {
				t.Fatalf("voiceAddress() = %q; want %q", got, c.want)
			}
		})
	}
}
```

`loadBalancerService` builds a TCP `minecraft` port; for the voice Service the address reader must look at the ingress only, so the helper is fine here.

- [ ] **Step 2: Run to verify it fails**

Run: `$NIX go test ./internal/controller/ -run TestVoiceAddress -count=1`
Expected: compile failure `undefined: voiceAddress`.

- [ ] **Step 3: Implement `voice.go`**

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/voice"
)

var errForeignVoiceService = errors.New("the voice Service name is taken by an object this group does not control")

// voiceServicePort is the UDP twin of the game's port; the same number and,
// for NodePort, the same node port, since the API server keys both on protocol.
func voiceServicePort(nodePort int32) corev1.ServicePort {
	return corev1.ServicePort{
		Name:       podspec.VoicePortName,
		Port:       voice.RelayPort,
		TargetPort: intstr.FromString(podspec.VoicePortName),
		Protocol:   corev1.ProtocolUDP,
		NodePort:   nodePort,
	}
}

// voiceOwnService says whether voice needs a Service of its own.
func voiceOwnService(network *spawneryv1alpha1.Network, group *spawneryv1alpha1.ProxyGroup) (spawneryv1alpha1.ExposeSpec, bool) {
	if _, on := network.VoiceBackendPort(); !on {
		return spawneryv1alpha1.ExposeSpec{}, false
	}
	spec, own := group.VoiceExpose()
	return spec, own && spec.Type != spawneryv1alpha1.ExposeHostPort
}

func (r *ProxyGroupReconciler) reconcileVoiceService(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
) (*corev1.Service, error) {
	name := podspec.VoiceServiceName(group.Name)
	spec, wanted := voiceOwnService(network, group)

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: group.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return nil, err
	}
	if existing != nil {
		if owner := metav1.GetControllerOf(existing); owner == nil || owner.UID != group.UID {
			if wanted {
				return nil, errForeignVoiceService
			}
			return nil, nil
		}
		if !wanted {
			return nil, client.IgnoreNotFound(r.Delete(ctx, existing))
		}
	}
	if !wanted {
		return nil, nil
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: group.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		var lbAnnotations map[string]string
		if spec.Type == spawneryv1alpha1.ExposeLoadBalancer && spec.LoadBalancer != nil {
			lbAnnotations = spec.LoadBalancer.Annotations
		}
		applyExposeAnnotations(svc, lbAnnotations)
		svc.Spec.Selector = podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
		port := voiceServicePort(0)
		svc.Spec.ExternalTrafficPolicy = ""
		switch spec.Type {
		case spawneryv1alpha1.ExposeLoadBalancer:
			svc.Spec.Type = corev1.ServiceTypeLoadBalancer
			policy := corev1.ServiceExternalTrafficPolicyLocal
			if spec.LoadBalancer != nil && spec.LoadBalancer.ExternalTrafficPolicy != "" {
				policy = spec.LoadBalancer.ExternalTrafficPolicy
			}
			svc.Spec.ExternalTrafficPolicy = policy
		case spawneryv1alpha1.ExposeNodePort:
			svc.Spec.Type = corev1.ServiceTypeNodePort
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			port.NodePort = spec.NodePort.Port
		case spawneryv1alpha1.ExposeClusterIP:
			svc.Spec.Type = corev1.ServiceTypeClusterIP
		default:
			return fmt.Errorf("voice expose.type %s reached reconcileVoiceService without a branch", spec.Type)
		}
		svc.Spec.Ports = []corev1.ServicePort{port}
		return controllerutil.SetControllerReference(group, svc, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return svc, nil
}

func allocatedVoiceNodePort(svc *corev1.Service) int32 {
	if svc == nil {
		return 0
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == podspec.VoicePortName {
			return p.NodePort
		}
	}
	return 0
}

func voiceAddress(
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
	gameSvc, voiceSvc *corev1.Service,
) string {
	if _, on := network.VoiceBackendPort(); !on {
		return ""
	}
	join := func(host string, port int32) string { return net.JoinHostPort(host, strconv.Itoa(int(port))) }
	spec, own := group.VoiceExpose()
	if !own {
		if group.Status.Address == "" {
			return ""
		}
		host, _, err := net.SplitHostPort(group.Status.Address)
		if err != nil {
			host = group.Status.Address
		}
		return join(host, voice.PublicPort(group))
	}
	switch spec.Type {
	case spawneryv1alpha1.ExposeHostPort:
		ip := readyHostIPBindingPort(pods, spec.HostPort.Port)
		if ip == "" {
			return ""
		}
		return join(ip, spec.HostPort.Port)
	case spawneryv1alpha1.ExposeNodePort:
		ip, port := readyHostIP(pods), allocatedVoiceNodePort(voiceSvc)
		if ip == "" || port == 0 {
			return ""
		}
		return join(ip, port)
	case spawneryv1alpha1.ExposeLoadBalancer:
		if voiceSvc == nil || readyHostIP(pods) == "" {
			return ""
		}
		for _, ing := range voiceSvc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				return join(ing.IP, voice.RelayPort)
			}
			if ing.Hostname != "" {
				return join(ing.Hostname, voice.RelayPort)
			}
		}
		return ""
	case spawneryv1alpha1.ExposeClusterIP:
		if voiceSvc == nil || readyHostIP(pods) == "" {
			return ""
		}
		return spec.ClusterIP.Address
	}
	return ""
}

func setVoiceCondition(group *spawneryv1alpha1.ProxyGroup, network *spawneryv1alpha1.Network, foreign bool, conflictPod string) {
	_, on := network.VoiceBackendPort()
	cond := metav1.Condition{Type: spawneryv1alpha1.ConditionVoice, Status: metav1.ConditionTrue,
		Reason: spawneryv1alpha1.ReasonVoiceRelaying, Message: "this group's proxies relay voice chat"}
	switch {
	case !on && group.Spec.Voice == nil:
		meta.RemoveStatusCondition(&group.Status.Conditions, spawneryv1alpha1.ConditionVoice)
		return
	case !on:
		cond.Status, cond.Reason = metav1.ConditionFalse, spawneryv1alpha1.ReasonVoiceOffInNetwork
		cond.Message = fmt.Sprintf("network %q has no spec.voice; this group's spec.voice is ignored", network.Name)
	case foreign:
		cond.Status, cond.Reason = metav1.ConditionFalse, spawneryv1alpha1.ReasonVoiceServiceNotOurs
		cond.Message = fmt.Sprintf("Service %q exists and is not controlled by this group; voice has no way out",
			podspec.VoiceServiceName(group.Name))
	case conflictPod != "":
		cond.Status, cond.Reason = metav1.ConditionFalse, spawneryv1alpha1.ReasonVoicePluginConflict
		cond.Message = fmt.Sprintf("proxy %s has Simple Voice Chat's own Velocity plugin loaded; "+
			"remove it, spawnery relays voice itself", conflictPod)
	}
	meta.SetStatusCondition(&group.Status.Conditions, cond)
}
```

Add `client` to the imports (`sigs.k8s.io/controller-runtime/pkg/client`). Add `VoiceServiceName` to `internal/podspec/netpol.go` beside `ProxyNetworkPolicyName`:

```go
func VoiceServiceName(group string) string { return group + "-voice" }
```

- [ ] **Step 4: Wire it into `proxygroup_controller.go`**

1. `reconcileService(ctx, network, group)`: after building `port`, set `svc.Spec.Ports = []corev1.ServicePort{port}` and then, when `_, on := network.VoiceBackendPort(); on` and `_, own := group.VoiceExpose(); !own`, append `voiceServicePort(port.NodePort)` (for NodePort that is `group.Spec.Expose.NodePort.Port`; otherwise 0). The HostPort branch above it still deletes the game Service.
2. In `Reconcile`, after the game `HostPortRefusal` block, the same block for the override:

```go
	if spec, own := group.VoiceExpose(); own && spec.Type == spawneryv1alpha1.ExposeHostPort && spec.HostPort != nil {
		if _, on := network.VoiceBackendPort(); on {
			if message, ok := podspec.HostPortRefusal(network, spec.HostPort.Port); !ok {
				// identical event + setProxyGroupAccepted(false, ReasonHostPortNotAllowed, message) + refuse
			}
		}
	}
```

   (copy the body of the game block verbatim, with `message` prefixed `"spec.voice.expose: "`).
3. In `reconcileObserved`, after `reconcileService`: `voiceSvc, err := r.reconcileVoiceService(ctx, network, group)`; on `errors.Is(err, errForeignVoiceService)` keep going with `foreignVoice = true` and `voiceSvc = nil` (the game must keep working); on any other error return it. Store `voiceSvc` and `foreignVoice` in `proxyObservation`.
4. `setStatus(group, network, obs.pods, obs.svc, obs.voiceSvc, obs.foreignVoice)`: after `group.Status.Address = proxyAddress(...)`, set `group.Status.VoiceAddress = voiceAddress(network, group, pods, svc, voiceSvc)`, then `setVoiceCondition(group, network, foreignVoice, voicePluginPod(r.Agents, pods))` where `voicePluginPod` returns `""` for now (Task 7 fills it in). In the phase switch, add `meta.IsStatusConditionFalse(group.Status.Conditions, spawneryv1alpha1.ConditionVoice)` to the `Degraded` case.
5. `SetupWithManager`: the group already `Owns(&corev1.Service{})`; check with `git grep -n "Owns(" internal/controller/proxygroup_controller.go`. A Network change already maps to its groups (`groupsOfNetwork`).

- [ ] **Step 5: envtest** `voice_envtest_test.go`, following `expose_test.go`'s `fixture` (read its first 100 lines for `newFixture`, the network/group builders and `reconcile` helper; use them, do not write a second harness). Tests:

```go
func TestAVoiceNetworkAddsUDPToTheGamesService(t *testing.T)
// LoadBalancer group in a network with spec.voice: the Service has two ports,
// minecraft/TCP 25565 and voice/UDP 25565 targeting "voice"; no <group>-voice Service exists.

func TestANodePortGroupSharesItsNodePortAcrossProtocols(t *testing.T)
// NodePort 30765 in a voice network: the API server accepts TCP and UDP on 30765;
// both ServicePorts carry NodePort 30765 after the reconcile.

func TestWithoutVoiceTheServiceIsUnchanged(t *testing.T)
// The same LoadBalancer group in a network without spec.voice: exactly one port, TCP.
// This is Review Focus 1.

func TestAVoiceOverrideGetsItsOwnServiceAndLeavesTheGameAlone(t *testing.T)
// Game LoadBalancer, override NodePort 30766: game Service TCP only; <group>-voice is
// NodePort, Local, one UDP port with NodePort 30766, controlled by the group.

func TestDroppingTheOverrideDeletesTheVoiceService(t *testing.T)
// After the previous state, remove spec.voice.expose and reconcile: <group>-voice is gone,
// the game Service gains the UDP port. This is Review Focus 2.

func TestAForeignVoiceServiceIsReportedAndLeftAlone(t *testing.T)
// Pre-create Service <group>-voice without an owner; override LoadBalancer.
// Voice condition False/VoiceServiceNotOwnedByGroup, phase Degraded, the foreign
// Service unchanged (compare resourceVersion), the game Service still reconciled.

func TestGroupVoiceWithoutNetworkVoiceIsReported(t *testing.T)
// spec.voice set, network without: Voice False/VoiceOffInNetwork; no UDP port anywhere.

func TestAHostPortOverrideOutsideTheRangeIsRefused(t *testing.T)
// Network hostPortRange 25000-25100, override HostPort 26000: Accepted False/HostPortNotAllowed,
// message starts with "spec.voice.expose: ".
```

Each test body: build the network (with or without `Voice`), the group, run the fixture's reconcile until settled, `Get` the Services/group and assert exactly the commented facts. Remember the envtest rule from CLAUDE.md: anything created must be deleted in `t.Cleanup` (the Services and namespaces the fixture creates already are; a pre-created foreign Service is not).

- [ ] **Step 6: Run**

Run: `$NIX go test ./internal/controller/ -run 'Voice|NodePortShares|WithoutVoice|ForeignVoice|HostPortOverride' -count=1 -p 1`
Then the whole package once: `$NIX go test ./internal/controller/ -count=1 -p 1` (~85 s).
Expected: PASS. Then demonstrate one test bites: in a throwaway worktree (`git worktree add --detach ../voice-mut HEAD`), comment out the UDP append in `reconcileService`, run `TestAVoiceNetworkAddsUDPToTheGamesService`, record the FAIL, remove the worktree. Paste both outputs in the report.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/ internal/podspec/netpol.go
git commit  # feat(controller): voice leaves each proxy group by UDP
```

---

### Task 6: Proto messages

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Regenerate: `internal/agentpb/`, `agent/common/src/proto/java/`

**Interfaces:**
- Produces (proto, exact):

```proto
// VoiceConfig is whether this proxy relays Simple Voice Chat, and what it
// rewrites a backend's secret packet to. A state, re-sent with every snapshot.
message VoiceConfig {
  bool enabled = 1;
  // The UDP port SVC listens on in every backend.
  int32 backend_port = 2;
  // The endpoint clients are told. announce_port 0: not known yet, and the
  // secret is passed through untouched. An empty host without own_node keeps
  // the host the client joined through.
  string announce_host = 3;
  int32 announce_port = 4;
  bool announce_own_node = 5;
}

// VoiceCounters are the relay's packet totals since the agent started, by
// reason. Cumulative: the operator turns them into increments.
message VoiceCounters {
  map<string, int64> totals = 1;
}
```

  - `OperatorToProxy`: `VoiceConfig voice_config = 12;`
  - `ProxyMessage`: `VoiceCounters voice_counters = 9;`
  - `Hello`: `bool voice_plugin_loaded = 4;` with the comment `// Proxy agents only: SVC's own Velocity plugin (id "voicechat") is loaded, which would bind the relay's port.`

- [ ] **Step 1:** Add the messages and fields above (messages next to `SetReady`, after the existing fields; field numbers exactly as given).
- [ ] **Step 2:** `$NIX make proto`, then `$NIX go build ./... && $NIX go test ./internal/agentpb/ -count=1`. Expected: PASS (`contract_test.go` checks the service name only).
- [ ] **Step 3:** `git add proto/ internal/agentpb/ agent/common/src/proto/java/` and commit `feat(proto): voice config, counters and the conflicting plugin on Hello`.

---

### Task 7: The operator side of the channel

**Files:**
- Modify: `internal/proxyreg/fleet.go`, `internal/proxyreg/fleet_test.go`
- Modify: `internal/agent/registry.go`, `internal/agent/registry_test.go`
- Modify: `internal/agentserver/server.go`, `internal/agentserver/metrics.go`, `internal/agentserver/server_test.go` (or the proxy test file that drives `handleProxy`: `git grep -ln "ProxyMessage_PlayerRoster" internal/agentserver/*_test.go`)
- Modify: `internal/controller/voice.go` (`voicePluginPod`)

**Interfaces:**
- Consumes: Tasks 2, 5, 6.
- Produces:
  - `Snapshot.VoicePluginLoaded bool`; `func (r *Registry) ReportVoicePlugin(key string, loaded bool)`
  - `func (r *Registry) ReportVoiceTotals(key string, totals map[string]int64) map[string]int64` — returns the increments; a total lower than the last one counts in full (restart); keys outside `agent.VoiceReasons` are dropped
  - `var VoiceReasons = []string{"relayed", "not_svc", "unknown_player", "unknown_server", "unknown_version", "ping"}` in `internal/agent`
  - `agentserver.VoicePackets` counter vec `spawnery_voice_packets_total{group,reason}`
  - `proxyreg` snapshot appends one `VoiceConfig` per session, built by `voiceConfig(network, group)`

- [ ] **Step 1: Failing registry tests** (`registry_test.go`, use the file's way of creating a connected proxy entry):

```go
func TestVoiceTotalsBecomeIncrements(t *testing.T) {
	r := newTestRegistry(t)            // the file's constructor helper
	connectProxy(t, r, "pod-a")        // the file's helper for a proxy entry
	got := r.ReportVoiceTotals("pod-a", map[string]int64{"relayed": 10, "not_svc": 2})
	if got["relayed"] != 10 || got["not_svc"] != 2 {
		t.Fatalf("first report = %v", got)
	}
	got = r.ReportVoiceTotals("pod-a", map[string]int64{"relayed": 15, "not_svc": 2})
	if got["relayed"] != 5 || got["not_svc"] != 0 {
		t.Fatalf("second report = %v; want relayed 5, not_svc 0", got)
	}
}

func TestADecreasingVoiceTotalIsARestartNotANegativeIncrement(t *testing.T) {
	r := newTestRegistry(t)
	connectProxy(t, r, "pod-a")
	r.ReportVoiceTotals("pod-a", map[string]int64{"relayed": 100})
	got := r.ReportVoiceTotals("pod-a", map[string]int64{"relayed": 7})
	if got["relayed"] != 7 {
		t.Fatalf("after a restart = %v; want relayed 7", got)
	}
}

func TestUnknownVoiceReasonsAreDropped(t *testing.T) {
	r := newTestRegistry(t)
	connectProxy(t, r, "pod-a")
	got := r.ReportVoiceTotals("pod-a", map[string]int64{"anything-a-plugin-says": 1, "relayed": 1})
	if _, ok := got["anything-a-plugin-says"]; ok || got["relayed"] != 1 {
		t.Fatalf("got %v; an agent must not mint metric label values", got)
	}
}

func TestTheVoicePluginFlagIsInTheSnapshot(t *testing.T) {
	r := newTestRegistry(t)
	connectProxy(t, r, "pod-a")
	r.ReportVoicePlugin("pod-a", true)
	if !r.Lookup("pod-a").VoicePluginLoaded {
		t.Fatal("Lookup lost the flag")
	}
}
```

Replace `newTestRegistry`/`connectProxy` with the helpers `registry_test.go` already uses (read its top); do not add new ones if equivalents exist.

- [ ] **Step 2:** Run `$NIX go test ./internal/agent/ -run 'Voice' -count=1`. Expected: compile failure.

- [ ] **Step 3: Implement the registry**

In `registry.go`: `VoiceReasons` (exported var), on `entry` the fields `voicePlugin bool` and `voiceTotals map[string]int64`, on `Snapshot` `VoicePluginLoaded bool` (filled in `Lookup`), and:

```go
// ReportVoicePlugin records what a proxy said on its Hello. Like readTimeout
// it cannot change without a restart, so it has no timestamp.
func (r *Registry) ReportVoicePlugin(key string, loaded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[key]; ok && e.role == RoleProxy {
		e.voicePlugin = loaded
	}
}

// ReportVoiceTotals turns a proxy's cumulative relay totals into increments.
// Kept per pod, not per stream: during a renewal both streams report the
// same process's totals.
func (r *Registry) ReportVoiceTotals(key string, totals map[string]int64) map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok || e.role != RoleProxy {
		return nil
	}
	if e.voiceTotals == nil {
		e.voiceTotals = make(map[string]int64, len(VoiceReasons))
	}
	out := make(map[string]int64, len(VoiceReasons))
	for _, reason := range VoiceReasons {
		now, reported := totals[reason]
		if !reported || now < 0 {
			continue
		}
		last := e.voiceTotals[reason]
		if now < last {
			out[reason] = now
		} else {
			out[reason] = now - last
		}
		e.voiceTotals[reason] = now
	}
	return out
}
```

- [ ] **Step 4: agentserver**

`metrics.go`:

```go
// No pod or player label; reason is one of agent.VoiceReasons.
var VoicePackets = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spawnery_voice_packets_total",
		Help: "Voice chat packets the proxies' relays handled, by proxy group and by what became of them.",
	},
	[]string{"group", "reason"},
)
```

registered in `init()` beside the others. In `handleProxy`: in the `Hello` case add `s.opts.Agents.ReportVoicePlugin(id.PodUID, m.Hello.GetVoicePluginLoaded())`; add

```go
	case *agentpb.ProxyMessage_VoiceCounters:
		for reason, n := range s.opts.Agents.ReportVoiceTotals(id.PodUID, m.VoiceCounters.GetTotals()) {
			if n > 0 {
				VoicePackets.WithLabelValues(id.Group, reason).Add(float64(n))
			}
		}
```

Test in the agentserver proxy test file: send a `Hello{VoicePluginLoaded: true}` then `VoiceCounters{"relayed": 3, "bogus": 9}` over the same in-process stream the other proxy tests use; assert `testutil.ToFloat64(VoicePackets.WithLabelValues("<the test group>", "relayed")) == 3` (read the value before and compare the delta, since the counter is global) and that the registry snapshot has `VoicePluginLoaded`. Check `hack/metrics-docs.sh` picks the new metric up: `$NIX make manifests` and commit the `docs/reference/metrics-and-alerts.md` diff.

- [ ] **Step 5: proxyreg**

In `fleet.go`, `snapshot` already reads the ProxyGroup for its fallbacks. Replace `f.fallbacks(...)` there with one read of the group and the network:

```go
func (f *Fleet) voiceConfig(ctx context.Context, namespace, group string) *agentpb.OperatorToProxy {
	pg := &spawneryv1alpha1.ProxyGroup{}
	if err := f.opts.Reader.Get(ctx, types.NamespacedName{Name: group, Namespace: namespace}, pg); err != nil {
		return nil
	}
	network := &spawneryv1alpha1.Network{}
	if err := f.opts.Reader.Get(ctx, types.NamespacedName{Name: pg.Spec.NetworkRef.Name, Namespace: namespace}, network); err != nil {
		return nil
	}
	cfg := &agentpb.VoiceConfig{}
	if port, on := network.VoiceBackendPort(); on {
		cfg.Enabled, cfg.BackendPort = true, port
		if ep, known := voice.Announce(network, pg); known {
			cfg.AnnounceHost, cfg.AnnouncePort, cfg.AnnounceOwnNode = ep.Host, ep.Port, ep.OwnNode
		}
	}
	return &agentpb.OperatorToProxy{Message: &agentpb.OperatorToProxy_VoiceConfig{VoiceConfig: cfg}}
}
```

Append its result in `snapshot` right after the `DrainPlayers` messages and before the `NetworkState` (a nil result is skipped). A group that cannot be read sends no `VoiceConfig`; the agent keeps what it had, which is the same rule `fallbacks` follows. The resync every 30 s carries a late LoadBalancer address to running proxies.

Every session now gets one more message per snapshot, so existing `fleet_test.go` cases that assert the exact message list need the `VoiceConfig` added at its place; change those expectations, not the order.

Tests in `fleet_test.go` (use its fake reader setup with a Network and a ProxyGroup):
- `TestASessionInAVoiceNetworkGetsItsVoiceConfig`: network with `Voice{BackendPort: 24454}`, LoadBalancer group; the join snapshot holds a `VoiceConfig{Enabled: true, BackendPort: 24454, AnnouncePort: 25565}`.
- `TestASessionWithoutVoiceIsToldVoiceIsOff`: network without voice; `VoiceConfig{Enabled: false}` present (an explicit off, so a proxy whose network turned voice off stops relaying).
- `TestTheVoiceConfigComesBeforeTheNetworkState`: index of `VoiceConfig` < index of `NetworkState`.

- [ ] **Step 6: `voicePluginPod`** in `controller/voice.go`:

```go
func voicePluginPod(agents *agent.Registry, pods []corev1.Pod) string {
	for i := range pods {
		snap := agents.Lookup(string(pods[i].UID))
		if snap.Connected && snap.VoicePluginLoaded {
			return pods[i].Name
		}
	}
	return ""
}
```

Add an envtest case to `voice_envtest_test.go`: register a connected proxy agent for one of the group's pods in the fixture's registry with `ReportVoicePlugin(uid, true)` (see how other envtests feed `r.Agents`, e.g. `git grep -n "Agents.Connect\|ReportPlayers" internal/controller/*_test.go`), reconcile, expect `Voice` False/`VoicePluginConflict` naming that pod.

- [ ] **Step 7: Run and commit**

Run: `$NIX go test ./internal/agent/ ./internal/proxyreg/ -count=1`, then `./internal/agentserver/` and `./internal/controller/` each alone with `-p 1` on dev.
Expected: PASS.

```bash
git add internal/ docs/reference/metrics-and-alerts.md
git commit  # feat(agentserver): voice config to proxies, counters and the plugin conflict back
```

---

### Task 8: The renderer keeps Velocity's query off the relay port

**Files:**
- Modify: `internal/render/values.go`, `internal/render/velocity.go`, `internal/render/velocity_test.go`
- Modify: `internal/controller/proxygroup_controller.go` (`proxyConfigValues`)

**Interfaces:**
- Produces: `Values.VoiceRelay bool` (`yaml:"voiceRelay,omitempty" json:"voiceRelay,omitempty"`); `proxyConfigValues(network, group)`.

- [ ] **Step 1: Failing tests** in `velocity_test.go` (use the file's helper that builds valid `Values`, e.g. the one other tests pass to `Velocity`):

```go
func TestAVoiceProxyRefusesQueryOnTheRelayPort(t *testing.T) {
	v := validProxyValues()   // the file's existing helper
	v.VoiceRelay = true
	_, err := Velocity(v, "/etc/spawnery/forwarding.secret", map[string]string{
		"velocity.toml": "[query]\nenabled = true\nport = 25565\n",
	})
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("err = %v; want a refusal naming query", err)
	}
}

func TestAVoiceProxyAcceptsQueryElsewhere(t *testing.T) {
	v := validProxyValues()
	v.VoiceRelay = true
	if _, err := Velocity(v, "/etc/spawnery/forwarding.secret", map[string]string{
		"velocity.toml": "[query]\nenabled = true\nport = 25570\n",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQueryOnTheGamePortIsFineWithoutVoice(t *testing.T) {
	if _, err := Velocity(validProxyValues(), "/etc/spawnery/forwarding.secret", map[string]string{
		"velocity.toml": "[query]\nenabled = true\nport = 25565\n",
	}); err != nil {
		t.Fatal(err)
	}
}
```

Add a fourth case with `[query]\nenabled = true\n` and no port: refused with voice on, if Velocity's default query port is 25565. Read the captured default config the renderer checks keys against (`git grep -n "velocityDeclared" internal/render`, then open the captured file it is built from) and take the default port from there; write the test to that number.

- [ ] **Step 2:** `$NIX go test ./internal/render/ -run Query -count=1` → compile failure on `VoiceRelay`.

- [ ] **Step 3: Implement.** `Values.VoiceRelay` as above. In `velocityToml`, after `checkTransferVersion`:

```go
		if err := checkQueryPort(v, fragment); err != nil {
			return "", err
		}
```

```go
// Velocity's query listens on UDP as well, and on the relay's port by default.
func checkQueryPort(v Values, fragment map[string]any) error {
	if !v.VoiceRelay {
		return nil
	}
	query, ok := fragment["query"].(map[string]any)
	if !ok || query["enabled"] != true {
		return nil
	}
	port := int64(velocityDefaultQueryPort)
	if p, ok := query["port"].(int64); ok {
		port = p
	}
	if port == 25565 {
		return fmt.Errorf("velocity.toml: the overlay enables [query] on UDP 25565, where this network's voice relay listens; move query.port")
	}
	return nil
}
```

with `velocityDefaultQueryPort` the number found in Step 1. `proxyConfigValues(network, group)` sets `VoiceRelay` from `network.VoiceBackendPort()`; update its three callers (`reconcileReplicas`, `reconcileNetworkPolicy`, `reconcileConfigMap` — the latter needs the network passed in from `reconcileObserved`).

- [ ] **Step 4:** `$NIX go test ./internal/render/ -count=1`, `$NIX go build ./...`, and `$NIX go test ./internal/podspec/ -run Golden -count=1` (the omitempty field must not move a golden). Expected: PASS.
- [ ] **Step 5:** Commit `feat(render): refuse Velocity query on the voice relay's port`.

---

### Task 9: SVC wire format in Kotlin

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/voice/VoiceWire.kt`
- Create: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/voice/VoiceWireTest.kt`

**Interfaces:**
- Produces (package `cloud.spawnery.agent.velocity.voice`):
  - `object VoiceWire { const val MAGIC: Byte = 0xFF.toByte(); const val MIN_FRAME = 17; val PING_UUID: UUID; const val SECRET_CHANNEL = "voicechat:secret"; const val REQUEST_SECRET_CHANNEL = "voicechat:request_secret"; const val PATCHED_VERSION = 20 }`
  - `fun frameUuid(packet: ByteBuffer): UUID?` — null unless magic and length hold; does not move the buffer's position
  - `fun compatibilityVersion(requestSecret: ByteArray): Int?`
  - `sealed interface SecretPatch { data class Patched(val bytes: ByteArray) ; data object Untouched }`
  - `fun patchSecret(packet: ByteArray, host: String, port: Int): SecretPatch` — rewrites `serverPort` and `voiceHost` only if the packet parses exactly as version 20's layout

- [ ] **Step 1: Failing tests**

```kotlin
package cloud.spawnery.agent.velocity.voice

import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.io.ByteArrayOutputStream
import java.io.DataOutputStream
import java.nio.ByteBuffer
import java.util.UUID

/**
 * Field order of SVC's SecretPacket at compatibility version 20
 * (bukkit/src/main/java/de/maxhenkel/voicechat/net/SecretPacket.java, toBytes).
 */
private fun secretV20(port: Int, host: String, tail: ByteArray = byteArrayOf(1)): ByteArray {
    val out = ByteArrayOutputStream()
    DataOutputStream(out).use { d ->
        d.write(ByteArray(16) { it.toByte() })    // secret
        d.writeInt(port)                          // serverPort
        d.writeLong(0x1122334455667788)           // player UUID msb
        d.writeLong(-0x1122334455667788)          // player UUID lsb
        d.writeByte(0)                            // codec
        d.writeInt(1024)                          // mtuSize
        d.writeDouble(48.0)                       // voiceChatDistance
        d.writeInt(1000)                          // keepAlive
        d.writeBoolean(true)                      // groupsEnabled
        val utf = host.toByteArray(Charsets.UTF_8)
        writeVarInt(d, utf.size)
        d.write(utf)                              // voiceHost
        d.write(tail)                             // allowRecording (+ whatever a newer version adds)
    }
    return out.toByteArray()
}

private fun writeVarInt(d: DataOutputStream, value: Int) {
    var v = value
    while (v and 0x7F.inv() != 0) {
        d.writeByte((v and 0x7F) or 0x80)
        v = v ushr 7
    }
    d.writeByte(v)
}

class VoiceWireTest {
    @Test
    fun `a frame yields its cleartext uuid`() {
        val id = UUID.randomUUID()
        val buf = ByteBuffer.allocate(40).put(VoiceWire.MAGIC)
            .putLong(id.mostSignificantBits).putLong(id.leastSignificantBits).put(ByteArray(23)).flip()
        assertEquals(id, frameUuid(buf))
        assertEquals(0, buf.position())
    }

    @Test
    fun `wrong magic or a short packet is no frame`() {
        assertNull(frameUuid(ByteBuffer.wrap(ByteArray(40))))
        assertNull(frameUuid(ByteBuffer.wrap(byteArrayOf(VoiceWire.MAGIC) + ByteArray(15))))
    }

    @Test
    fun `the request carries the compatibility version as its first int`() {
        assertEquals(20, compatibilityVersion(byteArrayOf(0, 0, 0, 20)))
        assertNull(compatibilityVersion(byteArrayOf(0, 0)))
    }

    @Test
    fun `a version 20 secret gets the new port and host and keeps everything else`() {
        val patched = patchSecret(secretV20(24454, ""), "voice.example.test", 25565)
        assertArrayEquals(secretV20(25565, "voice.example.test"), (patched as SecretPatch.Patched).bytes)
    }

    @Test
    fun `an empty host is written as an empty string`() {
        val patched = patchSecret(secretV20(24454, "backend.internal:24454"), "", 30765)
        assertArrayEquals(secretV20(30765, ""), (patched as SecretPatch.Patched).bytes)
    }

    @Test
    fun `a longer tail than version 20's is left untouched`() {
        assertEquals(SecretPatch.Untouched, patchSecret(secretV20(24454, "", byteArrayOf(1, 0, 0, 0, 7)), "", 25565))
    }

    @Test
    fun `a truncated secret is left untouched`() {
        val full = secretV20(24454, "abc")
        assertEquals(SecretPatch.Untouched, patchSecret(full.copyOf(full.size - 2), "", 25565))
    }

    @Test
    fun `a tail byte other than a boolean is left untouched`() {
        assertEquals(SecretPatch.Untouched, patchSecret(secretV20(24454, "", byteArrayOf(5)), "", 25565))
    }
}
```

- [ ] **Step 2:** Run: `nix --extra-experimental-features 'nix-command flakes' build /home/paul/git/spawnery#agents --no-link` after `git add` of the new files (the agents build runs the JUnit suites). Expected: compile failure in `VoiceWireTest`.

- [ ] **Step 3: Implement `VoiceWire.kt`**

```kotlin
package cloud.spawnery.agent.velocity.voice

import java.io.ByteArrayOutputStream
import java.nio.ByteBuffer
import java.util.UUID

object VoiceWire {
    const val MAGIC: Byte = 0xFF.toByte()
    const val MIN_FRAME = 17
    /** SVC answers this UUID as a ping; the relay drops and counts it. */
    val PING_UUID: UUID = UUID.fromString("58bc9ae9-c7a8-45e4-a11c-efbb67199425")
    const val SECRET_CHANNEL = "voicechat:secret"
    const val REQUEST_SECRET_CHANNEL = "voicechat:request_secret"
    const val PATCHED_VERSION = 20

    internal const val PORT_OFFSET = 16
    internal const val HOST_OFFSET = 54
    private const val MAX_HOST_BYTES = 32767 * 3
}

fun frameUuid(packet: ByteBuffer): UUID? {
    val start = packet.position()
    if (packet.remaining() < VoiceWire.MIN_FRAME || packet.get(start) != VoiceWire.MAGIC) return null
    return UUID(packet.getLong(start + 1), packet.getLong(start + 9))
}

fun compatibilityVersion(requestSecret: ByteArray): Int? =
    if (requestSecret.size < 4) null else ByteBuffer.wrap(requestSecret).int

sealed interface SecretPatch {
    class Patched(val bytes: ByteArray) : SecretPatch
    data object Untouched : SecretPatch
}

fun patchSecret(packet: ByteArray, host: String, port: Int): SecretPatch {
    if (packet.size < VoiceWire.HOST_OFFSET + 2) return SecretPatch.Untouched
    val (length, varIntSize) = readVarInt(packet, VoiceWire.HOST_OFFSET) ?: return SecretPatch.Untouched
    val tailAt = VoiceWire.HOST_OFFSET + varIntSize + length
    if (length < 0 || tailAt + 1 != packet.size) return SecretPatch.Untouched
    if (packet[tailAt] != 0.toByte() && packet[tailAt] != 1.toByte()) return SecretPatch.Untouched

    val out = ByteArrayOutputStream(packet.size + host.length)
    out.write(packet, 0, VoiceWire.PORT_OFFSET)
    out.write(ByteBuffer.allocate(4).putInt(port).array())
    out.write(packet, VoiceWire.PORT_OFFSET + 4, VoiceWire.HOST_OFFSET - VoiceWire.PORT_OFFSET - 4)
    val utf = host.toByteArray(Charsets.UTF_8)
    writeVarInt(out, utf.size)
    out.write(utf)
    out.write(packet[tailAt].toInt())
    return SecretPatch.Patched(out.toByteArray())
}

private fun readVarInt(bytes: ByteArray, at: Int): Pair<Int, Int>? {
    var value = 0
    for (i in 0 until 5) {
        if (at + i >= bytes.size) return null
        val b = bytes[at + i].toInt()
        value = value or ((b and 0x7F) shl (7 * i))
        if (b and 0x80 == 0) return value to (i + 1)
    }
    return null
}

private fun writeVarInt(out: ByteArrayOutputStream, value: Int) {
    var v = value
    while (v and 0x7F.inv() != 0) {
        out.write((v and 0x7F) or 0x80)
        v = v ushr 7
    }
    out.write(v)
}
```

Remove `MAX_HOST_BYTES` if nothing uses it.

- [ ] **Step 4:** Rebuild `#agents`. Expected: PASS, `VoiceWireTest` listed as passed in the log.
- [ ] **Step 5:** Commit `feat(agent): Simple Voice Chat's frame and secret layout`.

---

### Task 10: The relay

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/voice/Bridges.kt` (pure decisions)
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/voice/VoiceRelay.kt` (NIO)
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/voice/VoiceCounters.kt`
- Create: tests `BridgesTest.kt`, `VoiceRelayTest.kt` in the test tree's `voice` package

**Interfaces:**
- Consumes: Task 9.
- Produces:
  - `class VoiceCounters { fun inc(reason: String); fun totals(): Map<String, Long> }` with constants `RELAYED`, `NOT_SVC`, `UNKNOWN_PLAYER`, `UNKNOWN_SERVER`, `UNKNOWN_VERSION`, `PING`
  - `sealed interface Backend { data class At(val address: InetSocketAddress); data object UnknownPlayer; data object UnknownServer }`
  - `class Bridges(private val clock: () -> Long)` with `fun route(player: UUID, from: InetSocketAddress, backend: InetSocketAddress): Route`, `fun replyTo(player: UUID): InetSocketAddress?`, `fun expired(): List<UUID>`, `fun touch(player: UUID)`, `fun remove(player: UUID)`; `sealed interface Route { data class Existing(..); data class Open(val replaces: Boolean); }`; constants `TAKEOVER_MILLIS = 3_000`, `IDLE_MILLIS = 15_000`
  - `class VoiceRelay(bind: InetSocketAddress, resolve: (UUID) -> Backend, counters: VoiceCounters, log: (String, Throwable?) -> Unit, clock: () -> Long = System::currentTimeMillis) : AutoCloseable` with `fun start()`, `val localPort: Int`

`Bridges` rules (spec §5):
- New player → `Open`.
- Same player, same backend → `Existing`; the reply address becomes `from` only if `from` equals it, or the current one has been silent for `TAKEOVER_MILLIS`.
- Same player, different backend → `Open(replaces = true)`; the reply address is `from`.
- `expired()` lists players with no traffic either way for `IDLE_MILLIS`.

- [ ] **Step 1: Failing pure tests** `BridgesTest.kt`:

```kotlin
class BridgesTest {
    private var now = 0L
    private val bridges = Bridges { now }
    private val player = UUID.randomUUID()
    private val client = InetSocketAddress("192.0.2.10", 40000)
    private val rebound = InetSocketAddress("192.0.2.10", 40001)
    private val lobby = InetSocketAddress("10.0.0.1", 24454)
    private val game = InetSocketAddress("10.0.0.2", 24454)

    @Test
    fun `a new player opens a bridge`() {
        assertEquals(Route.Open(replaces = false), bridges.route(player, client, lobby))
        assertEquals(client, bridges.replyTo(player))
    }

    @Test
    fun `an active client keeps its reply path against another source`() {
        bridges.route(player, client, lobby)
        now += 2_999
        bridges.route(player, client, lobby)
        now += 1_000
        assertTrue(bridges.route(player, rebound, lobby) is Route.Existing)
        assertEquals(client, bridges.replyTo(player))
    }

    @Test
    fun `a source silent for three seconds hands the reply path over`() {
        bridges.route(player, client, lobby)
        now += 3_000
        bridges.route(player, rebound, lobby)
        assertEquals(rebound, bridges.replyTo(player))
    }

    @Test
    fun `a new backend replaces the bridge`() {
        bridges.route(player, client, lobby)
        assertEquals(Route.Open(replaces = true), bridges.route(player, client, game))
    }

    @Test
    fun `fifteen silent seconds expire a bridge, and a reply keeps it alive`() {
        bridges.route(player, client, lobby)
        now += 14_000
        bridges.touch(player)
        now += 14_000
        assertEquals(emptyList<UUID>(), bridges.expired())
        now += 1_000
        assertEquals(listOf(player), bridges.expired())
    }
}
```

`Route.Existing` carries nothing the tests compare; make it a `data object`.

- [ ] **Step 2: Implement `Bridges.kt` and `VoiceCounters.kt`**

```kotlin
package cloud.spawnery.agent.velocity.voice

import java.net.InetSocketAddress
import java.util.UUID

sealed interface Route {
    data object Existing : Route
    data class Open(val replaces: Boolean) : Route
}

/** Not thread-safe: owned by the relay's selector thread. */
class Bridges(private val clock: () -> Long) {
    private class State(var replyTo: InetSocketAddress, val backend: InetSocketAddress, var heardFromClient: Long, var lastTraffic: Long)

    private val states = HashMap<UUID, State>()

    fun route(player: UUID, from: InetSocketAddress, backend: InetSocketAddress): Route {
        val now = clock()
        val state = states[player]
        if (state == null || state.backend != backend) {
            states[player] = State(from, backend, now, now)
            return Route.Open(replaces = state != null)
        }
        if (from == state.replyTo) {
            state.heardFromClient = now
        } else if (now - state.heardFromClient >= TAKEOVER_MILLIS) {
            state.replyTo = from
            state.heardFromClient = now
        }
        state.lastTraffic = now
        return Route.Existing
    }

    fun replyTo(player: UUID): InetSocketAddress? = states[player]?.replyTo

    fun touch(player: UUID) {
        states[player]?.lastTraffic = clock()
    }

    fun expired(): List<UUID> {
        val now = clock()
        return states.filterValues { now - it.lastTraffic >= IDLE_MILLIS }.keys.toList()
    }

    fun remove(player: UUID) {
        states.remove(player)
    }

    companion object {
        /** The client echoes the backend's keepalive every second. */
        const val TAKEOVER_MILLIS = 3_000L
        /** Above SVC's 10 s timeout, so a bridge never ends under a live connection. */
        const val IDLE_MILLIS = 15_000L
    }
}
```

```kotlin
package cloud.spawnery.agent.velocity.voice

import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

class VoiceCounters {
    private val counts = ConcurrentHashMap<String, AtomicLong>()

    fun inc(reason: String) {
        counts.computeIfAbsent(reason) { AtomicLong() }.incrementAndGet()
    }

    fun totals(): Map<String, Long> = counts.mapValues { it.value.get() }

    companion object {
        const val RELAYED = "relayed"
        const val NOT_SVC = "not_svc"
        const val UNKNOWN_PLAYER = "unknown_player"
        const val UNKNOWN_SERVER = "unknown_server"
        const val UNKNOWN_VERSION = "unknown_version"
        const val PING = "ping"
    }
}
```

- [ ] **Step 3: Failing IO test** `VoiceRelayTest.kt`, over loopback:

```kotlin
class VoiceRelayTest {
    private val player = UUID.randomUUID()

    /** A backend that sends every datagram straight back, prefixed with its own port. */
    private fun echoBackend(): DatagramSocket {
        val socket = DatagramSocket(0, InetAddress.getLoopbackAddress())
        thread(isDaemon = true) {
            val buf = ByteArray(2048)
            while (!socket.isClosed) {
                val p = DatagramPacket(buf, buf.size)
                runCatching { socket.receive(p) }.onFailure { return@thread }
                val reply = ByteBuffer.allocate(p.length + 4).putInt(socket.localPort).put(buf, 0, p.length).array()
                socket.send(DatagramPacket(reply, reply.size, p.socketAddress))
            }
        }
        return socket
    }

    private fun frame(id: UUID, body: String) =
        ByteBuffer.allocate(17 + body.length).put(VoiceWire.MAGIC)
            .putLong(id.mostSignificantBits).putLong(id.leastSignificantBits).put(body.toByteArray()).array()

    private fun roundTrip(client: DatagramSocket, relayPort: Int, packet: ByteArray): ByteBuffer? {
        client.send(DatagramPacket(packet, packet.size, InetAddress.getLoopbackAddress(), relayPort))
        val buf = ByteArray(2048)
        val p = DatagramPacket(buf, buf.size)
        return runCatching { client.receive(p); ByteBuffer.wrap(buf, 0, p.length) }.getOrNull()
    }

    @Test
    fun `a packet reaches the player's backend and the reply comes back`() {
        val backend = echoBackend()
        val counters = VoiceCounters()
        VoiceRelay(InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { if (it == player) Backend.At(backend.localSocketAddress as InetSocketAddress) else Backend.UnknownPlayer },
            counters = counters, log = { _, _ -> }).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 2_000
                val reply = roundTrip(client, relay.localPort, frame(player, "hello"))!!
                assertEquals(backend.localPort, reply.int)
                assertEquals(VoiceWire.MAGIC, reply.get())
            }
            assertEquals(1L, counters.totals()[VoiceCounters.RELAYED])
        }
        backend.close()
    }

    @Test
    fun `a switch of backend reaches the new one`() {
        val lobby = echoBackend()
        val game = echoBackend()
        var current = lobby
        VoiceRelay(InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { Backend.At(current.localSocketAddress as InetSocketAddress) },
            counters = VoiceCounters(), log = { _, _ -> }).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 2_000
                assertEquals(lobby.localPort, roundTrip(client, relay.localPort, frame(player, "a"))!!.int)
                current = game
                assertEquals(game.localPort, roundTrip(client, relay.localPort, frame(player, "b"))!!.int)
            }
        }
        lobby.close(); game.close()
    }

    @Test
    fun `garbage opens nothing and is counted`() {
        val counters = VoiceCounters()
        var resolved = 0
        VoiceRelay(InetSocketAddress(InetAddress.getLoopbackAddress(), 0),
            resolve = { resolved++; Backend.UnknownPlayer },
            counters = counters, log = { _, _ -> }).use { relay ->
            relay.start()
            DatagramSocket().use { client ->
                client.soTimeout = 300
                assertNull(roundTrip(client, relay.localPort, ByteArray(40)))
                assertNull(roundTrip(client, relay.localPort, byteArrayOf(VoiceWire.MAGIC, 1, 2)))
                assertNull(roundTrip(client, relay.localPort, frame(VoiceWire.PING_UUID, "x")))
                assertNull(roundTrip(client, relay.localPort, frame(UUID.randomUUID(), "x")))
            }
        }
        assertEquals(2L, counters.totals()[VoiceCounters.NOT_SVC])
        assertEquals(1L, counters.totals()[VoiceCounters.PING])
        assertEquals(1L, counters.totals()[VoiceCounters.UNKNOWN_PLAYER])
        assertEquals(1, resolved)
    }
}
```

The garbage test is Review Focus 3: the resolver is never asked for anything that is not a frame.

- [ ] **Step 4: Implement `VoiceRelay.kt`**

```kotlin
package cloud.spawnery.agent.velocity.voice

import java.net.InetSocketAddress
import java.net.StandardProtocolFamily
import java.nio.ByteBuffer
import java.nio.channels.DatagramChannel
import java.nio.channels.SelectionKey
import java.nio.channels.Selector
import java.util.UUID

sealed interface Backend {
    data class At(val address: InetSocketAddress) : Backend
    data object UnknownPlayer : Backend
    data object UnknownServer : Backend
}

/**
 * One selector thread owns every socket and [Bridges]; nothing else touches
 * them. Per player, one connected channel towards the backend, so the backend
 * sees one stable source for each player, as SVC's own proxy arranges it.
 */
class VoiceRelay(
    bind: InetSocketAddress,
    private val resolve: (UUID) -> Backend,
    private val counters: VoiceCounters,
    private val log: (String, Throwable?) -> Unit,
    clock: () -> Long = System::currentTimeMillis,
) : AutoCloseable {
    private val selector = Selector.open()
    private val public = DatagramChannel.open(if (bind.address is java.net.Inet6Address) StandardProtocolFamily.INET6 else StandardProtocolFamily.INET)
        .bind(bind).also { it.configureBlocking(false) }
    private val bridges = Bridges(clock)
    private val upstream = HashMap<UUID, DatagramChannel>()
    private val buffer = ByteBuffer.allocateDirect(MAX_DATAGRAM)
    @Volatile private var running = false
    private var thread: Thread? = null

    val localPort: Int get() = (public.localAddress as InetSocketAddress).port

    fun start() {
        public.register(selector, SelectionKey.OP_READ, null)
        running = true
        thread = Thread(::loop, "spawnery-voice").apply { isDaemon = true; start() }
    }

    private fun loop() {
        while (running) {
            try {
                selector.select(SWEEP_MILLIS)
                val keys = selector.selectedKeys().iterator()
                while (keys.hasNext()) {
                    val key = keys.next()
                    keys.remove()
                    val player = key.attachment() as UUID?
                    if (player == null) fromClient() else fromBackend(player, key.channel() as DatagramChannel)
                }
                for (expired in bridges.expired()) close(expired)
            } catch (e: Exception) {
                if (running) log("spawnery: the voice relay's loop failed once; it keeps running", e)
            }
        }
    }

    private fun fromClient() {
        while (true) {
            buffer.clear()
            val from = public.receive(buffer) as InetSocketAddress? ?: return
            buffer.flip()
            val player = frameUuid(buffer)
            when {
                player == null -> counters.inc(VoiceCounters.NOT_SVC)
                player == VoiceWire.PING_UUID -> counters.inc(VoiceCounters.PING)
                else -> forward(player, from)
            }
        }
    }

    private fun forward(player: UUID, from: InetSocketAddress) {
        val backend = when (val b = resolve(player)) {
            is Backend.At -> b.address
            Backend.UnknownPlayer -> return counters.inc(VoiceCounters.UNKNOWN_PLAYER)
            Backend.UnknownServer -> return counters.inc(VoiceCounters.UNKNOWN_SERVER)
        }
        if (bridges.route(player, from, backend) is Route.Open) {
            upstream.remove(player)?.close()
            val channel = DatagramChannel.open().connect(backend).also { it.configureBlocking(false) }
            channel.register(selector, SelectionKey.OP_READ, player)
            upstream[player] = channel
        }
        upstream[player]?.write(buffer)
        counters.inc(VoiceCounters.RELAYED)
    }

    private fun fromBackend(player: UUID, channel: DatagramChannel) {
        while (true) {
            buffer.clear()
            val read = runCatching { channel.read(buffer) }.getOrElse { -1 }
            if (read <= 0) return
            buffer.flip()
            val client = bridges.replyTo(player) ?: return
            bridges.touch(player)
            public.send(buffer, client)
        }
    }

    private fun close(player: UUID) {
        bridges.remove(player)
        upstream.remove(player)?.close()
    }

    override fun close() {
        running = false
        selector.wakeup()
        thread?.join(2_000)
        upstream.values.forEach { it.close() }
        public.close()
        selector.close()
    }

    private companion object {
        /** SVC's mtu_size tops out at 2048 bytes of Opus plus framing. */
        const val MAX_DATAGRAM = 4096
        const val SWEEP_MILLIS = 1_000L
    }
}
```

A `read` on a connected UDP channel throws `PortUnreachableException` after an ICMP refusal; `runCatching` ends that read and the bridge stays until it expires or the backend changes.

- [ ] **Step 5:** `git add` the new files, rebuild `#agents`. Expected: `BridgesTest` and `VoiceRelayTest` pass. Mutation check in a throwaway worktree: make `Bridges.route` always adopt `from`; record that `an active client keeps its reply path against another source` FAILS; remove the worktree.
- [ ] **Step 6:** Commit `feat(agent): a UDP relay that routes any player by the UUID in the frame`.

---

### Task 11: Wiring the relay into the Velocity agent

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/voice/VoiceControl.kt`
- Create: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/voice/VoiceControlTest.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ProxyRole.kt`, its test `ProxyRoleTest.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ServerDirectory.kt` (`hostOf`)

**Interfaces:**
- Consumes: Tasks 6, 9, 10.
- Produces:
  - `ServerDirectory.hostOf(server: String): String?` — the registered backend's host, `@Synchronized`
  - `class VoiceControl(startRelay: (Int) -> AutoCloseable, hostIp: String?, counters: VoiceCounters, log)` with `fun apply(config: VoiceConfig)`, `fun secretFor(player: UUID, packet: ByteArray): ByteArray?` (null = forward unchanged), `fun recordVersion(player: UUID, request: ByteArray)`, `fun forget(player: UUID)`, `fun backendPort(): Int`, `fun report(): ProxyMessage?`
  - `ProxyRole` gains constructor params with defaults: `voicePluginLoaded: Boolean = false`, `voice: VoiceControl? = null`; `hello` sets `voicePluginLoaded`; `VOICE_CONFIG` messages go to `voice.apply`; `extraReports` appends `voice.report()` when non-null.

Behaviour of `VoiceControl`:
- `apply(enabled = true)` while stopped → `startRelay(25565)`; `apply(enabled = false)` while running → close it. A failing bind is logged once per attempt and leaves the relay stopped (the next resync retries).
- `secretFor`: no config, or `announce_port == 0` → null. Own-node form needs `hostIp`; without it → null and a log line. Version recorded and equal to `PATCHED_VERSION` → patch. Otherwise patch only if `patchSecret` returns `Patched`, and count `unknown_version` (logged once per player). `Untouched` → null and count `unknown_version`.
- `report()` → `ProxyMessage` with `VoiceCounters(totals)` while a config has ever said enabled, else null.

- [ ] **Step 1: Failing tests** `VoiceControlTest.kt`:

```kotlin
class VoiceControlTest {
    private val player = UUID.randomUUID()
    private var started = 0
    private var closed = 0
    private val counters = VoiceCounters()
    private fun control(hostIp: String? = "192.0.2.5") = VoiceControl(
        startRelay = { port -> assertEquals(25565, port); started++; AutoCloseable { closed++ } },
        hostIp = hostIp, counters = counters, log = { _, _ -> },
    )
    private fun config(enabled: Boolean = true, host: String = "", port: Int = 25565, ownNode: Boolean = false) =
        VoiceConfig.newBuilder().setEnabled(enabled).setBackendPort(24454)
            .setAnnounceHost(host).setAnnouncePort(port).setAnnounceOwnNode(ownNode).build()

    @Test
    fun `enabled starts the relay once, disabled stops it`() {
        val c = control()
        c.apply(config()); c.apply(config())
        assertEquals(1, started)
        c.apply(config(enabled = false))
        assertEquals(1, closed)
    }

    @Test
    fun `a version 20 client's secret points at the announced port`() {
        val c = control()
        c.apply(config(port = 30765))
        c.recordVersion(player, byteArrayOf(0, 0, 0, 20))
        val patched = c.secretFor(player, secretV20(24454, "x"))!!
        assertArrayEquals(secretV20(30765, ""), patched)
    }

    @Test
    fun `own node announces this pod's node`() {
        val c = control(hostIp = "192.0.2.5")
        c.apply(config(port = 25577, ownNode = true))
        c.recordVersion(player, byteArrayOf(0, 0, 0, 20))
        assertArrayEquals(secretV20(25577, "192.0.2.5"), c.secretFor(player, secretV20(24454, ""))!!)
    }

    @Test
    fun `an unknown endpoint forwards the secret unchanged`() {
        val c = control()
        c.apply(config(port = 0))
        assertNull(c.secretFor(player, secretV20(24454, "")))
    }

    @Test
    fun `an unknown version that parses is patched and counted`() {
        val c = control()
        c.apply(config())
        c.recordVersion(player, byteArrayOf(0, 0, 0, 21))
        assertNotNull(c.secretFor(player, secretV20(24454, "")))
        assertEquals(1L, counters.totals()[VoiceCounters.UNKNOWN_VERSION])
    }
}
```

Move `secretV20`/`writeVarInt` from `VoiceWireTest.kt` into a shared `SecretFixtures.kt` in the test `voice` package so both tests use the one fixture.

- [ ] **Step 2: Implement `VoiceControl.kt`** to exactly the behaviour above (store the config in an `@Volatile` field; versions and logged-once players in `ConcurrentHashMap`s, since plugin messages arrive on Velocity's network threads and `apply` on the gRPC thread; start/stop under `@Synchronized`).

- [ ] **Step 3: `ServerDirectory.hostOf`**

```kotlin
    @Synchronized
    fun hostOf(server: String): String? =
        backends[server.lowercase()]?.let { parseAddress(it.address)?.hostString }
```

with a `ServerDirectoryTest` case: after `apply(listOf(Backend("lobby-1", "10.0.0.1:25565", "lobby")))`, `hostOf("LOBBY-1") == "10.0.0.1"` and `hostOf("nope") == null`.

- [ ] **Step 4: `ProxyRole`**: add the two constructor parameters (with defaults, last), set `.setVoicePluginLoaded(voicePluginLoaded)` in `hello`, add

```kotlin
            OperatorToProxy.MessageCase.VOICE_CONFIG -> {
                voice?.apply(message.voiceConfig)
                Directive.None
            }
```

and in `extraReports` return `listOfNotNull(backends, roster, voice?.report())`. `ProxyRoleTest`: a `VoiceConfig` message reaches a `VoiceControl` built with a counting `startRelay`; `hello` carries `voicePluginLoaded = true` when constructed so.

- [ ] **Step 5: `AgentPlugin`**

- In `start(env)`: `val voicePlugin = proxy.pluginManager.isLoaded("voicechat")`. If the environment has `SPAWNERY_VOICE=1` and `voicePlugin` is false, build

```kotlin
        val voice = VoiceControl(
            startRelay = { port ->
                VoiceRelay(InetSocketAddress(port), resolve = ::resolveVoice, counters = voiceCounters, log = ::warn)
                    .also { it.start() }
            },
            hostIp = System.getenv("SPAWNERY_HOST_IP"),
            counters = voiceCounters,
            log = ::warn,
        )
```

  register the channels `proxy.channelRegistrar.register(MinecraftChannelIdentifier.from(VoiceWire.SECRET_CHANNEL), MinecraftChannelIdentifier.from(VoiceWire.REQUEST_SECRET_CHANNEL))`, and pass `voice` and `voicePluginLoaded = voicePlugin` to `ProxyRole`. With `voicePlugin` true, log one warning naming the conflict and pass `voice = null`.
- `resolveVoice(player: UUID): Backend`:

```kotlin
    private fun resolveVoice(player: UUID): Backend {
        val server = proxy.getPlayer(player).flatMap { it.currentServer }.map { it.serverInfo.name }.orElse(null)
            ?: mirror.players().firstOrNull { it.id() == player }?.server()?.orElse(null)
            ?: return Backend.UnknownPlayer
        val host = directory?.hostOf(server) ?: return Backend.UnknownServer
        val port = voice?.backendPort() ?: return Backend.UnknownServer
        return Backend.At(InetSocketAddress(host, port))
    }
```

  (`InetSocketAddress(host, port)` resolves; backend hosts are pod IPs, so no DNS round trip on the relay thread in a cluster.)
- Plugin messages:

```kotlin
    @Subscribe
    fun onPluginMessage(event: PluginMessageEvent) {
        val voice = voice ?: return
        when (event.identifier.id) {
            VoiceWire.REQUEST_SECRET_CHANNEL ->
                (event.source as? Player)?.let { voice.recordVersion(it.uniqueId, event.data) }
            VoiceWire.SECRET_CHANNEL -> {
                val server = event.source as? ServerConnection ?: return
                val patched = voice.secretFor(server.player.uniqueId, event.data) ?: return
                event.result = PluginMessageEvent.ForwardResult.handled()
                server.player.sendPluginMessage(event.identifier, patched)
            }
        }
    }
```

  and in the existing `onDisconnect`: `voice?.forget(event.player.uniqueId)`. On shutdown: `voice?.apply(VoiceConfig.getDefaultInstance())` to close the relay.

- [ ] **Step 6:** `git add` new files; rebuild `#agents`. Expected: all JUnit suites pass, including the untouched ones (`SubscriptionOrderTest` reads the `@Subscribe` methods; if it lists them explicitly, add `onPluginMessage` to its expectation and say so in the commit).
- [ ] **Step 7:** Commit `feat(agent): relay voice chat and point SVC clients at it`.

---

### Task 12: The relay against a real Velocity image

**Files:**
- Modify: `cmd/spawnery-stubop/main.go`, `cmd/spawnery-stubop/main_test.go`
- Create: `cmd/spawnery-voiceprobe/main.go`, `cmd/spawnery-voiceprobe/main_test.go`
- Modify: `hack/agent-test.sh`
- Modify: `Makefile` (build the probe beside `STUBOP` for `agent-test`; follow how `STUBOP` is built and passed)

**Interfaces:**
- Consumes: Tasks 6, 11.
- Produces:
  - stubop flags `--voice-echo <listen addr>` (a UDP echo, which answers with `"echo:"` + the datagram) and `--voice-player <uuid>`; with them, every proxy stream also receives `VoiceConfig{enabled, backend_port = <echo port>, announce_port 25565}` after the FullSync, the FullSync registers `syncedName` at `stubop:25565`, and the `NetworkState` carries a roster entry `{uuid, "voiceprobe", syncedName}`
  - `spawnery-voiceprobe --to host:port --uuid <uuid> [--timeout 5s]`: sends one SVC frame and exits 0 if the reply starts with `echo:` followed by the same frame, 1 otherwise, printing one JSON line `{"ok":bool,"rtt_ms":n,"error":"..."}`

- [ ] **Step 1: Failing tests**
  - `cmd/spawnery-voiceprobe/main_test.go`: start a UDP echo on loopback in the test (answering `"echo:"+datagram`), run `run([]string{"--to", addr, "--uuid", id}, &out, &errOut)`, expect exit 0 and `"ok":true`; against a closed port expect exit 1 within the timeout.
  - `cmd/spawnery-stubop/main_test.go`: with `--voice-echo 127.0.0.1:0 --voice-player <uuid>`, the proxy stream's opening messages contain a `VoiceConfig` with `Enabled` and the echo's port, and the `NetworkState` has the roster entry. Use the file's existing way of opening an in-process proxy stream.
- [ ] **Step 2:** Implement both (stubop: one goroutine with `net.ListenPacket("udp", addr)` writing `append([]byte("echo:"), buf[:n]...)` back; probe: `net.Dial("udp", to)`, write `0xFF + uuid bytes + "probe"`, read with deadline).
- [ ] **Step 3:** `$NIX go test ./cmd/spawnery-stubop/ ./cmd/spawnery-voiceprobe/ -count=1` → PASS.
- [ ] **Step 4: Harness phase** in `hack/agent-test.sh`, after the existing proxy phase (`NAME4`) and in its style: a new stub (`STUB8`, port 19449, `--proxy`, `--voice-echo :24460 --voice-player <fixed uuid>`), a new Velocity container `NAME8` started with `-e SPAWNERY_VOICE=1 -e SPAWNERY_HOST_IP=127.0.0.1` and published `-p 127.0.0.1::25565/udp`; wait for `full_sync_sent` and the readiness port like the proxy phase does; read the published port with `"$CONTAINER" port "$NAME8" 25565/udp`; run `"$VOICEPROBE" --to "127.0.0.1:$port" --uuid <fixed uuid>`; on failure print the container's `spawnery` log lines and the stub log and exit 1. Add the container/volume/pid to the cleanup trap. The proxy holds no player at all, so a reply proves the claim the multi-proxy design rests on: a proxy routes a player it only knows from `NetworkState`.
- [ ] **Step 5: Run on a machine with podman** (`paul-desktop`; on `dev` it works too but takes long): `$NIX make agent-test CONTAINER=podman`. Expected: the new phase prints its probe JSON with `"ok":true`. Then prove it bites: in a throwaway worktree, make `resolveVoice` ignore the mirror (return `UnknownPlayer` when Velocity does not have the player), rebuild the image there, run only the new phase via `hack/agent-test.sh` with the image tag that worktree produced, and record the probe failing. Paste both outputs.
- [ ] **Step 6:** Commit `test(agent): a real proxy relays voice for a player it only knows from the mirror`.

---

### Task 13: Docs, sample, spec amendments

**Files:**
- Create: `docs/guides/voice-chat.md`
- Modify: the docs navigation (`git grep -n "expose-strategies.md" docs/ mkdocs.yml` shows where guides are listed; add the new page beside it)
- Modify: `config/samples/network.yaml` (a commented `voice:` block under the Network, and a commented `voice.expose` under the ProxyGroup, in the sample's existing style)
- Modify: `docs/superpowers/specs/2026-10-12-voice-chat-relay-design.md` (§5 pings, §7 e2e, §3/§5 condition)

- [ ] **Step 1: Guide.** Short, the shape of `docs/guides/expose-strategies.md`: what it does in two sentences; the two YAML blocks from spec §3; the table from spec §4; "bring the SVC Paper plugin through `extraPlugins`; do not install SVC's Velocity plugin, the group reports `Voice=False/VoicePluginConflict`"; the limit that a TCP-only entry point carries no voice and `spec.voice.expose` is the way around it; how to check: `kubectl get proxygroup -o wide` shows `VOICE`, and `spawnery_voice_packets_total{reason="relayed"}` rises while people talk. Run `hack/docs-length.sh`; it must pass.
- [ ] **Step 2: Spec amendments** (dated, in place, no history narration):
  - §3/§5: the refusals sit on a `Voice` condition, and the phase is `Degraded` while it is `False`; add `VoiceServiceNotOwnedByGroup`.
  - §5: pings are dropped and counted as `ping`.
  - §7 e2e: replaced by the `hack/agent-test.sh` phase of Task 12, because `make e2e` runs no game image; the server-switch propagation time is measured on a real cluster as part of the real-cluster check and written into §5 then.
- [ ] **Step 3:** Run the humanizer skill (embedded mode) over the guide and the amended spec paragraphs before committing.
- [ ] **Step 4:** `$NIX make test` (the full gate: manifests, generate, linters, all Go tests; on `dev` expect it to take long and run with `-p 1` via `GOFLAGS=-p=1`). Expected: PASS. Then `$NIX make lint`. Expected: clean.
- [ ] **Step 5:** Commit `docs(guides): voice chat through the proxies`.

---

## After the last task

Not part of any task, and only on the user's word: the release (minor step: `imageVersion`, `operatorVersion`, chart `version`/`appVersion`, `make manifests` for `chart-values.md`), and the real-cluster check from spec §7, whose measured server-switch delay then goes into spec §5.
