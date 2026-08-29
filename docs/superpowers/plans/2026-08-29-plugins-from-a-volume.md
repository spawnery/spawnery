# Plugins from a Volume Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An administrator fills a `ReadWriteMany` claim with plugin jars and their configuration, and every server or proxy of a group loads them — with no image rebuild.

**Architecture:** A new `spec.extraPlugins.claimName` on `ServerGroup` and `ProxyGroup`. The operator mounts that claim read-only at a fixed path outside `/data`, and each entrypoint copies its whole tree into `/data/plugins` before it copies the agent jar. The group controller refuses a claim that is missing, not `ReadWriteMany`, or named on an installation that has not enabled the feature.

**Tech Stack:** Go (controller-runtime, envtest), Kubernetes CRDs, POSIX shell (both entrypoints), Nix, podman.

**Spec:** [`docs/superpowers/specs/2026-08-29-plugins-from-a-volume-design.md`](../specs/2026-08-29-plugins-from-a-volume-design.md)

---

## Global Constraints

Copied from the spec and from what this repository already enforces. Every task
is bound by all of them.

- **The images gain no third-party plugin.** Nothing under `nix/` grows a jar.
- **Every user mount is read-only**, and this new one is too. `internal/podspec`
  sets `ReadOnly` on all of them unconditionally.
- **`/data/plugins` is not a mount target.** `internal/podspec/server.go:53`
  carries the measured reason. The new volume mounts *outside* `/data` and the
  entrypoint copies from it.
- **The agent jar wins.** The volume's tree is copied first, the agent jar
  second, so a `spawnery-agent.jar` on the volume cannot displace the shipped
  one.
- **The switch is operational, not a security boundary**, and every comment
  about it must say so. See §3.5 of the spec: against a namespaced claim,
  anybody who can write a `ServerGroup` can write a `PersistentVolumeClaim`.
- Every build/test command needs the prefix
  `nix --extra-experimental-features 'nix-command flakes' develop -c`.
- **The whole-tree Go suite needs `-p 1`.** This machine has 7 GB and parallel
  envtest packages each start their own etcd and apiserver.
- `make agent-test` needs `CONTAINER=podman` and `TMPDIR="$HOME/.cache/spawnery-tmp"`.
- **`nix` filters the source tree through the git index.** `git add` a new file
  before any `nix build`, or it does not exist in the sandbox.
- Conventional Commits, English subject, and every commit ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push, never merge, never create a tag.**

---

## Facts measured before this plan was written

**The refusal pattern already exists.**
`internal/controller/servergroup_controller.go:145` is a `switch` that sets
`ConditionAccepted` before anything creates a `Server`. A new refusal is a new
`case` in that chain, not a new mechanism.

**Announcing a refusal has a rule.**
`internal/controller/network_controller.go:131` computes `entering` from
`hasConditionReason` and records the event **only on the transition** — "an
event per minute forever is not a report, it is noise that buries the one that
mattered". Follow it.

**Both entrypoints copy the agent jar identically.**
`image/entrypoint.sh:47` and `image/velocity-entrypoint.sh:25`, each
`mkdir -p plugins` then `cp -f`. The new copy goes immediately *before* each.

**A podspec error today returns from `Reconcile`** and requeues
(`internal/controller/server_controller.go:365`). That is the wrong surface for
this: a misconfigured claim must land on the group's status, which is why the
validation lives in the group controller and not in `podspec`.

**`/data` is an `emptyDir` unless the group is persistent** —
`internal/podspec/server.go:506`. Nothing on it survives a pod.

---

## File Structure

| File | Responsibility |
|---|---|
| `api/v1alpha1/common_types.go` | `ExtraPlugins` type, two new condition reasons |
| `api/v1alpha1/servergroup_types.go`, `proxygroup_types.go` | the `ExtraPlugins` field |
| `internal/podspec/server.go`, `proxy.go` | the read-only volume and its mount; `PluginSourceMountPath` |
| `internal/podspec/hash_golden_test.go` | the digests move once, deliberately |
| `image/entrypoint.sh`, `image/velocity-entrypoint.sh` | copy the tree, then the agent jar |
| `internal/controller/extraplugins.go` (new) | the one validation, shared by both group controllers |
| `internal/controller/servergroup_controller.go`, `proxygroup_controller.go` | a `case` in the Accepted chain |
| `internal/controller/setup.go`, `cmd/spawnery-operator/main.go` | `AllowPluginVolumes` and its flag |
| `internal/rbacaudit/required.go` | `persistentvolumeclaims: get` if not already granted |
| `hack/agent-test.sh` | a directory standing in for the volume, against the real image |
| `docs/`, `charts/spawnery/README.md` | what it is, and what Longhorn RWX adds |

