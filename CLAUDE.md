# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Kubernetes operator (Go, controller-runtime) plus two JVM agent plugins (Kotlin, Gradle) that run Paper/Purpur game servers behind Velocity proxies. Four namespaced CRDs in `spawnery.cloud/v1alpha1`: `Network`, `ServerGroup`, `ProxyGroup`, `Server` (plus `ScaleBoost`). Game pods never talk to the Kubernetes API; each agent opens one authenticated gRPC stream to the operator, which is how player counts flow up and server lists / drain orders flow down.

## Commands

Everything runs inside the Nix dev shell. Prefix every command with `nix develop -c` (or enter the shell once). The shell sets `KUBEBUILDER_ASSETS` for envtest.

```bash
make test        # manifests generate fmt vet chart-lint toolchain-lint, then go test -race ./...
make lint        # golangci-lint: errcheck + staticcheck only, uncapped, with the e2e build tag
make build       # bin/spawnery-operator
make agent       # nix build .#agents — both plugins and their JUnit suites
make e2e         # kind cluster + helm install + test/e2e (minutes; not part of test/all)
make proto       # regenerate internal/agentpb and agent/common/src/proto/java from the .proto
make manifests   # CRDs, RBAC role, and the chart templates derived from them
```

Single Go test:

```bash
nix develop -c go test ./internal/controller/ -run TestName -count=1
```

envtest-backed packages (`api/v1alpha1`, `internal/{agentserver,certs,controller,grpcauth,podspec,rbacaudit}`) each boot their own etcd + kube-apiserver. `internal/controller` alone takes ~85 s. On a small machine run several of them with `-p 1`, or the parallel API servers exhaust RAM.

The e2e package carries `//go:build e2e`, so `go test ./...` never sees it. Run it through `make e2e`; `E2E_KEEP=1` keeps the cluster and prints its `KUBECONFIG`. Under rootless Podman it needs `systemd-run --scope --user --property=Delegate=yes -- nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e`.

Image targets (`image-test`, `agent-test`, `image-repro`, `*-image`) need a container runtime, x86_64-linux, and `CONTAINER=podman` if there is no docker. `make agent-test` rebuilds both images first; to rerun against already-loaded images call `hack/agent-test.sh` directly with the tags it expects.

## Things that bite

- **Nix builds read the git index, not the working tree.** An untracked file does not exist for `nix build`. `git add` new files before `make agent` or any image build; the symptom is a compile error naming a symbol that is plainly in the file.
- **Generated files are committed and CI diffs them.** After changing API types, kubebuilder markers or the `.proto`, run `make manifests generate proto` and commit: `config/crd/bases/`, `config/rbac/role.yaml`, `charts/spawnery/templates/{crds,rbac}.yaml` (written by `hack/chart-templates.sh`, never by hand), `zz_generated.deepcopy.go`, `internal/agentpb/`, `agent/common/src/proto/java/`.
- **Adding a `+kubebuilder:rbac` marker turns `internal/rbacaudit` red** until the matching entry is added to its hand-maintained table (`required.go`). That is intentional. Also: a marker inside a doc comment is silently ignored by controller-gen. Diff `config/rbac/role.yaml` after adding one.
- **`go.mod` changes break the Nix build, not `make test`.** `flake.nix` carries the same `vendorHash` five times (one Go module set). After `go mod tidy`, rebuild with `nix build .#spawnery-operator --no-link`, take the `got:` hash, and replace all five. CI's e2e job is where this fails otherwise.
- **`make agent-deps` regenerates `agent/deps.json`** and must be run whenever a `build.gradle.kts` dependency changes. It reaches Maven Central and is part of no other target. CI diffs the result.
- **Constants shared across files or languages are checked by source-reading tests**, not by the compiler: the proxy ready port (`internal/podspec/kotlin_agreement_test.go` reads `agent/velocity`), the gRPC service name (`internal/agentpb/contract_test.go`), the reserved `SPAWNERY_` env prefix against its CRD markers (`internal/podspec/env_test.go`). Move one side and expect the test on the other to name it.
- **Hash goldens in `internal/podspec/hash_golden_test.go`.** `DesiredServerHash`/`DesiredProxyHash` decide when a whole group rolls. Any change to what feeds them moves the golden; update it deliberately and say so in the commit, because it means every existing server rolls on upgrade.
- **protoc and grpc-java are pinned twice**: in `flake.nix` (nixpkgs) and in `agent/common/build.gradle.kts`. `hack/toolchain-pins-agree.sh` (run by `make test`) fails when they drift after a `nix flake update`.
- **envtest shares one control plane per test binary and has no controller-manager.** Nothing is cleaned between tests and a deleted namespace stays `Terminating` forever. A test creating objects that production code lists cluster-wide must delete them in `t.Cleanup`.

## Architecture

