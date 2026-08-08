# Design: The Paper base image and the SLP health tool

**Date:** 2026-08-08
**Status:** Draft for approval
**Scope:** Milestone 2b — the base image a `Server` pod actually starts from, and
the Server-List-Ping tool its readiness probe calls. The Kotlin agent is
milestone 2c and gets a document of its own.

## 1. Purpose

Milestone 2a proved the agent channel end to end, and milestone 1 built the
state machine, but a `Server` pod has never run. `config/samples/network.yaml`
points at an image that does not exist, so the pod hangs in `ErrImagePull` —
that is the documented, expected end state of everything built so far.

This document describes the image that ends it.

**Success criterion:** In k3d, a `Server` pod runs, its readiness probe turns
green because a real Paper process answers a real server list ping — and the
`Server` stays in phase `Starting`, because nobody reports agent readiness.

That last half is not a shortfall, it is the result. Milestone 2a showed that an
agent without pod readiness is not enough; 2b shows the other half of the same
gate. `Ready` is reached in 2c.

**Not in 2b:** the Kotlin agent, the Velocity image, rendering configuration from
a ConfigMap, publishing to a registry, architectures other than `linux/amd64`.

## 2. Why the cut is here

Milestone 2 spans three build worlds; 2a took the Go one. What remains splits
cleanly again: the image build is Nix and containers, the agent is Gradle and
Kotlin. Cutting between them keeps 2b inside build worlds this repository
already has — the SLP tool is Go, and the image is a flake output beside the
devShell that already exists. The JVM toolchain arrives with 2c, where it is
first needed.

The order matters as well. An image without an agent is provable and
interesting: the pod runs, the probe goes green, the server hangs in `Starting`.
An agent without an image would run nowhere near a real Paper process, and the
cluster would look exactly as it does today.

## 3. Precondition: a container runtime

Nothing in this milestone can be proven with `make test`. Building the image,
the smoke test and the k3d run all need Docker or Podman. Both are present on
the machine 2b starts on — verified — which also makes the k3d flow in the
README executable for the first time; milestone 1 and 2a had no runtime and left
it unrun.

**This creates a gate in the other direction.** A Linux image needs a Linux
builder: `nix build .#paper-image` cannot work on `aarch64-darwin` without one.
2a had the mirror image of this problem — envtest was unavailable on Darwin
until the flake fetched the binaries per platform. Here it cannot be fixed with
a hash. The Darwin machine can run `make test` and everything else; it cannot
build or smoke-test the image. That belongs in `known-issues.md` and in the
handover, rather than being discovered.

### 3.1 The assumptions have been measured

Following the precedent of 2a, section 3.1, the load-bearing assumptions were
checked with a throwaway probe before this document was written, under the exact
constraints `internal/podspec` imposes: `--read-only`, `--tmpfs /tmp`, a volume
on `/data`, `--user 10001:10001`, all capabilities dropped, no new privileges,
memory limit 2 GB, and the JVM from `jdk25_headless` (openjdk 25.0.4+7).

| Question | Measurement |
|---|---|
| Does Paper 26.2 start under those constraints? | Yes. `Done` after 4.5–5.1 s. |
| Does it answer SLP? | Yes, and it reports `"max": 100` — the enforced `max-players` shows through. |
| `SIGTERM` to PID 1? | All chunks saved, clean shutdown, exit 143. |
| Is an `/etc/passwd` entry for uid 10001 needed? | **No**, as long as `HOME` is set. Measured on a base with no such entry. |
| Gap between the port accepting and SLP answering, cold start | 1.7–1.8 s. |

The last row deserves an honest reading. 1.8 s is **less** than the
`initialDelaySeconds: 20` the probe already waits. On a world this small a plain
`tcpSocket` probe would have gone green correctly too. The argument for a real
ping is not that the gap is large here — it is that the gap is unbounded: large
worlds and many plugins push the world load far past the point where the port
already accepts. The measurement establishes that the gap exists and which side
of it Paper answers on, not that a port check is wrong today.

### 3.2 What the probe found that the design did not foresee

The Paper distribution jar is not a server. It is a **paperclip bootstrap**, and
on first start it downloads Mojang's server jar from `piston-data.mojang.com`
and patches it. Without network access it dies with `UnknownHostException` and
exit 1.

