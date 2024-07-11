BINARY=process-wrapper
IMAGE=us-central1-docker.pkg.dev/production-4f83b34d/golang-docker-images/process-wrapper
VERSION := 0.9.22-test
GIT_COMMIT := $(shell git rev-list -1 HEAD)

.DEFAULT_GOAL := build
.PHONY: help lint-prepare lint unittest clean install build run stop

version: ## Show version
	@echo $(VERSION) \(git commit: $(GIT_COMMIT)\)

lint: # Run linters
	golangci-lint run --color always ./...

test: ## Run only quick tests
	go test -short  ./...

build: ## Build pulumi binary
	go build -o ${BINARY} main.go

build-version: ## Inject version
	go build -ldflags="-X 'main.version=$(VERSION)'" -o ${BINARY} main.go

docker-build-slim: ## Inject version
	docker build  -t $(IMAGE)-slim:$(VERSION) -f deployment/docker/Dockerfile-slim . && \
    echo "docker run -it $(IMAGE)-slim:$(VERSION) $(BINARY) -version"

docker-build: ## Inject version
	docker build -t $(IMAGE):$(VERSION) -f deployment/docker/Dockerfile . && \
    echo "docker run -it $(IMAGE):$(VERSION) $(BINARY) -version"

clean: ## Remove binary
	if [ -f ${BINARY} ] ; then rm ${BINARY} ; fi

