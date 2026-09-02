# Files from a volume — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `spec.extraFiles`, a claim whose tree the entrypoint copies into `/data` on every start, so a file outside `plugins/` — `config/sponge/sponge.conf` is the case — can reach a server without an image.

**Architecture:** `extraFiles` is `extraPlugins` one directory up and is built by mirroring it at every layer: the same `ClaimName`-only type, the same read-only volume outside `/data`, the same copy loop in both entrypoints, the same `ReadWriteMany` refusal, and an operator switch of its own. What is new is a refusal scan: before copying anything, the entrypoint rejects a source tree carrying a path another owner already writes, so the three writers into `/data` have disjoint paths and their order cannot decide the result.

**Tech Stack:** Go 1.x with kubebuilder/controller-runtime, POSIX shell entrypoints tested from Go in `image/`, Helm chart under `charts/spawnery`.

**Spec:** `docs/superpowers/specs/2026-09-02-files-from-a-volume-design.md`

## Global Constraints

- **Conventional Commits.** `feat(<scope>): what changed`, `fix(...)`, `docs(...)`, `chore(...)`. Scope is the part of the project touched (`api`, `podspec`, `image`, `controller`, `chart`, `docs`). Body says why, wrapped at 72 columns, with the `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>` trailer.
- **Never mention the coding-area network** in this repository — not in code, comments, docs or commit messages. Sponge is a public project and may be named; the network's tasks and tooling may not.
- **Branch:** `feat/files-from-a-volume`, already created, with the spec committed on it.
- **`make test` must pass at the end of every task.** It runs `manifests generate fmt vet chart-lint toolchain-lint` first, so a task that changes the API must have run `make manifests generate` before committing.
- **The mount path is `/var/run/spawnery/files`** and the env var that overrides it in tests is `SPAWNERY_FILE_SOURCE`, mirroring `SPAWNERY_PLUGIN_SOURCE`.
- **Three operator flags when this is done**, each default `false`: `--allow-plugin-volumes` (`spec.extraPlugins`, and only that from now on), `--allow-file-volumes` (`spec.extraFiles`, new), `--allow-mount-volumes` (claim-backed `spec.mounts`, new). Chart values `operator.allowPluginVolumes`, `operator.allowFileVolumes`, `operator.allowMountVolumes`.
- **None of them is a security control** and every place that documents one must say so, in the register `charts/spawnery/README.md` already uses for `allowPluginVolumes`.
- **Narrowing `--allow-plugin-volumes` is a breaking change** and is accepted as one: an installation using claim mounts must set `--allow-mount-volumes` after upgrading. It gets a section in `docs/upgrading.md`.

---

## File Structure

| File | Responsibility |
|---|---|
| `api/v1alpha1/common_types.go` | the `ExtraFiles` type, and the two new condition reasons |
| `api/v1alpha1/servergroup_types.go` | `ExtraFiles *ExtraFiles` on `ServerGroupSpec` |
| `api/v1alpha1/proxygroup_types.go` | `ExtraFiles *ExtraFiles` on `ProxyGroupSpec` |
| `api/v1alpha1/zz_generated.deepcopy.go` | generated — never hand-edited |
| `internal/podspec/server.go` | `FileSourceVolumeName`, `FileSourceMountPath`, the volume, the collision check |
| `internal/podspec/proxy.go` | the same volume on a proxy pod |
| `internal/controller/extrafiles.go` | `checkExtraFiles`, mirroring `extraplugins.go` |
| `internal/controller/extraplugins.go` | `checkGroupVolumes` gains the third question |
| `cmd/spawnery-operator/main.go` | the `--allow-file-volumes` flag, threaded to both controllers |
| `image/entrypoint.sh` | scan-then-copy, Paper's refusal list |
| `image/velocity-entrypoint.sh` | scan-then-copy, Velocity's refusal list |
| `internal/controller/mountclaims.go` or `extraplugins.go` | `checkMountClaims` moves onto its own flag |
| `charts/spawnery/values.yaml`, `templates/deployment.yaml`, `README.md` | the three switches |
| `docs/mounts.md`, `docs/plugins.md` | where the new field sits beside the two that exist |
| `docs/upgrading.md` | the breaking change, in that file's own register |

---

### Task 1: The API type and the two reasons

**Files:**
- Modify: `api/v1alpha1/common_types.go` (beside `type ExtraPlugins struct` at :376, and the reason block at :236-242)
- Modify: `api/v1alpha1/servergroup_types.go` (beside `ExtraPlugins *ExtraPlugins` at :192)
- Modify: `api/v1alpha1/proxygroup_types.go` (beside `ExtraPlugins *ExtraPlugins` at :323)
- Generated: `api/v1alpha1/zz_generated.deepcopy.go`, `charts/spawnery/templates/crds.yaml`, `config/crd/bases/*.yaml`
- Test: `internal/podspec/hash_golden_test.go` (must stay green unchanged)

**Interfaces:**
- Consumes: nothing.
- Produces: `spawneryv1alpha1.ExtraFiles{ClaimName string}`; `ServerGroupSpec.ExtraFiles *ExtraFiles`; `ProxyGroupSpec.ExtraFiles *ExtraFiles`; constants `ReasonFileVolumeUnusable = "FileVolumeUnusable"` and `ReasonFileVolumesDisabled = "FileVolumesDisabled"`.

