CONTROLLER_GEN ?= controller-gen
CONTAINER ?= docker
# Deferred (recursive) on purpose: the nix eval calls only run when $(IMAGE) is
# actually expanded, i.e. by image-test below. A plain `make test` never
# references it and pays nothing for it.
IMAGE ?= $(shell nix eval --raw .#paper-image.imageName):$(shell nix eval --raw .#paper-image.imageTag)
VELOCITY_IMAGE ?= $(shell nix eval --raw .#velocity-image.imageName):$(shell nix eval --raw .#velocity-image.imageTag)
OPERATOR_IMAGE ?= $(shell nix eval --raw .#operator-image.imageName):$(shell nix eval --raw .#operator-image.imageTag)
STUBOP ?= $(shell nix build .#spawnery-stubop --no-link --print-out-paths)/bin/spawnery-stubop

.PHONY: all
all: proto manifests generate fmt vet test build agent

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=spawnery-operator paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac
	./hack/chart-templates.sh

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: proto
proto:
	protoc \
		--proto_path=proto \
		--go_out=. --go_opt=module=github.com/spawnery/spawnery \
		--go-grpc_out=. --go-grpc_opt=module=github.com/spawnery/spawnery \
		proto/spawnery/agent/v1alpha1/agent.proto
	rm -rf agent/common/src/proto/java
	mkdir -p agent/common/src/proto/java
	protoc \
		--proto_path=proto \
		--java_out=agent/common/src/proto/java \
		--grpc-java_out=agent/common/src/proto/java \
		proto/spawnery/agent/v1alpha1/agent.proto

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
# -race unconditionally, rather than behind a second target. Milestone 6b added
# two mutex-guarded types (grpcauth.ReviewCache, grpcauth.PeerLimiter) that a
# gRPC interceptor reaches from one goroutine per stream, and until now nothing
# in the Makefile turned the detector on anywhere -- so the only race checks
# this project has ever had were run by hand during a review, which is the same
# as not having one. A separate `make test-race` would have the same problem:
# an unrun check is indistinguishable from an absent one.
#
# The cost was measured on this suite rather than assumed from the detector's
# usual 2-10x: 89.8s to 109.2s wall, because the run is dominated by envtest
# control-plane startup rather than by anything the detector instruments.
# internal/controller, the longest package, goes 83.9s to 99.6s.
#
# It is not a substitute for reasoning about concurrency. The peer rate limit's
# key was wrong for a whole milestone and -race would never have said so.
test: manifests generate fmt vet chart-lint
	go test -race ./... -coverprofile cover.out

.PHONY: chart-lint
chart-lint:
	helm lint charts/spawnery
	# helm lint alone accepts templates that fail to render with a real
	# namespace, and a chart that lints but does not template is a chart
	# nobody can install -- so rendering it here is not redundant with the
	# line above.
	helm template spawnery charts/spawnery --namespace chart-lint-check >/dev/null

.PHONY: build
build:
	go build -o bin/spawnery-operator ./cmd/spawnery-operator

.PHONY: lint
lint:
	golangci-lint run

.PHONY: agent
agent:
	nix build .#agents

# Regenerates agent/deps.json. Runs outside the Nix sandbox because it
# has to reach Maven Central, so it is deliberately in no other target: a
# dependency change is an explicit act, not a side effect of `make all`.
# The output path in the lockfile is relative to the working directory, so this
# only does the right thing from the repository root.
.PHONY: agent-deps
agent-deps:
	"$$(nix build --no-link --print-out-paths .#agents.mitmCache.updateScript)"

.PHONY: image
image:
	nix build .#paper-image --out-link result-paper

.PHONY: image-load
image-load: image
	$(CONTAINER) load < result-paper

.PHONY: image-test
# Design §9's test-strategy table promises "both images, offline" under this
# one name; velocity-image-test below used to be the only way to actually run
# the Velocity half, and nothing — not this target, not `all`, not the
# README — ever called it. Depending on both loads and running both scripts
# is what makes that table row true rather than aspirational, and it keeps
# one command as the single gate for "did I break either image", the same way
# `go build ./...` is for compilation.
image-test: image-load velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) hack/image-test.sh
	CONTAINER=$(CONTAINER) IMAGE=$(VELOCITY_IMAGE) hack/velocity-image-test.sh

# The level-2 proof from design section 9. Not part of `test` or `all`, for the
# same reason image-test is not: it needs a container runtime and only works on
# x86_64-linux.
.PHONY: agent-test
agent-test: image-load velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) VELOCITY_IMAGE=$(VELOCITY_IMAGE) \
		STUBOP=$(STUBOP) hack/agent-test.sh

.PHONY: velocity-image
velocity-image:
	nix build .#velocity-image --out-link result-velocity

.PHONY: velocity-image-load
velocity-image-load: velocity-image
	$(CONTAINER) load < result-velocity

.PHONY: velocity-image-test
velocity-image-test: velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(VELOCITY_IMAGE) hack/velocity-image-test.sh

# Its own out-link, like the two above. Sharing ./result is what let a parallel
# `make -j` load whichever image finished last; see docs/known-issues.md.
.PHONY: operator-image
operator-image:
	nix build .#operator-image --out-link result-operator

.PHONY: operator-image-load
operator-image-load: operator-image
	$(CONTAINER) load < result-operator

.PHONY: operator-image-test
operator-image-test: operator-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(OPERATOR_IMAGE) hack/operator-image-test.sh

# Not part of `test` or `all`, for the same reason image-test is not: it needs
# a container runtime's worth of build time and only works on x86_64-linux.
# Design 5.3 makes bit-reproducibility an acceptance criterion; this is the
# standing check for it, rather than a one-time measurement by hand.
# Each image is built and then rebuilt, and the pair is not optional. `nix
# build --rebuild` compares a fresh build against the output already in the
# store, so with nothing there it does not fail the check -- it declines to run
# it, with "some outputs ... are not valid, so checking is not possible" and an
# exit code make stops on. All three image derivations take the working tree as
# their source -- one appended line in docs/ was measured to move all three
# derivation hashes, though not the agents' -- so the store is empty of them
# after almost any edit, and the target that exists to prove reproducibility
# spent this milestone with nothing to check against on a tree anybody had
# touched. Measured, not reasoned about: see the milestone 6a final fix report.
#
# --no-link on both halves, because neither the build nor the check wants an
# out-link; leaving them to the default ./result is what docs/known-issues.md's
# closed entry is about.
.PHONY: image-repro
image-repro:
	nix build .#paper-image --no-link
	nix build .#paper-image --rebuild --no-link
	nix build .#velocity-image --no-link
	nix build .#velocity-image --rebuild --no-link
	nix build .#operator-image --no-link
	nix build .#operator-image --rebuild --no-link
	# The agent jars, directly. Both images embed them, so a non-reproducible
	# jar would eventually show up above -- but as a diff in an image layer,
	# which says nothing about which of the two agents moved. Rebuilding the
	# derivation that produces both is what turns that into a message naming
	# the jar.
	nix build .#agents --no-link
	nix build .#agents --rebuild --no-link

# Not part of `all`: it contacts a registry and needs a token. DRY_RUN=1 still
# builds every image it was asked for -- on this machine that is the expensive
# part -- and then prints what it would copy where instead of copying it, so
# nothing is sent and no credential is needed.
#
# IMAGES names a subset, e.g. `make publish IMAGES=operator-image`. Empty means
# all three; hack/publish.sh's header says why publishing one at a time is the
# ordinary case rather than an escape hatch.
IMAGES ?=
.PHONY: publish
publish:
	hack/publish.sh $(IMAGES)

# The driven run from the milestone 6a design. Explicitly not part of `test` or
# `all`: it builds a cluster and takes minutes, and the commit loop stays at
# around 25 seconds. See hack/e2e.sh's header for the rootless-podman
# invocation this machine needs.
#
# It depends on `manifests` for the same reason `test` does, and here the
# consequence is worse. hack/e2e.sh does apply
# config/rbac/forwarding-secret-reader.yaml by hand, twice, but that file is
# hand-written rather than controller-gen output, so it is not what this
# dependency is about: hack/e2e.sh also runs `helm install charts/spawnery`,
# and the chart's rbac.yaml and crds.yaml are hack/chart-templates.sh's
# output, which is the second half of `manifests`. So the stale-object hazard
# is the same one, one step further along the pipeline -- without this
# dependency a marker edit is driven against templates generated before it.
# The whole point of design §8's first mutation is that removing a verb from
# a marker turns this run red, which it cannot do if the run installs a chart
# built from the old markers.
# controller-gen takes about a second against the minutes the cluster costs.
.PHONY: e2e
e2e: manifests
	hack/e2e.sh
