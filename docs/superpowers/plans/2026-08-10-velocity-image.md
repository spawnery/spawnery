# Velocity Image and Configuration Rendering Implementation Plan (milestone 3b)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A second image this repository builds — Velocity — and one program inside both images that turns the operator's rendered configuration into the files Paper and Velocity actually read, so that modern forwarding is configured and a backend is no longer directly joinable.

**Architecture:** `nix/oci-common.nix` is extracted first, while there is still exactly one consumer, and both images are built on it. `cmd/spawnery-config` is baked into both; its logic lives in `internal/render` and resolves three layers in one place — the operator's rendered ConfigMap, a user overlay in the target's own dialect, and critical fields that nothing can reach. The operator renders one ConfigMap per group and mounts it read-only at `/etc/spawnery`, a path neither process ever writes to, so the `/data/config` collision never arises.

**Tech Stack:** Nix (`dockerTools.buildLayeredImage`, `buildGoModule`), Go, POSIX sh, `sigs.k8s.io/yaml` (already a direct dependency), `github.com/pelletier/go-toml/v2` (new — see Global Constraints), controller-runtime, envtest.

**Spec:** `docs/superpowers/specs/2026-08-10-velocity-image-design.md`. Read it before Task 1; the "why" behind every decision below is there in long form.

## Global Constraints

These bind every task.

