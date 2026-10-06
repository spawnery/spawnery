CONTROLLER_GEN ?= controller-gen
CONTAINER ?= docker
# Recursive on purpose: the nix evals run only when image-test expands these.
VELOCITY_IMAGE ?= $(shell nix eval --raw .#velocity-image.imageName):$(shell nix eval --raw .#velocity-image.imageTag)
PURPUR_IMAGE ?= $(shell nix eval --raw .#purpur-image.imageName):$(shell nix eval --raw .#purpur-image.imageTag)
PURPUR_IMAGE_26_2 ?= $(shell nix eval --raw .#purpur-image-26-2.imageName):$(shell nix eval --raw .#purpur-image-26-2.imageTag)
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
	./hack/crd-docs.sh
	./hack/chart-values-docs.sh
	./hack/metrics-docs.sh

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
# The e2e package's build tag keeps it out of ./...; gofmt ignores build tags.
	gofmt -l -w ./test/e2e

.PHONY: vet
vet:
	go vet ./...
	go vet -tags e2e ./test/...

.PHONY: test
# -race unconditionally: a separate test-race target would go unrun. envtest
# startup dominates the suite, so it costs about 20% rather than 2-10x.
test: manifests generate fmt vet chart-lint toolchain-lint image-tag-lint docs-length-lint crd-docs-test chart-values-docs-test chart-worldsync-test metrics-docs-test
	go test -race ./... -coverprofile cover.out

# protoc and protoc-gen-grpc-java against the Gradle pins; see flake.nix.
.PHONY: toolchain-lint
toolchain-lint:
	hack/toolchain-pins-agree.sh

# Tests of the check itself, against disagreements this tree does not
# contain. Like every hack/*-test.sh target, out of `test`.
.PHONY: toolchain-lint-test
toolchain-lint-test:
	hack/toolchain-pins-agree-test.sh

# docs/tutorial/network.yaml and config/samples/network.yaml pin the
# Purpur and Velocity image tags by hand; a stale tag still pulls fine.
.PHONY: image-tag-lint
image-tag-lint:
	hack/image-tag-pins-agree.sh

.PHONY: image-tag-lint-test
image-tag-lint-test:
	hack/image-tag-pins-agree-test.sh

.PHONY: docs-length-lint
docs-length-lint:
	hack/docs-length.sh

.PHONY: docs-length-lint-test
docs-length-lint-test:
	hack/docs-length-test.sh

# Each checks a reference-page generator's output against the real sources.
.PHONY: crd-docs-test
crd-docs-test:
	hack/crd-docs-test.sh

.PHONY: chart-values-docs-test
chart-values-docs-test:
	hack/chart-values-docs-test.sh

.PHONY: chart-worldsync-test
chart-worldsync-test:
	hack/chart-worldsync-test.sh

.PHONY: metrics-docs-test
metrics-docs-test:
	hack/metrics-docs-test.sh

.PHONY: chart-lint
chart-lint:
	helm lint charts/spawnery
	# helm lint accepts templates that fail to render with a real namespace.
	helm template spawnery charts/spawnery --namespace chart-lint-check >/dev/null

.PHONY: build
build:
	go build -o bin/spawnery-operator ./cmd/spawnery-operator

.PHONY: lint
lint:
	golangci-lint run

.PHONY: paper-pin
# Writes what nix/paper.nix has to say about a Paper build.
#
#   make paper-pin                    the newest STABLE (else BETA) build of the pinned version
#   make paper-pin ARGS="26.3"        the same for 26.3
#   make paper-pin ARGS="26.3 118"    exactly that build
#   make paper-pin-check              print, compare, change nothing
paper-pin:
	hack/paper-pin.sh $(ARGS)

.PHONY: paper-pin-check
paper-pin-check:
	CHECK=1 hack/paper-pin.sh $(ARGS)

.PHONY: purpur-pin
purpur-pin:
	hack/purpur-pin.sh $(ARGS)

.PHONY: purpur-pin-check
purpur-pin-check:
	CHECK=1 hack/purpur-pin.sh $(ARGS)

.PHONY: agent
# `nix build` reads the git index: `git add` new files first, or the build
# fails naming a symbol that is plainly in the file.
agent:
	nix build .#agents

# Regenerates agent/deps.json. Reaches Maven Central, so it is in no other
# target. Run from the repository root: the lockfile's paths are relative.
.PHONY: agent-deps
agent-deps:
	"$$(nix build --no-link --print-out-paths .#agents.mitmCache.updateScript)"

.PHONY: image-test
image-test: purpur-image-load velocity-image-load images-26-2-load aot-train-test
	CONTAINER=$(CONTAINER) IMAGE=$(PURPUR_IMAGE) hack/image-test.sh
	CONTAINER=$(CONTAINER) IMAGE=$(PURPUR_IMAGE_26_2) hack/image-test.sh
	CONTAINER=$(CONTAINER) IMAGE=$(VELOCITY_IMAGE) hack/velocity-image-test.sh

.PHONY: aot-train-test
aot-train-test: purpur-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(PURPUR_IMAGE) hack/aot-train-test.sh

# Not part of `test` or `all`: needs a container runtime and x86_64-linux.
.PHONY: agent-test
agent-test: purpur-image-load velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(PURPUR_IMAGE) VELOCITY_IMAGE=$(VELOCITY_IMAGE) \
		STUBOP=$(STUBOP) hack/agent-test.sh

.PHONY: purpur-image
purpur-image:
	nix build .#purpur-image --out-link result-purpur

.PHONY: purpur-image-load
purpur-image-load: purpur-image
	$(CONTAINER) load < result-purpur

.PHONY: purpur-image-test
purpur-image-test: purpur-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(PURPUR_IMAGE) hack/image-test.sh

.PHONY: images-26-2-load
images-26-2-load:
	nix build .#purpur-image-26-2 --out-link result-purpur-26-2
	$(CONTAINER) load < result-purpur-26-2

.PHONY: velocity-image
velocity-image:
	nix build .#velocity-image --out-link result-velocity

.PHONY: velocity-image-load
velocity-image-load: velocity-image
	$(CONTAINER) load < result-velocity

.PHONY: velocity-image-test
velocity-image-test: velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(VELOCITY_IMAGE) hack/velocity-image-test.sh

# Its own out-link: a shared ./result let a parallel `make -j` load the wrong image.
.PHONY: operator-image
operator-image:
	nix build .#operator-image --out-link result-operator

.PHONY: operator-image-load
operator-image-load: operator-image
	$(CONTAINER) load < result-operator

.PHONY: operator-image-test
operator-image-test: operator-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(OPERATOR_IMAGE) hack/operator-image-test.sh

# Not part of `test` or `all`: needs a container runtime and x86_64-linux.
# Each build precedes its --rebuild because --rebuild refuses to run, rather
# than fails, when the output is not in the store yet -- and any edit to the
# working tree moves all image derivations.
.PHONY: image-repro
image-repro:
	nix build .#purpur-image --no-link
	nix build .#purpur-image --rebuild --no-link
	nix build .#purpur-image-26-2 --no-link
	nix build .#purpur-image-26-2 --rebuild --no-link
	nix build .#velocity-image --no-link
	nix build .#velocity-image --rebuild --no-link
	nix build .#operator-image --no-link
	nix build .#operator-image --rebuild --no-link
	# Separately, so a non-reproducible jar is named rather than seen as a layer diff.
	nix build .#agents --no-link
	nix build .#agents --rebuild --no-link

# Needs network and an authenticated gh, so it is out of `test`, `all` and CI.
# Its green and red cases name commits whose ci.yml runs GitHub deletes after
# about 90 days (around 2026-11-20); then point GREEN_SHA and RED_SHA at runs
# that still exist rather than softening the assertions.
.PHONY: require-green-ci-test
require-green-ci-test:
	hack/require-green-ci-test.sh

# Needs network and an authenticated gh, like the target above.
.PHONY: require-no-red-nightly-test
require-no-red-nightly-test:
	hack/require-no-red-nightly-test.sh

.PHONY: image-derivations-changed-test
image-derivations-changed-test:
	hack/image-derivations-changed-test.sh

# Contacts a registry and needs a token. DRY_RUN=1 builds the images and prints
# what it would copy where. IMAGES names a subset, e.g. IMAGES=operator-image.
IMAGES ?=
.PHONY: publish
publish:
	hack/publish.sh $(IMAGES)

# Contacts a registry and needs a token; DRY_RUN=1 only packages and prints.
.PHONY: publish-chart
publish-chart:
	hack/publish-chart.sh

# Needs a token and a signing key; DRY_RUN=1 builds the bundle and prints.
.PHONY: publish-api
publish-api:
	hack/publish-api.sh

.PHONY: publish-chart-test
publish-chart-test:
	hack/publish-chart-test.sh

# Out of `test` and `all`: builds a cluster and takes minutes. Depends on
# `manifests` because it installs the chart, whose rbac.yaml and crds.yaml are
# generated from the markers.
.PHONY: e2e
e2e: manifests
	hack/e2e.sh

# Loads the real Purpur and Velocity images; nightly.yml runs it.
.PHONY: e2e-tutorial
e2e-tutorial: manifests
	hack/e2e-tutorial.sh

# Loads a real game image; nightly.yml runs it.
.PHONY: e2e-ondemand
e2e-ondemand: manifests
	hack/e2e-ondemand.sh

# mkdocs --strict is the only link checker. Out of `test`; ci.yml runs it in
# its own job.
.PHONY: docs
docs:
	nix build .#docs-site --no-link

# Gitignored build products `mkdocs serve` needs: mermaid.js (without it the
# home page's diagram is silently blank), fonts, and the plugin API Javadoc.
.PHONY: docs-assets
docs-assets:
	hack/vendor-mermaid.sh
	hack/vendor-fonts.sh
	hack/vendor-javadoc.sh

.PHONY: docs-serve
docs-serve: docs-assets
	mkdocs serve