---

## Task 1: The API, and one place that decides

The field and the validation, with no renderer and no entrypoint yet. It ends
with a group that is refused for a bad claim and accepted for a good one —
which is the whole of what an administrator sees before anything runs.

**Files:**
- Modify: `api/v1alpha1/common_types.go`, `servergroup_types.go`, `proxygroup_types.go`
- Create: `internal/controller/extraplugins.go`, `internal/controller/extraplugins_test.go`

**Interfaces:**
- Produces: `spawneryv1alpha1.ExtraPlugins{ClaimName string}`;
  `spawneryv1alpha1.ReasonPluginVolumeUnusable`, `ReasonPluginVolumesDisabled`;
  `controller.checkExtraPlugins(ctx, r client.Reader, namespace string, ep *spawneryv1alpha1.ExtraPlugins, allowed bool) (reason, message string, ok bool)`.
- Consumes: nothing.

- [ ] **Step 1: The type and the reasons**

In `api/v1alpha1/common_types.go`:

```go
// ExtraPlugins names a volume whose contents are copied into the server's
// plugins directory on every start.
//
// **The claim's contents are the truth, on every start.** A plugin that
// rewrites its own configuration at runtime loses that change when the pod is
// replaced. For an ephemeral group it would lose it anyway -- spec.type
// Ephemeral gives /data an emptyDir -- so this costs nothing there and makes
// the persistent case predictable rather than accumulating.
//
// Nothing about the contents reaches podspec.DesiredServerHash: the operator
// holds a claim name, not a filesystem. So changing a plugin does not roll a
// fleet, which is the point of this field existing -- and a change therefore
// takes effect when the group next restarts, which somebody triggers.
type ExtraPlugins struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	//
	// It must be ReadWriteMany. A ReadWriteOnce claim mounts on one node, so
	// the second server of a group would sit Pending with a scheduling error
	// naming volume affinity rather than the actual cause; the operator
	// refuses it instead. That refusal also catches a single-replica group,
	// for which ReadWriteOnce would in fact work -- see the design's §3.4 for
	// why the simpler rule was chosen.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`
}
```

And two reasons beside the existing ones:

```go
	// ReasonPluginVolumeUnusable says spec.extraPlugins names a claim that is
	// missing, or that cannot be mounted by every server of the group.
	ReasonPluginVolumeUnusable = "PluginVolumeUnusable"
	// ReasonPluginVolumesDisabled says spec.extraPlugins is set on an
	// installation whose operator was not started with
	// --allow-plugin-volumes.
	ReasonPluginVolumesDisabled = "PluginVolumesDisabled"
```

In both `servergroup_types.go` and `proxygroup_types.go`, beside `Mounts`:

```go
	// ExtraPlugins names a volume whose plugins and their configuration are
	// copied into this group's servers on every start. See ExtraPlugins.
	// +optional
	ExtraPlugins *ExtraPlugins `json:"extraPlugins,omitempty"`
```

- [ ] **Step 2: `make manifests` and `make generate`**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make manifests generate`
Expected: the two CRDs under `config/crd/bases/` and
`charts/spawnery/templates/crds.yaml` gain `extraPlugins`, and
`zz_generated.deepcopy.go` gains `ExtraPlugins`.

- [ ] **Step 3: Write the failing tests**

Create `internal/controller/extraplugins_test.go`. Copy the 16-line Apache
header from `internal/agentserver/writer.go`. This is an internal test
(`package controller`) because `checkExtraPlugins` is unexported.

