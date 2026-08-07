CONTROLLER_GEN ?= controller-gen

.PHONY: all
all: manifests generate fmt vet test build

.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=spawnery-operator paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

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