- **Everything in this repository is written in English** — code, comments, commit messages, documentation. No exceptions.
- **Commit messages use Conventional Commits**, as of 2026-08-10: `feat(<scope>): what changed`, `fix(<scope>): …`, `chore(<scope>): …`, `docs(<scope>): …`. The scope is the part of the project touched (`nix`, `render`, `image`, `podspec`, `controller`, `docs`). **This is a change of convention.** Every commit on this branch before `0713293` uses a sentence-style subject with no prefix, and the milestone 3a plan lists "no `feat:`/`fix:` prefixes" as a constraint — that rule is superseded. Do not match `git log` here; match this section.
- **The subject says what changed; the body still says why.** Hard-wrap the body at **72 columns** — count them. Blank line between subject, body and trailer. Use a heredoc (`git commit -F - <<'EOF'`), not `-m`. End with `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Every file carries the Apache 2.0 header** the rest of the repository uses.
- **Every new exported symbol carries a doc comment that says why, not what.** Comment density here is unusually high and deliberate.
- **One new direct Go dependency is authorised: `github.com/pelletier/go-toml/v2`.** Nothing else. It exists because a user overlay for `velocity.toml` has to be parsed and merged, and hand-rolling a TOML parser is strictly worse than one well-maintained library. YAML is already available through `sigs.k8s.io/yaml`; do not add a second YAML library.
- **`make test` stays Go-only.** Measured at the 3a merge: `go test ./... -count=1` takes **37.7 s**. Do not accept a larger number without knowing which test bought it.
- **A critical field is unreachable from any lower layer.** If a test can set `online-mode` from the ConfigMap or the overlay, the layering is wrong regardless of what else passes.
- **Both images run as `10001:10001` with a read-only root filesystem**, and that fact comes from `nix/oci-common.nix` after Task 1, not from two files agreeing.

## A deliberate change in how this plan is written

The milestone 3a plan inlined finished implementations, and its whole-branch
review named the consequence: when a plan hands over working code, the
implementer's job silently becomes "type this in" and the reviewer's becomes
"does this match the plan". Every defect that review traced to the plan itself
was of that shape — a `Join` that forgot to close a superseded session, a
struct field nothing read, comments that were already stale when written.

So this plan gives **invariants, exact names and signatures, and the tests that
must pass**, and inlines code only where code is genuinely clearer than prose.
Where a step describes an implementation rather than spelling it out, that is
not an omission: the names, the constraints and the tests are the specification,
and the implementer is expected to reason about the body rather than transcribe
it. Anything a step leaves to judgement, it says so explicitly.

Two things follow. Deviating from a described shape is fine when the tests still
pin the behaviour — say so in the report. And a block that *is* inlined is
inlined because getting it wrong is expensive; treat those as normative.

## Toolchain

Every build and test command must be run through the flake, and this machine needs the experimental flags spelled out:

    nix --extra-experimental-features 'nix-command flakes' develop -c <command>

A bare `go test` fails with "command not found"; envtest packages fail without `KUBEBUILDER_ASSETS`, which only the devShell sets. `make test` takes about 40 s in total and `internal/controller` about 34 of them — envtest booting a real API server, not a hang.

Image work additionally needs a container runtime and only works on `x86_64-linux`. `make image-test` and `make image-repro` are the targets.

## File Structure

**Created:**

| File | Responsibility |
|---|---|
| `nix/oci-common.nix` | What both images need verbatim: the numeric user, the entrypoint shebang rewrite, the binary copy, the layered-image frame |
| `nix/velocity.nix` | The pinned Velocity jar and the facts measured out of it |
| `nix/velocity-image.nix` | The Velocity image |
| `image/velocity-entrypoint.sh` | Render, copy the agent jar, exec the JVM |
| `internal/render/values.go` | The neutral document the operator renders, and its validation |
| `internal/render/layer.go` | The three-layer merge — the one place precedence is decided |
| `internal/render/paper.go` | The Paper flavour: `server.properties` and `config/paper-global.yml` |
| `internal/render/velocity.go` | The Velocity flavour: `velocity.toml` |
| `internal/render/load.go` | Reading the three inputs off disk, and every refusal |
| `cmd/spawnery-config/main.go` | Flag parsing and exit codes, nothing else |
| `internal/render/*_test.go` | Table tests per file above |

**Modified:**

| File | Change |
|---|---|
| `nix/paper-image.nix` | Rebuilt on `oci-common.nix`; must stay byte-identical |
| `image/entrypoint.sh` | `set_property` and the `SPAWNERY_MAX_PLAYERS` check deleted; runs the renderer |
| `flake.nix` | `velocity`, `velocity-image`, `spawnery-config` packages |
| `Makefile` | `velocity-image-test`, and `image-repro` covering both |
| `api/v1alpha1/servergroup_types.go`, `proxygroup_types.go` | `spec.configOverlay` |
| `internal/podspec/server.go`, `proxy.go`, `labels.go` | The config volume; `SPAWNERY_MAX_PLAYERS` removed |
| `internal/controller/servergroup_controller.go`, `proxygroup_controller.go` | Render and own the group's ConfigMap |
| `internal/rbacaudit/required.go` | Only if a verb changes — check, do not assume |
| `docs/known-issues.md` | Close what 3b closes; record what 3c inherits |

---

### Task 1: `nix/oci-common.nix`, with the Paper image rebuilt on it

The whole value of this task is that it happens while there is exactly one consumer. Doing it after the Velocity image exists means reconciling two copies that have already drifted, and the drift is the kind nobody notices — an image that starts fine while its user or its paths quietly differ from the other one's.

**Files:**
- Create: `nix/oci-common.nix`
- Modify: `nix/paper-image.nix`

**Interfaces:**
- Produces: a Nix attribute set with `passwd`, `group`, `entrypointFrom`, `binIn`, and `layeredImage` — exact shapes below.

- [ ] **Step 1: Record what the Paper image is today**

Before touching anything, capture the current output path. This is the baseline the refactor must not move.

```bash
nix --extra-experimental-features 'nix-command flakes' build .#paper-image --no-link --print-out-paths | tee /tmp/paper-image-before.txt
```

Expected: one `/nix/store/…-docker-image-paper.tar.gz` path. Keep the file; Step 5 compares against it.

- [ ] **Step 2: Write `nix/oci-common.nix`**

```nix
# What both Spawnery images need verbatim.
#
# Extracted while there was exactly one consumer, which is the only cheap time
# to do it: two images that each grew their own copy of the numeric user or the
# binary path would still start, and would differ in ways that only surface as
# a pod that runs as the wrong uid or an exec probe that cannot find its tool.
{ runCommand
, runtimeShell
, writeTextDir
, dockerTools
}:

rec {
  # runAsNonRoot refuses to start an image with no numeric user. The probe in
  # milestone 2b measured that Java itself does not need the passwd entry as
  # long as HOME is set; it is here because a failing getpwuid inside a library
  # on the classpath surfaces as an error that says nothing about its cause.
  uid = 10001;
  gid = 10001;

  passwd = writeTextDir "etc/passwd" ''
    root:x:0:0:root:/root:/bin/sh
    spawnery:x:${toString uid}:${toString gid}:spawnery:/data:/bin/sh
  '';

  group = writeTextDir "etc/group" ''
    root:x:0:
    spawnery:x:${toString gid}:
  '';

  # The shebang is rewritten to a shell that exists in this image. Relying on
  # /bin/sh in a Nix-built image would work today and break the day the tool
  # set changes.
  entrypointFrom = source: runCommand "spawnery-entrypoint" { } ''
    mkdir -p $out/usr/local/bin
    substitute ${source} $out/usr/local/bin/spawnery-entrypoint \
      --replace-fail '#!/bin/sh' '#!${runtimeShell}'
    chmod +x $out/usr/local/bin/spawnery-entrypoint
  '';

  # Copied rather than symlinked, so the path in the image is exactly the one
  # internal/podspec names and does not depend on a store link resolving.
  binIn = { package, name }: runCommand "${name}-image-path" { } ''
    mkdir -p $out/usr/local/bin
    cp ${package}/bin/${name} $out/usr/local/bin/${name}
  '';

  # The frame both images share. Callers pass what differs: the name, the tag,
  # the contents and the environment.
  #
  # architecture is amd64 explicitly rather than the host's: this is only a
  # label, dockerTools.buildLayeredImage does not cross-compile, and it is only
  # true because flake.nix exposes the image attributes exclusively on
  # x86_64-linux. If an image derivation is ever called from elsewhere, that
  # guarantee has to move with it.
  layeredImage = { name, tag, contents, config }: dockerTools.buildLayeredImage {
    inherit name tag contents;
    architecture = "amd64";

    # /data and /tmp are always mounted over in Kubernetes, so their mode there
    # comes from the kubelet, which creates an emptyDir world-writable. The mode
    # set here is what makes the same image usable under a plain container
    # runtime with a fresh volume — which is exactly what the image tests do.
    extraCommands = ''
      mkdir -p data tmp
      chmod 0777 data
      chmod 1777 tmp
    '';

    config = {
      User = "${toString uid}:${toString gid}";
      WorkingDir = "/data";
    } // config;
  };
}
```

- [ ] **Step 3: Rebuild `nix/paper-image.nix` on it**

Replace the inlined `passwd`, `group`, `entrypoint`, `slp` and `dockerTools.buildLayeredImage` blocks with calls into `oci-common`. Keep every comment that explains *why* a thing is the way it is — the layer-ordering rationale, the `contents`-not-`copyToRoot` note, the Paper build label. Those are the parts that cost real time to rediscover.

The derivation now takes `oci-common` as an argument. Wire it in `flake.nix` with `pkgs.callPackage ./nix/oci-common.nix { }`.

`config` for Paper keeps `Env`, `ExposedPorts`, `Entrypoint` and `Labels`; `User` and `WorkingDir` come from `layeredImage`.

- [ ] **Step 4: Build it**

```bash
nix --extra-experimental-features 'nix-command flakes' build .#paper-image --no-link --print-out-paths
```

Expected: a store path.

- [ ] **Step 5: Prove the refactor moved nothing**

```bash
diff <(cat /tmp/paper-image-before.txt) <(nix --extra-experimental-features 'nix-command flakes' build .#paper-image --no-link --print-out-paths)
```

Expected: no output. The store path is content-addressed by its inputs, so an identical path is proof the image did not change.

If the paths differ, do not proceed and do not "fix" it by accepting the new one. Compare the two tarballs (`tar tvf` both and diff the listings) and find what moved. A refactor that changes the image is not the refactor this task asked for.

- [ ] **Step 6: Run the image test**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make image-test
```

Expected: the existing Paper smoke test passes unchanged.

- [ ] **Step 7: Commit**

```bash
git add nix/oci-common.nix nix/paper-image.nix flake.nix
git commit -F - <<'EOF'
refactor(nix): extract oci-common while one image still consumes it

The numeric user, the entrypoint shebang rewrite, the binary copy into
/usr/local/bin and the layered-image frame are the four things the
Velocity image needs verbatim. Extracting them with two consumers means
reconciling copies that have already drifted, and this drift is the kind
nobody notices: an image that starts fine while its uid or its tool
paths quietly differ from the other one's.

The Paper image's store path is unchanged by the extraction, which is
what makes this a refactor rather than a rebuild.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 2: `nix/velocity.nix` — the pin, and the facts measured out of the jar

Velocity is a fat jar and needs no build-time patching, so this is much simpler than `nix/paper.nix`. What it is not is guesswork: the version, the URL, the hash and the config version all get measured and written down here.

**Files:**
- Create: `nix/velocity.nix`
- Modify: `flake.nix`

**Interfaces:**
- Produces: `velocity.velocityVersion`, `velocity.velocityBuild`, `velocity.jar`, and a comment recording `configVersion`.

- [ ] **Step 1: Find the current Velocity build**

PaperMC publishes Velocity through the same fill API Paper uses. Query it:

```bash
curl -s https://fill.papermc.io/v3/projects/velocity | head -40
```

Pick the latest stable version, then list its builds:

```bash
curl -s https://fill.papermc.io/v3/projects/velocity/versions/<VERSION>/builds | head -60
```

Take the newest build whose channel is stable, and record its download URL and SHA-256 from the response. Write both down; they go into the derivation.

If the API shape has changed since this plan was written, do not guess — report it. A wrong URL that happens to fetch something is worse than a failed task.

- [ ] **Step 2: Write the derivation**

```nix
# The pinned Velocity artifact.
#
# Unlike Paper, this needs no build-time patching: Velocity ships as a fat jar
# with nothing to download on first start. The hash was computed from a
# download and checked in here; that does not make the source trustworthy, it
# makes the artifact frozen — a changed upstream breaks the build instead of
# substituting a jar quietly.
{ fetchurl }:

rec {
  velocityVersion = "<VERSION>";
  velocityBuild = "<BUILD>";

  jar = fetchurl {
    url = "<URL from step 1>";
    hash = "<sha256 from step 1, in SRI form>";
  };
}
```

Convert the API's hex SHA-256 to SRI form with:

```bash
nix --extra-experimental-features 'nix-command flakes' hash convert --hash-algo sha256 --to sri <HEX>
```

- [ ] **Step 3: Measure the config version out of the jar**

Velocity validates and migrates `velocity.toml` against a `config-version` it carries internally. The renderer has to write the version this pinned jar expects, and the only reliable source is the jar itself:

```bash
JAR=$(nix --extra-experimental-features 'nix-command flakes' build .#velocity.jar --no-link --print-out-paths)
unzip -p "$JAR" default-velocity.toml
```

Expected: the complete default configuration, starting with a `config-version` line.

Record two things as a comment in `nix/velocity.nix`: the `config-version` value, and the command above that produced it. Task 5 writes that exact value into the rendered file, and a future version bump has to re-measure rather than assume.

If `default-velocity.toml` is not at that path inside the jar, list the archive (`unzip -l "$JAR" | grep -i toml`) and record the real path in the comment.

- [ ] **Step 4: Wire it into `flake.nix`**

```nix
velocity = pkgs.callPackage ./nix/velocity.nix { };
```

and expose `velocity-jar = velocity.jar;` beside `paper-repo`, so it can be fetched and inspected without building an image. Jars are architecture-independent, so it belongs outside the `x86_64-linux` block.

- [ ] **Step 5: Build it**

```bash
nix --extra-experimental-features 'nix-command flakes' build .#velocity-jar --no-link --print-out-paths
```

Expected: a store path. A hash mismatch here means step 1's hash was recorded wrong — fix the hash, never `--impure` around it.

- [ ] **Step 6: Commit**

```bash
git add nix/velocity.nix flake.nix
git commit -F - <<'EOF'
feat(nix): pin the Velocity jar and record its config version

Velocity needs no build-time patching — it is a fat jar with nothing to
fetch on first start — so this is a fetchurl and two facts beside it.

The config-version is measured out of the pinned jar rather than taken
from documentation, because it is what that jar validates the rendered
velocity.toml against, and a version bump that does not re-measure it
produces a config Velocity migrates out from under the renderer.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 3: `internal/render` — the neutral document and the three-layer merge

This is the one place precedence is decided. Everything downstream is translation.

**Files:**
- Create: `internal/render/values.go`, `internal/render/layer.go`
- Test: `internal/render/values_test.go`, `internal/render/layer_test.go`

**Interfaces:**
- Produces:
  - `render.Values` with `MaxPlayers *int32`, `PlayerLimit *int32`, `Motd *string` (yaml tags `maxPlayers`, `playerLimit`, `motd`)
  - `render.Layer(base, overlay, critical map[string]string) map[string]string`

- [ ] **Step 1: Write the failing tests**

`internal/render/layer_test.go`:

```go
package render

import "testing"

// The order is the whole contract: rendered defaults lose to the user, and
// both lose to the fields an operator cannot be allowed to break.
func TestLayerAppliesThreeSourcesInOrder(t *testing.T) {
	got := Layer(
		map[string]string{"motd": "default", "max-players": "20"},
		map[string]string{"motd": "mine", "online-mode": "true"},
		map[string]string{"online-mode": "false"},
	)

	if got["motd"] != "mine" {
		t.Errorf("motd = %q, want the overlay to outrank the default", got["motd"])
	}
	if got["max-players"] != "20" {
		t.Errorf("max-players = %q, want the default to survive an overlay that does not mention it", got["max-players"])
	}
	// The one that matters: a user who writes online-mode=true into their
	// overlay is asking for a backend anyone can join. They do not get it.
	if got["online-mode"] != "false" {
		t.Errorf("online-mode = %q, want the critical layer to win", got["online-mode"])
	}
}

func TestLayerDoesNotMutateItsInputs(t *testing.T) {
	base := map[string]string{"a": "1"}
	overlay := map[string]string{"a": "2"}
	critical := map[string]string{"a": "3"}

	Layer(base, overlay, critical)

	if base["a"] != "1" || overlay["a"] != "2" || critical["a"] != "3" {
		t.Error("Layer wrote through to one of its inputs")
	}
}

func TestLayerHandlesNilSources(t *testing.T) {
	got := Layer(map[string]string{"a": "1"}, nil, nil)
	if got["a"] != "1" {
		t.Errorf("a = %q, want the base to survive nil upper layers", got["a"])
	}
}
```

`internal/render/values_test.go`:

```go
package render

import (
	"strings"
	"testing"
)

// Absent and zero are different answers and the difference is load-bearing:
// a group that says nothing must be refused, not defaulted to Paper's 20.
func TestValuesRejectsAnAbsentMaxPlayers(t *testing.T) {
	var v Values
	err := v.RequireMaxPlayers()
	if err == nil {
		t.Fatal("an absent maxPlayers was accepted")
	}
	if !strings.Contains(err.Error(), "maxPlayers") {
		t.Errorf("error = %q, want it to name the key", err)
	}
}

func TestValuesRejectsAZeroMaxPlayers(t *testing.T) {
	zero := int32(0)
	v := Values{MaxPlayers: &zero}
	if err := v.RequireMaxPlayers(); err == nil {
		t.Fatal("maxPlayers: 0 was accepted")
	}
}

func TestValuesAcceptsAPositiveMaxPlayers(t *testing.T) {
	n := int32(100)
	v := Values{MaxPlayers: &n}
	if err := v.RequireMaxPlayers(); err != nil {
		t.Fatalf("RequireMaxPlayers: %v", err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -v
```

Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write `values.go`**

```go
// Package render turns the operator's rendered configuration into the files
// Paper and Velocity actually read.
//
// It exists as a package rather than as code inside cmd/ for the same reason
// internal/slp does: the interesting part is a pure function of its inputs,
// and a table test is a far better way to find out whether online-mode came
// out right than starting a container.
package render

import "fmt"

// Values is the neutral document the operator renders into a ConfigMap. It is
// deliberately neither target's dialect: the operator stays out of the
// business of TOML and YAML, and adding a field later is one CRD field and one
// line in a flavour.
//
// Every field is a pointer because absent and zero are different answers, and
// the difference decides whether a server starts. See RequireMaxPlayers.
type Values struct {
	// MaxPlayers is a backend's player capacity. The Paper agent reports it to
	// the operator as slots, and the operator scales on that number.
	MaxPlayers *int32 `yaml:"maxPlayers,omitempty" json:"maxPlayers,omitempty"`
	// PlayerLimit is a proxy's player capacity.
	PlayerLimit *int32 `yaml:"playerLimit,omitempty" json:"playerLimit,omitempty"`
	// Motd is what a player sees in the server list.
	Motd *string `yaml:"motd,omitempty" json:"motd,omitempty"`
}

// RequireMaxPlayers refuses a backend that does not know its own capacity.
//
// Starting with the upstream default of 20 while the group promises 100 makes
// the operator plan against capacity the server can never honour: it will keep
// sending players to a server that is already full. This refusal lived in
// image/entrypoint.sh against an environment variable until milestone 3b; it
// moved here rather than disappearing.
func (v Values) RequireMaxPlayers() error {
	if v.MaxPlayers == nil {
		return fmt.Errorf("config.yaml: maxPlayers is not set")
	}
	if *v.MaxPlayers <= 0 {
		return fmt.Errorf("config.yaml: maxPlayers is %d, want a positive number", *v.MaxPlayers)
	}
	return nil
}

// RequirePlayerLimit refuses a proxy that does not know its own capacity. The
// agent reports it as slots, and internal/agent.Registry discards any report
// where players exceed slots — so a zero limit would silently throw away every
// player count the proxy ever sent.
func (v Values) RequirePlayerLimit() error {
	if v.PlayerLimit == nil {
		return fmt.Errorf("config.yaml: playerLimit is not set")
	}
	if *v.PlayerLimit <= 0 {
		return fmt.Errorf("config.yaml: playerLimit is %d, want a positive number", *v.PlayerLimit)
	}
	return nil
}
```

- [ ] **Step 4: Write `layer.go`**

```go
package render

// Layer resolves the three configuration sources into one flat key set.
//
// The order is the contract section 3 of the design fixes and is the reason
// this is a function rather than three assignments spread through two
// flavours: rendered defaults lose to the user's overlay, and both lose to the
// fields an operator must not be able to break. A flavour that applied them in
// its own order would be a second answer to a question that has one.
//
// Every target format reduces to a flat key set before it is serialised —
// dotted keys for the nested ones — so one merge serves all three files.
//
// The inputs are not mutated: callers hold them for the next file.
func Layer(base, overlay, critical map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay)+len(critical))
	for _, source := range []map[string]string{base, overlay, critical} {
		for k, v := range source {
			out[k] = v
		}
	}
	return out
}
```

- [ ] **Step 5: Run the tests**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -v
```

Expected: PASS, six tests.

- [ ] **Step 6: Commit**

```bash
git add internal/render
git commit -F - <<'EOF'
feat(render): resolve configuration precedence in one place

Rendered defaults lose to the user's overlay and both lose to the fields
an operator must not be able to break. Putting that order in a function
rather than in each flavour is what stops it from becoming two answers
to a question that has one — and the field it protects is online-mode,
where the wrong answer starts cleanly and leaves every backend joinable.

Values uses pointers because absent and zero are different answers: a
group that says nothing about maxPlayers must be refused, not defaulted
to the upstream 20, or the operator plans against capacity the server
cannot honour.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 4: The Paper flavour

**Files:**
- Create: `internal/render/paper.go`, `internal/render/paper_test.go`

**Interfaces:**
- Consumes: `render.Values`, `render.Layer` (Task 3)
- Produces: `render.Paper(v Values, secret string, overlay map[string]string) (map[string][]byte, error)` — returns file paths relative to `/data` mapped to their contents.

- [ ] **Step 1: Write the failing tests**

`internal/render/paper_test.go`:

```go
package render

import (
	"strings"
	"testing"
)

func paperValues() Values {
	n := int32(100)
	return Values{MaxPlayers: &n}
}

// The two file names Paper reads, at the paths it reads them from.
func TestPaperWritesBothFiles(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	for _, name := range []string{"server.properties", "config/paper-global.yml"} {
		if _, ok := files[name]; !ok {
			t.Errorf("no %s among %v", name, keysOf(files))
		}
	}
}

// The inversion this milestone exists to get right, on the backend half.
func TestPaperTurnsOnlineModeOff(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	props := string(files["server.properties"])
	if !strings.Contains(props, "online-mode=false") {
		t.Errorf("server.properties does not contain online-mode=false:\n%s", props)
	}
}

// Paper uses the same two words for the opposite setting: paper-global.yml's
// proxies.velocity.online-mode means "trust what Velocity forwards", and it
// must be true while server.properties says false. Both at once is correct.
func TestPaperEnablesVelocityForwarding(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	global := string(files["config/paper-global.yml"])
	for _, want := range []string{"enabled: true", "online-mode: true", "s3cret"} {
		if !strings.Contains(global, want) {
			t.Errorf("paper-global.yml does not contain %q:\n%s", want, global)
		}
	}
}

func TestPaperCarriesMaxPlayersThrough(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if !strings.Contains(string(files["server.properties"]), "max-players=100") {
		t.Error("max-players did not reach server.properties")
	}
}

// An overlay reaches a field the API does not model.
func TestPaperOverlayReachesAnUnmodelledField(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"server.properties": "view-distance=8\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	if !strings.Contains(string(files["server.properties"]), "view-distance=8") {
		t.Error("the overlay did not reach server.properties")
	}
}

// And cannot reach a critical one.
func TestPaperOverlayCannotTurnOnlineModeOn(t *testing.T) {
	files, err := Paper(paperValues(), "s3cret", map[string]string{
		"server.properties": "online-mode=true\nserver-port=1234\n",
	})
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	props := string(files["server.properties"])
	if strings.Contains(props, "online-mode=true") {
		t.Errorf("an overlay turned online-mode on:\n%s", props)
	}
	if !strings.Contains(props, "server-port=25565") {
		t.Errorf("an overlay moved the port:\n%s", props)
	}
}

func TestPaperRefusesAnEmptySecret(t *testing.T) {
	_, err := Paper(paperValues(), "", nil)
	if err == nil {
		t.Fatal("an empty forwarding secret was accepted")
	}
	if !strings.Contains(err.Error(), "forwarding secret") {
		t.Errorf("error = %q, want it to name the secret", err)
	}
}

func TestPaperRefusesAnOverlayForAFileItDoesNotWrite(t *testing.T) {
	_, err := Paper(paperValues(), "s3cret", map[string]string{"velocity.toml": "x = 1\n"})
	if err == nil {
		t.Fatal("an overlay for a foreign file was accepted")
	}
	if !strings.Contains(err.Error(), "velocity.toml") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -run TestPaper -v
```

Expected: FAIL — `undefined: Paper`.

- [ ] **Step 3: Write `paper.go`**

Structure it as: parse the overlay fragments, build each file as `Layer(base, overlay, critical)`, serialise.

`server.properties` is a flat properties file, so its key set is already flat. `paper-global.yml` is nested; represent it with dotted keys and nest on write — with only three keys under `proxies.velocity`, write the YAML directly rather than pulling in a generic nester.

```go
package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PaperFiles are the files the Paper flavour writes, relative to /data.
var PaperFiles = []string{"server.properties", "config/paper-global.yml"}

// Paper renders the two files a Spawnery-managed Paper server reads.
//
// The three fields in server.properties that the operator relies on are in the
// critical layer and no overlay can move them:
//
//   - server-port, because internal/podspec names 25565 and a pod whose
//     process listens elsewhere passes no probe;
//   - online-mode=false, because the proxy authenticates players and forwards
//     the result — with it on, modern forwarding fails every join;
//   - enable-status, because with it off the server answers no server list
//     ping, the readiness probe stays red forever, and nothing in the log says
//     why.
func Paper(v Values, secret string, overlay map[string]string) (map[string][]byte, error) {
	if err := v.RequireMaxPlayers(); err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, fmt.Errorf("the forwarding secret is empty: a backend with online-mode=false and no secret is joinable by anyone")
	}
	if err := checkOverlayFiles(overlay, PaperFiles); err != nil {
		return nil, err
	}

	props := Layer(
		map[string]string{
			"max-players": strconv.FormatInt(int64(*v.MaxPlayers), 10),
			"motd":        valueOr(v.Motd, ""),
		},
		parseProperties(overlay["server.properties"]),
		map[string]string{
			"server-port":            "25565",
			"online-mode":            "false",
			"enable-status":          "true",
			"enforce-secure-profile": "false",
		},
	)

	return map[string][]byte{
		"server.properties":       []byte(writeProperties(props)),
		"config/paper-global.yml": []byte(paperGlobal(secret, overlay["paper-global.yml"])),
	}, nil
}
```

Write the helpers below it. Two of them are inlined here because their exact behaviour is load-bearing:

```go
// parseProperties reads a .properties fragment into a flat key set. Blank
// lines and comments are skipped; a line with no '=' is skipped rather than
// failing, because a properties file with a stray line is still a properties
// file and refusing one would turn a harmless overlay into a crash loop.
func parseProperties(fragment string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(fragment, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

// writeProperties serialises a flat key set.
//
// Sorted, because Go map iteration is randomised: an unsorted file changes its
// bytes on every render, which breaks nothing at runtime and makes every diff
// between two pods useless for telling whether their configuration differs.
func writeProperties(props map[string]string) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, props[k])
	}
	return b.String()
}
```

The other three — `checkOverlayFiles`, `paperGlobal` and `valueOr` — are yours to write against the tests above. `checkOverlayFiles` refuses any overlay key not in the flavour's file list, naming the offending file. `valueOr` dereferences a pointer or returns a fallback.

`paperGlobal` writes the minimum Paper needs, with the secret quoted:

```go
// paperGlobal writes the Velocity block of paper-global.yml.
//
// proxies.velocity.online-mode: true reads as the opposite of
// server.properties' online-mode=false and both are correct at once: the
// properties flag says "do not authenticate players yourself", this one says
// "trust the authentication result Velocity forwards". Setting this one false
// while the other is false too gives every player an offline-mode UUID, which
// silently detaches them from their own inventories.
func paperGlobal(secret, overlay string) string
```

The overlay for `paper-global.yml` is YAML; parse it with `sigs.k8s.io/yaml` into `map[string]any`, apply it under the rendered base, then overwrite the three critical keys. Keep it to the depth Paper needs.

- [ ] **Step 4: Run the tests**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/render
git commit -F - <<'EOF'
feat(render): render the Paper side of modern forwarding

server.properties gets online-mode=false and paper-global.yml gets
proxies.velocity.online-mode: true. The two read as contradictions and
are both correct: the first says "do not authenticate players yourself",
the second says "trust the result Velocity forwards". Setting the second
false as well hands every player an offline-mode UUID and silently
detaches them from their own inventories.

server-port, online-mode, enable-status and enforce-secure-profile sit
in the critical layer, so an overlay can reach view-distance but cannot
open the server up.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 5: The Velocity flavour

**Files:**
- Create: `internal/render/velocity.go`, `internal/render/velocity_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `render.Values`, `render.Layer`
- Produces: `render.Velocity(v Values, secretPath string, overlay map[string]string) (map[string][]byte, error)`

Note the signature difference from `Paper`: Velocity takes the secret's **path**, not its content. `forwarding-secret-file` points at the mount directly, so the secret never lands in a writable layer.

- [ ] **Step 1: Add the TOML dependency**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go get github.com/pelletier/go-toml/v2
nix --extra-experimental-features 'nix-command flakes' develop -c go mod tidy
```

This is the one new direct dependency this milestone authorises. It exists because a user overlay for `velocity.toml` has to be parsed and merged, and hand-rolling a TOML parser is strictly worse.

Note that adding it changes `vendorHash` in `flake.nix` for every `buildGoModule` package. Build one and take the hash the error reports:

```bash
nix --extra-experimental-features 'nix-command flakes' build .#spawnery-slp 2>&1 | grep -A2 'got:'
```

Update every `vendorHash` in `flake.nix` to the reported value — they are all the same, because they all vendor the same module.

- [ ] **Step 2: Write the failing tests**

```go
package render

import (
	"strings"
	"testing"
)

func velocityValues() Values {
	n := int32(500)
	m := "A Spawnery network"
	return Values{PlayerLimit: &n, Motd: &m}
}

const testSecretPath = "/etc/spawnery/forwarding.secret"

// The proxy half of the inversion: true here, false on the backends.
func TestVelocityKeepsOnlineModeOn(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "online-mode = true") {
		t.Errorf("velocity.toml does not keep online-mode on:\n%s", toml)
	}
}

// 25565, not Velocity's own default of 25577: internal/podspec names 25565 and
// the Service targets it by name.
func TestVelocityBindsThePortThePodspecNames(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !strings.Contains(string(files["velocity.toml"]), `bind = "0.0.0.0:25565"`) {
		t.Errorf("velocity.toml does not bind 25565:\n%s", files["velocity.toml"])
	}
}

func TestVelocityPointsAtTheSecretFileRatherThanCopyingIt(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, `forwarding-secret-file = "`+testSecretPath+`"`) {
		t.Errorf("velocity.toml does not point at the mounted secret:\n%s", toml)
	}
	if strings.Contains(toml, "forwarding-secret =") {
		t.Error("velocity.toml carries the secret inline; it must only reference the file")
	}
}

func TestVelocityUsesModernForwarding(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !strings.Contains(string(files["velocity.toml"]), `player-info-forwarding-mode = "modern"`) {
		t.Error("velocity.toml is not on modern forwarding")
	}
}

// The agent registers backends over the operator channel. A static list here
// would be a second truth about which servers exist.
func TestVelocityShipsNoServers(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "[servers]") {
		t.Error("velocity.toml has no [servers] table at all; Velocity needs the table even when it is empty")
	}
	if strings.Contains(toml, "try = [\"") {
		t.Errorf("velocity.toml ships a non-empty try list:\n%s", toml)
	}
}

func TestVelocityCarriesTheMotdAndLimit(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if !strings.Contains(toml, "A Spawnery network") {
		t.Error("the motd did not reach velocity.toml")
	}
	if !strings.Contains(toml, "show-max-players = 500") {
		t.Error("the player limit did not reach velocity.toml")
	}
}

func TestVelocityOverlayCannotTurnOnlineModeOff(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "online-mode = false\nbind = \"0.0.0.0:1234\"\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	toml := string(files["velocity.toml"])
	if strings.Contains(toml, "online-mode = false") {
		t.Errorf("an overlay turned the proxy's online-mode off:\n%s", toml)
	}
	if !strings.Contains(toml, "25565") {
		t.Errorf("an overlay moved the bind port:\n%s", toml)
	}
}

func TestVelocityOverlayReachesAnUnmodelledField(t *testing.T) {
	files, err := Velocity(velocityValues(), testSecretPath, map[string]string{
		"velocity.toml": "kick-existing-players = true\n",
	})
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !strings.Contains(string(files["velocity.toml"]), "kick-existing-players = true") {
		t.Error("the overlay did not reach velocity.toml")
	}
}

func TestVelocityRefusesAnOverlayForAFileItDoesNotWrite(t *testing.T) {
	_, err := Velocity(velocityValues(), testSecretPath, map[string]string{"server.properties": "x=1\n"})
	if err == nil {
		t.Fatal("an overlay for a foreign file was accepted")
	}
}
```

- [ ] **Step 3: Run them and watch them fail**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -run TestVelocity -v
```

Expected: FAIL — `undefined: Velocity`.

- [ ] **Step 4: Write `velocity.go`**

Build a `map[string]any` for the top level, merge the parsed overlay under it, overwrite the critical keys, then marshal with `go-toml/v2`. Add the `[servers]` table explicitly — Velocity needs the table present even when empty.

`config-version` is the value Task 2 measured out of the jar and recorded in `nix/velocity.nix`. Put it in a named constant here with a comment pointing at that file, so a version bump has one place to look:

```go
// velocityConfigVersion is what the pinned jar validates velocity.toml
// against. Measured out of the jar's own default-velocity.toml — see
// nix/velocity.nix for the version this belongs to and the command that read
// it. A Velocity bump that does not re-measure this produces a config
// Velocity migrates out from under the renderer on first start.
const velocityConfigVersion = "<the value Task 2 recorded>"
```

The critical set for Velocity:

| Key | Value | Why |
|---|---|---|
| `bind` | `"0.0.0.0:25565"` | `internal/podspec` names 25565; the Service targets it by name |
| `online-mode` | `true` | the proxy authenticates players; false makes the whole network offline-mode |
| `player-info-forwarding-mode` | `"modern"` | anything else and the backends cannot verify a forwarded player |
| `forwarding-secret-file` | the mount path | so the secret never lands in a writable layer |

- [ ] **Step 5: Run the tests**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -v
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/render go.mod go.sum flake.nix
git commit -F - <<'EOF'
feat(render): render velocity.toml for modern forwarding

The proxy keeps online-mode on — it is the layer that authenticates
players with Mojang and forwards the result — and binds 25565 rather
than Velocity's own 25577, because internal/podspec names that port and
the Service targets it by name.

forwarding-secret-file points at the mount rather than the renderer
copying the secret, so it never lands in a writable layer. [servers]
ships empty: the agent registers backends over the operator channel, and
a static list here would be a second truth about which servers exist.

Adds go-toml as the one new direct dependency this milestone authorises.
A user overlay for velocity.toml has to be parsed and merged, and hand
rolling a TOML parser is strictly worse than one maintained library.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 6: `internal/render` loading, `cmd/spawnery-config`, and the paired assertion

**Files:**
- Create: `internal/render/load.go`, `internal/render/load_test.go`, `internal/render/paired_test.go`, `cmd/spawnery-config/main.go`
- Modify: `flake.nix`

**Interfaces:**
- Produces:
  - `render.ConfigDir` = `"/etc/spawnery"`, `render.SecretFile`, `render.OverlayDir`, `render.ValuesFile`
  - `render.Load(dir string) (Values, string, map[string]string, error)`
  - `render.WriteAll(root string, files map[string][]byte) error`

- [ ] **Step 1: Write the failing tests**

`internal/render/load_test.go` builds a directory in `t.TempDir()` and asserts each refusal by message: a missing `config.yaml`, an unparseable one, a missing secret file, an empty secret file, and an unreadable overlay entry. Write each as its own test with a `t.TempDir()` fixture — no table, because the failure messages differ and each one is the deliverable.

`internal/render/paired_test.go` is the assertion the design calls out separately:

```go
package render

import (
	"strings"
	"testing"
)

// The one test whose absence would not show up as a failure anywhere else.
//
// Both halves look correct in isolation: a backend with online-mode=false is
// what forwarding needs, and a proxy with online-mode=true is what
// authentication needs. Swap them and everything still starts, every other
// test still passes, and the network is open — anyone can connect straight to
// a backend under any name. Only asserting both in one place says otherwise.
func TestOnlineModeIsOffOnTheBackendAndOnOnTheProxy(t *testing.T) {
	backend, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	proxy, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}

	props := string(backend["server.properties"])
	if !strings.Contains(props, "online-mode=false") {
		t.Errorf("the backend authenticates players itself, which breaks every forwarded join:\n%s", props)
	}

	toml := string(proxy["velocity.toml"])
	if !strings.Contains(toml, "online-mode = true") {
		t.Errorf("the proxy does not authenticate players, so the whole network is offline-mode:\n%s", toml)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -v
```

Expected: FAIL — `undefined: Load`.

- [ ] **Step 3: Write `load.go` and the paths**

```go
package render

// The read-only mount the operator's rendered configuration arrives on.
//
// Deliberately not under /var/run/spawnery: that is the agent's credential
// mount, and podspec.checkMountCollision guards it with a bidirectional
// nesting check it applies to nothing else. Keeping the two apart keeps that
// rule saying the one thing it exists to say.
const (
	ConfigDir   = "/etc/spawnery"
	ValuesFile  = "config.yaml"
	SecretFile  = "forwarding.secret"
	OverlayDir  = "overlay"
)
```

`Load` reads `ValuesFile` with `sigs.k8s.io/yaml`, reads and trims `SecretFile`, and reads every regular file in `OverlayDir` into the overlay map keyed by base name. A missing overlay directory is not an error — the overlay is optional. A missing or empty secret is.

`WriteAll` creates parent directories and writes each file with mode 0644, so `config/paper-global.yml` lands even though `config/` does not exist yet on a fresh volume.

- [ ] **Step 4: Write `cmd/spawnery-config/main.go`**

A thin main: parse `--flavor`, `--config-dir` (defaulting to `render.ConfigDir`) and `--out` (defaulting to `/data`), call `Load`, dispatch to `Paper` or `Velocity`, call `WriteAll`, and exit non-zero with the error on stderr. No logic beyond that — the logic is in the package, where the tests are.

An unknown `--flavor` is a usage error naming both valid values.

**One asymmetry to get right, because the types will not catch it.** `Load` returns the secret's *content*, which is what it needs to refuse an empty one. `Paper` takes that content, because Paper has no file reference for the secret and the renderer must write it into `paper-global.yml`. `Velocity` takes the secret's *path* instead — `filepath.Join(configDir, render.SecretFile)` — because `forwarding-secret-file` points at the mount and the secret must never land in a writable layer. Both are `string`, so passing the wrong one compiles and produces a `velocity.toml` with the secret in plaintext where the path belongs. `TestVelocityPointsAtTheSecretFileRatherThanCopyingIt` is what catches it; do not weaken that test.

- [ ] **Step 5: Add it to `flake.nix`**

Beside `spawnery-slp`, with the same shape: `buildGoModule`, `subPackages = [ "cmd/spawnery-config" ]`, `env.CGO_ENABLED = 0`, `ldflags = [ "-s" "-w" ]` — static, because neither image carries a libc for it.

- [ ] **Step 6: Run everything**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/render/ -v
nix --extra-experimental-features 'nix-command flakes' build .#spawnery-config --no-link --print-out-paths
```

Expected: tests PASS, a store path.

- [ ] **Step 7: Commit**

```bash
git add internal/render cmd/spawnery-config flake.nix
git commit -F - <<'EOF'
feat(render): load the three inputs and refuse rather than guess

A missing config.yaml, a missing or empty forwarding secret, and an
unreadable overlay each stop the start with a message naming the file
and the key. Continuing would be worse in every case: an empty secret
leaves a backend with online-mode=false joinable by anyone, and an
overlay that silently does nothing looks exactly like a configuration
that did not take effect.

Also adds the paired online-mode assertion. Both halves look correct
alone — false on a backend is what forwarding needs, true on a proxy is
what authentication needs — so only checking them together catches the
swap that leaves the network open.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 7: The Velocity image and its entrypoint

**Files:**
- Create: `nix/velocity-image.nix`, `image/velocity-entrypoint.sh`, `hack/velocity-image-test.sh`
- Modify: `flake.nix`, `Makefile`

**Interfaces:**
- Consumes: `oci-common` (Task 1), `velocity` (Task 2), `spawnery-config` (Task 6)

- [ ] **Step 1: Write the entrypoint**

`image/velocity-entrypoint.sh` — smaller than Paper's, because it validates nothing itself:

```sh
#!/bin/sh
# Entrypoint of the Spawnery Velocity base image.
#
# It renders configuration and starts the proxy. Everything it used to be
# tempted to validate lives in spawnery-config, which has the values and can
# name the key that is missing.
set -eu

VELOCITY_HOME="${VELOCITY_HOME:-/opt/velocity}"

# The configuration Velocity actually reads, written from the operator's
# rendered ConfigMap, the user's overlay and the fields neither may move.
/usr/local/bin/spawnery-config --flavor velocity

# The agent plugin. It ships in the read-only part of the image and is copied
# out on every start, unconditionally: the image is the truth, not whatever a
# previous start left in the volume. Milestone 3c is what puts a jar here; the
# copy is written now so the image contract does not change under 3c.
if [ -f "$VELOCITY_HOME/agent/spawnery-agent.jar" ]; then
	mkdir -p plugins
	cp -f "$VELOCITY_HOME/agent/spawnery-agent.jar" plugins/spawnery-agent.jar
fi

# exec, so the JVM becomes PID 1 and receives SIGTERM directly. With a shell in
# between, a proxy would never get its signal and would drop every player on it
# instead of draining.
exec java \
	-XX:MaxRAMPercentage=75 \
	-XX:+UseG1GC \
	-XX:+ParallelRefProcEnabled \
	-XX:MaxGCPauseMillis=200 \
	-XX:+UnlockExperimentalVMOptions \
	-XX:+DisableExplicitGC \
	-XX:+AlwaysPreTouch \
	-jar "$VELOCITY_HOME/velocity.jar"
```

- [ ] **Step 2: Write `nix/velocity-image.nix`**

Built on `oci-common`, layers ordered by rate of change: the JRE and the Velocity jar first, then `spawnery-config`, then the entrypoint. Contents: a `buildEnv` with `bash`, `coreutils` and `jdk25_headless`; `oci-common.passwd`; `oci-common.group`; a `velocityHome` derivation copying the jar to `/opt/velocity/velocity.jar`; `oci-common.binIn { package = spawnery-config; name = "spawnery-config"; }`; `oci-common.entrypointFrom ../image/velocity-entrypoint.sh`.

`config` adds `Env` (`HOME=/data`, `PATH=/bin:/usr/local/bin`, `VELOCITY_HOME=/opt/velocity`), `ExposedPorts` for `25565/tcp`, `Entrypoint`, and `Labels` mirroring the Paper image's with the Velocity version.

No `spawnery-slp`: a proxy's readiness is the agent's ready port, not a server list ping.

- [ ] **Step 3: Wire `flake.nix` and the `Makefile`**

`velocity-image` goes in the `x86_64-linux` block beside `paper-image`, for the same reason and with the same comment. Add `VELOCITY_IMAGE` alongside `IMAGE` in the Makefile, and targets `velocity-image`, `velocity-image-load`, `velocity-image-test`.

Extend `image-repro` to rebuild both.

- [ ] **Step 4: Write the smoke test**

`hack/velocity-image-test.sh`, modelled on `hack/image-test.sh` and running under the same constraints — `--network none`, `--read-only`, `--cap-drop ALL`, no `--user`. It differs in what it needs and what it asserts:

- It must mount a `/etc/spawnery` with a `config.yaml` and a `forwarding.secret`, because the renderer refuses without them. Build that directory on the host and mount it read-only.
- It asserts uid 10001 from the image's own `config.User`, as the Paper test does.
- It waits for a TCP connection on 25565 to be accepted. Velocity with an empty server list will accept the connection and then disconnect the player with "no available server" — that is correct for 3b, and the assertion is deliberately "the port answers", not "a player can join". A player joining is 3c.
- It asserts the rendered `velocity.toml` contains `online-mode = true`, by reading it out of the running container. This is the one place the paired invariant is checked against a real file rather than a unit test's output.

Use `nc -z` or a bash `/dev/tcp` probe from inside the container; do not add a new tool to the image for the test's convenience.

- [ ] **Step 5: Run it**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make velocity-image-test
```

Expected: uid 10001, the port answers, `online-mode = true` present.

- [ ] **Step 6: Prove reproducibility**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make image-repro
```

Expected: both images build identically twice.

- [ ] **Step 7: Commit**

```bash
git add nix/velocity-image.nix image/velocity-entrypoint.sh hack/velocity-image-test.sh flake.nix Makefile
git commit -F - <<'EOF'
feat(image): build the Velocity image on the shared OCI base

The second image this repository builds, and the first consumer of
oci-common besides the one it was extracted from — so the numeric user
and the tool paths are the same by construction rather than because two
files agree.

The entrypoint validates nothing: spawnery-config has the values and can
name the key that is missing, where a shell can only fail with a bare
message. It does exec the JVM, because a proxy that never receives
SIGTERM drops every player on it instead of draining.

The smoke test asserts the port answers, not that a player can join.
Velocity with an empty server list accepts the connection and then
disconnects with "no available server", which is correct here — the
server list arrives with the agent in 3c.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 8: The Paper entrypoint loses `set_property`

**Files:**
- Modify: `image/entrypoint.sh`, `image/entrypoint_test.go`

- [ ] **Step 1: Read what the entrypoint test asserts today**

`image/entrypoint_test.go` exercises the shell. Whatever it says about `set_property` and `SPAWNERY_MAX_PLAYERS` is about to become false; read it before you change the script so the test changes are deliberate rather than reactive.

- [ ] **Step 2: Rewrite the entrypoint**

Delete the `SPAWNERY_MAX_PLAYERS` validation block, the `set_property` function and its three calls, and the `[ -f server.properties ] || : >server.properties` line. Insert the renderer call before the agent-jar copy:

```sh
# The configuration Paper actually reads, written from the operator's rendered
# ConfigMap, the user's overlay and the fields neither may move. It replaces
# the three set_property calls this script used to make: a .properties helper
# in shell could not reach paper-global.yml, which is YAML, and it failed on a
# read-only file with a bare mv message that said nothing about why.
/usr/local/bin/spawnery-config --flavor paper
```

Keep the EULA line — running the image is accepting it, and the README says so rather than leaving it buried.

- [ ] **Step 3: Update the entrypoint test**

Rewrite the assertions that covered `set_property` to cover what the script does now. Do not delete coverage without replacing it: the script still has behaviour worth pinning — the EULA file, the agent-jar copy, and that it `exec`s rather than forks.

- [ ] **Step 4: Run the image test**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make image-test
```

Expected: the Paper server still reaches a server list ping. It will now refuse to start unless `/etc/spawnery` is mounted, so `hack/image-test.sh` needs the same fixture directory the Velocity test builds — add it there too, and drop `-e SPAWNERY_MAX_PLAYERS=100`.

- [ ] **Step 5: Commit**

```bash
git add image internal/render hack
git commit -F - <<'EOF'
refactor(image): hand server.properties to the renderer

set_property was a .properties helper and it did not generalise: the
forwarding secret and online-mode live in paper-global.yml, which is
YAML, and editing YAML from shell is the wrong tool. It also failed on a
read-only server.properties with a bare mv message that said nothing
about why.

SPAWNERY_MAX_PLAYERS leaves the pod contract with it. Only this script
ever read it — the agent takes its slots from Bukkit, which takes them
from server.properties — so the value now travels one way, through the
ConfigMap.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 9: The config volume and `spec.configOverlay`

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go`, `api/v1alpha1/proxygroup_types.go`, `internal/podspec/server.go`, `internal/podspec/proxy.go`, `internal/podspec/labels.go`
- Test: `internal/podspec/server_test.go`, `internal/podspec/proxy_test.go`

**Interfaces:**
- Produces: `podspec.ConfigVolumeName`, `podspec.ConfigMountPath` = `"/etc/spawnery"`, `podspec.ConfigValuesKey`, `podspec.ForwardingSecretKey`, `podspec.GroupConfigMapName(group string) string`

- [ ] **Step 1: Add the API field**

To both `ServerGroupSpec` and `ProxyGroupSpec`:

```go
	// ConfigOverlay names a ConfigMap whose keys are configuration files to
	// merge over the rendered defaults — "server.properties",
	// "paper-global.yml" or "velocity.toml", in the target's own dialect.
	//
	// It is a field of its own rather than a reserved name inside mounts,
	// because mounts is documented as raw files for plugins and worlds and a
	// name-based convention is invisible until someone picks that name by
	// accident. It outranks the rendered defaults and is outranked by the
	// operationally critical fields, which nothing can reach.
	// +optional
	ConfigOverlay *ObjectRef `json:"configOverlay,omitempty"`
```

- [ ] **Step 2: Write the failing podspec tests**

Assert, on both builders: a volume named `ConfigVolumeName` mounted read-only at `/etc/spawnery`; its projected sources are the group's ConfigMap and the Network's forwarding secret; a third source appears only when `configOverlay` is set; and — on the server builder — that `SPAWNERY_MAX_PLAYERS` is gone.

```go
func TestServerPodNoLongerCarriesMaxPlayersAsAnEnvVar(t *testing.T) {
	pod := buildServer(t) // the existing helper
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "SPAWNERY_MAX_PLAYERS" {
			t.Error("SPAWNERY_MAX_PLAYERS is still on the pod; the value travels through the ConfigMap now")
		}
	}
}
```

- [ ] **Step 3: Run and watch them fail**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/podspec/ -v
```

- [ ] **Step 4: Implement**

Add the constants to `internal/podspec/agent.go` beside the existing mount paths, with the comment about why this is not under `AgentMountPath`. Add the volume to both builders. Extend `checkMountCollision` to refuse a user mount at or nested inside `ConfigMountPath` — the same bidirectional check `AgentMountPath` gets, for the same reason: a user mount there would shadow the file the renderer reads its forwarding secret from.

- [ ] **Step 5: Regenerate and run**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make manifests
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/podspec/ -v
```

- [ ] **Step 6: Commit**

```bash
git add api internal/podspec config/crd
git commit -F - <<'EOF'
feat(podspec): mount the rendered configuration at /etc/spawnery

One projected volume carries the group's rendered ConfigMap, the
network's forwarding secret and the user's overlay. It is mounted where
neither process ever writes, so the /data/config collision that
known-issues has recorded since 2b never arises rather than getting
resolved.

Deliberately not under /var/run/spawnery: that is the agent's credential
mount, and checkMountCollision guards it with a bidirectional nesting
check it applies to nothing else. The new path gets the same check for
the same reason — a user mount there would shadow the file the renderer
reads the forwarding secret from.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 10: The operator renders the ConfigMap

**Files:**
- Modify: `internal/controller/servergroup_controller.go`, `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/servergroup_controller_test.go`, `internal/controller/proxygroup_controller_test.go`

- [ ] **Step 1: Write the failing tests**

For each controller: after a reconcile, a ConfigMap named `podspec.GroupConfigMapName(group)` exists in the namespace, is owned by the group, carries `podspec.LabelManagedBy`, and its `config.yaml` key parses into the expected values. Plus: changing `spec.config` and reconciling again updates it.

The managed-by assertion is not decoration. `cmd/spawnery-operator` narrows the manager's cache for ConfigMaps to that label, so an unlabelled one is invisible to the controller that just wrote it — the exact trap `Bootstrapper.ensureConfigMap` documents at length.

- [ ] **Step 2: Run and watch them fail**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'ConfigMap' -v
```

- [ ] **Step 3: Implement `reconcileConfigMap` on both controllers**

Model it on `ProxyGroupReconciler.reconcileService`: `controllerutil.CreateOrUpdate`, set the label inside the mutate function, `SetControllerReference` so deletion cascades. Marshal a `render.Values` with `sigs.k8s.io/yaml` into the `config.yaml` key.

**Call it before any pod is created** — a pod whose projected volume names a ConfigMap that is not there does not start. In the ProxyGroup controller that means beside the existing `Bootstrap.Ensure` call; in the ServerGroup controller, before `createServer`.

Only `maxPlayers` for server groups; `playerLimit` and `motd` for proxy groups. Critical fields do not go in — they live in the renderer and nowhere else.

- [ ] **Step 4: Run the package**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -v
```

Expected: PASS, including every pre-existing test.

- [ ] **Step 5: Check the audit**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/rbacaudit/ -v
```

`configmaps` already carries `get;list;watch;create;update` for the CA bundle, so this should stay green. If it does not, the marker changed — add the table entry with a real `Why`, do not widen the marker to make it pass.

- [ ] **Step 6: Commit**

```bash
git add internal/controller
git commit -F - <<'EOF'
feat(controller): render one ConfigMap per group

Section 5.4 of the main design promised it and three milestones went by
without it. It carries only what a user can influence — maxPlayers for
server groups, playerLimit and motd for proxy groups. The critical
fields are deliberately absent: they live in the renderer and nowhere
else, so there is one truth per fact.

It is written before any pod of the group is created, because a pod
whose projected volume names a missing ConfigMap does not start, and it
carries the managed-by label, because the manager's cache is narrowed to
that label and an unlabelled one would be invisible to the controller
that just wrote it.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 11: Documentation, the sample, and full verification

**Files:**
- Modify: `docs/known-issues.md`, `config/samples/network.yaml`, `README.md`

- [ ] **Step 1: Close what 3b closed in `docs/known-issues.md`**

Under the milestone 3 preconditions, mark the two image-layer items met, in the pattern the file already uses: keep the original reasoning, add a `*Met* by …` paragraph naming the file and mechanism.

Then strike or amend the three entries this milestone resolved elsewhere in the file: the `/data/config` collision, the `/data/plugins` read-only mount, and `set_property`'s read-only assumption. The first is now structurally impossible; the second still stands for user mounts and should say so rather than being deleted; the third is gone with the function.

- [ ] **Step 2: Record what 3c inherits**

At minimum: that the NetworkPolicy restricting backends to proxies-only is now overdue rather than merely deferred — `online-mode` is off as of this milestone, so the invariant it would guard is finally real, and milestone 6 owning NetworkPolicies as a group is the only reason it is still out. Say that plainly; it is the entry most likely to be read as a formality and it is the one with a security consequence.

- [ ] **Step 3: Update the sample**

`config/samples/network.yaml` gets the Velocity image tag from this milestone, and the header comment stops saying the proxy layer's image does not exist.

- [ ] **Step 4: Full verification**

Run all of it and paste the real output into the final report:

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make manifests
nix --extra-experimental-features 'nix-command flakes' develop -c make generate
nix --extra-experimental-features 'nix-command flakes' develop -c make fmt
nix --extra-experimental-features 'nix-command flakes' develop -c make vet
nix --extra-experimental-features 'nix-command flakes' develop -c make test
nix --extra-experimental-features 'nix-command flakes' develop -c go build ./...
nix --extra-experimental-features 'nix-command flakes' develop -c make image-test
nix --extra-experimental-features 'nix-command flakes' develop -c make velocity-image-test
nix --extra-experimental-features 'nix-command flakes' develop -c make image-repro
```

- [ ] **Step 5: Commit**

```bash
git add docs config README.md
git commit -F - <<'EOF'
docs(3b): record what the second image closed and what it opens

The two image-layer preconditions are met and are kept rather than
deleted, because the reasoning is what 3c inherits.

The entry that matters most is the one easiest to read as a formality:
the NetworkPolicy restricting backends to proxies-only is now overdue
rather than deferred. It was left out of 3a because online-mode was
still on and the policy would have guarded an invariant nothing relied
on. As of this milestone online-mode is off, so the invariant is real
and the only thing still keeping the policy out is that milestone 6 owns
NetworkPolicies as a group.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

## Done when

1. `nix/oci-common.nix` exists, both images build on it, and the Paper image's store path is unchanged by the extraction.
2. `make velocity-image-test` starts the Velocity image offline as uid 10001 and gets an answer on 25565.
3. `spawnery-config --flavor paper` writes `server.properties` and `config/paper-global.yml`; `--flavor velocity` writes `velocity.toml`.
4. A ConfigMap value reaches the written file; an overlay outranks it; a critical field outranks both.
5. A missing secret, a missing `maxPlayers` and an unparseable overlay each refuse the start naming the file and key.
6. `online-mode` is `false` on the backend and `true` on the proxy, asserted in one test.
7. `set_property` and `SPAWNERY_MAX_PLAYERS` appear nowhere in the repository.
8. Both group controllers create and own a labelled ConfigMap before their first pod.
9. `make test` is green; `make image-repro` builds both images identically twice.