```go
package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func claim(name string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: modes},
	}
}

func readerWith(t *testing.T, objects ...runtime.Object) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...)
}

func TestNoExtraPluginsIsAccepted(t *testing.T) {
	// The overwhelmingly common case, and the one a regression here would
	// break for every installation that never asked for this.
	c := readerWith(t).Build()

	if _, _, ok := checkExtraPlugins(context.Background(), c, "minecraft", nil, false); !ok {
		t.Error("a group with no extraPlugins was refused")
	}
}

func TestAReadWriteManyClaimIsAccepted(t *testing.T) {
	c := readerWith(t, claim("plugins", corev1.ReadWriteMany)).Build()

	_, _, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true)
	if !ok {
		t.Error("a ReadWriteMany claim was refused")
	}
}

func TestAReadWriteOnceClaimIsRefusedAndSaysWhy(t *testing.T) {
	// The failure this replaces: the second server sits Pending with a
	// scheduling error about volume affinity, and nothing names the claim.
	c := readerWith(t, claim("plugins", corev1.ReadWriteOnce)).Build()

	reason, message, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true)

	if ok {
		t.Fatal("a ReadWriteOnce claim was accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Errorf("reason = %q, want ReasonPluginVolumeUnusable", reason)
	}
	// The claim and the mode, because an administrator reading this has to be
	// able to fix it without guessing which of the two is wrong.
	if !strings.Contains(message, "plugins") || !strings.Contains(message, "ReadWriteMany") {
		t.Errorf("message = %q, want it to name the claim and the mode it needs", message)
	}
}

func TestAMissingClaimIsRefusedRatherThanMounted(t *testing.T) {
	// Kubernetes would leave the pod Pending forever on a claim that does not
	// exist. Refusing here puts the answer on the group, where somebody is
	// looking.
	c := readerWith(t).Build()

	reason, message, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "absent"}, true)

	if ok {
		t.Fatal("a claim that does not exist was accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumeUnusable {
		t.Errorf("reason = %q, want ReasonPluginVolumeUnusable", reason)
	}
	if !strings.Contains(message, "absent") {
		t.Errorf("message = %q, want it to name the claim", message)
	}
}

func TestTheSwitchOffRefusesBeforeTheClaimIsEvenRead(t *testing.T) {
	// Two things at once, and both matter. The refusal names the flag, not the
	// claim -- an administrator whose claim is perfect needs to be sent to the
	// operator's arguments and nowhere else. And it does not depend on the
	// claim existing, so an installation with the feature off never reads a
	// PersistentVolumeClaim at all.
	c := readerWith(t).Build()

	reason, message, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "absent"}, false)

	if ok {
		t.Fatal("extraPlugins was accepted on an installation that disabled it")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumesDisabled {
		t.Errorf("reason = %q, want ReasonPluginVolumesDisabled", reason)
	}
	if !strings.Contains(message, "--allow-plugin-volumes") {
		t.Errorf("message = %q, want it to name the flag", message)
	}
}

func TestAClaimWithSeveralModesIsAcceptedIfOneIsReadWriteMany(t *testing.T) {
	// accessModes is a list. A claim that is both RWO and RWX is mountable by
	// every node, and reading only the first entry would refuse it.
	c := readerWith(t, claim("plugins", corev1.ReadWriteOnce, corev1.ReadWriteMany)).Build()

	if _, _, ok := checkExtraPlugins(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}, true); !ok {
		t.Error("a claim listing ReadWriteMany among its modes was refused")
	}
}
```

