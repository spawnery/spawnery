CONTROLLER_GEN ?= controller-gen
CONTAINER ?= docker
# Deferred (recursive) on purpose: the nix eval calls only run when $(IMAGE) is
# actually expanded, i.e. by image-test below. A plain `make test` never
# references it and pays nothing for it.
IMAGE ?= $(shell nix eval --raw .#paper-image.imageName):$(shell nix eval --raw .#paper-image.imageTag)
VELOCITY_IMAGE ?= $(shell nix eval --raw .#velocity-image.imageName):$(shell nix eval --raw .#velocity-image.imageTag)
PURPUR_IMAGE ?= $(shell nix eval --raw .#purpur-image.imageName):$(shell nix eval --raw .#purpur-image.imageTag)
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
# The e2e package a second time, by hand: its //go:build e2e constraint keeps
# it out of ./..., so `go fmt ./...` above never sees it. gofmt takes files
# rather than packages and does not consult build constraints, so naming the
# directory is enough.
	gofmt -l -w ./test/e2e

.PHONY: vet
vet:
	go vet ./...
# And the same package with its tag, for the same reason. `make lint` is the
# one of the three that actually fails a build on what it finds; this and the
# gofmt above only rewrite or report, so treat .golangci.yml's build-tags as
# the enforcing half.
	go vet -tags e2e ./test/...

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
test: manifests generate fmt vet chart-lint toolchain-lint
	go test -race ./... -coverprofile cover.out

# The standing check docs/known-issues.md has asked for since milestone 2c.
# protoc and protoc-gen-grpc-java are pinned in flake.nix, protobuf-java and
# the io.grpc:grpc-* artifacts in agent/common/build.gradle.kts and
# agent/deps.json, and a `nix flake update` moves only the first of each pair.
#
# A prerequisite of `test` rather than a target beside it, for the reason the
# -race comment above gives: an unrun check is indistinguishable from an absent
# one, and this one costs two process spawns and four greps. The failure it
# prevents is a Gradle build that takes minutes to reach "cannot find symbol",
# in a file nobody edited, about a pin in a file they did.
.PHONY: toolchain-lint
toolchain-lint:
	hack/toolchain-pins-agree.sh

# hack/toolchain-pins-agree-test.sh drives the check above through the
# disagreements this tree does not contain. Out of `test` for the same reason
# image-derivations-changed-test is: it is about the check, not about the
# operator, and `test` exercises the check itself on every run.
.PHONY: toolchain-lint-test
toolchain-lint-test:
	hack/toolchain-pins-agree-test.sh

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

.PHONY: paper-pin
# Computes what nix/paper.nix has to say about a Paper build, and writes it in.
#
#   make paper-pin                    the newest STABLE build of the pinned version
#   make paper-pin ARGS="26.3"        the newest STABLE build of 26.3
#   make paper-pin ARGS="26.3 118"    exactly that build
#   make paper-pin-check              print, compare, change nothing
#
# It does not decide to upgrade; it answers what the values are once somebody
# has. What it removes is four hashes read by hand out of two places and
# converted from hex to SRI, which is mechanical and is four chances to put a
# wrong hash into a build that then fails somewhere else entirely.
paper-pin:
	hack/paper-pin.sh $(ARGS)

.PHONY: paper-pin-check
paper-pin-check:
	CHECK=1 hack/paper-pin.sh $(ARGS)

# Purpur's sibling. It differs from the Paper one in what the upstream API can
# be asked for -- a SHA-256 there, an MD5 here -- and hack/purpur-pin.sh's
# header is where that difference is spelled out rather than glossed.
.PHONY: purpur-pin
purpur-pin:
	hack/purpur-pin.sh $(ARGS)

.PHONY: purpur-pin-check
purpur-pin-check:
	CHECK=1 hack/purpur-pin.sh $(ARGS)

.PHONY: agent
# `nix build` filters the source tree through the git index, so an untracked
# file does not exist for a sandboxed build. This is worth reading before
# diagnosing anything else, because it presents as a compile failure naming a
# symbol that is plainly there in the file in front of you -- milestone 4c-1's
# was 35 copies of `package cloud.spawnery.agent.pb.SetReady does not exist`
# from this target, immediately after `make proto` had generated the Java
# stubs, which looks exactly like the protoc/runtime version drift documented
# in flake.nix and is not. It cost time in milestone 2c as well.
#
# The agents derivation builds from `src = ../agent` (nix/agents.nix), the Go
# binaries from `src = ./.` in flake.nix; either way the source is the git
# tree. So: `git add` before the build, not just before the commit. Staging is
# enough -- nothing has to be committed.
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
image-test: image-load purpur-image-load velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) hack/image-test.sh
	CONTAINER=$(CONTAINER) IMAGE=$(PURPUR_IMAGE) hack/image-test.sh
	CONTAINER=$(CONTAINER) IMAGE=$(VELOCITY_IMAGE) hack/velocity-image-test.sh

# The level-2 proof from design section 9. Not part of `test` or `all`, for the
# same reason image-test is not: it needs a container runtime and only works on
# x86_64-linux.
.PHONY: agent-test
agent-test: image-load velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) VELOCITY_IMAGE=$(VELOCITY_IMAGE) \
		STUBOP=$(STUBOP) hack/agent-test.sh

# Purpur, through hack/image-test.sh unchanged. The script asserts on Paper's
# own behaviour -- that Paper rewrote /data/config/paper-global.yml, that the
# agent plugin loaded -- and Purpur is a Paper fork that does all of it, so a
# second script would have been the same script with a different name. If the
# two ever diverge enough for that to stop being true, this is the line that
# fails and says so.
.PHONY: purpur-image
purpur-image:
	nix build .#purpur-image --out-link result-purpur

.PHONY: purpur-image-load
purpur-image-load: purpur-image
	$(CONTAINER) load < result-purpur

.PHONY: purpur-image-test
purpur-image-test: purpur-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(PURPUR_IMAGE) hack/image-test.sh

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
	nix build .#purpur-image --no-link
	nix build .#purpur-image --rebuild --no-link
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

# hack/require-green-ci-test.sh: three cases against this repository's live run
# history through `gh`, two against a fixture through CI_RUNS_CMD. Deliberately
# not a prerequisite of `test` or `all`, and not a CI job either: it needs
# network access and an authenticated
# `gh`, neither of which `make test` has ever required, and a unit suite that
# fails on an expired token or an api.github.com outage stops being a signal
# about this repository.
#
# It has an expiry date, which is the price of testing a gate against real
# evidence. Its green and red cases name two commit shas and assert on the
# ci.yml runs GitHub still holds for them; GitHub retains workflow runs for
# about 90 days, so from roughly 2026-11-20 those two cases start failing with
# nothing in this repository having changed. When they do, the fix is to point
# GREEN_SHA and RED_SHA at a green and a red run that still exist -- the
# script's own header records the `gh run list` invocation that confirms a
# candidate -- and not to soften what they assert.
.PHONY: require-green-ci-test
require-green-ci-test:
	hack/require-green-ci-test.sh

# hack/require-no-red-nightly-test.sh: one case against the live repository
# through `gh`, four against fixtures through NIGHTLY_ISSUES_CMD. Not a
# prerequisite of `test` or `all`, for the same reason as the target above --
# it needs the network and an authenticated gh.
#
# Unlike that target this one has no expiry date, because it asserts about
# issues rather than about workflow runs: GitHub expires runs after about 90
# days and expires nothing about an issue. Its live case asserts the ordinary
# state -- no open nightly-red issue -- so it starts failing exactly when the
# nightly is red and somebody has not dealt with it yet, which is a true
# statement about this repository rather than a stale fixture. If it fails,
# read the issue it is telling you about before touching this file.
.PHONY: require-no-red-nightly-test
require-no-red-nightly-test:
	hack/require-no-red-nightly-test.sh

# hack/image-derivations-changed-test.sh: six cases, all against this
# repository's own git history. Unlike the two targets above it needs no
# network and no token, and unlike require-green-ci-test it has no expiry date
# -- it asserts about commits rather than about workflow runs, and git does not
# drop reachable history the way GitHub drops runs after ninety days. It is
# still out of `test` and `all`: it is about a CI job's decision, not about the
# operator, and ci.yml exercises the decision itself on every push.
.PHONY: image-derivations-changed-test
image-derivations-changed-test:
	hack/image-derivations-changed-test.sh

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

# The chart, as an OCI artefact beside the three images, so that installing
# the operator needs no checkout of this repository. Out of `all` for the same
# reason as `publish`: it contacts a registry and needs a token. DRY_RUN=1
# packages the chart and prints what it would push where -- unlike `publish`
# that is cheap, because nothing here builds an image.
.PHONY: publish-chart
publish-chart:
	hack/publish-chart.sh

# hack/publish-chart-test.sh: nine cases, five against this repository and
# four against throwaway git repositories built on the spot. Unlike the two
# gh-driven targets above it needs no network and no token -- the registry's
# three possible answers go through the CHART_INSPECT_CMD seam -- but it is
# still out of `test` and `all`, which is where every hack/ script's test
# sits: `make test` is about the operator, and these are about a publishing
# decision.
.PHONY: publish-chart-test
publish-chart-test:
	hack/publish-chart-test.sh

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
