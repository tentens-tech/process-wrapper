BINARY=process-wrapper
IMAGE=process-wrapper
VERSION := 0.9.22-test
GIT_COMMIT := $(shell git rev-list -1 HEAD)

.DEFAULT_GOAL := build
.PHONY: help lint vuln sec test test-race e2e clean build build-version docker-build

version: ## Show version
	@echo $(VERSION) \(git commit: $(GIT_COMMIT)\)

lint: # Run linters
	golangci-lint run --color always ./...

vuln: ## Check dependencies and stdlib for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

sec: ## Run gosec static security analysis
	go run github.com/securego/gosec/v2/cmd/gosec@latest ./...

test: ## Run only quick tests
	go test -short  ./...

test-race: ## Run all unit tests with the race detector
	go test -race ./...

e2e: ## Run end-to-end tests in docker, e.g. make e2e SUITES="basic signals"
	test/e2e/run.sh $(SUITES)

build: ## Build pulumi binary
	go build -o ${BINARY} main.go

build-version: ## Inject version
	go build -ldflags="-X 'main.version=$(VERSION)'" -o ${BINARY} main.go

docker-build: ## Inject version
	docker build -t $(IMAGE):$(VERSION) -f deployment/docker/Dockerfile . && \
    echo "docker run -it $(IMAGE):$(VERSION) $(BINARY) -version"

clean: ## Remove binary
	if [ -f ${BINARY} ] ; then rm ${BINARY} ; fi