- [ ] **Step 4: Run them and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'ExtraPlugins|PluginVolume|ReadWriteMany|ReadWriteOnce|MissingClaim|SwitchOff|SeveralModes' -count=1`
Expected: FAIL, `undefined: checkExtraPlugins`.

- [ ] **Step 5: Write the check**

Create `internal/controller/extraplugins.go` with the Apache header:

```go
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// checkExtraPlugins decides whether a group's spec.extraPlugins can be served.
//
// One function for both group kinds. A ServerGroup and a ProxyGroup ask the
// identical question of the identical field, and two copies would be two
// answers the day somebody improves one message.
//
// It returns the condition reason and the sentence for a person, or ok when
// there is nothing wrong. It reports rather than writes, so the caller decides
// where the answer lands -- which is what lets both controllers put it in
// their own Accepted chain.
func checkExtraPlugins(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ep *spawneryv1alpha1.ExtraPlugins,
	allowed bool,
) (string, string, bool) {
	if ep == nil {
		return "", "", true
	}
	if !allowed {
		// Before the claim is read, so an installation with the feature off
		// never touches a PersistentVolumeClaim -- and so the message sends
		// somebody whose claim is perfectly good to the operator's arguments
		// rather than to their own storage.
		return spawneryv1alpha1.ReasonPluginVolumesDisabled,
			"spec.extraPlugins is set, and this operator was started without " +
				"--allow-plugin-volumes so it renders no plugin volume",
			false
	}

	var pvc corev1.PersistentVolumeClaim
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ep.ClaimName}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		// Refused here rather than left to the scheduler. Kubernetes would
		// leave every pod of the group Pending on a claim that does not
		// exist, and the answer would be in a pod event rather than on the
		// group somebody is looking at.
		return spawneryv1alpha1.ReasonPluginVolumeUnusable,
			fmt.Sprintf("spec.extraPlugins names claim %q, which does not exist in this namespace",
				ep.ClaimName),
			false
	case err != nil:
		return spawneryv1alpha1.ReasonPluginVolumeUnusable,
			fmt.Sprintf("could not read claim %q: %v", ep.ClaimName, err),
			false
	}

	// The whole list, not the first entry: a claim may carry several modes,
	// and one that is both RWO and RWX is mountable by every node.
	for _, m := range pvc.Spec.AccessModes {
		if m == corev1.ReadWriteMany {
			return "", "", true
		}
	}
	return spawneryv1alpha1.ReasonPluginVolumeUnusable,
		fmt.Sprintf("spec.extraPlugins names claim %q, whose access modes are %v; "+
			"every server of a group must mount it, which needs ReadWriteMany",
			ep.ClaimName, pvc.Spec.AccessModes),
		false
}
```

- [ ] **Step 6: Run the tests**

Expected: PASS, six tests.

- [ ] **Step 7: Mutate each claim on its own**

One at a time, restoring between each, and **read which named test failed** —
a mutation that fails the wrong test has told you nothing.

1. Return `true` unconditionally for a missing claim.
   Expected: `TestAMissingClaimIsRefusedRatherThanMounted` fails.
2. Read only `pvc.Spec.AccessModes[0]` instead of ranging.
   Expected: `TestAClaimWithSeveralModesIsAcceptedIfOneIsReadWriteMany` fails.
3. Move the `!allowed` branch below the `Get`, so a disabled installation
   reads the claim first and reports it missing.
   Expected: `TestTheSwitchOffRefusesBeforeTheClaimIsEvenRead` fails on its
   reason — the claim in that test does not exist, so the reordered code
   answers `ReasonPluginVolumeUnusable` and sends an administrator to their
   storage instead of to the operator's arguments.
4. Accept `ReadWriteOnce` as sufficient.
   Expected: `TestAReadWriteOnceClaimIsRefusedAndSaysWhy` fails.

- [ ] **Step 8: Commit**

---

## Task 2: The operator renders the volume

**Files:**
- Modify: `internal/podspec/server.go`, `internal/podspec/proxy.go`
- Modify: `internal/podspec/server_test.go`, `proxy_test.go`, `hash_golden_test.go`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ExtraPlugins` from Task 1.
- Produces: `podspec.PluginSourceVolumeName = "extra-plugins"`,
  `podspec.PluginSourceMountPath = "/var/run/spawnery/plugins"`.

- [ ] **Step 1: The constants**

In `internal/podspec/server.go`, beside `PluginsMountPath`:

```go
	// PluginSourceVolumeName and PluginSourceMountPath are where a group's
	// spec.extraPlugins claim is mounted.
	//
	// **Outside /data, and that is the whole reason for a second path.** A
	// user mount may not target PluginsMountPath -- this package's own comment
	// above says why -- and every user mount is read-only, so the claim cannot
	// be the plugins directory. It is a source the entrypoint copies out of,
	// exactly as it copies the agent jar out of the image.
	//
	// The path is a constant known to both sides rather than something the
	// user chooses, because the entrypoint has to find it. A user-chosen path
	// would have to reach the entrypoint through an environment variable,
	// which is a second thing to keep in step with this constant.
	PluginSourceVolumeName = "extra-plugins"
	PluginSourceMountPath  = "/var/run/spawnery/plugins"
```

- [ ] **Step 2: Write the failing tests**

In `internal/podspec/server_test.go`:

```go
func TestExtraPluginsMountsTheClaimReadOnlyOutsideData(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ExtraPlugins = &spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}
	})

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == PluginSourceVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatal("no plugin source volume was rendered")
	}
	if vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "plugins" {
		t.Errorf("volume source = %+v, want the named claim", vol.VolumeSource)
	}
	// Read-only at the volume as well as at the mount: a claim several groups
	// share must not be writable by any of them, and the pod-level flag is the
	// one a reader checks first.
	if !vol.PersistentVolumeClaim.ReadOnly {
		t.Error("the claim is mounted writable")
	}

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == PluginSourceVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("the plugin source volume is not mounted")
	}
	if !mount.ReadOnly {
		t.Error("the plugin source is mounted writable")
	}
	if mount.MountPath != PluginSourceMountPath {
		t.Errorf("mountPath = %q, want %q", mount.MountPath, PluginSourceMountPath)
	}
	// The bound that makes this work at all. A mount under /data/plugins would
	// fail the entrypoint's own copy under `set -eu`.
	if isPathUnder(path.Clean(mount.MountPath), path.Clean(DataMountPath)) {
		t.Errorf("mountPath %q is under %s, where a read-only mount breaks the start",
			mount.MountPath, DataMountPath)
	}
}

func TestNoExtraPluginsRendersNoVolume(t *testing.T) {
	// Every installation that never asks for this must get the pod it got
	// before -- which is also what keeps the golden digests still for them.
	pod := build(t, nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == PluginSourceVolumeName {
			t.Fatal("a plugin source volume was rendered for a group that named none")
		}
	}
}
```

