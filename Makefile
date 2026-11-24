BINARY=process-wrapper
IMAGE=process-wrapper
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT := $(shell git rev-list -1 HEAD)

.DEFAULT_GOAL := build
.PHONY: help version lint vuln sec test test-race cover e2e clean build build-version docker-build docker-build-slim release-tag

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

version: ## Show version
	@echo $(VERSION) \(git commit: $(GIT_COMMIT)\)

lint: ## Run linters
	golangci-lint run --color always ./...

vuln: ## Check dependencies and stdlib for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

sec: ## Run gosec static security analysis
	go run github.com/securego/gosec/v2/cmd/gosec@latest ./...

test: ## Run only quick tests
	go test -short ./...

test-race: ## Run all unit tests with the race detector
	go test -race ./...

cover: ## Run unit tests with coverage, writes coverage.out
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

e2e: ## Run end-to-end tests in docker, e.g. make e2e SUITES="basic signals"
	test/e2e/run.sh $(SUITES)

build: ## Build the binary
	go build -o ${BINARY} main.go

build-version: ## Build the binary with the version injected
	go build -ldflags="-X 'main.version=$(VERSION)'" -o ${BINARY} main.go

docker-build: ## Build the full docker image (process-wrapper with sidecars)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -f deployment/docker/Dockerfile . && \
	echo "docker run --rm $(IMAGE):$(VERSION) $(BINARY) -version"

docker-build-slim: ## Build the slim docker image (process-wrapper only)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION)-slim -f deployment/docker/Dockerfile-slim . && \
	echo "docker run --rm $(IMAGE):$(VERSION)-slim $(BINARY) -version"

release-tag: ## Create and push a release tag, e.g. make release-tag TAG=v0.10.90
	@test -n "$(TAG)" || { echo "usage: make release-tag TAG=vX.Y.Z"; exit 1; }
	@echo "$(TAG)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { echo "TAG must look like vX.Y.Z"; exit 1; }
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin refs/tags/$(TAG)

clean: ## Remove the binary
	rm -f ${BINARY} coverage.out
