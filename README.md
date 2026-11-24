# Process wrapper

[![CI](https://github.com/tentens-tech/process-wrapper/actions/workflows/ci.yml/badge.svg)](https://github.com/tentens-tech/process-wrapper/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/tentens-tech/process-wrapper)](https://goreportcard.com/report/github.com/tentens-tech/process-wrapper)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Release](https://img.shields.io/github/v/release/tentens-tech/process-wrapper)](https://github.com/tentens-tech/process-wrapper/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tentens-tech/process-wrapper)](https://go.dev/)

Process manager for containers: runs several processes in one container, checks their health,
restarts unhealthy ones and exposes the overall state to Kubernetes.

## Features

- run multiple processes (and replicas of a process) in one container
- per-process envs, envs files, working directory and `SysProcAttr` (uid/gid, chroot, …)
- restart a process when it exits or when its readiness probe fails `failureThreshold` times in a row
  (SIGTERM, then SIGKILL after `terminationGracePeriod`), with an exponential back-off;
  `restartPolicy: never` and run-to-completion jobs (`isJob`)
- probes: `http`, `tcp`, `exec`, `file`, `env`, `sql`, `amqp`, `dns`, `floatingPid`, `heartBeat`, `lock`
- `postStart` hooks (command or SQL), executed once after the probes pass for the first time
- aggregated `/readiness` and `/liveness` endpoints, `/tmp/started` readiness file
- graceful shutdown on SIGTERM/SIGQUIT, fast shutdown on SIGINT
- stdout/stderr interception with logfmt output, labels, severity auto-detection and level filtering
- Prometheus metrics (readiness, CPU, memory, execution duration, heartbeats) and
  `/prommerge`: one endpoint that merges metrics of all processes
- distributed lock ([shared-lock](https://github.com/tentens-tech/shared-lock) server), so only one instance of a process runs across pods
- Google Cloud preemption handling (readiness switches to 503, optional envoy drain, VMAgent metric)
- lifecycle events (restart, readiness failure, termination, preemption) as JSON lines on stdout
- PHP continuous profiling with phpscope/phpspy
- memory limit per process
- small static binary (UPX), runs as non-root with a read-only root filesystem

## Installation

Every [release](https://github.com/tentens-tech/process-wrapper/releases) publishes a multi-arch image
(linux/amd64, linux/arm64) and static binaries.

```sh
# Docker image
docker pull ghcr.io/tentens-tech/process-wrapper:latest

# Binary
VERSION=v0.10.89
curl -fsSL "https://github.com/tentens-tech/process-wrapper/releases/download/${VERSION}/process-wrapper_${VERSION}_linux_amd64.tar.gz" | tar xz process-wrapper
```

Add the binary to your own image:

```dockerfile
COPY --from=ghcr.io/tentens-tech/process-wrapper:v0.10.89 /usr/bin/process-wrapper /usr/bin/process-wrapper
ENTRYPOINT ["/usr/bin/process-wrapper"]
```

## Quick start

```sh
go run . -config examples/basic.yaml
curl -i localhost:8111/readiness
curl localhost:8111/status
```

Every file in [examples](examples) is self-contained and starts with a comment explaining how to run it:

| Example | What it shows |
| --- | --- |
| [basic.yaml](examples/basic.yaml) | two processes, file probe, restart after failed probes, `terminationGracePeriod` |
| [probes.yaml](examples/probes.yaml) | `http`, `tcp`, `exec` and `env` probes on one process |
| [post-start-hook.yaml](examples/post-start-hook.yaml) | command `postStart` hook (and an SQL one, commented out) |
| [graceful-termination.yaml](examples/graceful-termination.yaml) | replicas, SIGTERM handling, graceful vs fast shutdown |
| [heartbeat.yaml](examples/heartbeat.yaml) | `heartBeat` probe fed via `PATCH /heartbeat` |
| [floating-pid.yaml](examples/floating-pid.yaml) | crash loop detection with `floatingPid` |
| [restart-backoff.yaml](examples/restart-backoff.yaml) | `restartBackoff`, `restartPolicy: never` |
| [job.yaml](examples/job.yaml) | `isJob`: a run-to-completion command next to the app |
| [log-severity.yaml](examples/log-severity.yaml) | `logFormat`: `auto`, `logfmt`, `free` |
| [log-levels.yaml](examples/log-levels.yaml) | `logs: false`, `logLevels`, `LOG_LEVELS` |
| [envs-file.yaml](examples/envs-file.yaml) | `envs` + `envsFile` precedence |
| [memory-limit.yaml](examples/memory-limit.yaml) | `memoryLimitMB` (Linux only) |
| [sql-probe.yaml](examples/sql-probe.yaml) | `sql` probe against MySQL (docker) |
| [amqp.yaml](examples/amqp.yaml) | `amqp` probe against RabbitMQ (docker) |
| [dns-probe.yaml](examples/dns-probe.yaml) | `dns` probe: restart when the addresses of a name change |
| [prommerge.yaml](examples/prommerge.yaml) | merged metrics on `/prommerge` |
| [shared-lock.yaml](examples/shared-lock.yaml) | singleton process across instances ([shared-lock](https://github.com/tentens-tech/shared-lock) server) |
| [prommerge-shared-lock.yaml](examples/prommerge-shared-lock.yaml) | shared lock + prommerge |
| [php-profiling.yaml](examples/php-profiling.yaml) | PHP profiling flags and requirements |

## Options

```text
  -config string
        config file path (if empty, the config is read from the CONFIG env in JSON)
  -debug
        enable debug
  -envoy-drain
        drain envoy listeners in case of preemption
  -graceful-shutdown-deadline duration
        time between SIGTERM/SIGQUIT and SIGKILL (default 10s)
  -php-app-name string
        pyroscope application name for PHP profiling (default: $POD_NAMESPACE.$HOSTNAME)
  -php-pyroscope-url string
        pyroscope URL for PHP profiling via phpscope/phpspy; empty disables it
  -php-rate-hz int
        phpspy sampling rate in Hz (default 400)
  -pid-file string
        pid file path, written when process-wrapper is not PID 1 (empty disables it) (default "pid")
  -prommerge-async
        collect all prometheus targets concurrently (default true)
  -prommerge-empty-on-failure
        return empty metric list if one of the targets failed
  -prommerge-omit-meta
        omit HELP strings from merged metrics
  -prommerge-sort
        sort merged series by labels (default true)
  -prommerge-suppress-errors
        do not log prometheus target errors
  -prommerge-supress-errors
        deprecated, use -prommerge-suppress-errors
  -pyroscope-url string
        pyroscope profiler address, e.g. http://localhost:4040
  -readiness-file string
        readiness file path (default "/tmp/started")
  -shared-lock-url string
        shared-lock server url (default "http://localhost:8191")
  -skip-commands string
        comma-separated list of ignored commands
  -version
        prints version
  -wait
        wait until -readiness-file appears and exit (0 - ready, 1 - deadline exceeded)
  -wait-deadline duration
        wait deadline duration (default 2m0s)
```

`-skip-commands` lets you keep one universal config with all possible processes and disable some of them per deployment.
`-wait` is handy as a `postStart` lifecycle hook of another container that must start after the wrapped processes are ready.

## Environment variables

| Variable | Description |
| --- | --- |
| `CONFIG` | config in JSON format, used when `-config` is not set |
| `HEALTH_SOCKET` | health/metrics server address, default `:8111` |
| `LOG_FORMAT` | default `logFormat` for all processes: `free` or `auto` |
| `LOG_LEVELS` | default `logLevels` for all processes, comma-separated, e.g. `warning,error` |
| `POD_NAMESPACE`, `NODE_NAME`, `HOSTNAME` | added as labels to events, `POD_NAMESPACE` is required for `HANDLE_TERMINATION` |
| `HANDLE_PREEMPTION` | `true` enables Google Cloud preemption handling |
| `METADATA_URL` | preemption metadata URL; if not set, `http://$NODE_NAME:9889/computeMetadata/v1/instance/preempted` (gke-metadata-server) is used when it responds, otherwise `http://metadata.google.internal/computeMetadata/v1/instance/preempted`. `?wait_for_change=true&timeout_sec=55` is appended |
| `ENVOY_DRAIN_URL` | envoy admin [drain listeners](https://www.envoyproxy.io/docs/envoy/latest/operations/admin#post--drain_listeners) URL used with `-envoy-drain`, default `http://localhost:15000/drain_listeners?inboundonly` |
| `VMAGENT_PUSH_URL` | VMAgent [import](https://docs.victoriametrics.com/vmagent/) URL for the `preempted` metric, e.g. `http://vmagent:8429/api/v1/import/prometheus`; not set — the metric isn't pushed |
| `HANDLE_TERMINATION` | `true` makes the wrapper stop when the other containers of the pod are terminated (fast sidecar termination). Needs the pod service account to `get` pods in its namespace |
| `METRICS_JSON_EXTRA_LABELS` | extra event labels in JSON, e.g. `{"k8s_cluster":"test"}` |
| `METRICS_EXTRA_LABELS_FILE_PATH` | file with pod labels mounted from the downward API, added to events |
| `METRICS_EXTRA_LABELS_FILE_EXCLUDE` | comma-separated pod labels to exclude, e.g. `pod-template-hash,security.istio.io/tlsMode` |
| `AMQP_PROBE_RABBITMQ_DSN_TEMPLATE`, `AMQP_PROBE_RABBITMQ_USER`, `AMQP_PROBE_RABBITMQ_PASS` | defaults for the `amqp` probe, see below |

```yaml
env:
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  - name: NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName
```

## Configuration

The config is a YAML list of processes (or the same structure in JSON in the `CONFIG` env).
It is close to the Kubernetes container spec.

```yaml
- name: "app"                      # unique name, used in logs, metrics and endpoints
  entrypoint: "sh"
  args: ["-c", "exec /usr/bin/app --port $PORT"]
  dir: "/srv/app"                  # working directory
  envs:
    PORT: "8080"
  envsFile: "/etc/app/envs"        # KEY=value lines; values from the file override `envs`
  logFormat: "auto"                # see "Logs"
  logLevels: ["warning", "error"]  # forward only these levels, see "Logs"
  logs: true                       # false: don't forward stdout/stderr
  restartPolicy: "always"          # or "never"
  restartBackoff:                  # delay between restarts, see "Restarts"
    initialDelay: "1s"
    maxDelay: "1m"
  isJob: false                     # true: run to completion, see "Restarts"
  replicas: 0                      # >0 runs N copies named app_0, app_1, …
  memoryLimitMB: 512               # kill (and restart) the process above this RSS, Linux only
  terminationGracePeriod: "10s"    # SIGTERM -> SIGKILL delay on restart by a probe
  promTarget: "http://localhost:8080/metrics"   # merged into /prommerge
  sysProcAttr:                     # syscall.SysProcAttr, e.g. credential: {uid: 1000, gid: 1000}
    setpgid: true
  readiness:
    delay: "5s"
    period: "5s"
    failureThreshold: 3
    http:
      url: "http://localhost:8080/health"
  postStart:
    deadline: "30s"
    command:
      entrypoint: "sh"
      args: ["-c", "echo started"]
```

Processes inherit the wrapper's environment, `envs` and `envsFile` are applied on top of it. `readiness.startupDeadline` is
parsed but not implemented.

### Restarts

A process that exits (or is restarted by a probe or `memoryLimitMB`) is started again after a back-off
delay that grows exponentially while the process keeps failing:

```yaml
restartBackoff:
  initialDelay: "1s"       # first delay
  multiplier: 2            # growth factor
  maxDelay: "1m"           # cap
  jitter: 0.2              # ±20% random component, 0 disables it
  successThreshold: "10s"  # uptime that resets the delay to initialDelay
```

All fields are optional (the values above are the defaults). The delay is also reset when the readiness
probe passes. The current delay is exported as `sidecar_cmd_restart_backoff_seconds`. A shutdown signal
interrupts the back-off.

- `restartPolicy: never` — the process is not restarted after it exits (`always` is the default); after exit
  code 0 it counts as ready, after a non-zero exit code the pod stays not ready.
- `isJob: true` — the process runs to completion (e.g. a migration): after exit code 0 it isn't restarted
  and counts as ready, a non-zero exit code stops the wrapper with exit code 1. A job killed by the wrapper (failed probe,
  memory limit) is restarted.

The wrapper keeps running while there is nothing left to run; stop it with a signal.

### Health checks

Each process starts with a `readiness` probe runner:

1. wait `delay` (default `5s`), then run all configured probes every `period` (default `5s`);
2. if every probe passes, the process is marked healthy;
3. if any probe fails, the process is marked unhealthy immediately; after `failureThreshold`
   (default `3`) failed cycles in a row it is restarted: SIGTERM, then SIGKILL if it is still running
   after `terminationGracePeriod` (default `10s`);
4. a process without `readiness` is healthy as soon as it starts.

A process that exits on its own is restarted after the back-off (see "Restarts", use `floatingPid` to catch crash loops).
A process that can't be started (missing entrypoint, `dir` or `envsFile`) doesn't stop the wrapper: it is
marked unhealthy and retried with a backoff from 1s to 30s (`sidecar_cmd_readiness_count{status="start_failure"}`).
Until the first successful check the process is not ready.

Every process runs in its own process group: stop signals (probe restart, memory limit, shutdown) are sent
to the whole group, and processes left in the group after the command exits (e.g. shell background jobs)
are killed. Descendants that create their own session (`setsid`) are not tracked.

| Probe | Fields | Passes when |
| --- | --- | --- |
| `http` | `url` | GET returns 2xx within 5s |
| `tcp` | `socket` (`host:port`) | connection is established within 2s |
| `exec` | `command` (`entrypoint`, `args`, `envs`, `dir`) | command exits with 0 within 30s; it inherits the wrapper's env, runs in the process `dir` (unless set) and with the process `credential` |
| `file` | `path` | file exists |
| `env` | `name`, `value` | the wrapper's env `name` equals `value` |
| `sql` | `envs`, `queries`, `contains` | every query succeeds within 10s and the first column contains the expected values |
| `amqp` | `envs`, `operationsTimeout` (`5s`), `tlsInsecureSkipVerify` | `basic.get` on the durable `amqp-probe-queue` queue succeeds; the connection is kept open between checks (`reuseConn` is deprecated) |
| `dns` | `url` (a host name), `timeout` (lookup timeout, default 5s) | the name resolves to the same addresses as when the process started; when they change the process is restarted after `failureThreshold` |
| `floatingPid` | `period` (`10s`), `threshold` (`1`) | the process wasn't started more than `threshold` times within `period` (the default allows no restarts within 10s) |
| `heartBeat` | `period` (`30s`) | the process sent a heartbeat (`PATCH /heartbeat?name=<name>`) within `period` |
| `lock` | `ttlDuration` (`2s`) | the shared lock lease is kept alive (see "Shared lock") |

`sql` probe and hook connect to MySQL using `MYSQL_DSN` from the process envs
([go-sql-driver/mysql](https://github.com/go-sql-driver/mysql#dsn-data-source-name) format), see
[sql-probe.yaml](examples/sql-probe.yaml).
`amqp` builds the DSN from `AMQP_PROBE_RABBITMQ_DSN_TEMPLATE` (e.g. `amqp://%v:%v@localhost:5672/`),
`AMQP_PROBE_RABBITMQ_USER` and `AMQP_PROBE_RABBITMQ_PASS` — see [amqp.yaml](examples/amqp.yaml). For `amqps://`
the broker certificate is verified unless `tlsInsecureSkipVerify: true`.

### postStart hooks

`postStart` runs once per process start, after the first successful readiness check (or right after the
start if there is no `readiness`). The process becomes ready only after the hook succeeds; if the hook fails
or exceeds `deadline` (default `30s`), the process is restarted.

A command hook gets the process envs with `postStart.envs` and `command.envs` on top (`postStart.envs` apply to the
`sql` hook too, e.g. another `MYSQL_DSN`), runs in the process `dir` (unless set) and never with more privileges
than the process: if the process drops privileges with `sysProcAttr.credential`, the hook (as well as `exec`
probes) runs with the same credential. Hook output is limited to 64 KiB per stream, a hook that writes more
is killed and considered failed.

```yaml
postStart:
  deadline: "10s"
  envs:
    WARMUP_URL: "localhost:8080/warmup"
  command:
    entrypoint: "sh"
    args: ["-c", "curl -sf -X POST $WARMUP_URL"]
  # or
  sql:                             # uses MYSQL_DSN from the process envs
    queries: ["SET GLOBAL read_only = 0"]
```

### Logs

stdout/stderr of each process is written by the wrapper as logfmt with `name`, `pid` and `out` labels.
Lines longer than 64 KiB are logged in parts.
`logFormat` (or the `LOG_FORMAT` env for all processes):

- empty — lines in logfmt are parsed: `level` becomes the severity and `msg` the message; other lines are logged as info;
- `auto` — the same, plus lines containing `CRITICAL`/`ERROR` are logged as errors and `WARNING` as warnings;
- `free` — lines that aren't logfmt are printed as is.

`logs: false` disables forwarding of the process output (stdout/stderr go to `/dev/null`).

`logLevels` (or the `LOG_LEVELS` env for all processes, comma-separated) forwards only the listed levels:
`debug`, `info`, `warning` (`warn`), `error` (`err`, `critical`, `fatal`, `panic`). It applies to logfmt lines
and to `logFormat: auto` (where the level is detected by the `CRITICAL`/`ERROR`/`FATAL`/`PANIC`, `WARNING` and
`DEBUG` keywords, case-insensitive); other lines are never filtered. Debug lines listed in the filter are forwarded
whatever the wrapper log level. `logLevels` of a process overrides `LOG_LEVELS`, both empty disable the filter. See [log-levels.yaml](examples/log-levels.yaml).

### Events

Lifecycle events are written to stdout as one JSON line each, for log collectors (e.g. Grafana Alloy matching
`_emitter`):

```json
{"_emitter":"process-wrapper","name":"restart","labels":{"event_type":"restart","process_name":"app","pod":"app-5d9c","namespace":"default","node":"node-1"},"op":"event","value":1}
```

| Event | When |
| --- | --- |
| `restart` | a process is restarted after `failureThreshold` failed probes |
| `readiness` | a probe failed (successful checks are reported with `-debug` only) |
| `termination` | the wrapper received a termination signal or the pod is terminated |

Labels: `pod` (`HOSTNAME`), `namespace` (`POD_NAMESPACE`), `node` (`NODE_NAME`), `METRICS_JSON_EXTRA_LABELS`,
labels from `METRICS_EXTRA_LABELS_FILE_PATH`, and event-specific ones. The UDP metrics-server
(`METRICS_SERVER_ADDRESS`) was removed.

With `HANDLE_PREEMPTION` and `VMAGENT_PUSH_URL` set, the wrapper also pushes the `preempted` metric (0 after every
metadata poll, 1 on preemption) to `VMAGENT_PUSH_URL`; failed pushes are counted in `sidecar_vmagent_push_errors_total`.

### PHP profiling

`-php-pyroscope-url` runs phpscope (`/usr/bin/phpscope`, e.g. from the `hiload/phpscope` image, it spawns
[phpspy](https://github.com/adsr/phpspy)) next to the processes and sends PHP profiles to Pyroscope; it is
restarted when it exits (`sidecar_phpscope_restarts_total`). The default image doesn't contain phpscope/phpspy
(if the binary is missing, an error is logged and profiling is disabled), the process needs `SYS_PTRACE` and
must see the PHP processes. See [php-profiling.yaml](examples/php-profiling.yaml).
The URL is passed to phpscope as a command-line argument and is visible in the process list: don't put
credentials into it, send profiles to an endpoint without authentication (e.g. an in-cluster Pyroscope
or an authenticating proxy).

### Shared lock

`readiness.lock` makes a process a singleton across instances (e.g. pods of a deployment): before start the
wrapper acquires a lease in the shared lock server and keeps it alive while the process runs; the other
instances wait until the lease is released or expires. A completed job (`isJob`) or a `restartPolicy: never`
process keeps the lease until the wrapper stops, so another instance doesn't run it again. A waiting (standby) process is excluded from
`/readiness` and `/status`, so standby pods stay Ready and rolling updates don't get stuck.

The lock server is a separate service, [tentens-tech/shared-lock](https://github.com/tentens-tech/shared-lock)
(backed by etcd, with a Kubernetes manifest); the `-shared-lock` server mode of process-wrapper was removed.

```sh
process-wrapper -config examples/shared-lock.yaml -shared-lock-url http://shared-lock:8080
```

See [shared-lock.yaml](examples/shared-lock.yaml).

## Health endpoints

Served on `HEALTH_SOCKET` (default `:8111`).

| Endpoint | Description |
| --- | --- |
| `GET /readiness` | 200 if all processes are healthy, 503 otherwise, during preemption and graceful shutdown |
| `GET /liveness` | always 200 while the wrapper serves requests: unhealthy processes are restarted by the wrapper itself, so kubelet doesn't restart the whole container |
| `GET /readiness?wait`, `GET /liveness?wait` | block until the check passes or the client disconnects |
| `GET /status` | health of every process, `{"app":true}` |
| `GET /pids` | pids of every process, 204 if none |
| `GET /pidof?name=<name>` | pid of a process, 404 if it isn't running |
| `GET /heartbeat[?name=<name>]` | last heartbeat of all processes or of one |
| `PATCH /heartbeat?name=<name>` | report a heartbeat (202), 404 for an unknown process |
| `GET /metrics` | wrapper metrics |
| `GET /prommerge` | metrics of all `promTarget`s merged, with a `process_name` label added |

Only these endpoints are served: handlers that libraries register globally (e.g. `/debug/vars`,
`/debug/requests`) are not exposed. The endpoints have no authentication, don't publish the port outside
the pod.

When all processes are healthy for the first time, the wrapper creates `-readiness-file` (default
`/tmp/started`) and removes it on shutdown.

## Metrics

| Metric | Type | Labels |
| --- | --- | --- |
| `sidecar_cmd_readiness_count` | counter | `status`, `process_name`, `cmd` |
| `sidecar_cmd_acquire_lock_total` | counter | `status`, `process_name`, `cmd` |
| `sidecar_cmd_duration_seconds` | gauge | `process_name`, `exit_code` |
| `sidecar_cmd_cpu_usage_seconds` | counter | `process_name` |
| `sidecar_cmd_cpu_usage_seconds_gauge` | gauge | `process_name` |
| `sidecar_cmd_memory_usage_bytes` | gauge | `process_name` |
| `sidecar_cmd_heartbeat_period_seconds` | gauge | `process_name` |
| `sidecar_cmd_restart_backoff_seconds` | gauge | `process_name` |
| `sidecar_metrics_merge_duration_seconds` | histogram | |
| `sidecar_vmagent_push_errors_total` | counter | |
| `sidecar_phpscope_restarts_total` | counter | |

`cmd` is the process entrypoint (arguments are not exported, they may contain secrets).
CPU and memory are also reported for the wrapper itself (`process_name="process-wrapper"`); they are read
from `/proc` and available on Linux only.

## Signals

| Signal | Behaviour |
| --- | --- |
| `SIGTERM`, `SIGQUIT` | graceful shutdown: readiness switches to 503, the readiness file is removed, process groups of all processes get SIGTERM; processes still running after `-graceful-shutdown-deadline` are killed |
| `SIGINT` | fast shutdown: processes are killed immediately |
| `SIGUSR1`, `SIGUSR2` | print the state of all processes |

Processes are not restarted during shutdown. Signals are sent to the process group, so with a shell entrypoint
both the shell and the application get them; use `exec` or `trap` to control the shutdown order, see
[graceful-termination.yaml](examples/graceful-termination.yaml).

## Docker

```sh
make docker-build
# or
docker build -t process-wrapper:${VERSION} -f deployment/docker/Dockerfile .
```

The image runs as an unprivileged user (UID/GID `1010`, change it with `--build-arg UID=... --build-arg GID=...`) and supports a read-only root filesystem.
process-wrapper only writes `-readiness-file` (default `/tmp/started`) and `-pid-file` (relative to the
working directory `/tmp`), so `/tmp` must be writable:

```sh
docker run --rm --read-only --tmpfs /tmp -v $PWD/config.yaml:/config.yaml:ro \
  process-wrapper:<version> process-wrapper -config /config.yaml
```

```yaml
# Kubernetes
securityContext:
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
volumeMounts:
  - name: tmp
    mountPath: /tmp
volumes:
  - name: tmp
    emptyDir: {}
```

`sysProcAttr` options that switch credentials (`credential.uid/gid`) or use `chroot` need root
or the corresponding capabilities (`CAP_SETUID`, `CAP_SETGID`, `CAP_SYS_CHROOT`) and won't work as non-root.

To use the wrapper with an existing image, copy the binary from this image into a shared `emptyDir` in an
init container and run it as the entrypoint of the application container.

## Development

```sh
make test        # go test
make test-race   # go test -race
make e2e         # end-to-end tests in docker, make e2e SUITES="basic signals" for some suites
make lint        # golangci-lint
make vuln        # govulncheck
make sec         # gosec
```

See [test/README.md](test/README.md) for the e2e suites and which test covers every option.

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow and how releases are made,
and [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

## License

Apache License 2.0, see [LICENSE](LICENSE).