And the same pair in `proxy_test.go` against `BuildProxyPod`, written out
rather than shared: the two pods are built by two functions, and "it is the
same shape" is how the one that is subtly not the same gets in.

- [ ] **Step 3: Run and watch them fail**

Expected: FAIL, `g.Spec.ExtraPlugins undefined` is already gone after Task 1,
so the failure is `no plugin source volume was rendered`.

- [ ] **Step 4: Render it**

In `BuildServerPod`, after the existing volumes:

```go
	if group.Spec.ExtraPlugins != nil {
		volumes = append(volumes, corev1.Volume{
			Name: PluginSourceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: group.Spec.ExtraPlugins.ClaimName,
					// Read-only at the volume too, not only at the mount. One
					// claim may serve several groups, and a group that could
					// write it could change what every other group loads.
					ReadOnly: true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      PluginSourceVolumeName,
			MountPath: PluginSourceMountPath,
			ReadOnly:  true,
		})
	}
```

The same in `BuildProxyPod`.

- [ ] **Step 5: Run the tests; the golden digests will not move**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/podspec/ -count=1`
Expected: PASS, **including `TestTheServerPodDigestHasNotMoved` and
`TestTheProxyPodDigestHasNotMoved`** — the golden fixtures set no
`ExtraPlugins`, so nothing about them changes.

**If a golden digest moved, stop.** It would mean the render path changed for
groups that named nothing, which is a bug in Step 4 and not a decision to
record.

- [ ] **Step 6: Mutate**

1. Drop `ReadOnly` from the volume source.
   Expected: `TestExtraPluginsMountsTheClaimReadOnlyOutsideData` fails.
2. Set `MountPath: PluginsMountPath`.
   Expected: the same test fails on its under-`/data` assertion.
3. Render the volume unconditionally rather than under the `nil` check.
   Expected: `TestNoExtraPluginsRendersNoVolume` fails **and both golden digest
   tests fail** — which is the guard doing exactly what it exists for.

- [ ] **Step 7: Commit**

---

## Task 3: Both entrypoints copy the tree

**Files:**
- Modify: `image/entrypoint.sh`, `image/velocity-entrypoint.sh`
- Modify: `image/entrypoint_test.go`

**Interfaces:**
- Consumes: `PluginSourceMountPath` from Task 2, as a literal path in shell.

- [ ] **Step 1: The copy, in `image/entrypoint.sh`**

The source path is a variable with a production default, overridable from the
environment. That is the seam `PAPER_HOME` already is in this script, and it
exists for the same reason: a test cannot create `/var/run/spawnery/plugins`,
which needs root.

**This is not the operator passing the path.** The operator mounts at
`internal/podspec.PluginSourceMountPath` and sets nothing; the default below is
the only spelling that matters in production, and the override exists so the
tests below can run at all.

Immediately **before** the existing agent-jar block:

```sh
# Plugins from the group's own volume, if it has one.
#
# The default is internal/podspec.PluginSourceMountPath. The operator mounts
# exactly there and passes nothing -- the variable is overridable only so
# image/entrypoint_test.go can point it at a temporary directory, the same seam
# PAPER_HOME already is.
#
# The whole tree, not just *.jar. A plugin's configuration lives at
# plugins/<Name>/config.yml, and copying jars without it would leave every
# plugin at its defaults on an ephemeral group.
#
# The source wins. A plugin that rewrote its own config at runtime loses that
# change here, which is the rule the design chose: on an ephemeral group /data
# is an emptyDir and the change was going anyway, so this makes the persistent
# case predictable instead of letting it accumulate.
#
# The trailing dot copies the directory's *contents*. Without it the tree lands
# at plugins/plugins.
PLUGIN_SOURCE="${SPAWNERY_PLUGIN_SOURCE:-/var/run/spawnery/plugins}"
if [ -d "$PLUGIN_SOURCE" ]; then
	mkdir -p plugins
	cp -a "$PLUGIN_SOURCE/." plugins/
	# The mount is read-only, so the copies arrive read-only too. Paper writes
	# its plugins' data folders inside this directory, and a plugin that cannot
	# rewrite its own config file fails in its own way rather than in one the
	# server reports. This is the same reason the agent-jar copy below uses
	# `cp -f`.
	chmod -R u+w plugins
