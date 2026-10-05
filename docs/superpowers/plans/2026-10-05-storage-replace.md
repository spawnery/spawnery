# spec.storage.replace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a group name the paths its sources own, so prune deletes them at start without the world guard.

**Architecture:** `internal/prune.Run` takes the replace entries next to keep; planning walks into their ancestors and the guard skips what they match. `spawnery-config --prune` gains `--replace`, the Paper entrypoint passes `SPAWNERY_REPLACE` to it, `internal/podspec` sets that variable from the new `StorageSpec.Replace`, and CEL ties the field to keep.

**Tech Stack:** Go (controller-runtime, kubebuilder markers, envtest), POSIX sh entrypoint, Nix dev shell.

**Spec:** `docs/superpowers/specs/2026-10-05-storage-replace-design.md`

## Global Constraints

- Every build and test command runs in the dev shell: `nix develop -c <command>`.
- On a small machine run envtest packages (`api/v1alpha1`, `internal/podspec` is not one) with `-p 1`.
- Same entry syntax as keep: relative paths, `*` and `?` per segment, 1 to 64 entries of at most 256 characters.
- `SPAWNERY_REPLACE` is emitted only when the field is set; the hash golden in `internal/podspec/hash_golden_test.go` must not move.
- `--replace` with an empty value is the same as no flag.
- A bad replace entry refuses with exit 1 and its message names `spec.storage.replace`.
- No version bump in this branch; the release is a separate PR.
- Commits are Conventional Commits with a scope; body says why, wrapped at 72 columns.
- Generated files are committed: run `make manifests generate` after the API change.

## Review Focus

- A replaced path that is also a mount point, or holds one: the mount must survive (mount check comes before the replace exemption). Pinned in Task 1.
- A replace entry that is a proper ancestor of a keep entry: the kept path survives, its unkept siblings go without the guard. Pinned in Task 1.
- A replace entry with no matching path on the claim: prune runs as without it. Pinned in Task 1.
- An entrypoint without `SPAWNERY_KEEP` but with `SPAWNERY_REPLACE`: prune must not run. Pinned in Task 3.
- An on-demand group setting the field must not roll running members; covered by the existing rule that on-demand members take the group at their next start (no new test; check the podspec change touches only env assembly).

---

### Task 1: prune takes replace entries

**Files:**
- Modify: `internal/prune/prune.go`
- Modify: `internal/prune/prune_test.go`
- Modify: `cmd/spawnery-config/main.go:147` (call site only, passes `nil`)

**Interfaces:**
- Produces: `func Run(dir string, keep, replace []string, mountinfo string, pairs []sourcetree.Pair, log io.Writer) error`

- [ ] **Step 1: Write the failing tests**

In `internal/prune/prune_test.go`, change the helper and add one next to it:

```go
func run(dir string, keep []string, mountinfo string, pairs ...sourcetree.Pair) error {
	return Run(dir, keep, nil, mountinfo, pairs, io.Discard)
}

func runReplacing(dir string, keep, replace []string, mountinfo string, pairs ...sourcetree.Pair) error {
	return Run(dir, keep, replace, mountinfo, pairs, io.Discard)
}
```

Append these tests:

