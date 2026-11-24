# Contributing to process-wrapper

Thank you for your interest in contributing to process-wrapper! This document provides guidelines and instructions for contributing.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [Testing](#testing)
- [Submitting Changes](#submitting-changes)
- [Releases](#releases)
- [Style Guidelines](#style-guidelines)
- [Project Structure](#project-structure)
- [Community](#community)

## Code of Conduct

This project and everyone participating in it is governed by our [Code of Conduct](CODE_OF_CONDUCT.md). By participating, you are expected to uphold this code.

## Getting Started

### Prerequisites

- Go (the version from [go.mod](go.mod))
- Docker (for the image and the end-to-end tests)
- [golangci-lint](https://golangci-lint.run/) v2
- Linux or macOS (some features, such as `memoryLimitMB` and process stats, work on Linux only)

### Fork and Clone

```bash
# Fork the repository on GitHub, then clone your fork
git clone https://github.com/YOUR_USERNAME/process-wrapper.git
cd process-wrapper

# Add upstream remote
git remote add upstream https://github.com/tentens-tech/process-wrapper.git
```

## Development Setup

### Install Dependencies

```bash
# Download Go modules
go mod download

# Optional: install the pre-commit hooks
pre-commit install
```

### Build the Project

```bash
# Build the binary
make build

# Build the Docker images
make docker-build       # with sidecars
make docker-build-slim  # process-wrapper only
```

### Run Locally

```bash
go run . -config examples/basic.yaml
curl -i localhost:8111/readiness
curl localhost:8111/status
```

Every file in [examples](examples) starts with a comment explaining how to run it.

## Making Changes

### Branch Naming

Use descriptive branch names:

- `feature/add-grpc-probe`
- `fix/restart-backoff-reset`
- `docs/update-shared-lock-guide`
- `refactor/cleanup-health-checker`

### Commit Messages

Follow the conventional commits specification:

```text
type(scope): short description

Longer description if needed.

Fixes #123
```

Types:

- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `refactor`: Code refactoring
- `test`: Adding or updating tests
- `chore`: Maintenance tasks

### Adding Options

When you add a configuration option, flag or environment variable:

1. Document it in the [README](README.md)
2. Add or update an example in [examples](examples) if it helps to understand the option
3. Cover it with a unit or e2e test and list it in [test/README.md](test/README.md)

## Testing

### Unit Tests

```bash
make test-race

# Run tests for a specific package
go test -race -v ./pkg/process/...

# Run with coverage
make cover
```

### End-to-end Tests

The e2e tests run the real image in Docker the way it runs in production (read-only root filesystem, non-root user, no capabilities):

```bash
make e2e                         # all suites (~10 min)
make e2e SUITES="basic signals"  # some suites
test/e2e/run.sh -l               # list suites
```

See [test/README.md](test/README.md) for how they work and which test covers every option.

## Submitting Changes

### Before Submitting

1. Ensure all tests pass:

   ```bash
   make test-race
   make e2e SUITES="..."  # the suites your change affects
   ```

2. Run linting:

   ```bash
   make lint
   gofmt -l .
   go mod tidy
   ```

3. Check for uncommitted changes:

   ```bash
   git status
   ```

### Pull Request Process

1. Update documentation if needed
2. Add tests for new functionality
3. Ensure the CI pipeline passes
4. Request review from maintainers
5. Address review feedback
6. Once approved, a maintainer will merge your PR

### Pull Request Checklist

- [ ] Tests pass locally
- [ ] Code follows style guidelines
- [ ] Documentation updated (if applicable)
- [ ] Behavior changes are described in the PR

## Releases

Releases are made by maintainers. Pushing a `vX.Y.Z` tag runs the [release workflow](.github/workflows/release.yml), which:

- runs the unit tests;
- builds linux/amd64 and linux/arm64 binaries and attaches them with `checksums.txt` to a GitHub release;
- pushes multi-arch images: `ghcr.io/tentens-tech/process-wrapper:vX.Y.Z` with sidecars and `:vX.Y.Z-slim` with process-wrapper only
  (and `:latest`, `:latest-slim` for non-prerelease tags).

```bash
make release-tag TAG=vX.Y.Z
```

Tags with a suffix, such as `v1.2.0-rc.1`, are published as prereleases. The workflow can also be started manually from the Actions tab for an existing tag.

## Style Guidelines

### Go Code

- Follow the [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
- Use `gofmt` for formatting
- Keep functions focused and small
- Add comments for exported functions and types
- Log with fields (`name`, `probe`, ...) via logrus, like the existing code

### Documentation

- Use clear, concise language
- Include configuration examples where helpful
- Keep README sections up to date

### YAML

- Use 2-space indentation
- Add comments explaining non-obvious configurations

## Project Structure

```text
process-wrapper/
├── main.go             # Flags, environment variables and startup
├── deployment/docker/  # Dockerfile (full image), Dockerfile-slim
├── examples/           # Self-contained configuration examples
├── pkg/
│   ├── config/         # Configuration loading (file, CONFIG env, JSON)
│   ├── metrics/        # Prometheus metrics and the VMAgent push
│   ├── phpscope/       # PHP profiling runner
│   ├── process/        # Supervisor: processes, probes, hooks, health endpoints
│   ├── prommerge/      # Merges metrics of all processes on /prommerge
│   ├── signals/        # Signal handling
│   └── stat/           # Process CPU and memory stats
└── test/
    ├── README.md       # Test coverage map
    └── e2e/            # End-to-end tests in Docker
```

## Community

### Getting Help

- Open a [GitHub Issue](https://github.com/tentens-tech/process-wrapper/issues)
- Check existing issues
- Review the [documentation](README.md)

### Reporting Bugs

Use the [bug report template](.github/ISSUE_TEMPLATE/bug_report.md) and include:

- Steps to reproduce
- Expected vs actual behavior
- Environment details
- Relevant logs

Report security vulnerabilities privately, see [SECURITY.md](SECURITY.md).

### Requesting Features

Use the [feature request template](.github/ISSUE_TEMPLATE/feature_request.md) and describe:

- The problem you're trying to solve
- Your proposed solution
- Alternative approaches considered

## License

By contributing to this project, you agree that your contributions will be licensed under the Apache License 2.0.

---

Thank you for contributing! 🎉