- [ ] **Step 1: Add the type**

In `api/v1alpha1/common_types.go`, directly after the `ExtraPlugins` struct:

```go
// ExtraFiles names a volume whose tree is copied into a server's working
// directory on every start.
//
// It is ExtraPlugins one directory up. ExtraPlugins reaches /data/plugins and
// nothing else, so a plugin whose configuration lives elsewhere -- Sponge
// reads config/sponge/sponge.conf -- could not be configured without an image.
// A mount cannot deliver there either: see ServerConfigDirPath, whose comment
// carries the kubelet-ownership measurement that rules it out for good.
//
// The entrypoint refuses a tree carrying a path another owner writes, so this
// volume, the renderer and ExtraPlugins never write the same file and the
// order between them cannot decide the result.
type ExtraFiles struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	//
	// It must be ReadWriteMany, for the reason ExtraPlugins.ClaimName gives:
	// every pod of a group mounts it, and a ReadWriteOnce claim would leave
	// the second server Pending with a scheduling error naming volume
	// affinity rather than the cause.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`
}
```

- [ ] **Step 2: Add the two reasons**

In the same file, beside `ReasonPluginVolumesDisabled`:

```go
	// ReasonFileVolumeUnusable says spec.extraFiles names a claim that is
	// missing or not ReadWriteMany.
	ReasonFileVolumeUnusable = "FileVolumeUnusable"
	// ReasonFileVolumesDisabled says spec.extraFiles is set on an
	// installation started without --allow-file-volumes.
	ReasonFileVolumesDisabled = "FileVolumesDisabled"
```

- [ ] **Step 3: Add the field to both group kinds**

In `api/v1alpha1/servergroup_types.go`, after the `ExtraPlugins` field:

```go
	// ExtraFiles names a volume whose tree is copied into this group's
	// servers on every start. See ExtraFiles.
	// +optional
	ExtraFiles *ExtraFiles `json:"extraFiles,omitempty"`
```

Add the identical block to `api/v1alpha1/proxygroup_types.go` after its `ExtraPlugins` field.

- [ ] **Step 4: Regenerate**

Run: `make manifests generate`
Expected: `zz_generated.deepcopy.go` gains `ExtraFiles` deepcopy functions; `charts/spawnery/templates/crds.yaml` and the files under `config/crd/bases/` gain an `extraFiles` property with a required `claimName`. There is no `charts/spawnery/crds/` directory — the chart carries its CRDs as a template.

- [ ] **Step 5: Prove the hash did not move**

Run: `go test ./internal/podspec/ -run 'TestTheServerPodDigestHasNotMoved|TestTheProxyPodDigestHasNotMoved' -v`
Expected: PASS, both named, with no golden file edited. A pointer field left nil must not change a hash — if this fails, the field was added inside something the hash digests and the spec's section 3.7 is being violated.

**Check the run actually selected those two tests.** `go test -run` exits 0 when its pattern matches nothing, so a wrong name here is a green step that verified nothing. The output must name both tests.

- [ ] **Step 6: Commit**

```bash
git add api/ charts/spawnery/templates/crds.yaml config/crd/bases/
git commit -m "feat(api): extraFiles, a claim copied into /data on every start

extraPlugins reaches /data/plugins and nothing else, so a plugin whose
configuration lives elsewhere cannot be configured without an image.
A mount under /data/config is refused for the kubelet-ownership reason
ServerConfigDirPath carries.

The field alone, with its reasons. Nothing reads it yet.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: The volume on both pod kinds

**Files:**
- Modify: `internal/podspec/server.go` (constants near `PluginSourceVolumeName` at :114-115; the volume block at :383-399; the collision checks)
- Modify: `internal/podspec/proxy.go`
- Test: `internal/podspec/server_test.go`, `internal/podspec/proxy_test.go`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ExtraFiles` from Task 1.
- Produces: `podspec.FileSourceVolumeName = "extra-files"`, `podspec.FileSourceMountPath = "/var/run/spawnery/files"`.

- [ ] **Step 1: Write the failing tests**

In `internal/podspec/server_test.go`:

```go
func TestExtraFilesIsMountedReadOnlyOutsideData(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ExtraFiles = &spawneryv1alpha1.ExtraFiles{ClaimName: "files"}
	})

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == FileSourceVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("no extra-files mount on a group that names a claim")
	}
	if mount.MountPath != FileSourceMountPath {
		t.Errorf("mounted at %q, want %q", mount.MountPath, FileSourceMountPath)
	}
	if !mount.ReadOnly {
		t.Error("the source is writable; every user volume this package renders is read-only")
	}
	if strings.HasPrefix(mount.MountPath, DataMountPath) {
		t.Errorf("mounted inside %s, which a read-only mount may not be", DataMountPath)
	}
}

func TestNoExtraFilesVolumeWithoutTheField(t *testing.T) {
	pod := build(t, nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == FileSourceVolumeName {
			t.Error("a group that names no claim got the volume anyway")
		}
	}
}
```

`build(t, mutate)` is the helper at `internal/podspec/server_test.go:88`; it wraps `BuildServerPod(net, group, testServer(), testEndpoint)` and takes a mutator over `(*Network, *ServerGroup)`. There is no bare `ServerPod` function — the exported builders are `BuildServerPod` (`server.go:286`) and `BuildProxyPod` (`proxy.go:101`). The two tests to mirror are `TestExtraPluginsMountsTheClaimReadOnlyOutsideData` (:1166) and `TestNoExtraPluginsRendersNoVolume` (:1213).

For the proxy counterpart in `proxy_test.go`, that file has `buildProxy(t)` at :45 for the unmutated case and calls `BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil)` directly when it needs a modified group — see :93. Follow whichever of the two fits.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/podspec/ -run ExtraFiles -v`
Expected: FAIL — `undefined: FileSourceVolumeName`.

