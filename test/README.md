# Tests

```sh
make test-race                   # unit tests
make e2e                         # all e2e suites (~10 min, docker)
make e2e SUITES="basic signals"  # some suites
test/e2e/run.sh -l               # list suites
```

e2e env: `E2E_KEEP=1` keeps the containers, `E2E_NO_BUILD=1` reuses the images,
`E2E_LOGDIR` changes the logs directory (default `test/e2e/.logs`, every container log is saved there).

## How e2e works

[`e2e/run.sh`](e2e/run.sh) builds the image from `deployment/docker/Dockerfile` plus a tools layer
([`e2e/tools.Dockerfile`](e2e/tools.Dockerfile): `curl`, `python3`, the user stays 65532) and runs every
wrapper like in production: read-only root FS, tmpfs `/tmp`, non-root, `--cap-drop ALL`, `no-new-privileges`.
All containers share the network namespace of `pw-netns`, so `localhost` in the configs reaches the dependencies.
Dependencies are started only by the suites that need them: MySQL 8.4 (`sql`), RabbitMQ 4 (`amqp`),
etcd 3.6 (`lock`, `lock-tls` with mTLS certificates generated per run) and
[`e2e/fake.py`](e2e/fake.py) (`events`): GCE metadata, envoy admin and a UDP metrics-server.

Configs: [`examples/`](../examples) (every example is run) and [`e2e/configs/`](e2e/configs) for the cases the
examples don't cover.

## Coverage

Where every option is tested. *unit* — `go test`, otherwise the e2e suite.

### Flags

| Flag | Tests |
|---|---|
| `-config` | every suite; invalid yaml: `flags` |
| `-skip-commands` | `flags` |
| `-readiness-file` | `flags` (default: `basic`, `termination`) |
| `-pid-file` | `flags` (`--init`, wrapper isn't PID 1) |
| `-pyroscope-url` | `flags` (unreachable server doesn't break the wrapper) |
| `-shared-lock`, `-shared-lock-url` | `lock` |
| `-shared-lock-etcd-tls` | `lock-tls`, unit (`TestTLSInfoDefaults`) |
| `-debug` | `flags` |
| `-wait`, `-wait-deadline` | `flags` (success and timeout) |
| `-version`, `-h` | `image` |
| `-envoy-drain` | `events` |
| `-prommerge-empty-on-failure`, `-prommerge-suppress-errors`, `-prommerge-omit-meta`, `-prommerge-sort`, `-prommerge-async` | `prommerge`, unit (`pkg/prommerge`) |
| `-prommerge-supress-errors` (deprecated) | `prommerge` |
| `-graceful-shutdown-deadline` | `signals` |

### Environment variables

| Variable | Tests |
|---|---|
| `CONFIG` | `flags`, unit (`TestJSONEnvLoadAllFields`) |
| `HEALTH_SOCKET` | `flags`, `lock` |
| `LOG_FORMAT` | `logs` |
| `POD_NAMESPACE`, `NODE_NAME`, `HOSTNAME` | `events`, unit (`TestEventLabels`) |
| `HANDLE_PREEMPTION`, `METADATA_URL` | `events` |
| `ENVOY_DRAIN_URL` | `events` |
| `HANDLE_TERMINATION` | unit only (`TestPodStatusStopsWrapperWhenMainContainerCompleted`): the Kubernetes API URL is fixed |
| `METRICS_SERVER_ADDRESS` | `events`, unit |
| `METRICS_JSON_EXTRA_LABELS`, `METRICS_EXTRA_LABELS_FILE_PATH`, `METRICS_EXTRA_LABELS_FILE_EXCLUDE` | `events`, unit |
| `AMQP_PROBE_RABBITMQ_DSN_TEMPLATE`, `_USER`, `_PASS` | `amqp` (from the process `envs` and from the wrapper env) |
| `SHARED_LEASE_SOCKET`, `SHARED_LEASE_ETCD_ENDPOINTS` | `lock`, `lock-tls` |
| `SHARED_LEASE_ETCD_CA_CERT`, `_CLIENT_CERT`, `_CLIENT_KEY` | `lock-tls` (set and default paths) |

### Config

Every field is parsed from YAML and JSON in unit tests (`pkg/config/load_test.go`, `testdata/all-fields.yaml`).

| Field | Tests |
|---|---|
| `name`, `entrypoint`, `args` | every suite |
| `envs`, `envsFile` | `envs`, `hardening` (missing file) |
| `dir` | `envs`, `probes` (exec probe), `hooks`, `hardening` (missing dir: unit) |
| `logFormat` (empty, `auto`, `free`) | `logs`, unit |
| `replicas` | `termination` |
| `memoryLimitMB` | `memory` |
| `terminationGracePeriod` | `basic`, unit |
| `promTarget` | `prommerge`, `lock` |
| `sysProcAttr` (`credential`) | `credential`, unit |
| `readiness.delay`, `period`, `failureThreshold` | `basic`, `probes`, unit |
| `readiness.http`, `tcp`, `file`, `env`, `exec` | `probes` (pass and fail), unit |
| `readiness.sql` | `sql` |
| `readiness.amqp` (`reuseConn`) | `amqp` |
| `readiness.floatingPid` | `floating`, unit |
| `readiness.heartBeat` | `heartbeat`, unit |
| `readiness.lock` (`ttlDuration`) | `lock`, unit |
| `postStart.command`, `deadline`, `envs` | `hooks`, `hardening`, unit |
| `postStart.sql` | `sql` |
| `restartPolicy`, `readiness.startupDeadline` | parsed only (unit), not implemented |

### Endpoints, signals, metrics

| | Tests |
|---|---|
| `/readiness`, `/liveness` (`?wait`), `/status`, `/pids`, `/pidof` | `basic`, `termination` |
| `/heartbeat` (GET, PATCH, errors) | `basic`, `heartbeat` |
| `/metrics`, `/prommerge` | `basic`, `prommerge` |
| `/debug/*` not exposed | `basic`, `lock`, unit |
| `SIGTERM`, `SIGQUIT`, `SIGINT`, `SIGUSR1` | `signals`, `termination` |
| process groups, start retry, output limits | `hardening`, unit |
| `sidecar_cmd_*` metrics | `basic`, `floating`, `heartbeat`, `hardening`, `lock`, `prommerge` |
| image: non-root, read-only binary | `image` |
