# Process wrapper
Process manager for containers

### Features
- run multiple processes in the container
- handle processes lifecycle including spawn/termination/restart
- handle unix signals from kubelet
- intercept processes stdout/stderr
- separated process logs by using logfmt format with labels support
- separated env variables for each process
- support load envs from file
- multiple types of process health checks (http, tcp, exec, file, env, sql, floatingPid, heartBeat), which allows restarting one process as the container in kubernetes pod
- prometheus metrics (execution duration, readiness failed, process cpu/memory usage)
- elasticsearch metrics (probe status, preemption, termination, restart events)
- liveness endpoint http://0.0.0.0:8111/liveness which reflects the state of all processes, e.g., if one of the apps isn't healthy, the liveness endpoint will return 503 and kubelet will restart the container, http://0.0.0.0:8111/liveness?wait waits until all processes become healthy
- readiness endpoint http://0.0.0.0:8111/readiness which reflects the state of all processes and preemption status, e.g., if one of the apps isn't ready, the readiness endpoint will return 503, http://0.0.0.0:8111/readiness?wait waits until all processes become healthy
- when all processes started and healthy, process-wrapper creates file /tmp/started (it can be overridden by -readiness-file option), and removes it after shutdown
- handle google cloud preemption gracefully, e.g., if the node is in the preemption state, readiness endpoint will return 503
- small binary size packed by UPX
- ability to start process with specific attributes (uid, gid, chroot, etc)
- support post start hooks (command, sql) which executed after all process probes successfully passed
- write a universal config with all possible sidecar processes and ignore running specific using -skip-commands option
- binary injection via init container, which allows using existing docker images
- yaml configuration is mostly the same as the kubernetes container spec
- unified endpoint for merging multiple Prometheus targets. You can access this functionality at http://0.0.0.0:8111/prommerge. This update simplifies monitoring configurations by consolidating multiple targets into a single endpoint, improving efficiency and manageability.


### Start
```sh
go run main.go
```

### Options
```
  -config string
    	config file path
  -debug
    	enable debug
  -envoy-drain
    	drain envoy listeners in case of preemption
  -graceful-shutdown-deadline duration
    	time between SIGTERM/SIGQUIT and SIGKILL (default 10s)
  -pyroscope-url string
    	pyroscope profiler address, e.g. http://localhost:4040
  -readiness-file string
    	readiness file path (default "/tmp/started")
  -shared-lock
    	shared lock server
  -shared-lock-url string
    	shared-lock server url, e.g. http://localhost:8191 (default "http://localhost:8191")
  -skip-commands string
    	comma-separated list of ignored commands
  -version
    	prints version
  -wait
    	wait readiness
  -wait-deadline duration
    	wait deadline duration (default 2m0s)

Environment Variables:
  CONFIG: process-wrapper configuration in JSON format
  POD_NAMESPACE: must be set in config to handle fast sidecar termination, and to enrich loki events by namespace label
  NODE_NAME: must be set in config to enrich events with labels send to metrics-server
  HANDLE_PREEMPTION: handle google cloud preemption events gracefully
  ENVOY_DRAIN_URL: make request to envoy admin url to [drain listener](https://www.envoyproxy.io/docs/envoy/latest/operations/admin#post--drain_listeners) connections, default is http://localhost:15000/drain_listeners
  METADATA_URL: google metadata url, default is http://metadata.google.internal/computeMetadata/v1/instance/preempted
  HANDLE_TERMINATION: enable status checking for other containers within the pod
  METRICS_SERVER_ADDRESS: metrics-server udp address to send metric events like preemption, restart, probe failure
  METRICS_JSON_EXTRA_LABELS: extra labels in JSON format, e.g {\"k8s_cluster\":\"test\"}
  METRICS_EXTRA_LABELS_FILE_PATH: extra pod labels mounted from kubernetes downwardAPI
  METRICS_EXTRA_LABELS_FILE_EXCLUDE: exclude some pod labels separated by comma, e.g. pod-template-hash,security.istio.io/tlsMode
  LOG_FORMAT: LOG_FORMAT=free, disable logfmt format; LOG_FORMAT=auto, autodetect severity level
  SHARED_LEASE_SOCKET: shared lock server socket, e.g. ':8191'
  SHARED_LEASE_ETCD_ENDPOINTS: etcd endpoints for shared-lease server, e.g. 'localhost:2379'
  AMQP_PROBE_RABBITMQ_DSN_TEMPLATE: amqp probe dsn template, e.g. 'amqp://%v:%v@localhost:5673'
  AMQP_PROBE_RABBITMQ_USER: amqp probe user 'guest'
  AMQP_PROBE_RABBITMQ_PASS: amqp probe password, e.g. 'guest'
```

