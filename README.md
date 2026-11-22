# Process wrapper

Process manager for containers: runs several processes in one container, checks their health,
restarts unhealthy ones and exposes the overall state to Kubernetes.

## Features

- run multiple processes (and replicas of a process) in one container
- per-process envs, envs files, working directory and `SysProcAttr` (uid/gid, chroot, …)
- restart a process when it exits or when its readiness probe fails `failureThreshold` times in a row
  (SIGTERM, then SIGKILL after `terminationGracePeriod`)
- probes: `http`, `tcp`, `exec`, `file`, `env`, `sql`, `amqp`, `floatingPid`, `heartBeat`, `lock`
- `postStart` hooks (command or SQL), executed once after the probes pass for the first time
- aggregated `/readiness` and `/liveness` endpoints, `/tmp/started` readiness file
- graceful shutdown on SIGTERM/SIGQUIT, fast shutdown on SIGINT
- stdout/stderr interception with logfmt output, labels and severity auto-detection
- Prometheus metrics (readiness, CPU, memory, execution duration, heartbeats) and
  `/prommerge`: one endpoint that merges metrics of all processes
- distributed lock (etcd based), so only one instance of a process runs across pods
- Google Cloud preemption handling (readiness switches to 503, optional envoy drain)
- memory limit per process
- small static binary (UPX), runs as non-root with a read-only root filesystem

## Quick start

```sh
go run . -config examples/basic.yaml
curl -i localhost:8111/readiness
curl localhost:8111/status
```

Every file in [examples](examples) is self-contained and starts with a comment explaining how to run it:

| Example | What it shows |
|---|---|
| [basic.yaml](examples/basic.yaml) | two processes, file probe, restart after failed probes, `terminationGracePeriod` |
| [probes.yaml](examples/probes.yaml) | `http`, `tcp`, `exec` and `env` probes on one process |
| [post-start-hook.yaml](examples/post-start-hook.yaml) | command `postStart` hook (and an SQL one, commented out) |
| [graceful-termination.yaml](examples/graceful-termination.yaml) | replicas, SIGTERM handling, graceful vs fast shutdown |
| [heartbeat.yaml](examples/heartbeat.yaml) | `heartBeat` probe fed via `PATCH /heartbeat` |
| [floating-pid.yaml](examples/floating-pid.yaml) | crash loop detection with `floatingPid` |
| [log-severity.yaml](examples/log-severity.yaml) | `logFormat`: `auto`, `logfmt`, `free` |
| [envs-file.yaml](examples/envs-file.yaml) | `envs` + `envsFile` precedence |
| [memory-limit.yaml](examples/memory-limit.yaml) | `memoryLimitMB` (Linux only) |
| [sql-probe.yaml](examples/sql-probe.yaml) | `sql` probe against MySQL (docker) |
| [amqp.yaml](examples/amqp.yaml) | `amqp` probe against RabbitMQ (docker) |
| [prommerge.yaml](examples/prommerge.yaml) | merged metrics on `/prommerge` |
| [shared-lock.yaml](examples/shared-lock.yaml) | singleton process across instances (etcd, docker) |
| [prommerge-shared-lock.yaml](examples/prommerge-shared-lock.yaml) | shared lock + prommerge |

## Options