- [ ] **Step 3: Add the constants**

In `internal/podspec/server.go`, after `PluginSourceMountPath`:

```go
	// FileSourceVolumeName and FileSourceMountPath are where a group's
	// spec.extraFiles claim is mounted.
	//
	// Outside DataMountPath for the same reason PluginSourceMountPath is: the
	// claim cannot *be* the directory it fills, because every mount this
	// package renders is read-only and a read-only /data breaks everything
	// the server writes. It is a source the entrypoint copies out of.
	FileSourceVolumeName = "extra-files"
	FileSourceMountPath  = "/var/run/spawnery/files"
```

- [ ] **Step 4: Render the volume**

In `internal/podspec/server.go`, immediately after the `group.Spec.ExtraPlugins != nil` block:

```go
	if group.Spec.ExtraFiles != nil {
		volumes = append(volumes, corev1.Volume{
			Name: FileSourceVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: group.Spec.ExtraFiles.ClaimName,
					ReadOnly:  true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      FileSourceVolumeName,
			MountPath: FileSourceMountPath,
			ReadOnly:  true,
		})
	}
```

Add the same block to the proxy pod in `internal/podspec/proxy.go`, at the point its `ExtraPlugins` block sits.

- [ ] **Step 5: Guard the path against a user mount**

Find the loop at `internal/podspec/server.go:671` that lists the reserved volume names and add `FileSourceVolumeName` to it. Then find `checkMountCollision` and give `FileSourceMountPath` the same treatment `PluginSourceMountPath` gets, so a `spec.mounts` entry cannot shadow the source.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/podspec/ -v`
Expected: PASS, including the golden hash tests — a nil field still hashes as before.

- [ ] **Step 7: Commit**

```bash
git add internal/podspec/
git commit -m "feat(podspec): mount the extraFiles claim outside /data

Read-only, at /var/run/spawnery/files, for the reason
PluginSourceMountPath already carries: a read-only mount cannot be the
directory it fills, so the claim is a source to copy out of. The path
joins the collision checks that guard the plugin source, so a user mount
cannot shadow it.

Nothing copies out of it yet.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: Scan and copy in the Paper entrypoint

**Files:**
- Modify: `image/entrypoint.sh` (immediately before the `PLUGIN_SOURCE` block at :71)
- Test: `image/entrypoint_test.go`

**Interfaces:**
- Consumes: `SPAWNERY_FILE_SOURCE` (defaulting to `/var/run/spawnery/files`), the path Task 2 mounts.
- Produces: the copy behaviour Task 5's proxy version mirrors.

- [ ] **Step 1: Write the failing tests**

In `image/entrypoint_test.go`, following `runEntrypoint`'s existing shape:

```go
func TestAFileFromTheVolumeLandsUnderConfig(t *testing.T) {
	// The case the whole field exists for: a path no mount can reach, because
	// the kubelet would create /data/config root-owned.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "config", "sponge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config", "sponge", "sponge.conf"),
		[]byte("version=1\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a source carrying config/sponge/sponge.conf failed the start: %v", err)
	}

	landed := filepath.Join(dir, "config", "sponge", "sponge.conf")
	info, err := os.Stat(landed)
	if err != nil {
		t.Fatalf("the file did not reach %s: %v", landed, err)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Error("the copy is read-only; Sponge rewrites this file on every start")
	}
}

func TestAFileTheRendererOwnsRefusesTheStart(t *testing.T) {
	for _, owned := range []string{
		"server.properties",
		"config/paper-global.yml",
		"config/paper-world-defaults.yml",
	} {
		t.Run(owned, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "volume")
			if err := os.MkdirAll(filepath.Join(source, filepath.Dir(owned)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, owned), []byte("x"), 0o444); err != nil {
				t.Fatal(err)
			}

			out, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

			if err == nil {
				t.Fatalf("a source carrying %s started anyway", owned)
			}
			if !strings.Contains(out, owned) {
				t.Errorf("the message does not name the file:\n%s", out)
			}
			if !strings.Contains(out, "extraFiles") {
				t.Errorf("the message does not name the field:\n%s", out)
			}
		})
	}
}

func TestAPluginOnTheFileVolumeRefusesTheStart(t *testing.T) {
	// plugins/ has a mechanism of its own, and the message has to send
	// somebody to it rather than only saying no.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plugins", "p.jar"), []byte("jar"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying plugins/ started anyway")
	}
	if !strings.Contains(out, "extraPlugins") {
		t.Errorf("the message does not name the mechanism that owns plugins/:\n%s", out)
	}
}

func TestNothingIsCopiedWhenTheScanRefuses(t *testing.T) {
	// The scan runs before the copy, so a refused tree leaves no half-written
	// /data behind.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "config", "sponge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config", "sponge", "sponge.conf"),
		[]byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "server.properties"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err == nil {
		t.Fatal("the start was not refused")
	}

	if _, err := os.Stat(filepath.Join(dir, "config", "sponge", "sponge.conf")); err == nil {
		t.Error("the good file was copied before the bad one was noticed")
	}
}

func TestLostFoundOnTheFileVolumeIsSkipped(t *testing.T) {
	// The same ext4 artefact the plugin copy already skips: mode 0700 owned by
	// root, unreadable to this container, and never anybody's configuration.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "bukkit.yml"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a source carrying lost+found failed the start: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "lost+found")); err == nil {
		t.Error("lost+found was copied into the working directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "bukkit.yml")); err != nil {
		t.Errorf("the real file did not survive the skip: %v", err)
	}
}

func TestNoFileVolumeIsNotAnError(t *testing.T) {
	dir := t.TempDir()

	if _, err := runEntrypoint(t, dir, 0,
		"SPAWNERY_FILE_SOURCE="+filepath.Join(dir, "nothing-here")); err != nil {
		t.Fatalf("a start with no file volume failed: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./image/ -run 'FileVolume|FromTheVolumeLands|RendererOwns|PluginOnTheFile|ScanRefuses|LostFoundOnTheFile' -v`