That breaks the main design's promise in section 5 — "no downloads happen at
runtime" — and it breaks it at the first start of every pod, not at some edge.

The fix falls out of the jar's own metadata. `META-INF/download-context` names
the URL **and** the SHA-256 of the Mojang jar; `META-INF/libraries/` holds every
other dependency inside the jar already, so the Mojang server jar is the only
thing ever fetched. Paperclip understands two system properties:
`paperclip.patchonly` (patch and exit) and `bundlerRepoDir` (where the result
goes).

So the patching moves into the image build, and section 5.1 describes it.
Verified with `--network none`: starts, `Done` after 4.5 s, no download
attempted.

The correction pays twice. Without it, every ephemeral pod extracts and patches
**166 MB into its `emptyDir` on every single start**. With it, `/data` holds
2.1 MB.

## 4. Building blocks

| Path | Task |
|---|---|
| `internal/slp` | The Server-List-Ping protocol: VarInt framing, handshake, status request, deadline. Knows neither flags nor process — provable without Minecraft. |
| `cmd/spawnery-slp` | The binary the readiness probe calls. Flags and exit code, nothing else. |
| `nix/paper.nix` | The pinned fetches for the Paper jar and the Mojang jar, and the derivation that pre-patches them. |
| `nix/paper-image.nix` | `dockerTools.buildLayeredImage`: JRE, the patched repo, entrypoint, SLP binary. |
| `image/entrypoint.sh` | EULA, the operationally critical fields in `server.properties`, `exec java`. |
| `flake.nix` | New outputs `packages.spawnery-slp` and `packages.paper-image`. |
| `Makefile` | `make image`, `make image-load`, `make image-test`. |

**Changed:** `config/samples/network.yaml` (the image reference), `README.md`
(the k3d flow, which now runs through for the first time).

## 5. The image

### 5.1 Pre-patching, and the two pinned hashes

Two fixed-output fetches, both with the hash checked into `nix/paper.nix`:

| Artifact | SHA-256 |
|---|---|
| `paper-26.2-111.jar` | `3ec81e3ea50cc6090b94aab024491846a202702e8a874308a5d7510f6b3aa012` |
| Mojang `server.jar` for 26.2 | `cdacdfb25898de5e4b4b0e5ddcc2722f77067e46605709c2d886c000ebb63ec5` |

The second hash is worth a sentence. It comes from
`META-INF/download-context` **inside the Paper jar**, which is itself pinned by
the first hash — so it does not come from Mojang, the source that serves the
artifact. That satisfies the main design's requirement ("a checksum from the
same source as the artifact only secures the transport, not the upstream") more
cleanly than any other hash in this project. The first hash does not: it was
computed from a download from PaperMC. What it buys is what a checked-in hash
always buys — the artifact is frozen in git, reviewable, and a changed upstream
breaks the build instead of silently substituting a jar.

The build derivation then places the Mojang jar as `cache/mojang_26.2.jar`, runs
`java -Dpaperclip.patchonly=true -DbundlerRepoDir=.`, and captures the resulting
`versions/`, `libraries/` and `cache/`. It needs no network — every input is
already fetched.

**`cache/mojang_26.2.jar` ships in the image**, 61 MB of the repo's 166 MB, even
though nothing reads it after patching. Paperclip touches the cache directory
*before* it decides whether patching is needed at all; on a read-only path it
fails there with `FileSystemException`. Measured, not assumed. Dropping the
61 MB would mean a writable cache directory in every pod, which is worse.

### 5.2 Layers and contents

`buildLayeredImage` splits by rate of change: the JRE at the bottom (large,
rarely changes), the patched Paper repo above it (changes per version), our two
small files on top. In 2c the agent plugin becomes another small layer without
touching the JRE layer.

```
/opt/paper/paper.jar          the paperclip launcher
/opt/paper/repo/              versions/, libraries/, cache/ — pre-patched, read-only
/usr/local/bin/spawnery-slp   buildGoModule from this repository, CGO off
/usr/local/bin/spawnery-entrypoint
<nix-store>/…/bin/java        jdk25_headless
```

Image configuration: `User = "10001:10001"`, `WorkingDir = /data`, `HOME=/data`,
`ExposedPorts = 25565/tcp`, and the OCI label `cloud.spawnery.paper-build=111`.