fi
```

**Order matters and is the point.** The agent-jar block that follows overwrites
anything this copied, so a `spawnery-agent.jar` on the volume cannot displace
the shipped one. Do not move this after it.

- [ ] **Step 2: The same in `image/velocity-entrypoint.sh`**

Identical block, immediately before its own agent-jar block at line 25, with
`VELOCITY_HOME` in the surrounding code untouched.

- [ ] **Step 3: Write the failing tests**

Add to `image/entrypoint_test.go`, following the file's own harness — it builds
a temporary working directory and runs the script against a stub
`spawnery-config`. Read `TestCopiesTheAgentPluginIntoAWritablePluginsDirectory`
at line 186 first; these are shaped after it.

```go
func TestPluginsFromTheVolumeAreCopiedInWithTheirConfiguration(t *testing.T) {
	dir := t.TempDir()

	// The volume, as the operator would have mounted it: a jar and a nested
	// configuration file. The configuration is half the point -- jars alone
	// would leave every plugin at its defaults on an ephemeral group.
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "LuckPerms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "luckperms.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "LuckPerms", "config.yml"),
		[]byte("server: lobby"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	jar, err := os.ReadFile(filepath.Join(dir, "plugins", "luckperms.jar"))
	if err != nil {
		t.Fatalf("the jar did not reach the plugins directory: %v", err)
	}
	if string(jar) != "jar" {
		t.Errorf("plugins/luckperms.jar = %q, want the volume's copy", jar)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "plugins", "LuckPerms", "config.yml"))
	if err != nil {
		t.Fatalf("the nested configuration did not reach the plugins directory: %v", err)
	}
	if string(cfg) != "server: lobby" {
		t.Errorf("plugins/LuckPerms/config.yml = %q, want the volume's copy", cfg)
	}
}

func TestCopiedPluginFilesAreWritable(t *testing.T) {
	// The mount is read-only, so every file arrives read-only. Paper writes
	// its plugins' data folders inside this directory, and a plugin that
	// cannot rewrite its own config fails in its own way rather than in one
	// the server reports.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugin.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "plugins", "plugin.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Errorf("plugins/plugin.jar is %v, want it writable by its owner", info.Mode().Perm())
	}
}

func TestTheAgentJarWinsOverOneOnTheVolume(t *testing.T) {
	// The bound. Somebody pinning an older agent by dropping it on the volume
	// would otherwise leave the operator talking to a version it never
	// published -- and every object in the cluster would say the right thing.
	dir := t.TempDir()

	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paperHome, "agent", "spawnery-agent.jar"),
		[]byte("from the image"), 0o444); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "spawnery-agent.jar"),
		[]byte("from the volume"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0,
		"PAPER_HOME="+paperHome, "SPAWNERY_PLUGIN_SOURCE="+source); err != nil {
		t.Fatalf("entrypoint: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "plugins", "spawnery-agent.jar"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from the image" {
		t.Errorf("plugins/spawnery-agent.jar = %q, want the image's copy to win", got)
	}
}

func TestNoSourceDirectoryIsNotAnError(t *testing.T) {
	// The overwhelmingly common case: a group with no extraPlugins renders no
	// volume, so the path does not exist. Under `set -eu` a missing guard here
	// would fail every start in every installation.
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, 0,
		"SPAWNERY_PLUGIN_SOURCE="+filepath.Join(dir, "nothing-here")); err != nil {
		t.Fatalf("a missing plugin source failed the start: %v", err)
	}
}
```

- [ ] **Step 4: Run them and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./image/ -run 'Volume|Writable|AgentJarWins|NoSource' -count=1`
Expected: FAIL — the jar does not reach the plugins directory.