Expected: FAIL — the file lands nowhere and nothing is refused, because the entrypoint does not read `SPAWNERY_FILE_SOURCE` at all.

- [ ] **Step 3: Write the block**

In `image/entrypoint.sh`, immediately before the `PLUGIN_SOURCE=` line:

```sh
# Files an administrator put on a volume, copied into the working directory.
#
# **The scan runs before the copy, and that is the whole safety property.**
# Three things write into /data on a start: spawnery-config above, this, and
# the plugin copy below. Refusing a source that carries a path one of the
# others owns makes their paths disjoint, so the order between them cannot
# decide the result -- rather than a rule about which runs first, which would
# make these line numbers load-bearing.
#
# lost+found and the two globs are the plugin copy's reasoning exactly; see
# the comment on PLUGIN_SOURCE below for the measurements behind both.
FILE_SOURCE="${SPAWNERY_FILE_SOURCE:-/var/run/spawnery/files}"
if [ -d "$FILE_SOURCE" ]; then
	# The renderer's own files, and the directory extraPlugins owns. A Paper
	# server does not refuse velocity.toml or lang/: nothing writes them here,
	# and refusing a path no owner claims would be a rule with no reason.
	if [ -d "$FILE_SOURCE/plugins" ]; then
		echo "spawnery: spec.extraFiles carries plugins/, which spec.extraPlugins owns." >&2
		echo "spawnery: move those files to the extraPlugins claim. Refusing to start." >&2
		exit 1
	fi
	for owned in server.properties config/paper-global.yml config/paper-world-defaults.yml; do
		if [ -e "$FILE_SOURCE/$owned" ]; then
			echo "spawnery: spec.extraFiles carries $owned, which the operator writes itself." >&2
			echo "spawnery: use spec.configOverlay for it. Refusing to start." >&2
			exit 1
		fi
	done

	for entry in "$FILE_SOURCE"/* "$FILE_SOURCE"/.[!.]*; do
		[ -e "$entry" ] || continue
		name="${entry##*/}"
		case "$name" in
		lost+found) continue ;;
		esac
		# Merge into a directory that is already there rather than nesting
		# inside it. `cp -R src dest/` puts src *under* dest when dest/src
		# exists, so a plain copy of a `config` directory would land the tree
		# at config/config -- and config always exists by now, because
		# spawnery-config wrote paper-global.yml into it above.
		if [ -d "$entry" ] && [ -d "./$name" ]; then
			cp -R "$entry/." "./$name/"
		else
			cp -R "$entry" ./
		fi
		# Scoped to what was just copied, and not `chmod -R u+w .`: this
		# script runs under `set -eu`, every user mount is read-only, and a
		# mount under /data would make a chmod of the whole working directory
		# fail with a bare `chmod:` naming no cause. The mount this copies
		# from is read-only too, so the copies arrive read-only and the files
		# it carries are exactly the ones a server rewrites -- Sponge writes
		# sponge.conf back on every start.
		chmod -R u+w "./$name"
	done
fi
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./image/ -v`
Expected: PASS, including every existing plugin-copy test — the new block must not have disturbed them.

- [ ] **Step 5: Commit**

```bash
git add image/entrypoint.sh image/entrypoint_test.go
git commit -m "feat(image): copy an extraFiles volume into the working directory

Scan first, then copy. Refusing a source that carries a path the
renderer or extraPlugins owns makes the three writers into /data
disjoint, so the order between them cannot decide the result -- and a
refused tree leaves nothing half-copied behind.

The file the field exists for is config/sponge/sponge.conf: no mount can
reach it, because the kubelet would create /data/config root-owned.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: Scan and copy in the Velocity entrypoint

**Files:**
- Modify: `image/velocity-entrypoint.sh` (immediately before its `PLUGIN_SOURCE` block at :43)
- Test: `image/velocity_entrypoint_test.go`

**Interfaces:**
- Consumes: the same `SPAWNERY_FILE_SOURCE`.
- Produces: nothing further.

- [ ] **Step 1: Write the failing tests**

In `image/velocity_entrypoint_test.go`, using `runVelocityEntrypoint(t, workDir, configExit, env...)` defined at :33 — that file's own runner, not the Paper one:

```go
func TestVelocityRefusesLangOnTheFileVolume(t *testing.T) {
	// lang/ belongs to Velocity itself: it migrates lang/messages.properties
	// to MiniMessage on every start and writes the result back, so a file
	// placed there is overwritten before anybody reads it. Nothing breaks,
	// which is exactly why it is refused -- a copy that silently does nothing
	// is worse than a collision that announces itself.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "lang"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "lang", "messages.properties"),
		[]byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying lang/ started anyway")
	}
	if !strings.Contains(out, "lang/") {
		t.Errorf("the message does not name the directory:\n%s", out)
	}
}

func TestVelocityRefusesItsOwnRenderedFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "velocity.toml"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	out, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source)

	if err == nil {
		t.Fatal("a source carrying velocity.toml started anyway")
	}
	if !strings.Contains(out, "velocity.toml") {
		t.Errorf("the message does not name the file:\n%s", out)
	}
}

func TestVelocityDoesNotRefuseThePaperFiles(t *testing.T) {
	// The list follows the flavour. Nothing writes server.properties on a
	// proxy, so refusing it would be a rule with no reason behind it.
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "server.properties"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("a proxy refused a file no proxy owns: %v", err)
	}
}

func TestVelocityCopiesAFileFromTheVolume(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "volume")
	if err := os.MkdirAll(filepath.Join(source, "extra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "extra", "note.txt"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := runVelocityEntrypoint(t, dir, 0, "SPAWNERY_FILE_SOURCE="+source); err != nil {
		t.Fatalf("the start failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "extra", "note.txt")); err != nil {
		t.Errorf("the file did not reach the working directory: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./image/ -run Velocity -v`
Expected: FAIL — nothing is refused and nothing is copied.

- [ ] **Step 3: Write the block**

In `image/velocity-entrypoint.sh`, immediately before its `PLUGIN_SOURCE=` line, the same block as Task 3 Step 3 with one difference — the refusal list is Velocity's:

```sh
	if [ -d "$FILE_SOURCE/plugins" ]; then
		echo "spawnery: spec.extraFiles carries plugins/, which spec.extraPlugins owns." >&2
		echo "spawnery: move those files to the extraPlugins claim. Refusing to start." >&2
		exit 1
	fi
	if [ -d "$FILE_SOURCE/lang" ]; then
		echo "spawnery: spec.extraFiles carries lang/, which Velocity owns -- it migrates" >&2
		echo "spawnery: lang/messages.properties on every start and writes it back, so a" >&2
		echo "spawnery: file placed there is overwritten unread. Refusing to start." >&2
		exit 1
	fi
	for owned in velocity.toml; do
		if [ -e "$FILE_SOURCE/$owned" ]; then
			echo "spawnery: spec.extraFiles carries $owned, which the operator writes itself." >&2
			echo "spawnery: use spec.configOverlay for it. Refusing to start." >&2
			exit 1
		fi
	done
```

Keep the copy loop, the `lost+found` skip and the per-entry `chmod -R u+w "./$name"` identical to Task 3 — including the merge branch for a directory that already exists, which matters on a proxy too: `spawnery-config --flavor velocity` runs before this and a persistent group's second start finds its own directories in place.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./image/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add image/velocity-entrypoint.sh image/velocity_entrypoint_test.go
git commit -m "feat(image): the proxy copies an extraFiles volume too

Same block, Velocity's refusal list. lang/ is on it because Velocity
owns that directory rather than because anything breaks: it migrates
lang/messages.properties on every start and writes it back, so a file
placed there is overwritten unread, and a copy that silently does
nothing is what this refusal exists to prevent.

The list follows the flavour: a proxy does not refuse the Paper files,
because nothing writes them there.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: The operator switch and the claim check

**Files:**
- Create: `internal/controller/extrafiles.go`
- Modify: `internal/controller/extraplugins.go` (`checkGroupVolumes`)
- Modify: `cmd/spawnery-operator/main.go` (:313, beside `allowPluginVolumes`)
- Test: Create `internal/controller/extrafiles_test.go`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ExtraFiles`, `ReasonFileVolumeUnusable`, `ReasonFileVolumesDisabled` from Task 1; `checkClaimMountable(ctx, reader, namespace, claimName) (string, bool)` from `extraplugins.go`.
- Produces: `checkExtraFiles(ctx context.Context, reader client.Reader, namespace string, ef *spawneryv1alpha1.ExtraFiles, allowed bool) (string, string, bool)`, returning `(reason, message, ok)` exactly as `checkExtraPlugins` does.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/extrafiles_test.go`, modelled on `extraplugins_test.go` — read it first and reuse its `pluginReader`-style fixtures:

```go
func TestNoExtraFilesIsAlwaysFine(t *testing.T) {
	if _, _, ok := checkExtraFiles(context.Background(), fileReader(t), "minecraft", nil, false); !ok {
		t.Error("a group naming no claim was refused")
	}
}

func TestExtraFilesWithoutTheFlagIsRefused(t *testing.T) {
	c := fileReader(t)

	reason, message, ok := checkExtraFiles(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraFiles{ClaimName: "files"}, false)

	if ok {
		t.Fatal("a group was accepted on an installation that disabled the feature")
	}
	if reason != spawneryv1alpha1.ReasonFileVolumesDisabled {
		t.Errorf("reason %q, want %q", reason, spawneryv1alpha1.ReasonFileVolumesDisabled)
	}
	if !strings.Contains(message, "--allow-file-volumes") {
		t.Errorf("the message does not name the flag: %s", message)
	}
}

func TestAReadWriteOnceFilesClaimIsRefused(t *testing.T) {
	c := fileReaderWithClaim(t, "files", corev1.ReadWriteOnce)

	reason, message, ok := checkExtraFiles(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraFiles{ClaimName: "files"}, true)

	if ok {
		t.Fatal("a ReadWriteOnce claim was accepted")
	}
	if reason != spawneryv1alpha1.ReasonFileVolumeUnusable {
		t.Errorf("reason %q, want %q", reason, spawneryv1alpha1.ReasonFileVolumeUnusable)
	}
	if !strings.Contains(message, "ReadWriteMany") {
		t.Errorf("the message does not say what is needed: %s", message)
	}
}

func TestAGroupWithBothFieldsWrongReportsThePluginOneFirst(t *testing.T) {
	// checkGroupVolumes' existing order: extraPlugins is the older field and
	// the one more installations set, and when both are wrong there is no
	// reason to prefer the other.
	c := fileReader(t)

	reason, _, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		&spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"},
		&spawneryv1alpha1.ExtraFiles{ClaimName: "files"},
		nil, false, false)

	if ok {
		t.Fatal("both fields wrong was accepted")
	}
	if reason != spawneryv1alpha1.ReasonPluginVolumesDisabled {
		t.Errorf("reason %q, want the plugin one first", reason)
	}
}
```

`checkGroupVolumes` today is (`internal/controller/extraplugins.go:78`):

```go
func checkGroupVolumes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ep *spawneryv1alpha1.ExtraPlugins,
	mounts []spawneryv1alpha1.Mount,
	allowed bool,
) (string, string, bool)
```

It becomes:

```go
func checkGroupVolumes(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ep *spawneryv1alpha1.ExtraPlugins,
	ef *spawneryv1alpha1.ExtraFiles,
	mounts []spawneryv1alpha1.Mount,
	allowed bool,
	filesAllowed bool,
) (string, string, bool)
```

with the body asking `checkExtraPlugins` first, then `checkExtraFiles`, then `checkMountClaims` — the existing order, extended rather than reordered.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run ExtraFiles -v`
Expected: FAIL — `undefined: checkExtraFiles`.

- [ ] **Step 3: Write `checkExtraFiles`**

Create `internal/controller/extrafiles.go` with the licence header every file in this package carries, then:

```go
// checkExtraFiles decides whether the claim a group's spec.extraFiles names
// can be served, exactly as checkExtraPlugins does for its own field.
//
// A second flag rather than a wider one: --allow-plugin-volumes exists so an
// operator can say "this installation runs no third-party plugins" and have it
// be a fact, and making it also govern files would leave its name covering
// something that is not a plugin. Like that one, this switch is an operational
// statement and not a security boundary -- a PersistentVolumeClaim is a
// namespaced object in the same trust domain as the group naming it.
func checkExtraFiles(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ef *spawneryv1alpha1.ExtraFiles,
	allowed bool,
) (string, string, bool) {
	if ef == nil {
		return "", "", true
	}
	if !allowed {
		return spawneryv1alpha1.ReasonFileVolumesDisabled,
			"spec.extraFiles is set, and this operator was started without " +
				"--allow-file-volumes so it renders no file volume",
			false
	}

	if problem, ok := checkClaimMountable(ctx, reader, namespace, ef.ClaimName); !ok {
		return spawneryv1alpha1.ReasonFileVolumeUnusable,
			fmt.Sprintf("spec.extraFiles names claim %q, which %s", ef.ClaimName, problem),
			false
	}
	return "", "", true
}
```

- [ ] **Step 4: Thread it through `checkGroupVolumes` and both controllers**

Add the `extraFiles` question to `checkGroupVolumes` after the `extraPlugins` one and before the mounts question, so the existing order holds. There are exactly two call sites, both of which gain `group.Spec.ExtraFiles` and the new flag:

- `internal/controller/servergroup_controller.go:169`
- `internal/controller/proxygroup_controller.go:254`

- [ ] **Step 5: Add the flag**

In `cmd/spawnery-operator/main.go`, beside the `allow-plugin-volumes` flag at :313:

```go
	flag.BoolVar(&allowFileVolumes, "allow-file-volumes", false,
		"let a group name a spec.extraFiles claim whose tree is copied into "+
			"every server's working directory on start. Not a security control: "+
			"a claim is a namespaced object in the same trust domain as the group "+
			"naming it. It lets an installation say it runs no administrator-supplied files.")
```

Declare `allowFileVolumes` beside `allowPluginVolumes` and thread it into both controllers the same way that one is threaded.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/controller/ ./cmd/... -v`
Expected: PASS. Existing controller tests that call `checkGroupVolumes` will need their call updated for the new parameter; that is a compile fix, not a behaviour change.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/ cmd/spawnery-operator/
git commit -m "feat(controller): --allow-file-volumes, and the claim it gates

extraFiles gets a switch of its own rather than widening
--allow-plugin-volumes, whose name would then cover something that is
not a plugin. The two statements an operator might want to make are
different, and an installation may want one without the other.

Not a security control, and the flag's help says so: a claim is a
namespaced object in the same trust domain as the group naming it.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: `spec.mounts` moves onto a switch of its own

**Files:**
- Modify: `internal/controller/extraplugins.go` (`checkMountClaims` at :136, `checkGroupVolumes` at :78)
- Modify: `cmd/spawnery-operator/main.go`
- Test: `internal/controller/mountclaims_test.go`

**Interfaces:**
- Consumes: `checkGroupVolumes` as Task 5 left it.
- Produces: `checkGroupVolumes(..., ep, ef, mounts, pluginsAllowed, filesAllowed, mountsAllowed bool)`; the flag `--allow-mount-volumes`.

**Why this is here.** `--allow-plugin-volumes` gates claim-backed `spec.mounts`
today, and its refusal says so in as many words. That was never what the flag's
name promised, and with a third feature arriving the imprecision stops being
survivable. This narrows the old flag to the field it names.

- [ ] **Step 1: Write the failing test**

In `internal/controller/mountclaims_test.go`:

```go
func TestAClaimMountNeedsItsOwnFlagAndNotThePluginOne(t *testing.T) {
	// --allow-plugin-volumes used to gate this too. It no longer does: the
	// flag names extraPlugins and now governs only that.
	c := mountReaderWithClaim(t, "worlds", corev1.ReadWriteMany)
	mounts := []spawneryv1alpha1.Mount{{
		Name:                  "worlds",
		MountPath:             "/data/worlds",
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "worlds"},
	}}

	reason, message, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		nil, nil, mounts, true, false, false)

	if ok {
		t.Fatal("a claim mount was accepted with only the plugin flag set")
	}
	if reason != spawneryv1alpha1.ReasonMountVolumesDisabled {
		t.Errorf("reason %q, want %q", reason, spawneryv1alpha1.ReasonMountVolumesDisabled)
	}
	if !strings.Contains(message, "--allow-mount-volumes") {
		t.Errorf("the message does not name the flag to set: %s", message)
	}
	if strings.Contains(message, "--allow-plugin-volumes") {
		t.Errorf("the message still sends somebody to the plugin flag: %s", message)
	}
}

func TestAClaimMountIsAcceptedWithItsOwnFlag(t *testing.T) {
	c := mountReaderWithClaim(t, "worlds", corev1.ReadWriteMany)
	mounts := []spawneryv1alpha1.Mount{{
		Name:                  "worlds",
		MountPath:             "/data/worlds",
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "worlds"},
	}}

	if _, _, ok := checkGroupVolumes(context.Background(), c, "minecraft",
		nil, nil, mounts, false, false, true); !ok {
		t.Error("a claim mount was refused with its own flag set")
	}
}
```

Use whatever fixture `mountclaims_test.go` already defines in place of
`mountReaderWithClaim` and the real `Mount`/`MountClaim` field names — read the
file's existing tests at :141 and :161, which already call `checkGroupVolumes`,
and follow them.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run ClaimMount -v`
Expected: FAIL — too many arguments to `checkGroupVolumes`, and once that compiles, the mount is accepted on the plugin flag.

- [ ] **Step 3: Split the parameter**

Give `checkMountClaims` its own `allowed bool` fed from the new flag, and change its refusal text from `--allow-plugin-volumes` to `--allow-mount-volumes`. Add the third bool to `checkGroupVolumes` and pass each one to the check it belongs to. Update both call sites — `servergroup_controller.go:169` and `proxygroup_controller.go:254`.

- [ ] **Step 4: Add the flag**

In `cmd/spawnery-operator/main.go`, beside the other two:

```go
	flag.BoolVar(&allowMountVolumes, "allow-mount-volumes", false,
		"let a group's spec.mounts name a PersistentVolumeClaim. Not a security "+
			"control: a claim is a namespaced object in the same trust domain as the "+
			"group naming it. Until 0.2.x this was governed by --allow-plugin-volumes, "+
			"which now governs only spec.extraPlugins.")
```

- [ ] **Step 5: Run everything**

Run: `go test ./internal/controller/ ./cmd/... -v`
Expected: PASS. Any existing test that set `--allow-plugin-volumes` to reach a claim mount now needs the mount flag instead; that is the breaking change showing up in the suite, and updating those tests is the right response.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/ cmd/spawnery-operator/
git commit -m "feat(controller): claim mounts get --allow-mount-volumes

--allow-plugin-volumes gated spec.mounts as well as spec.extraPlugins,
and said so in its own refusal -- which its name never promised. With a
third claim-consuming field arriving, one flag per feature is the only
shape where each name is true.

Breaking: an installation using claim mounts must set
--allow-mount-volumes after upgrading. The failure is loud and names its
own fix -- the group goes Accepted=False with MountVolumesDisabled and a
message carrying the flag.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 7: The chart, the docs and the upgrade note

**Files:**
- Modify: `charts/spawnery/values.yaml` (:93, beside `allowPluginVolumes`)
- Modify: `charts/spawnery/templates/deployment.yaml` (:54)
- Modify: `charts/spawnery/README.md` (:151)
- Modify: `docs/mounts.md`, `docs/plugins.md`, `docs/upgrading.md`

**Interfaces:**
- Consumes: `--allow-file-volumes` from Task 5 and `--allow-mount-volumes` from Task 6.
- Produces: nothing.

- [ ] **Step 1: Add the chart value**

In `charts/spawnery/values.yaml`, after `allowPluginVolumes: false`:

```yaml
  # Lets a group name a spec.extraFiles claim whose tree is copied into every
  # server's working directory on start -- the way to deliver a file that is
  # not a plugin and that no mount can reach, config/sponge/sponge.conf being
  # the case that motivated it.
  #
  # Like allowPluginVolumes, this is not a security control. It exists so an
  # operator can say "this installation runs nothing an administrator dropped
  # in" and have that be a fact rather than a convention.
  allowFileVolumes: false
```

- [ ] **Step 2: Add the mount value**

Also in `charts/spawnery/values.yaml`:

```yaml
  # Lets a group's spec.mounts name a PersistentVolumeClaim. Until 0.2.x this
  # was governed by allowPluginVolumes, whose name never promised it; each of
  # the three claim-consuming fields has its own switch now.
  #
  # Not a security control, for the same reason as the other two.
  allowMountVolumes: false
```

- [ ] **Step 3: Pass both to the operator**

In `charts/spawnery/templates/deployment.yaml`, after the `--allow-plugin-volumes` line:

```yaml
            - --allow-file-volumes={{ .Values.operator.allowFileVolumes }}
            - --allow-mount-volumes={{ .Values.operator.allowMountVolumes }}
```

- [ ] **Step 4: Document all three**

Add two rows to the table in `charts/spawnery/README.md` beside `operator.allowPluginVolumes`, each in that row's own register: what it does, then **It is not a security control.** and why, then what it actually buys. Edit the existing `allowPluginVolumes` row too — it currently describes a flag that also governs claim mounts, and after Task 6 it does not.

In `docs/mounts.md`, add `extraFiles` to whatever comparison of the delivery mechanisms that file carries, including the two properties an administrator will otherwise discover the hard way: **the source wins on every start**, so a world does not belong in this claim, and **nothing about the volume reaches the pod hash**, so a changed file reaches a server on its next start rather than replacing one.

In `docs/plugins.md`, add one sentence pointing at `extraFiles` for configuration that does not live under `plugins/`.

- [ ] **Step 5: Write the upgrade note**

Add a section to `docs/upgrading.md` in the register the `## Plugins can come from a volume, and nothing moves until you ask` section (:586) uses — that one is the model, including its habit of saying plainly what does *not* move. It must carry:

- **the breaking half:** an installation that set `--allow-plugin-volumes` and uses claim-backed `spec.mounts` must now also set `--allow-mount-volumes`, or those groups go `Accepted=False` with `MountVolumesDisabled`. Quote the message they will see.
- **the inert half:** `extraFiles` moves no pod. A group that does not set it renders the pod it rendered before, and `internal/podspec/hash_golden_test.go` is unchanged — checked, not assumed.
- **what turning it on costs:** adding the field to a group rolls that group, because the rendered pod really is different. One group, when you edit it, not the fleet on upgrade.

- [ ] **Step 6: Verify the chart renders**

Run: `make chart-lint`
Expected: PASS.

Run: `helm template charts/spawnery --set operator.allowFileVolumes=true --set operator.allowMountVolumes=true | grep -E 'allow-(file|mount)-volumes'`
Expected: both `- --allow-file-volumes=true` and `- --allow-mount-volumes=true`

- [ ] **Step 7: Run everything**

Run: `make test`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add charts/ docs/
git commit -m "docs(chart): a switch per claim field, and where extraFiles sits

Both new switches in the chart, and the existing row corrected: it
described a flag that governed claim mounts as well, and it no longer
does. The upgrade note carries the breaking half and the inert half
separately, because "a new field" reads as "something is about to
change" and here almost nothing is.

Two properties an administrator would otherwise find out the hard way
are written down: the source wins on every start, so a world does not
belong in this claim, and nothing about the volume reaches the pod hash,
so a changed file arrives on the next start rather than replacing a
server.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Self-review notes

**Spec coverage.** 3.1 → Task 1; 3.2 → Task 2; 3.3 → Tasks 3 and 4; 3.4 → Task 3 Step 3 (the copy is unconditional, which is what "the source wins" means) and written down in Task 7; 3.5 → Task 3's block position and its comment; 3.6 → Tasks 5, 6 and 7 together (the new switch, the narrowed one, and the documentation of all three); 3.7 → Task 1 Step 5, and again in Task 7's upgrade note; 3.8 → Task 5 via `checkClaimMountable`. Section 5's test list is spread across Tasks 2-6 and every entry has a test.

**One vague instruction left, deliberately.** Task 6 Step 1 says to use whatever fixture `mountclaims_test.go` already defines rather than naming it, because that file was not read while writing this plan — only its two `checkGroupVolumes` call sites at :141 and :161 were. An implementer must read it. Every other reference in this plan is to a name and line checked in the tree.

**Not covered by any task, deliberately:** section 6's open question about a Paper equivalent of `lang/`. It needs a measured start, not a code change, and belongs in a follow-up.

**Independently shippable.** Tasks 1-5 and 7 are `extraFiles`. Task 6 is the correction to `--allow-plugin-volumes` and stands on its own — if the breaking change needs to wait for a release boundary, it can be lifted out and the rest still works, with Task 7's chart rows and upgrade note trimmed to match.