The probe measured that a `passwd` entry for uid 10001 is not needed when `HOME`
is set. One is added anyway: it costs a single line in `dockerTools`, and a
failing `getpwuid` in a library on the classpath surfaces as an error that says
nothing about its cause.

What the podspec imposes then holds without exception: everything written goes
to `/data` (Paper, the working directory) and `/tmp` (the JVM, `hsperfdata`);
everything else is read-only. The numeric user satisfies `runAsNonRoot`, which
otherwise refuses the start outright.

### 5.3 Reproducibility as a measurement

`buildLayeredImage` pins the timestamp so two builds produce the same bytes.
That is an acceptance criterion, not a claim: build twice, compare the digest.
The Nix route was chosen for exactly this — if it does not deliver it, the
decision has lost its reason and belongs revisited.

There is a specific risk here that the plan has to check rather than assume: the
patch step writes a jar, and jars readily embed timestamps. If `paper-26.2.jar`
comes out differently on two runs, the criterion fails on that step, and the fix
is to normalize the patch output rather than to lower the bar.

### 5.4 Tag and distribution

`ghcr.io/spawnery/paper:26.2-0.1.0`, where `0.1.0` is the version of our image.
The Paper build number stays out of the tag and lives in the flake and in the
OCI label — otherwise every sample manifest would need touching on every
upstream rebuild.

Nothing is published. `make image-load` hands the image to the local runtime and
`k3d image import` puts it into the cluster. The name is already correct so that
a later push to a registry changes nothing in the manifests.

## 6. The entrypoint

Four steps, in order:

1. Write `eula.txt` with `eula=true`. We accept Mojang's EULA on behalf of
   whoever runs the image. That is unavoidable — Paper does not start otherwise
   — and therefore belongs stated in the README rather than buried in a script.
2. Create `server.properties` if it is absent.
3. **Enforce** three fields in it, including against a user mount:
   `server-port=25565`, `max-players=$SPAWNERY_MAX_PLAYERS`,
   `enable-status=true`.
4. `exec java … -DbundlerRepoDir=/opt/paper/repo -jar /opt/paper/paper.jar
   --nogui`.

Step 3 is the reservation from section 5.4 of the main design — user mounts take
precedence over defaults, but not over operationally critical fields. The port
is obvious. `max-players` is not: in 2c the agent reports `slots` and the
operator counts on it; at Paper's default of 20 while the group says 100, the
operator would scale from 2c onwards against a number the server can never
honour. `enable-status` would be the quietest failure of the three — switched
off, the server answers no SLP, the probe stays red forever, and nothing in the
log says why.

Step 4's `exec` is the point: Java becomes PID 1 and receives `SIGTERM`
directly, saves the world and shuts down cleanly. With a shell in between, the
group's grace period would run out empty and every server would lose its last
world state on every stop. Measured: clean save, exit 143.

**JVM flags:** the G1 flags Paper itself recommends, plus
`-XX:MaxRAMPercentage=75` instead of a fixed `-Xmx` — the memory bound comes
from the group's `resources`, and the image does not know it. There is no
override knob: the podspec sets a fixed env list, so a group could not reach one
anyway.

**`online-mode` stays at Paper's default, meaning on.** Modern forwarding needs
it off, but a backend with `online-mode=false`, no proxy in front and no
NetworkPolicy is precisely the open backend section 8 of the main design warns
about. It is flipped in milestone 3, together with the forwarding secret and the
policy that secures it — in one change, not three.

## 7. The SLP tool

The readiness probe has to answer one question a port check cannot: *is the
world loaded?* Paper listens on 25565 before it is. The status path — handshake
with `next state 1`, then a status request — answers only afterwards.

`internal/slp` speaks that path directly: length-prefixed packets with VarInt
framing, handshake packet `0x00` carrying the protocol version, host, port and
next state, then an empty status request `0x00`, then the JSON response. Success
is a response that parses as a JSON object and carries a `version` key. It goes
no further than that: player counts are the agent's business in 2c, and a probe
that interprets them would be a second truth about the same thing.

One measured convenience: the protocol version in the handshake does not matter
for a status request. The probe sent 771 to a server speaking 776 and was
answered. The tool therefore never needs to track Minecraft versions.