- [ ] **Step 5: Implement, then run the whole file**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./image/ -count=1`
Expected: PASS, including the existing agent-jar tests.

- [ ] **Step 6: Mutate**

1. Move the copy block *after* the agent-jar block.
   Expected: `TestTheAgentJarWinsOverOneOnTheVolume` fails.
2. Drop the `if [ -d ... ]` guard.
   Expected: `TestNoSourceDirectoryIsNotAnError` fails.
3. Copy `"$PLUGIN_SOURCE"` without the trailing `/.`.
   Expected: `TestPluginsFromTheVolumeAreCopiedInWithTheirConfiguration` fails —
   the tree lands one level too deep.
4. Drop the `chmod -R u+w`.
   Expected: `TestCopiedPluginFilesAreWritable` fails.

- [ ] **Step 7: Commit**

---

## Task 4: The switch, and the refusal on the group

**Files:**
- Modify: `internal/controller/setup.go`, `cmd/spawnery-operator/main.go`
- Modify: `internal/controller/servergroup_controller.go`, `proxygroup_controller.go`
- Modify: `internal/rbacaudit/required.go`
- Test: `internal/controller/servergroup_envtest_test.go` (or the file its
  Accepted-chain tests already live in — find it first)

- [ ] **Step 1: The flag**

In `cmd/spawnery-operator/main.go`, beside the others:

```go
	flag.BoolVar(&allowPluginVolumes, "allow-plugin-volumes", false,
		"allow groups to name a spec.extraPlugins claim")
```

And on `controller.Options`:

```go
	// AllowPluginVolumes lets a group name a spec.extraPlugins claim.
	//
	// **An operational switch and not a security boundary.** A
	// PersistentVolumeClaim is a namespaced object in the same trust domain as
	// the group that names it: anybody who can write one can write the other,
	// so this stops nobody who was not already stopped. What it is for is an
	// operator being able to say "this installation runs no third-party
	// plugins" and have that be true rather than a convention.
	//
	// Off by default for that reason and not for safety. Documenting it as a
	// security control would be the kind of check that reads like a bound and
	// cannot fail, which is worse than no check -- the next reader would trust
	// it.
	AllowPluginVolumes bool
```

- [ ] **Step 2: The RBAC**

The operator now reads `PersistentVolumeClaim`s it did not create. Check
`config/rbac/role.yaml` — `persistentvolumeclaims` already carries
`create, get, list, patch, watch`, so **no marker changes**. Confirm by running
`make manifests` and seeing no diff, and add a row to
`internal/rbacaudit/required.go` only if one is genuinely missing. Do not add a
grant that is already there.

- [ ] **Step 3: The `case` in each Accepted chain**

Compute it above the `switch` at `servergroup_controller.go:145`, beside
`networkUsable`:

```go
	// Read before the switch, like networkUsable above it, so the chain below
	// stays a list of plain conditions rather than a call with side effects
	// hidden in a case expression.
	pluginReason, pluginMessage, pluginsOK := checkExtraPlugins(
		ctx, r.Client, group.Namespace, group.Spec.ExtraPlugins, r.AllowPluginVolumes)
```

Then a new `case` **before** the `default`:

```go
	case !pluginsOK:
		logger.Info("plugin volume unusable, no servers are created for this group",
			"group", group.Name, "reason", pluginReason)
		meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:    spawneryv1alpha1.ConditionAccepted,
			Status:  metav1.ConditionFalse,
			Reason:  pluginReason,
			Message: pluginMessage,
		})
		requeue = networkRetryInterval
