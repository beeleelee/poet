ORG ?= spacemeshos
IMAGE ?= poet
BINARY := poet
PROJECT := poet
BRANCH := $(shell git rev-parse --abbrev-ref HEAD)
VERSION ?= $(shell git describe --tags)

# Flags appended to `go test` command in `make test`
TEST_FLAGS ?=

GOLANGCI_LINT_VERSION := v1.57.0
STATICCHECK_VERSION := v0.4.7
GOTESTSUM_VERSION := v1.11.0
GOSCALE_VERSION := v1.2.0
MOCKGEN_VERSION := v0.4.0

BUF_VERSION := 1.30.0
PROTOC_VERSION = 26.0

GRPC_JSON_PROXY_VERSION := v2.19.1
PROTOC_GO_VERSION := v1.33.0
PROTOC_GEN_GO_VERSION := v1.3.0
PROTOC_OPENAPI_VERSION := v2.19.1

# Everything below this line is meant to be static, i.e. only adjust the above variables. ###

ifeq ($(OS),Windows_NT)
	UNAME_OS := windows
	ifeq ($(PROCESSOR_ARCHITECTURE),AMD64)
		UNAME_ARCH := x86_64
	endif
	ifeq ($(PROCESSOR_ARCHITECTURE),ARM64)
		UNAME_ARCH := aarch64
	endif
	PROTOC_BUILD := win64

	BIN_DIR := $(abspath .)/bin
	export PATH := $(BIN_DIR);$(PATH)
	TMP_PROTOC := $(TEMP)/protoc-$(RANDOM)
else
	UNAME_OS := $(shell uname -s)
	UNAME_ARCH := $(shell uname -m)
	PROTOC_BUILD := $(shell echo ${UNAME_OS}-${UNAME_ARCH} | tr '[:upper:]' '[:lower:]' | sed 's/darwin/osx/' | sed 's/arm64/aarch_64/' | sed 's/aarch64/aarch_64/')

 	BIN_DIR := $(abspath .)/bin
 	export PATH := $(BIN_DIR):$(PATH)
 	TMP_PROTOC := $(shell mktemp -d)
endif

# `go install` will put binaries in $(GOBIN), avoiding
# messing up with global environment.
export GOBIN := $(BIN_DIR)
GOTESTSUM := $(GOBIN)/gotestsum
GOVULNCHECK := $(GOBIN)/govulncheck
GOLINES := $(GOBIN)/golines

FUZZTIME ?= "10s"

$(GOVULNCHECK):
	@go install golang.org/x/vuln/cmd/govulncheck@v1.0.4

$(GOLINES):
	@go install github.com/segmentio/golines@v0.11.0

$(BIN_DIR)/mockgen:
	@go install go.uber.org/mock/mockgen@$(MOCKGEN_VERSION)

install-buf:
	@mkdir -p $(BIN_DIR)
	curl -sSL "https://github.com/bufbuild/buf/releases/download/v$(BUF_VERSION)/buf-$(UNAME_OS)-$(UNAME_ARCH)" -o $(BIN_DIR)/buf
	@chmod +x $(BIN_DIR)/buf
.PHONY: install-buf

install-protoc: protoc-plugins
	@mkdir -p $(BIN_DIR)
ifeq ($(OS),Windows_NT)
	@mkdir -p $(TMP_PROTOC)
endif
	curl -sSL https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-${PROTOC_BUILD}.zip -o $(TMP_PROTOC)/protoc.zip
	@unzip $(TMP_PROTOC)/protoc.zip -d $(TMP_PROTOC)
	@cp -f $(TMP_PROTOC)/bin/protoc $(BIN_DIR)/protoc
	@chmod +x $(BIN_DIR)/protoc
.PHONY: install-protoc

# Download protoc plugins
protoc-plugins:
	@go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@$(GRPC_JSON_PROXY_VERSION)
	@go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@$(PROTOC_OPENAPI_VERSION)
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GO_VERSION)
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_VERSION)
.PHONY: protoc-plugins

all: build
.PHONY: all

test:
	$(GOTESTSUM) -- -race -timeout 5m -p 1 $(TEST_FLAGS) ./...
.PHONY: test

install: install-buf install-protoc $(GOVULNCHECK) $(GOLINES)
	@go mod download
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s $(GOLANGCI_LINT_VERSION)
	@go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	@go install gotest.tools/gotestsum@$(GOTESTSUM_VERSION)
	@go install github.com/spacemeshos/go-scale/scalegen@$(GOSCALE_VERSION)
.PHONY: install

tidy:
	go mod tidy
.PHONY: tidy

test-tidy:
	# Working directory must be clean, or this test would be destructive
	@git diff --quiet || (echo "\033[0;31mWorking directory not clean!\033[0m" && git --no-pager diff && exit 1)
	# We expect `go mod tidy` not to change anything, the test should fail otherwise
	@make tidy
	@git diff --exit-code || (git --no-pager diff && git checkout . && exit 1)
.PHONY: test-tidy

fmt: $(GOLINES)
	@go fmt ./...
	@$(GOLINES) -m 120 --shorten-comments -w .
.PHONY: fmt

test-fmt:
	@git diff --quiet || (echo "\033[0;31mWorking directory not clean!\033[0m" && git --no-pager diff && exit 1)
	# We expect `go fmt` and `golines` not to change anything, the test should fail otherwise
	@make fmt
	@git diff --exit-code || (git --no-pager diff && git checkout . && exit 1)
.PHONY: test-fmt

clear-test-cache:
	go clean -testcache
.PHONY: clear-test-cache

lint:
	golangci-lint run --config .golangci.yml
.PHONY: lint

vulncheck: $(GOVULNCHECK)
	$(GOVULNCHECK) ./...
.PHONY: vulncheck

# Auto-fixes golangci-lint issues where possible.
lint-fix:
	golangci-lint run --config .golangci.yml --fix
.PHONY: lint-fix

lint-github-action:
	golangci-lint run --config .golangci.yml --out-format=github-actions
.PHONY: lint-github-action

# Lint .proto files
lint-protos:
	buf lint
.PHONY: lint-protos

cover:
	go test -coverprofile=cover.out -timeout 0 -p 1 -coverpkg=./... ./...
.PHONY: cover

staticcheck:
	staticcheck ./...
.PHONY: staticcheck

build:
	go build -ldflags "-X main.version=${VERSION}" -o $(BINARY)
.PHONY: build

docker:
	@DOCKER_BUILDKIT=1 docker build --build-arg version=${VERSION} -t $(ORG)/$(IMAGE):$(BRANCH) .
.PHONY: docker

push:
	docker push $(ORG)/$(IMAGE):$(BRANCH)
.PHONY: push

# Rebuild .proto files
generate: $(BIN_DIR)/mockgen
	go generate ./...
	buf generate
.PHONY: generate

# Verify if files built from .proto are up to date.
test-generate:
	@git diff --quiet || (echo "\033[0;31mWorking directory not clean!\033[0m" && git --no-pager diff && exit 1)
	@make generate
	@git diff --name-only --diff-filter=AM --exit-code . || { echo "\nPlease rerun 'make generate' and commit changes.\n"; exit 1; }
.PHONY: test-generate

fuzz:
	./scripts/fuzz.sh $(FUZZTIME)
.PHONY: fuzz