```go
func TestAReplacedWorldIsDeletedEvenWithFilesNoSourceShips(t *testing.T) {
	dir := claim(t, "worlds/world/level.dat",
		"worlds/templates/lobby/region/r.0.0.mca", "worlds/templates/lobby/region/r.0.-2.mca")
	src := claim(t, "worlds/templates/lobby/region/r.0.0.mca")
	err := runReplacing(dir, []string{"worlds/world"}, []string{"worlds/templates"}, noMounts(t),
		sourcetree.Pair{From: src, Into: "."})
	if err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[worlds/world/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestAWorldBesideAReplacedPathStillRefuses(t *testing.T) {
	dir := claim(t, "worlds/world/level.dat",
		"worlds/templates/lobby/region/r.0.0.mca", "worlds/other/region/r.0.0.mca")
	err := runReplacing(dir, []string{"worlds/world"}, []string{"worlds/templates"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "worlds/other") {
		t.Fatalf("err = %v, want a refusal naming worlds/other", err)
	}
	if got := left(t, dir); len(got) != 3 {
		t.Errorf("deleted before refusing: %v", got)
	}
}

func TestAReplaceGlobMatchesPerSegment(t *testing.T) {
	dir := claim(t, "world/level.dat", "worlds/templates/lobby/region/r.0.0.mca",
		"worlds/templates/arena/level.dat", "worlds/templates/readme")
	err := runReplacing(dir, []string{"world"}, []string{"worlds/templates/*"}, noMounts(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[world/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestKeepWinsOverReplace(t *testing.T) {
	dir := claim(t, "worlds/templates/lobby/level.dat", "worlds/templates/old/region/r.0.0.mca")
	err := runReplacing(dir, []string{"worlds/templates/lobby"}, []string{"worlds/templates"}, noMounts(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := left(t, dir); fmt.Sprint(got) != "[worlds/templates/lobby/level.dat]" {
		t.Errorf("left %v", got)
	}
}

func TestAMountUnderAReplacedPathSurvives(t *testing.T) {
	dir := claim(t, "world/level.dat", "worlds/templates/shared/region/r.0.0.mca", "worlds/templates/old/level.dat")
	err := runReplacing(dir, []string{"world"}, []string{"worlds/templates"},
		mountinfoWith(t, filepath.Join(dir, "worlds/templates/shared")))
	if err != nil {
		t.Fatal(err)
	}
	want := "[world/level.dat worlds/templates/shared/region/r.0.0.mca]"
	if got := left(t, dir); fmt.Sprint(got) != want {
		t.Errorf("left %v, want %s", got, want)
	}
}

func TestAReplaceEntryWithNothingToMatchChangesNothing(t *testing.T) {
	dir := claim(t, "world/level.dat", "old/world/level.dat")
	err := runReplacing(dir, []string{"world"}, []string{"worlds/templates"}, noMounts(t))
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Fatalf("err = %v, want the usual refusal naming old", err)
	}
}

func TestABadReplaceEntryRefusesAndNamesTheField(t *testing.T) {
	for _, r := range []string{"", "/x", "a//b", "a[b"} {
		err := runReplacing(claim(t), []string{"world"}, []string{r}, "none")
		if err == nil || !strings.Contains(err.Error(), "spec.storage.replace") {
			t.Errorf("entry %q: err = %v, want a refusal naming spec.storage.replace", r, err)
		}
	}
}
```

`mountinfoWith` writes a one-line mountinfo for a mount point. Check the file for an existing helper used by `TestMountPointsAndTheirParentsAreKept` first and reuse it under its own name if there is one; otherwise add:

```go
func mountinfoWith(t *testing.T, mountPoint string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	line := "100 1 0:50 / " + mountPoint + " rw,relatime - ext4 /dev/sdz rw\n"
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `nix develop -c go test ./internal/prune/ -count=1`
Expected: build failure, `too many arguments in call to Run`.

- [ ] **Step 3: Implement**

In `internal/prune/prune.go`:

```go
func Run(dir string, keep, replace []string, mountinfo string, pairs []sourcetree.Pair, log io.Writer) error {
	pats, err := parseEntries("spec.storage.keep", keep)
	if err != nil {
		return err
	}
	repl, err := parseEntries("spec.storage.replace", replace)
	if err != nil {
		return err
	}
```

Keep the rest of the setup as is, then:

```go
	var doomed []string
	ways := append(append([][]string{}, pats...), repl...)
	if err := plan(root, nil, pats, ways, mounts, &doomed); err != nil {
		return err
	}
	...
	for _, rel := range doomed {
		if kept(repl, strings.Split(rel, "/")) {
			continue
		}
		world, err := holdsWorld(filepath.Join(root, rel))
```

Rename `parseKeep` to `parseEntries(field string, entries []string)` with the error
`fmt.Errorf("%s entry %q is not a relative path of plain, * and ? segments", field, k)`.

Give `plan` the walk patterns separately from the kept ones:

```go
// plan appends to doomed the relative paths below root that are neither kept
// nor on the way to something keep or replace could match.
func plan(root string, rel []string, pats, ways, mounts [][]string, doomed *[]string) error {
	...
		case kept(pats, r) || isOneOf(mounts, r):
		case e.Type()&fs.ModeType == fs.ModeDir && onTheWay(ways, mounts, r):
			if err := plan(root, r, pats, ways, mounts, doomed); err != nil {
```

Update the `Run` doc comment: add "A path a replace entry matches, or one below it, is deleted without that check." after the sentence about worlds.

In `cmd/spawnery-config/main.go`, change the call to `prune.Run(".", keep, nil, *mountinfo, ps, stderr)`.

- [ ] **Step 4: Run the tests**

Run: `nix develop -c go test ./internal/prune/ ./cmd/spawnery-config/ -count=1`
Expected: PASS, including every test that existed before.

- [ ] **Step 5: Commit**

```bash
git add internal/prune cmd/spawnery-config/main.go
git commit -m "feat(prune): replace entries skip the world guard"
```

### Task 2: spawnery-config --replace

**Files:**
- Modify: `cmd/spawnery-config/main.go` (`runPrune`)
- Modify: `cmd/spawnery-config/main_test.go`

**Interfaces:**
- Consumes: `prune.Run(dir, keep, replace, mountinfo, pairs, log)` from Task 1.
- Produces: flag `--replace <entries, one per line>` on `spawnery-config --prune`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/spawnery-config/main_test.go`:

```go
func TestPruneReplaceSkipsTheWorldGuard(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"keep/a", "old/level.dat"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	var stderr bytes.Buffer
	if code := run([]string{"--prune", "keep", "--mountinfo", emptyMountinfo(t), "--replace", "old"}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "old")); err == nil {
		t.Error("old survived")
	}
}

func TestAnEmptyReplaceIsNoReplace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old", "level.dat"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	var stderr bytes.Buffer
	if code := run([]string{"--prune", "world", "--mountinfo", emptyMountinfo(t), "--replace", ""}, &stderr); code != 1 {
		t.Errorf("exit code is %d, want 1: an empty --replace must not exempt anything", code)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `nix develop -c go test ./cmd/spawnery-config/ -run 'Replace' -count=1`
Expected: FAIL, exit 2 with `flag provided but not defined: -replace`.

- [ ] **Step 3: Implement**

In `runPrune`, next to `mountinfo`:

```go
	replace := fs.String("replace", "", "the spec.storage.replace entries, one per line; empty for none")
```

and before the call:

```go
	var repl []string
	if *replace != "" {
		repl = strings.Split(*replace, "\n")
	}
	if err := prune.Run(".", keep, repl, *mountinfo, ps, stderr); err != nil {
```

- [ ] **Step 4: Run the tests**

Run: `nix develop -c go test ./cmd/spawnery-config/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/spawnery-config
git commit -m "feat(spawnery-config): --replace for the prune"
```

### Task 3: the entrypoint passes SPAWNERY_REPLACE

**Files:**
- Modify: `image/entrypoint.sh:88-91`
- Modify: `image/entrypoint_test.go`

**Interfaces:**
- Consumes: `--replace` from Task 2.
- Produces: the entrypoint reads `SPAWNERY_REPLACE`.

- [ ] **Step 1: Write the failing test**

Append to `image/entrypoint_test.go`:

```go
func TestPrunePassesTheReplaceEntries(t *testing.T) {
	out, err := runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_KEEP=world", "SPAWNERY_REPLACE=worlds/templates\nworlds/arena")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--replace worlds/templates\nworlds/arena") {
		t.Errorf("replace entries did not reach the prune:\n%s", out)
	}
	out, err = runEntrypoint(t, t.TempDir(), 0, "SPAWNERY_REPLACE=worlds/templates")
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	if strings.Contains(out, "--prune") {
		t.Errorf("prune ran with replace entries but no keep entries:\n%s", out)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `nix develop -c go test ./image/ -run 'TestPrunePassesTheReplaceEntries' -count=1`
Expected: FAIL, "replace entries did not reach the prune".

- [ ] **Step 3: Implement**

In `image/entrypoint.sh`:

```sh
if [ -n "${SPAWNERY_KEEP:-}" ]; then
	spawnery-config --prune "$SPAWNERY_KEEP" --mountinfo "$MOUNTINFO" \
		--pair "$FILE_SOURCE=." --pair "$PLUGIN_SOURCE=plugins" \
		--replace "${SPAWNERY_REPLACE:-}" || exit 1
fi
```

Extend the comment above the block to "spec.storage.keep and spec.storage.replace." if it names keep alone.

- [ ] **Step 4: Run the image tests**

Run: `nix develop -c go test ./image/ -count=1`
Expected: PASS, including `TestEveryVariableTheEntrypointsReadIsReserved` and `TestPruneRunsOnlyWithKeepEntriesAndBeforeTheRenderer`.

- [ ] **Step 5: Commit**

```bash
git add image/entrypoint.sh image/entrypoint_test.go
git commit -m "feat(image): pass spec.storage.replace to the prune"
```

### Task 4: the API field

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (`StorageSpec`)
- Modify: `api/v1alpha1/servergroup_envtest_test.go`
- Modify: `docs/guides/persistent-worlds.md` (section "What survives a start")
- Generated: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `docs/reference/crds.md`

**Interfaces:**
- Produces: `StorageSpec.Replace []string` (`json:"replace,omitempty"`).

- [ ] **Step 1: Write the failing tests**

Append to `api/v1alpha1/servergroup_envtest_test.go`:

```go
func TestServerGroupStorageReplaceAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	g := onDemandGroup(ns, "replaces-on-demand")
	g.Spec.Storage.Keep = []string{"world"}
	g.Spec.Storage.Replace = []string{"worlds/templates", "worlds/arena-*"}
	if err := c.Create(ctx, g); err != nil {
		t.Fatalf("create on-demand group with replace: %v", err)
	}
}

func TestServerGroupStorageReplaceRefusals(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	tests := map[string]struct {
		keep, replace []string
		msg           string
	}{
		"without keep":   {nil, []string{"worlds/templates"}, "spec.storage.replace needs spec.storage.keep"},
		"in both lists":  {[]string{"world"}, []string{"world"}, "a path cannot be in both"},
		"absolute":       {[]string{"world"}, []string{"/x"}, "a replace entry is a relative path"},
		"parent segment": {[]string{"world"}, []string{"a/../b"}, "a replace entry is a relative path"},
		"empty segment":  {[]string{"world"}, []string{"a//b"}, "a replace entry is a relative path"},
		"bracket":        {[]string{"world"}, []string{"a[b"}, "a replace entry is a relative path"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			g := onDemandGroup(ns, "replace-"+strings.ReplaceAll(name, " ", "-"))
			g.Spec.Storage.Keep = tc.keep
			g.Spec.Storage.Replace = tc.replace
			err := c.Create(ctx, g)
			if err == nil {
				t.Fatalf("keep %q, replace %q was accepted", tc.keep, tc.replace)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("err = %v, want %q", err, tc.msg)
			}
		})
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `nix develop -c go test ./api/v1alpha1/ -run 'StorageReplace' -count=1 -p 1`
Expected: build failure, `g.Spec.Storage.Replace undefined`.

- [ ] **Step 3: Implement**

Above `type StorageSpec struct`, after its doc comment line:

```go
// StorageSpec describes the PVC of a persistent or on-demand group.
// +kubebuilder:validation:XValidation:rule="!has(self.replace) || has(self.keep)",message="spec.storage.replace needs spec.storage.keep"
// +kubebuilder:validation:XValidation:rule="!has(self.replace) || !has(self.keep) || self.replace.all(r, !(r in self.keep))",message="a path cannot be in both spec.storage.keep and spec.storage.replace"
type StorageSpec struct {
```

After the `Keep` field:

```go
	// Replace lists paths on the data claim, relative to /data, that the
	// sources own. A start deletes them like any other path keep does not
	// match, without the check for worlds, and the copy after it writes
	// back what extraFiles and extraPlugins ship now. Meant for world
	// templates the server copies from and never loads, whose shipped form
	// changes between releases. Same syntax as keep; where both match, keep
	// wins. Needs keep.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('/') && !self.contains('[') && !self.contains(']') && !self.contains('\\\\') && !self.contains('\\n') && !self.contains('\\r') && self.split('/').all(s, s != '' && s != '.' && s != '..')",message="a replace entry is a relative path without [ ] \\, line breaks or empty, . and .. segments"
	// +optional
	Replace []string `json:"replace,omitempty"`
```

Regenerate:

Run: `nix develop -c make manifests generate`
Expected: diffs in `zz_generated.deepcopy.go`, `config/crd/bases/spawnery.cloud_servergroups.yaml`, `charts/spawnery/templates/crds.yaml`, `docs/reference/crds.md`, nothing else.

In `docs/guides/persistent-worlds.md`, after the paragraph that ends "so keep one or ship the other.", add:

```markdown
A world template that changes between releases trips the first refusal on
every claim that still holds the old copy. `spec.storage.replace` names such
paths: they are deleted at start without the check, and the copy writes the
current version back.

```yaml
spec:
  storage:
    keep:
      - world
    replace:
      - worlds/templates
```

Entries follow the syntax of `keep`. A path both lists match is kept, the same
entry in both lists is refused, and `replace` without `keep` is refused,
because without `keep` nothing is deleted at all.
```

and extend the upgrade paragraph's first sentence to "before a group uses `keep` or `replace`".

- [ ] **Step 4: Run the tests**

Run: `nix develop -c go test ./api/v1alpha1/ -count=1 -p 1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add api config charts docs
git commit -m "feat(api): spec.storage.replace"
```

### Task 5: podspec delivers SPAWNERY_REPLACE

**Files:**
- Modify: `internal/podspec/sources.go`
- Modify: `internal/podspec/server.go:283-288`
- Modify: `internal/podspec/sources_test.go`

**Interfaces:**
- Consumes: `StorageSpec.Replace` from Task 4.
- Produces: `const EnvReplace = "SPAWNERY_REPLACE"`, read by the entrypoint from Task 3.

- [ ] **Step 1: Write the failing test**

Append to `internal/podspec/sources_test.go`:

```go
func TestReplaceReachesTheContainerOneEntryPerLine(t *testing.T) {
	storage := &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi"),
		Keep: []string{"world"}, Replace: []string{"worlds/templates", "worlds/arena"}}
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Storage = storage
	})
	var got string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvReplace {
			got = e.Value
		}
	}
	if got != "worlds/templates\nworlds/arena" {
		t.Errorf("%s = %q", EnvReplace, got)
	}

	storage.Replace = nil
	if bare := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Storage = storage
	}); envHas(bare, EnvReplace) {
		t.Error("a group without replace got the variable")
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `nix develop -c go test ./internal/podspec/ -run 'TestReplaceReachesTheContainer' -count=1`
Expected: build failure, `undefined: EnvReplace`.

- [ ] **Step 3: Implement**

In `internal/podspec/sources.go`:

```go
// EnvReplace carries spec.storage.replace to the entrypoint, one entry per line.
const EnvReplace = "SPAWNERY_REPLACE"
```

```go
func replaceEnv(s *spawneryv1alpha1.StorageSpec) []corev1.EnvVar {
	if s == nil || len(s.Replace) == 0 {
		return nil
	}
	return []corev1.EnvVar{{Name: EnvReplace, Value: strings.Join(s.Replace, "\n")}}
}
```

In `internal/podspec/server.go`, extend the env assembly by one `append` level:

```go
		Env: append(append(append(append([]corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: "SPAWNERY_SERVER", Value: srv.Name},
			{Name: EnvOperatorEndpoint, Value: agentEndpoint},
		}, substitutionEnv(group.Spec.Substitution)...), keepEnv(group.Spec.Storage)...), replaceEnv(group.Spec.Storage)...), group.Spec.Env...),
```

- [ ] **Step 4: Run the podspec tests and the whole suite**

Run: `nix develop -c go test ./internal/podspec/ -count=1`
Expected: PASS, the hash golden unchanged.

Run: `nix develop -c make test` (on the dev VM: `nix develop -c go test -p 1 ./...` after `make manifests generate` shows no diff)
Expected: PASS, no generated-file drift.

- [ ] **Step 5: Commit**

```bash
git add internal/podspec
git commit -m "feat(podspec): SPAWNERY_REPLACE from spec.storage.replace"
```