```

Place it **after** the two network cases. A group whose Network is missing has
a bigger problem than its plugin volume, and reporting the smaller one first
would send somebody to their storage while the real cause sits one condition
away.

**Announce on the transition only**, following
`internal/controller/network_controller.go:131`: compute `entering` with
`hasConditionReason` before the status write, and record the event after it.
An event per resync forever is noise that buries the one that mattered.

The identical change in `proxygroup_controller.go`.

- [ ] **Step 4: An envtest that the refusal reaches the object**

Find the existing Accepted-chain tests with
`grep -rn 'ReasonNetworkNotFound' internal/controller/*_test.go` and follow the
fixture they use — do not build a second one. Assert, for a group naming an RWO
claim:

- `Accepted` is `False` with `ReasonPluginVolumeUnusable`
- **no `Server` is created** — the refusal has to stop the group, not decorate it
- exactly one event is recorded across two reconciles, not two

That last one is its own test and its own mutation: remove the `entering`
guard and it must fail.

- [ ] **Step 5: Run everything, mutate, commit**

Run:
```
nix --extra-experimental-features 'nix-command flakes' develop -c go test -p 1 \
  ./internal/controller/ ./internal/podspec/ ./internal/rbacaudit/ -count=1
```

---

## Task 5: Driven against the real image

**Files:**
- Modify: `hack/agent-test.sh`

- [ ] **Step 1: A directory standing in for the volume**

`start_agent` already takes extra arguments that land before the image.
7c-2 added `-i` to one container that way. Add a phase that creates a host
directory holding one recognisable jar and one nested config file, passes
`-v "$dir:/var/run/spawnery/plugins:ro"`, and waits for the server's log to
name the plugin.

**Do not download a plugin.** Write a file called `probe-plugin.jar`
containing nothing that is a valid jar, and assert on Paper's own complaint
about it — it logs a failure naming the file when it cannot load something in
`plugins/`. That complaint is the proof: Paper only produces it for a file it
found where it looks, which is precisely what this phase is testing. A jar that
loaded would be testing Paper's plugin loader, which is not this project's to
test.

Grep the container log for the file name, not for a particular wording:
Paper's message changes between builds and the file name does not.

- [ ] **Step 2: State what this does and does not prove**

In the script, plainly: this exercises the entrypoint's half — a file on the
mount reaches `plugins/` inside the shipped image. It does not exercise the
operator's half, because this harness has no Kubernetes and never will. The
volume, the claim and the refusal are covered by envtest in
`internal/controller` and by nothing that runs the real jar.

- [ ] **Step 3: Run the whole harness, in the background**

Run:
```
TMPDIR="$HOME/.cache/spawnery-tmp" nix --extra-experimental-features 'nix-command flakes' \
  develop -c make agent-test CONTAINER=podman
```
It takes about thirteen minutes. **Read the log, not the exit code** — a
background completion notification has reported success over a failed run in
this repository before.

- [ ] **Step 4: Mutate the new check**

Make the copy block in `image/entrypoint.sh` a no-op and confirm the phase
fails at exactly that point with its own message. Restore and re-verify the
file is byte-identical to the version that passed.

- [ ] **Step 5: Commit**

---

## Task 6: Documentation

**Files:**
- Modify: `charts/spawnery/README.md`, `docs/upgrading.md`
- Create: `docs/plugins.md`

- [ ] **Step 1: `docs/plugins.md`**

What it is, and the four things somebody will otherwise learn the hard way:

- **The claim must be `ReadWriteMany`**, and on this project's own cluster that
  means Longhorn. A `ReadWriteOnce` claim is refused with a message that says
  so.
- **Longhorn serves RWX through a `share-manager` pod** that exports NFS, which
  every consuming node mounts. That is one more moving part between the volume
  and a starting server: if it is down or rescheduling, the mount hangs and the
  pod's events name an NFS mount and no plugin volume. Meeting that sentence
  here is cheaper than meeting the symptom.
- **The source wins on every start.** A plugin that rewrites its own config
  loses the change.
- **Changing the volume rolls nothing.** The operator cannot see the contents,
  so they are not in the pod hash; a change takes effect at the group's next
  restart, which somebody triggers.

- [ ] **Step 2: The chart README and the upgrade note**

The README gains `--allow-plugin-volumes` in its values/flags material, and
says in one line what §3.5 of the spec says: an operational switch, not a
security control.

`docs/upgrading.md` gains a section saying the field exists, is inert unless
both the flag and the field are set, and that **an upgrade that only adds the
field to the CRD moves no pod** — the golden digests did not move, and that is
worth stating because "a new field" reads as "something is about to change".

- [ ] **Step 3: Commit**

---

## Done when

- [ ] `make test`, `make agent`, `make lint` all pass
- [ ] `make agent-test CONTAINER=podman` passes
- [ ] The Go suite is run as `go test -p 1 ./internal/... ./api/...` and every
      package is `ok`
- [ ] **The golden pod digests did not move**, and that was verified rather
      than assumed: this feature renders nothing for a group that names no
      claim, so no installation is rolled by adopting it
- [ ] Every bound was mutated on its own and the *named* test that failed was
      read each time
- [ ] `make manifests` leaves no diff
- [ ] Nothing was pushed and no tag was created

## What this leaves

- **No plugin lifecycle.** Nothing installs, updates, resolves dependencies for
  or version-checks anything. The tree is copied verbatim.
- **No per-server plugins.** The claim belongs to a group.
- **No write-back**, and §3.2 of the spec says why that is the coherent rule
  rather than a limitation.
- **The open question in §6 of the spec** — whether `ProxyGroup` needs this in
  the first cut — is answered here as yes, because the entrypoints are already
  identical and splitting them would be the only asymmetry in the pair. A
  reviewer who wants the change halved should say so before Task 2.