Two constraints come from `internal/podspec/server.go`, which calls the binary
hard-wired as `spawnery-slp --host 127.0.0.1 --port 25565`:

- Every further flag needs a sensible default, because the probe will never pass
  one.
- Its own deadline has to sit below the probe's `timeoutSeconds: 5`, so the tool
  exits with a message instead of being killed by the kubelet. `--timeout`
  defaults to 4 seconds.

Exit 0 when the server answered, non-zero otherwise, with the reason on stderr.

## 8. Error handling

| Case | Behaviour |
|---|---|
| Server not up yet, connection refused | `spawnery-slp` exits non-zero; the probe counts a failure. Three of them turn it red — during startup that is the normal path, which is what `initialDelaySeconds` covers. |
| Port accepts, world still loading | No status response within the deadline → non-zero. Exactly the case the tool exists for. |
| Truncated or malformed response | Non-zero, with the reason. Never a partial success. |
| A user mount overwrites `server.properties` | The entrypoint rewrites the three enforced fields afterwards; everything else the mount says stands. |
| `SPAWNERY_MAX_PLAYERS` absent or unparsable | The entrypoint fails loudly rather than starting with Paper's default of 20, which would misreport `slots` to the operator from 2c on. |
| No memory limit on the pod | The JVM sizes itself against the whole node. See section 10. |

## 9. How it is proven

`make test` cannot see a container. The proof therefore falls on three levels,
each answering a different question.

**Level A — `internal/slp`, unit, inside `make test`.** The protocol against a
fake server in Go: a correct status response, a response truncated in the middle
of a VarInt, garbage instead of a packet, a wrong packet id, connection refused,
and a server that accepts and then stays silent (the deadline has to fire). The
rejections are the actual proof here too — a tool that always returns 0 makes
every readiness probe green, and the mistake would only surface when a player
lands on a server that has not loaded.

**Level B — `make image-test`, a smoke test against the real runtime.** Start
the image under Docker or Podman, under the podspec's constraints rather than
more comfortable ones: `--read-only`, `--tmpfs /tmp`, a volume on `/data`,
`--user 10001:10001`, all capabilities dropped, `SPAWNERY_MAX_PLAYERS` set the
way the podspec sets it — and **`--network none`**. Then poll `spawnery-slp`
from inside the container until it turns green, with an upper bound.

That answers the questions only a real runtime answers: does the JVM start as
10001 on a read-only root filesystem, does Paper load a world, does it answer
SLP, and does our binary work in the environment it actually runs in. Running it
offline is what keeps the promise from 3.2 guarded rather than remembered — the
day somebody unpins the pre-patched repo, this test fails instead of a pod
quietly downloading from Mojang in production. Not part of `make test`, because
it requires a container runtime.

**Level C — k3d, the whole path.** The README flow, now with an image that
exists. Expected: pod `Running`, readiness green, `Server` in phase `Starting`,
and staying there.

**Acceptance criteria:**

- `nix build .#paper-image` twice yields the same digest.
- `make image-test` green — and it runs offline, so passing it is the guard on
  section 3.2.
- k3d: pod ready, `Server` in `Starting`, no agent in the operator log.
- `make test` still green.

## 10. What 2b leaves open

- **The Darwin machine cannot build this image.** A Linux image needs a Linux
  builder. `make test` and everything else still work there; the image build and
  the smoke test do not.
- **Without a memory limit the JVM sizes itself against the node.** Neither the
  group nor the network is required to set `resources`, and the CRD enforces
  nothing. `AlwaysPreTouch` then claims that share immediately. The example
  manifest sets 2Gi; nothing makes anyone else do so.
- **`fsGroup` is missing.** For ephemeral groups it does not bite: the
  `emptyDir` is writable. A PVC in milestone 5 arrives owned by root, and uid
  10001 does not write into it. That belongs to milestone 5, before the first
  persistent server exists.
- **The server does not reach `Ready`.** Until 2c, by design.
- **Following upstream is manual.** A new Paper build means new hashes in
  `nix/paper.nix`, by hand. The automated image pipeline is project 3 in the
  main design, and this milestone does not anticipate it.
- **Configuration rendering per group** (main design, 5.4) does not exist, and
  `ServerGroup` has no `config` field to hang it on. The first field that really
  needs it is the forwarding mode, which arrives with milestone 3.