```
  -config string
    	config file path (if empty, the config is read from the CONFIG env in JSON)
  -debug
    	enable debug
  -envoy-drain
    	drain envoy listeners in case of preemption
  -graceful-shutdown-deadline duration
    	time between SIGTERM/SIGQUIT and SIGKILL (default 10s)
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
  -shared-lock
    	run the shared lock server instead of processes
  -shared-lock-etcd-tls
    	use tls transport for etcd
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
|---|---|
| `CONFIG` | config in JSON format, used when `-config` is not set |
| `HEALTH_SOCKET` | health/metrics server address, default `:8111` |
| `LOG_FORMAT` | default `logFormat` for all processes: `free` or `auto` |
| `POD_NAMESPACE`, `NODE_NAME`, `HOSTNAME` | added as labels to events, `POD_NAMESPACE` is required for `HANDLE_TERMINATION` |
| `HANDLE_PREEMPTION` | `true` enables Google Cloud preemption handling |
| `METADATA_URL` | preemption metadata URL, default `http://metadata.google.internal/computeMetadata/v1/instance/preempted` |
| `ENVOY_DRAIN_URL` | envoy admin [drain listeners](https://www.envoyproxy.io/docs/envoy/latest/operations/admin#post--drain_listeners) URL used with `-envoy-drain`, default `http://localhost:15000/drain_listeners` |
| `HANDLE_TERMINATION` | `true` makes the wrapper stop when the other containers of the pod are terminated (fast sidecar termination). Needs the pod service account to `get` pods in its namespace |
| `METRICS_SERVER_ADDRESS` | UDP address of a metrics-server to send events to (preemption, termination, restart, probe failure) |
| `METRICS_JSON_EXTRA_LABELS` | extra event labels in JSON, e.g. `{"k8s_cluster":"test"}` |
| `METRICS_EXTRA_LABELS_FILE_PATH` | file with pod labels mounted from the downward API, added to events |
| `METRICS_EXTRA_LABELS_FILE_EXCLUDE` | comma-separated pod labels to exclude, e.g. `pod-template-hash,security.istio.io/tlsMode` |
| `AMQP_PROBE_RABBITMQ_DSN_TEMPLATE`, `AMQP_PROBE_RABBITMQ_USER`, `AMQP_PROBE_RABBITMQ_PASS` | defaults for the `amqp` probe, see below |
| `SHARED_LEASE_SOCKET` | shared lock server address, e.g. `:8191` |
| `SHARED_LEASE_ETCD_ENDPOINTS` | etcd endpoints for the shared lock server, e.g. `localhost:2379` |
| `SHARED_LEASE_ETCD_CA_CERT`, `SHARED_LEASE_ETCD_CLIENT_CERT`, `SHARED_LEASE_ETCD_CLIENT_KEY` | etcd TLS files, used with `-shared-lock-etcd-tls` |

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

Processes inherit the wrapper's environment, `envs` and `envsFile` are applied on top of it. `restartPolicy` and `readiness.startupDeadline` are
parsed but not implemented: a process is always restarted.

### Health checks

Each process starts with a `readiness` probe runner:

1. wait `delay` (default `5s`), then run all configured probes every `period` (default `5s`);
2. if every probe passes, the process is marked healthy;
3. if any probe fails, the process is marked unhealthy immediately; after `failureThreshold`
   (default `3`) failed cycles in a row it is restarted: SIGTERM, then SIGKILL if it is still running
   after `terminationGracePeriod` (default `10s`);
4. a process without `readiness` is healthy as soon as it starts.

A process that exits on its own is restarted immediately (use `floatingPid` to catch crash loops).
Until the first successful check the process is not ready.

| Probe | Fields | Passes when |
|---|---|---|
| `http` | `url` | GET returns 2xx within 5s |
| `tcp` | `socket` (`host:port`) | connection is established within 2s |
| `exec` | `command` (`entrypoint`, `args`, `envs`, `dir`) | command exits with 0 within 30s; it inherits the wrapper's env |
| `file` | `path` | file exists |
| `env` | `name`, `value` | the wrapper's env `name` equals `value` |
| `sql` | `envs`, `queries`, `contains` | every query succeeds within 10s and the first column contains the expected values |
| `amqp` | `envs`, `reuseConn` | connection to RabbitMQ succeeds |
| `floatingPid` | `period` (`10s`), `threshold` (`1`) | the process wasn't restarted more than `threshold` times within `period` |
| `heartBeat` | `period` (`30s`) | the process sent a heartbeat (`PATCH /heartbeat?name=<name>`) within `period` |
| `lock` | `ttlDuration` (`2s`) | the shared lock lease is kept alive (see "Shared lock") |

`sql` probe and hook connect to MySQL using `MYSQL_DSN` from the process envs
([go-sql-driver/mysql](https://github.com/go-sql-driver/mysql#dsn-data-source-name) format), see
[sql-probe.yaml](examples/sql-probe.yaml).
`amqp` builds the DSN from `AMQP_PROBE_RABBITMQ_DSN_TEMPLATE` (e.g. `amqp://%v:%v@localhost:5672/`),
`AMQP_PROBE_RABBITMQ_USER` and `AMQP_PROBE_RABBITMQ_PASS` — see [amqp.yaml](examples/amqp.yaml).

### postStart hooks

`postStart` runs once per process start, after the first successful readiness check (or right after the
start if there is no `readiness`). The process becomes ready only after the hook succeeds; if the hook fails
or exceeds `deadline` (default `30s`), the process is restarted.

```yaml
postStart:
  deadline: "10s"
  command:
    entrypoint: "sh"
    args: ["-c", "curl -sf -X POST localhost:8080/warmup"]
  # or
  sql:                             # uses MYSQL_DSN from the process envs
    queries: ["SET GLOBAL read_only = 0"]
```

### Logs

stdout/stderr of each process is written by the wrapper as logfmt with `name`, `pid` and `out` labels.
`logFormat` (or the `LOG_FORMAT` env for all processes):

- empty — lines in logfmt are parsed: `level` becomes the severity and `msg` the message; other lines are logged as info;
- `auto` — the same, plus lines containing `CRITICAL`/`ERROR` are logged as errors and `WARNING` as warnings;
- `free` — lines that aren't logfmt are printed as is.

### Shared lock

`readiness.lock` makes a process a singleton across instances (e.g. pods of a deployment): before start the
wrapper acquires a lease in the shared lock server and keeps it alive while the process runs; the other
instances wait until the lease is released or expires. A waiting (standby) process is excluded from
`/readiness`, `/liveness` and `/status`, so standby pods stay Ready and rolling updates don't get stuck.

```sh
# shared lock server (backed by etcd)
SHARED_LEASE_SOCKET=:8191 SHARED_LEASE_ETCD_ENDPOINTS=localhost:2379 process-wrapper -shared-lock
# instances
process-wrapper -config examples/shared-lock.yaml -shared-lock-url http://localhost:8191
```

See [shared-lock.yaml](examples/shared-lock.yaml) and [deployment/shared-lock.yaml](deployment/shared-lock.yaml).

## Health endpoints

Served on `HEALTH_SOCKET` (default `:8111`).

| Endpoint | Description |
|---|---|
| `GET /readiness` | 200 if all processes are healthy, 503 otherwise, during preemption and graceful shutdown |
| `GET /liveness` | 200 if all processes are healthy, 503 otherwise (keeps passing during shutdown) |
| `GET /readiness?wait`, `GET /liveness?wait` | block until the check passes or the client disconnects |
| `GET /status` | health of every process, `{"app":true}` |
| `GET /pids` | pids of every process, 204 if none |
| `GET /pidof?name=<name>` | pid of a process, 404 if it isn't running |
| `GET /heartbeat[?name=<name>]` | last heartbeat of all processes or of one |
| `PATCH /heartbeat?name=<name>` | report a heartbeat (202), 404 for an unknown process |
| `GET /metrics` | wrapper metrics |
| `GET /prommerge` | metrics of all `promTarget`s merged, with a `process_name` label added |

When all processes are healthy for the first time, the wrapper creates `-readiness-file` (default
`/tmp/started`) and removes it on shutdown.

## Metrics

| Metric | Type | Labels |
|---|---|---|
| `sidecar_cmd_readiness_count` | counter | `status`, `process_name`, `cmd` |
| `sidecar_cmd_acquire_lock_total` | counter | `status`, `process_name`, `cmd` |
| `sidecar_cmd_duration_seconds` | gauge | `process_name`, `exit_code` |
| `sidecar_cmd_cpu_usage_seconds` | counter | `process_name` |
| `sidecar_cmd_cpu_usage_seconds_gauge` | gauge | `process_name` |
| `sidecar_cmd_memory_usage_bytes` | gauge | `process_name` |
| `sidecar_cmd_heartbeat_period_seconds` | gauge | `process_name` |
| `sidecar_metrics_merge_duration_seconds` | histogram | |

`cmd` is the process entrypoint (arguments are not exported, they may contain secrets).
CPU and memory are also reported for the wrapper itself (`process_name="process-wrapper"`); they are read
from `/proc` and available on Linux only.

## Signals

| Signal | Behaviour |
|---|---|
| `SIGTERM`, `SIGQUIT` | graceful shutdown: readiness switches to 503, the readiness file is removed, all processes get SIGTERM; processes still running after `-graceful-shutdown-deadline` are killed |
| `SIGINT` | fast shutdown: processes are killed immediately |
| `SIGUSR1`, `SIGUSR2` | print the state of all processes |

Processes are not restarted during shutdown. Signals are sent to the process pid only: if the entrypoint is a
shell, use `exec` (or `trap`) so the signal reaches the application, see
[graceful-termination.yaml](examples/graceful-termination.yaml).

## Docker

```sh
make docker-build
# or
docker build -t process-wrapper:${VERSION} -f deployment/docker/Dockerfile .
```

The image runs as an unprivileged user (UID/GID `65532`) and supports a read-only root filesystem.
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
make test   # go test
make lint   # golangci-lint
make vuln   # govulncheck
make sec    # gosec
```
