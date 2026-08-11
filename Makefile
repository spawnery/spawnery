CONTROLLER_GEN ?= controller-gen
CONTAINER ?= docker
# Deferred (recursive) on purpose: the nix eval calls only run when $(IMAGE) is
# actually expanded, i.e. by image-test below. A plain `make test` never
# references it and pays nothing for it.
IMAGE ?= $(shell nix eval --raw .#paper-image.imageName):$(shell nix eval --raw .#paper-image.imageTag)
VELOCITY_IMAGE ?= $(shell nix eval --raw .#velocity-image.imageName):$(shell nix eval --raw .#velocity-image.imageTag)
STUBOP ?= $(shell nix build .#spawnery-stubop --no-link --print-out-paths)/bin/spawnery-stubop

.PHONY: all
all: proto manifests generate fmt vet test build agent

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=spawnery-operator paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

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
	rm -rf agent/paper/src/proto/java
	mkdir -p agent/paper/src/proto/java
	protoc \
		--proto_path=proto \
		--java_out=agent/paper/src/proto/java \
		--grpc-java_out=agent/paper/src/proto/java \
		proto/spawnery/agent/v1alpha1/agent.proto

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: manifests generate fmt vet
	go test ./... -coverprofile cover.out

.PHONY: build
build:
	go build -o bin/spawnery-operator ./cmd/spawnery-operator

.PHONY: lint
lint:
	golangci-lint run

.PHONY: agent
agent:
	nix build .#paper-agent

# Regenerates agent/paper/deps.json. Runs outside the Nix sandbox because it
# has to reach Maven Central, so it is deliberately in no other target: a
# dependency change is an explicit act, not a side effect of `make all`.
# The output path in the lockfile is relative to the working directory, so this
# only does the right thing from the repository root.
.PHONY: agent-deps
agent-deps:
	"$$(nix build --no-link --print-out-paths .#paper-agent.mitmCache.updateScript)"

.PHONY: image
image:
	nix build .#paper-image

.PHONY: image-load
image-load: image
	$(CONTAINER) load < result

.PHONY: image-test
image-test: image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) hack/image-test.sh

# The level-2 proof from design section 9. Not part of `test` or `all`, for the
# same reason image-test is not: it needs a container runtime and only works on
# x86_64-linux.
.PHONY: agent-test
agent-test: image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) STUBOP=$(STUBOP) hack/agent-test.sh

.PHONY: velocity-image
velocity-image:
	nix build .#velocity-image

.PHONY: velocity-image-load
velocity-image-load: velocity-image
	$(CONTAINER) load < result

.PHONY: velocity-image-test
velocity-image-test: velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(VELOCITY_IMAGE) hack/velocity-image-test.sh

# Not part of `test` or `all`, for the same reason image-test is not: it needs
# a container runtime's worth of build time and only works on x86_64-linux.
# Design 5.3 makes bit-reproducibility an acceptance criterion; this is the
# standing check for it, rather than a one-time measurement by hand.
.PHONY: image-repro
image-repro:
	nix build .#paper-image --rebuild
	nix build .#velocity-image --rebuild
