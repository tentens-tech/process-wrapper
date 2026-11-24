---
name: Bug Report
about: Create a report to help us improve process-wrapper
title: '[BUG] '
labels: bug
assignees: ''
---

## Bug Description

A clear and concise description of what the bug is.

## Steps to Reproduce

1. Run process-wrapper with config '...'
2. Send signal / call endpoint '...'
3. Observe '...'
4. See error

## Expected Behavior

A clear and concise description of what you expected to happen.

## Actual Behavior

A clear and concise description of what actually happened.

## Environment Information

- **process-wrapper Version**: [output of `process-wrapper -version`]
- **Runtime**: [e.g., Kubernetes 1.31, Docker 27, bare Linux]
- **Image / OS**: [e.g., ghcr.io/tentens-tech/process-wrapper, alpine 3.24, debian 12]
- **Architecture**: [e.g., amd64, arm64]
- **Runs as**: [PID 1 or not, uid, read-only root filesystem]

## Configuration

Include your process-wrapper configuration (sanitize sensitive data):

```yaml
# CONFIG / -config file
```

Relevant flags and environment variables:

```text
Paste flags and env vars here
```

## Logs

Include relevant process-wrapper logs (run with `-debug` if possible):

```text
Paste logs here
```

## Additional Context

Add any other context about the problem here, such as:

- Probe types involved (http, tcp, exec, sql, amqp, dns, lock, ...)
- Shutdown / signal handling details
- Kubernetes pod spec (probes, securityContext, terminationGracePeriodSeconds)

## Checklist

- [ ] I have searched existing issues to ensure this is not a duplicate
- [ ] I have included all relevant configuration and logs
- [ ] I have sanitized any sensitive information
