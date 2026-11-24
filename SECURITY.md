# Security Policy

## Supported Versions

We release patches for security vulnerabilities for the latest minor version:

| Version | Supported          |
| ------- | ------------------ |
| 0.10.x  | :white_check_mark: |
| < 0.10  | :x:                |

## Reporting a Vulnerability

We take security bugs seriously. We appreciate your efforts to responsibly disclose your findings, and will make every effort to acknowledge your contributions.

### How to Report

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report them privately through GitHub:
[**Report a vulnerability**](https://github.com/tentens-tech/process-wrapper/security/advisories/new)
(the "Security" tab of the repository → "Report a vulnerability").

Please include the following information in your report:

- **Type of issue** (e.g., privilege escalation, command injection, information disclosure)
- **Full paths of source file(s)** related to the manifestation of the issue
- **Location of the affected source code** (tag/branch/commit or direct URL)
- **Any special configuration required** to reproduce the issue
- **Step-by-step instructions** to reproduce the issue
- **Proof-of-concept or exploit code** (if possible)
- **Impact of the issue**, including how an attacker might exploit the issue

### What to Expect

- **Acknowledgment**: We will acknowledge receipt of your vulnerability report within 48 hours.
- **Communication**: We will keep you informed about our progress throughout the process.
- **Resolution**: We aim to resolve critical issues within 90 days of the initial report.
- **Credit**: If you would like, we will credit you in any public disclosure of the vulnerability.

## Security Considerations

### Health Listener

process-wrapper serves `/readiness`, `/liveness`, `/status`, `/pids`, `/pidof`, `/heartbeat`, `/prommerge` and `/metrics`
on `HEALTH_SOCKET` (default `:8111`) without authentication. These endpoints expose process names, PIDs and metrics,
and `PATCH /heartbeat` changes the state of `heartBeat` probes.

- Don't expose the health port outside the pod; use Kubernetes network policies to restrict access.
- Bind it to localhost (`HEALTH_SOCKET=127.0.0.1:8111`) when only local probes need it.

### Processes and Privileges

- The image runs as an unprivileged user (UID/GID `1010`) and supports a read-only root filesystem.
- Supervised commands, `postStart` command hooks and `exec` probes run with the identity, working directory and
  process attributes of the supervised command.
- `sysProcAttr` options that switch credentials or use `chroot` need root or the corresponding capabilities.
  Grant them only when you need them.
- Signals are sent to the whole process group, and descendants are stopped with the command.

### Secrets

- Credentials in `sql` and `amqp` probes, in `postStart` SQL hooks and in the Pyroscope URL are taken from the
  configuration and environment variables. Keep them in Kubernetes Secrets and mount them as env vars or `envsFile`.
- The Pyroscope URL with credentials is passed to `phpscope` as a command line argument, so it is visible
  in the process list of the container.
- process-wrapper logs the Pyroscope URL with the credentials redacted.

## Security Best Practices

### For Operators

1. **Keep Updated**: Regularly update to the latest version
2. **Network Policies**: Restrict access to the health port
3. **Least Privilege**: Run as non-root with `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false` and dropped capabilities
4. **Secrets**: Don't put credentials into the configuration file in plain text

### For Contributors

1. **Dependency Scanning**: Dependabot is configured to keep dependencies up to date
2. **Code Review**: All changes require code review before merging
3. **Security Testing**: Include security-focused tests for new features
4. **Static Analysis**: golangci-lint (with gosec), gosec, govulncheck and Trivy run as part of CI

## Security Updates

Security updates will be released as patch versions and announced via:

- GitHub Security Advisories
- Release notes

## Attribution

We would like to thank the following individuals for responsibly disclosing security issues:

*None yet - be the first!*