Read `internal/controller/setup.go` (`Options`, `SetupAll`) to see how the pieces are wired. The design lives in `docs/superpowers/specs/` (one per milestone; start with `2026-08-07-minecraft-cloud-operator-design.md` and `2026-08-08-agent-channel-design.md`).

**Two directions, two registries, one picture.**

- `internal/agentserver` is the gRPC endpoint agents dial. `internal/grpcauth` turns the bearer token (a pod-bound ServiceAccount token, via TokenReview) into exactly one pod identity. Identity never comes from the message.
- `internal/agent` is what the gRPC server writes and the controllers read: in-memory player counts and readiness. The CR status is for observers, not for the control loop.
- `internal/proxyreg` (proxies) and `internal/serverreg` (backends) are the reverse: what the controllers write and the gRPC server sends down. Both build their view of the namespace through `internal/netstate`, so both agent kinds see one identical network picture.
- `internal/certs`: the operator is its own CA for the agent channel and pins the bundle into the pods it creates.

**Pure cores, thin controllers.**

- `internal/phase`: the `Server` state machine as a pure `Decide` function. Every registration/deletion rule lives there.
- `internal/podspec`: API objects to pod specs, no client. Inheritance, overrides, mounts, NetworkPolicies, and the desired-state hashes.
- `internal/render`: the config files Paper and Velocity read (`server.properties`, `paper-global.yml`, `velocity.toml`), used by `cmd/spawnery-config` inside the images.
- `internal/boost`, `internal/cloudevent`: small shared rules split out only because two packages that must not import each other both need them.
- `internal/controller`: the Network / ServerGroup / ProxyGroup / Server reconcilers, slot-based scaling, rolling updates, orphan sweep, persistent-group handling, the namespace bootstrap (CA ConfigMap + agent ServiceAccount).

**Images and entrypoints.** `image/entrypoint.sh` (Paper and Purpur) and `image/velocity-entrypoint.sh` run in this order: `eula.txt`, `spawnery-config`, copy of the `extraFiles` claim into the working directory (refused up front if it carries `plugins/` or a file the renderer writes), copy of the `extraPlugins` claim into `plugins/`, agent jar, `exec java`. Later steps win over earlier ones; the refusal list is what keeps the three writers' paths disjoint. Each claim-backed source has its own operator flag: `--allow-plugin-volumes`, `--allow-file-volumes`, `--allow-mount-volumes`. The images are built by `nix/*.nix`; `image/*_test.go` tests the scripts with test doubles on `PATH`.

**Agents** (`agent/`): Gradle root with subprojects `api` (the public plugin API, published to Maven Central as `cloud.spawnery:spawnery-api`, consumed `compileOnly`), `common` (session loop, token source, channel, network mirror), `paper`, `velocity`. `make agent` builds them through Nix with `agent/deps.json` as the Maven lockfile.

**Auxiliary binaries** in `cmd/`: `spawnery-slp` (server-list-ping readiness probe in the game image), `spawnery-config` (config renderer in the images), `spawnery-stubop` and `spawnery-join` (test-only; never shipped in an image).

**Chart** (`charts/spawnery/`) is the only installation form. CRDs live in `templates/crds.yaml` with `helm.sh/resource-policy: keep` so `helm upgrade` carries schema changes. `internal/rbacaudit` checks the rendered ClusterRole/Role against its table both ways and through SubjectAccessReview.

## Versions and releases

Three numbers move independently and each is commented at length where it lives:

- `imageVersion` in `flake.nix`: the agent/game-image version. Moves when anything under `agent/`, `image/`, `internal/render`, or the JRE derivations changes.
- `operatorVersion` in `flake.nix`: moves when the operator binary changes.
- `charts/spawnery/Chart.yaml` `version` (moves whenever `charts/` does) and `appVersion` (tracks the operator).

A `v*` tag runs `.github/workflows/release.yml`, which publishes only the artefacts whose tag is new and refuses to overwrite existing ones. Gaps in the sequences are deliberate. Before tagging, check CI on master: `gh run list --workflow=ci.yml --limit 3`.

## Conventions

- **Commits are Conventional Commits** with a scope naming the part touched: `feat(podspec): …`, `fix(api): …`, `chore: 0.2.24, …`. The subject says what changed; the body says why, wrapped at 72 columns. Older plans under `docs/superpowers/plans/` say "no prefixes"; that rule is superseded.
- `docs/known-issues.md` holds only open problems; a fixed entry is deleted, and its story lives in the commit that removed it. Claims about the `paulwtf` cluster belong in the GitOps repository, not here.
- Docs and code comments in this repo record measurements ("measured on …") rather than assumptions. Keep that when adding to them.
- New features come as a spec in `docs/superpowers/specs/` and a plan in `docs/superpowers/plans/` (dated filenames), then implementation.