### Health endpoints
```sh
curl -v http://localhost:8111/readiness
curl -v http://localhost:8111/liveness
curl -v http://localhost:8111/readiness?wait
curl -v http://localhost:8111/liveness?wait
```

### Metrics endpoint
http://localhost:8111/metrics


### Docker build
```sh
make docker-build-slim
```

### Build/Push
```sh
export VERSION=0.1.8
docker build -t process-wrapper:${VERSION} -f deployment/docker/Dockerfile .
docker push process-wrapper:${VERSION}
```

### Deploy process-wrapper inject example
```sh
helm template -f values.yaml --release-name process-wrapper base_new/ | kubectl apply -f-
```

## Configuration
### Environment variables
- **CONFIG** - process-wrapper configuration in JSON format
- **POD_NAMESPACE** - must be set in config to handle fast sidecar termination, and to enrich loki events by namespace label
``` 
      env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
```
- **NODE_NAME** - must be set in config to enrich events with labels send to metrics-server
```
      env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
```
- **HANDLE_PREEMPTION** - handle google cloud preemption events gracefully by using global health check which propagate traffic switch off to the pod
- **ENVOY_DRAIN_URL** - make request to envoy admin url to [drain listener](https://www.envoyproxy.io/docs/envoy/latest/operations/admin#post--drain_listeners) connections, default is http://localhost:15000/drain_listeners
- **METADATA_URL** - google metadata url, default is http://metadata.google.internal/computeMetadata/v1/instance/preempted, for local tests run:
```
go run cmd/metadata-mock/main.go &
NODE_NAME=gke HOSTNAME=local HANDLE_PREEMPTION=true METADATA_URL=http://localhost:8989/computeMetadata/v1/instance/preempted METRICS_SERVER_ADDRESS=localhost:9999 go run -race main.go
```
- **HANDLE_TERMINATION** - enable pod status check of other containers in the pod
- **METRICS_SERVER_ADDRESS** - metrics-server udp address to send metric events like preemption, restart, probe failure
- **METRICS_JSON_EXTRA_LABELS** - extra labels in JSON format, e.g {"k8s_cluster":"test"}
- **METRICS_EXTRA_LABELS_FILE_PATH** - extra pod labels mounted from kubernetes downwardAPI
- **METRICS_EXTRA_LABELS_FILE_EXCLUDE** - exclude some pod labels separated by comma, e.g pod-template-hash,security.istio.io/tlsMode
- **LOG_FORMAT** - LOG_FORMAT=free, disable logfmt format; LOG_FORMAT=auto, autodetect severity level
- **SHARED_LEASE_SOCKET** - shared lock server socket ':8191'
- **SHARED_LEASE_ETCD_ENDPOINTS** - etcd endpoints for shared-lease server, e.g 'localhost:2379'

### Config file format
```yaml
- name: "app-name"
  entrypoint: "sh"
  envs:
    LISTEN_ADDRESS: "0.0.0.0"
    LISTEN_PORT: "5673"
    AMQP_URL: "amqp://127.0.0.1"
  args:
    - -c
    - "exec /usr/bin/amqproxy -l $LISTEN_ADDRESS -p $LISTEN_PORT $AMQP_URL"
  readiness:
    tcp:
      socket: "127.0.0.1:5673"
```


### Documentation todo
- functionality
  - local run (prepare configs)
  - signals (SIGTERM, SIGQUIT, SIGKILL, SIGINT, SIGUSR), long termination example
  - health checks logic
  - postStart hooks
- observability
  - metrics (grafana dashboard)
  - events (metrics-server/opensearch)
  - loki/logfmt
- overhead/pyroscope
- deploy strategies
  - big container
  - init container
- configuration
  - env 
  - config.yaml
  - values.yaml
  - default values (periods, thresholds)



### AMQP probe test
```shell
rm -rf /tmp/amqp;
mkdir -p /tmp/amqp;
openssl rand -base64 32 > /tmp/amqp/.erlang.cookie;
chmod 0600 /tmp/amqp/.erlang.cookie;
docker run -it --rm --name rabbitmq -v /tmp/amqp/:/var/lib/rabbitmq/ -p 5672:5672 -p 15672:15672 rabbitmq:3-management;
```