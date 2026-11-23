#!/usr/bin/env bash
# End-to-end tests of process-wrapper in docker.
#
#   test/e2e/run.sh                 # all suites
#   test/e2e/run.sh basic signals   # selected suites
#   test/e2e/run.sh -l              # list suites
#
# Env: E2E_KEEP=1 keeps containers after the run, E2E_LOGDIR overrides the
# logs directory (default test/e2e/.logs), E2E_NO_BUILD=1 reuses the images.
#
# Every container shares the network namespace of pw-netns, so "localhost"
# reaches mysql/rabbitmq and the fake services exactly like on a
# developer machine. Wrappers run like in production: read-only root FS,
# non-root user, no capabilities.
# Checks are passed to eval in single quotes on purpose
# shellcheck disable=SC2016
set -u

REPO=$(cd "$(dirname "$0")/../.." && pwd)
E2E=$REPO/test/e2e
IMG=process-wrapper:e2e
TIMG=process-wrapper:e2e-tools
LOGDIR=${E2E_LOGDIR:-$E2E/.logs}
SUITES=(image basic probes hooks logs envs floating heartbeat prommerge termination signals
  memory flags events credential hardening lifecycle sql amqp lock)
PASS=0 FAIL=0 FAILED=()

# ---------------------------------------------------------------- helpers

ok()  { echo "  PASS: $*"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $*"; FAIL=$((FAIL + 1)); FAILED+=("$SUITE: $*"); }
# check <desc> <command...>
check() { local desc=$1; shift; if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi; }
# waitcheck <desc> <seconds> <command...>: retry the command every second
waitcheck() {
  local desc=$1 secs=$2 i; shift 2
  for ((i = 0; i < secs; i++)); do
    "$@" >/dev/null 2>&1 && { ok "$desc"; return; }
    sleep 1
  done
  bad "$desc"
}

# Helpers act on the current container $CUR
x()       { docker exec "$CUR" "$@"; }
get()     { x curl -s "$@"; }
# code <expected> <curl args...>: HTTP status code of a request
code()    { local want=$1; shift; [ "$(x curl -s -o /dev/null -w '%{http_code}' "$@")" = "$want" ]; }
ready()   { code 200 "localhost:${PORT:-8111}/readiness"; }
unready() { code 503 "localhost:${PORT:-8111}/readiness"; }
logs()    { docker logs "$CUR" 2>&1; }
has_log() { logs | grep -q -- "$1"; }
no_log()  { ! logs | grep -q -- "$1"; }
count_log() { logs | grep -c -- "$1"; }
running() { [ "$(docker inspect -f '{{.State.Running}}' "$CUR" 2>/dev/null)" = true ]; }
stopped() { ! running; }
exit_code() { [ "$(docker inspect -f '{{.State.ExitCode}}' "$CUR")" = "$1" ]; }
metric()  { get "localhost:${PORT:-8111}/metrics" | grep -q -- "$1"; }
file_is() { [ "$(x cat "$1" 2>/dev/null)" = "$2" ]; }

# start <name> <config|-> [docker args...] [-- wrapper args...]
# Starts a hardened wrapper container, <config> is relative to the repo root
start() {
  CUR=pw-$1; local cfg=$2 dargs=() wargs=(); shift 2
  while [ $# -gt 0 ]; do
    [ "$1" = "--" ] && { shift; wargs=("$@"); break; }
    dargs+=("$1"); shift
  done
  [ "$cfg" != - ] && wargs=(-config "/repo/$cfg" ${wargs[@]+"${wargs[@]}"})
  docker rm -f "$CUR" >/dev/null 2>&1
  docker run -d --name "$CUR" --network container:pw-netns \
    --read-only --tmpfs /tmp:rw,mode=1777 \
    --security-opt no-new-privileges --cap-drop ALL \
    -v "$REPO/examples:/repo/examples:ro" -v "$E2E/configs:/repo/test/e2e/configs:ro" -v "$E2E/configs:/e2e:ro" \
    ${dargs[@]+"${dargs[@]}"} \
    $TIMG process-wrapper ${wargs[@]+"${wargs[@]}"} >/dev/null
}
# finish [container]: save logs and remove the container
finish() {
  local c=${1:-$CUR}
  docker logs "$c" >"$LOGDIR/$c.log" 2>&1
  [ -n "${E2E_KEEP:-}" ] || docker rm -f "$c" >/dev/null 2>&1
}
# stop_with <signal>: send a signal to the wrapper, return the seconds until it exited
stop_with() {
  local t0=$SECONDS
  docker kill -s "$1" "$CUR" >/dev/null
  for ((i = 0; i < 40; i++)); do stopped && break; sleep 0.5; done
  echo $((SECONDS - t0))
}

cleanup() { docker ps -aq --filter name='^pw-' | xargs docker rm -f >/dev/null 2>&1; }
# save logs of the dependencies, then remove all containers
teardown() {
  local c
  for c in pw-mysql pw-rabbitmq; do
    docker inspect "$c" >/dev/null 2>&1 && docker logs "$c" >"$LOGDIR/$c.log" 2>&1
  done
  [ -n "${E2E_KEEP:-}" ] || cleanup
}

# need <dependency>: start a dependency once
STARTED=" "
need() {
  case $STARTED in *" $1 "*) return ;; esac
  STARTED="$STARTED$1 "
  case $1 in
    mysql)
      docker run -d --name pw-mysql --network container:pw-netns -e MYSQL_ROOT_PASSWORD=secret mysql:8.4 >/dev/null
      waitcheck "mysql is up" 120 docker exec pw-mysql mysqladmin -uroot -psecret ping --silent ;;
    rabbitmq)
      docker run -d --name pw-rabbitmq --network container:pw-netns rabbitmq:4-management >/dev/null
      waitcheck "rabbitmq is up" 120 docker exec -u rabbitmq pw-rabbitmq rabbitmq-diagnostics -q check_port_connectivity ;;
    fake)
      docker run -d --name pw-fake --network container:pw-netns $TIMG e2e-fake >/dev/null
      waitcheck "fake services are up" 15 docker exec pw-fake curl -sf localhost:18080/drains ;;
  esac
}

# ---------------------------------------------------------------- suites

suite_image() {
  check "image runs as uid 1010" eval '[ "$(docker run --rm $TIMG id -u)" = 1010 ]'
  check "binary is root-owned and read-only" eval \
    'docker run --rm $TIMG stat -c "%u %a" /usr/bin/process-wrapper | grep -qx "0 555"'
  check "-version" docker run --rm $IMG process-wrapper -version
  check "-help lists env variables" eval 'docker run --rm $IMG process-wrapper -h 2>&1 | grep -q HEALTH_SOCKET'
}

suite_basic() {
  start basic examples/basic.yaml
  waitcheck "ready" 20 ready
  check "readiness file created" x test -f /tmp/started
  check "/liveness" code 200 localhost:8111/liveness
  check "/readiness?wait" code 200 -m 5 'localhost:8111/readiness?wait'
  check "/liveness?wait" code 200 -m 5 'localhost:8111/liveness?wait'
  check "/status" eval 'get localhost:8111/status | grep -q "\"worker-1\":true"'
  check "/pidof" eval "get -f 'localhost:8111/pidof?name=worker-2' | grep -qE '^[0-9]+$'"
  check "/pidof unknown -> 404" code 404 'localhost:8111/pidof?name=nope'
  check "/pidof without name -> 400" code 400 localhost:8111/pidof
  check "/pids json" eval 'get -i localhost:8111/pids | grep -qi "content-type: application/json"'
  check "/heartbeat lists all processes" code 200 localhost:8111/heartbeat
  check "PATCH /heartbeat without name -> 400" code 400 -X PATCH localhost:8111/heartbeat
  local d
  for d in /debug/vars /debug/requests /debug/events /debug/pprof/; do
    check "$d not exposed" code 404 "localhost:8111$d"
  done
  check "/metrics" metric sidecar_cmd_readiness_count
  check "metrics cmd label is the entrypoint only" eval '! get localhost:8111/metrics | grep sidecar_cmd_readiness_count | grep -q "cmd=\"sh -c"'
  check "memory usage metric" metric 'sidecar_cmd_memory_usage_bytes{process_name="worker-2"}'
  check "cpu usage metric" metric 'sidecar_cmd_cpu_usage_seconds{process_name="worker-2"}'
  check "wrapper stats metric" metric 'sidecar_cmd_memory_usage_bytes{process_name="process-wrapper"}'
  x rm -f /tmp/worker-1.ready
  waitcheck "not ready after the probe fails" 10 unready
  check "liveness passes while a process is not ready" code 200 localhost:8111/liveness
  check "/status reports the failing process" eval 'get localhost:8111/status | grep -q "\"worker-1\":false"'
  waitcheck "restart after failureThreshold" 25 has_log 'Restart command'
  check "SIGTERM before SIGKILL on restart" has_log 'Send SIGTERM'
  waitcheck "ready again after restart" 20 ready
  check "duration metric with exit code" metric 'sidecar_cmd_duration_seconds{exit_code='
  stop_with TERM >/dev/null
  check "graceful stop" stopped
  check "exit code 0" exit_code 0
  finish
}

suite_probes() {
  start probes examples/probes.yaml -e PROBE_ENV=enabled
  waitcheck "http+tcp+exec+env probes pass" 25 ready
  finish
  start probes-env examples/probes.yaml
  sleep 8
  check "env probe fails without PROBE_ENV" unready
  finish
  start probes-fail test/e2e/configs/probes-fail.yaml -e PROBE_ENV=disabled
  sleep 6
  local p
  for p in http-fail http-500 tcp-fail file-fail env-fail exec-fail; do
    check "$p probe fails" eval "get localhost:8111/status | grep -q '\"$p\":false'"
  done
  check "exec probe runs in the process dir" eval "get localhost:8111/status | grep -q '\"exec-dir\":true'"
  check "failed probes don't restart below failureThreshold" no_log 'Restart command'
  finish
  start dns test/e2e/configs/dns.yaml
  waitcheck "dns probe passes" 10 eval "get localhost:8111/status | grep -q '\"dns\":true'"
  check "dns probe fails for an unresolvable name" eval "get localhost:8111/status | grep -q '\"dns-fail\":false'"
  finish
}

suite_hooks() {
  start hook examples/post-start-hook.yaml
  waitcheck "ready" 20 ready
  sleep 4
  check "hook ran exactly once" file_is /tmp/post-start-hook.log "hello from postStart"
  finish
  start hooks test/e2e/configs/hooks.yaml
  waitcheck "hook gets process, postStart and command envs and the process dir" 15 \
    file_is /tmp/hook-envs.out "process hook cmd hook /e2e"
  check "hook-envs ready" eval "get localhost:8111/status | grep -q '\"hook-envs\":true'"
  waitcheck "hook over the deadline fails" 15 eval "logs | grep 'name=hook-deadline' | grep -q 'hook executed with an error'"
  waitcheck "process restarted after a hook failure" 20 eval "[ \$(x cat /tmp/hook-fail.log | wc -l) -ge 2 ]"
  check "process with a failing hook is not ready" eval "get localhost:8111/status | grep -q '\"hook-fail\":false'"
  finish
}

suite_logs() {
  start logs examples/log-severity.yaml
  sleep 3
  check "logfmt level=error" has_log 'level=error msg="logfmt error"'
  check "logfmt level=warning" has_log 'level=warning msg="logfmt warning"'
  check "auto: CRITICAL -> error" eval "logs | grep severity-auto | grep -q 'level=error msg=\"CRITICAL'"
  check "auto: WARNING -> warning" eval "logs | grep severity-auto | grep -q 'level=warning msg=\"WARNING'"
  check "auto: other lines -> info" eval "logs | grep severity-auto | grep -q 'level=info msg=\"INFO'"
  check "free: printed as is" eval "logs | grep -qx 'printed as is'"
  finish
  start log-env test/e2e/configs/log-env.yaml -e LOG_FORMAT=free
  sleep 3
  check "LOG_FORMAT env applies to all processes" eval "logs | grep -qx 'raw line from LOG_FORMAT'"
  finish
  start log-filter test/e2e/configs/logs-filter.yaml -e LOG_LEVELS=warning,error
  sleep 3
  check "logs: false drops the output" no_log 'msg="hidden-line'
  check "logLevels keeps allowed levels" has_log 'msg=cmd-error'
  check "logLevels overrides LOG_LEVELS" no_log 'msg=cmd-info'
  check "LOG_LEVELS keeps allowed levels" has_log 'msg=global-warning'
  check "LOG_LEVELS drops other levels" eval "no_log msg=global-info && no_log msg=global-debug"
  check "LOG_LEVELS filters auto-detected levels" eval "has_log 'msg=\"WARNING auto-warning line' && no_log 'msg=\"auto-info line'"
  finish
}

suite_envs() {
  # envsFile is relative to the repo root; the pid file can't be written there (read-only): a warning only
  start envs examples/envs-file.yaml -w /repo -e E2E_INHERITED=yes
  sleep 3
  check "envs + envsFile precedence" has_log 'FROM_CONFIG=config FROM_FILE=file OVERRIDDEN=file'
  check "wrapper env is inherited" eval "x sh -c 'tr \"\\000\" \"\\n\" < /proc/\$(pidof sleep)/environ' | grep -q '^E2E_INHERITED=yes'"
  finish
  start dir test/e2e/configs/dir.yaml
  sleep 3
  check "process runs in dir" has_log 'PWD_IS=/e2e'
  finish
}

suite_floating() {
  start floating examples/floating-pid.yaml
  sleep 15
  check "crash loop -> not ready" unready
  check "process restarted several times" eval '[ $(count_log "Restart command") -ge 3 ]'
  check "duration metric with exit code 1" metric 'exit_code="1"'
  finish
}

suite_heartbeat() {
  start heartbeat examples/heartbeat.yaml
  waitcheck "ready with heartbeats" 25 ready
  check "GET /heartbeat?name" eval "get -f 'localhost:8111/heartbeat?name=heart' | grep -q heart"
  check "PATCH unknown -> 404" code 404 -X PATCH 'localhost:8111/heartbeat?name=nope'
  check "POST -> 405" code 405 -X POST 'localhost:8111/heartbeat?name=heart'
  check "heartbeat period metric" metric 'sidecar_cmd_heartbeat_period_seconds{process_name="heart"}'
  finish
  start heartbeat-manual test/e2e/configs/heartbeat-manual.yaml
  sleep 3
  check "not ready without heartbeats" unready
  check "PATCH -> 202" code 202 -X PATCH 'localhost:8111/heartbeat?name=manual'
  waitcheck "ready after a heartbeat" 5 ready
  waitcheck "not ready after heartBeat.period without heartbeats" 10 unready
  finish
}

suite_prommerge() {
  start prommerge examples/prommerge.yaml
  waitcheck "ready" 20 ready
  check "both targets merged" eval 'out=$(get localhost:8111/prommerge); echo "$out" | grep -q "process_name=\"app-1\"" && echo "$out" | grep -q "process_name=\"app-2\""'
  check "valid exposition (one TYPE per family)" eval '[ $(get localhost:8111/prommerge | grep -c "^# TYPE go_goroutines ") = 1 ]'
  check "HELP kept by default" eval 'get localhost:8111/prommerge | grep -q "^# HELP"'
  check "merge duration metric" metric sidecar_metrics_merge_duration_seconds_count
  finish

  start prommerge-meta examples/prommerge.yaml -- -prommerge-omit-meta -prommerge-sort=false -prommerge-async=false
  waitcheck "ready" 20 ready
  check "-prommerge-omit-meta drops HELP" eval '! get localhost:8111/prommerge | grep -q "^# HELP"'
  check "TYPE kept" eval 'get localhost:8111/prommerge | grep -q "^# TYPE"'
  check "-prommerge-sort=false -prommerge-async=false merge both targets" eval 'out=$(get localhost:8111/prommerge); echo "$out" | grep -q "process_name=\"app-1\"" && echo "$out" | grep -q "process_name=\"app-2\""'
  finish

  start prommerge-fail test/e2e/configs/prommerge-fail.yaml
  sleep 2
  check "failed target skipped" eval 'out=$(get localhost:8111/prommerge); echo "$out" | grep -q "process_name=\"app-ok\"" && ! echo "$out" | grep -q "process_name=\"app-down\""'
  check "failed target logged" has_log 'Prommerge target failed'
  finish
  start prommerge-empty test/e2e/configs/prommerge-fail.yaml -- -prommerge-empty-on-failure -prommerge-suppress-errors
  sleep 2
  check "-prommerge-empty-on-failure returns no metrics" eval '! get localhost:8111/prommerge | grep -q process_name'
  check "-prommerge-suppress-errors hides target errors" no_log 'Prommerge target failed'
  finish
  start prommerge-legacy examples/prommerge.yaml -- -prommerge-supress-errors
  sleep 2
  check "deprecated -prommerge-supress-errors accepted with a warning" eval 'has_log "is deprecated" && running'
  finish
}

suite_termination() {
  start term examples/graceful-termination.yaml
  waitcheck "3 replicas running" 15 eval '[ $(get localhost:8111/status | grep -o "term-delay_" | wc -l) = 3 ]'
  check "replicas named name_N" eval 'get localhost:8111/status | grep -q "\"term-delay_2\""'
  docker kill -s TERM "$CUR" >/dev/null
  waitcheck "readiness file removed on shutdown" 3 eval '! x test -f /tmp/started'
  check "readiness 503 during shutdown" unready
  check "liveness keeps passing during shutdown" code 200 localhost:8111/liveness
  waitcheck "stopped after replicas finished cleanup" 25 stopped
  check "replicas received SIGTERM" eval '[ $(count_log "msg=\"SIGTERM received") = 3 ]'
  check "replicas finished cleanup" eval '[ $(count_log "msg=\"done") = 3 ]'
  finish
  start replicas test/e2e/configs/replicas.yaml
  waitcheck "postStart hook runs for every replica" 15 eval '[ "$(x cat /tmp/replica-hooks | wc -l)" -eq 2 ]'
  check "replicas ready" eval 'get localhost:8111/status | grep -q "\"rep_0\":true" && get localhost:8111/status | grep -q "\"rep_1\":true"'
  check "promTarget of replicas merged once" eval '[ $(get localhost:8111/prommerge | grep -c "^go_goroutines{.*process_name=\"rep\"") = 1 ]'
  finish
}

suite_signals() {
  local secs
  start sig-usr test/e2e/configs/signals.yaml
  sleep 2
  docker kill -s USR1 "$CUR" >/dev/null
  waitcheck "SIGUSR1 prints the state" 5 has_log 'print processes state'
  check "SIGUSR1 doesn't stop the wrapper" running
  finish

  start sig-term test/e2e/configs/signals.yaml -- -graceful-shutdown-deadline 3s
  sleep 2
  secs=$(stop_with TERM)
  check "SIGTERM: graceful process finishes cleanup" has_log 'msg="bye'
  check "SIGTERM: stubborn process killed after -graceful-shutdown-deadline ($secs s)" \
    eval "has_log 'Force kill all hanged processes' && [ $secs -ge 3 ] && [ $secs -le 9 ]"
  finish

  start sig-quit test/e2e/configs/signals.yaml -- -graceful-shutdown-deadline 3s
  sleep 2
  stop_with QUIT >/dev/null
  check "SIGQUIT: graceful shutdown" eval "has_log 'Received quit, graceful shutdown' && has_log 'msg=\"bye'"
  finish

  start sig-int test/e2e/configs/signals.yaml
  sleep 2
  secs=$(stop_with INT)
  check "SIGINT: fast shutdown ($secs s)" eval "has_log 'fast shutdown' && [ $secs -le 3 ]"
  check "SIGINT: processes killed without cleanup" no_log 'msg="bye'
  finish
}

suite_memory() {
  start oom examples/memory-limit.yaml
  waitcheck "process killed over memoryLimitMB" 40 has_log 'Out of memory'
  waitcheck "and restarted" 10 has_log 'Restart command'
  finish
}

suite_flags() {
  start json - -e CONFIG='[{"name":"j","entrypoint":"sleep","args":["3600"],"readiness":{"delay":"1s","period":"1s","exec":{"command":{"entrypoint":"true"}}}}]'
  waitcheck "CONFIG env (json)" 15 ready
  check "-wait returns when ready" x process-wrapper -wait -wait-deadline 5s
  finish

  start not-ready test/e2e/configs/heartbeat-manual.yaml
  sleep 1
  check "-wait fails after -wait-deadline" eval '! x process-wrapper -wait -wait-deadline 2s'
  finish

  start readiness-file test/e2e/configs/two.yaml -- -readiness-file /tmp/custom.ready
  waitcheck "-readiness-file created" 10 x test -f /tmp/custom.ready
  check "default readiness file not created" eval '! x test -f /tmp/started'
  check "-wait -readiness-file" x process-wrapper -wait -readiness-file /tmp/custom.ready -wait-deadline 5s
  finish

  start skip test/e2e/configs/two.yaml -- -skip-commands b
  waitcheck "-skip-commands" 10 eval 'get localhost:8111/status | grep -q "\"a\":true" && ! get localhost:8111/status | grep -q "\"b\""'
  check "skipped command logged" has_log 'Skip b command'
  finish

  start socket test/e2e/configs/two.yaml -e HEALTH_SOCKET=:8122
  waitcheck "HEALTH_SOCKET" 10 code 200 localhost:8122/readiness
  finish

  start debug test/e2e/configs/two.yaml -- -debug
  sleep 2
  check "-debug logs the config" eval 'has_log "level=debug" && has_log "CONFIG="'
  finish

  start pyroscope test/e2e/configs/two.yaml -- -pyroscope-url http://127.0.0.1:1
  waitcheck "-pyroscope-url with an unreachable server doesn't break the wrapper" 10 eval 'ready && running'
  finish

  start phpscope test/e2e/configs/two.yaml -- -php-pyroscope-url http://127.0.0.1:1 -php-app-name e2e.php
  waitcheck "-php-pyroscope-url without the phpscope binary only logs an error" 10 \
    eval 'ready && running && has_log "phpscope binary is missing"'
  finish

  start pid-file test/e2e/configs/two.yaml --init -- -pid-file /tmp/pw.pid
  waitcheck "-pid-file written when not PID 1 (--init)" 10 eval '[ "$(x cat /tmp/pw.pid)" -gt 1 ]'
  check "signals forwarded by --init" eval "[ \$(stop_with TERM) -le 5 ] && has_log 'Received terminated'"
  finish

  start invalid test/e2e/configs/invalid.yaml
  waitcheck "invalid config: exits with an error" 10 eval 'stopped && ! exit_code 0 && has_log "Unmarshal yaml config"'
  finish
}

suite_events() {
  need fake
  start events test/e2e/configs/events.yaml \
    -e HOSTNAME=pod-e2e -e POD_NAMESPACE=ns-e2e -e NODE_NAME=node-e2e \
    -e METRICS_JSON_EXTRA_LABELS='{"k8s_cluster":"e2e"}' \
    -e METRICS_EXTRA_LABELS_FILE_PATH=/repo/test/e2e/configs/labels -e METRICS_EXTRA_LABELS_FILE_EXCLUDE=pod-template-hash \
    -e HANDLE_PREEMPTION=true -e METADATA_URL=http://127.0.0.1:18080/preempted \
    -e ENVOY_DRAIN_URL=http://127.0.0.1:18080/drain -e VMAGENT_PUSH_URL=http://127.0.0.1:18080/api/v1/import/prometheus \
    -- -envoy-drain
  # events are JSON lines on stdout
  # shellcheck disable=SC2329 # called through eval
  events() { logs | grep '^{"_emitter":"process-wrapper"'; }
  # shellcheck disable=SC2329 # called through eval
  vmagent() { docker exec pw-fake curl -s localhost:18080/vmagent; }
  waitcheck "restart event" 20 eval 'events | grep "\"name\":\"restart\"" | grep -q "\"process_name\":\"evt\""'
  check "readiness failure event" eval 'events | grep -q "\"name\":\"readiness\""'
  check "env labels (HOSTNAME, POD_NAMESPACE, NODE_NAME)" eval 'events | grep "\"name\":\"restart\"" | grep "\"pod\":\"pod-e2e\"" | grep "\"namespace\":\"ns-e2e\"" | grep -q "\"node\":\"node-e2e\""'
  check "METRICS_JSON_EXTRA_LABELS" eval 'events | grep "\"name\":\"restart\"" | grep -q "\"k8s_cluster\":\"e2e\""'
  check "METRICS_EXTRA_LABELS_FILE_PATH (dashes replaced)" eval 'events | grep "\"name\":\"restart\"" | grep "\"app_name\":\"web\"" | grep -q "\"team\":\"core\""'
  check "METRICS_EXTRA_LABELS_FILE_EXCLUDE" eval '! events | grep -q pod_template_hash'
  waitcheck "initial preempted 0 pushed to VMAgent" 10 eval 'vmagent | grep "^preempted{" | grep "pod=\"pod-e2e\"" | grep -q " 0$"'
  x touch /tmp/evt.ready
  waitcheck "ready before preemption" 10 ready
  docker exec pw-fake curl -s -X POST localhost:18080/preempt >/dev/null
  waitcheck "HANDLE_PREEMPTION: not ready after preemption" 10 unready
  check "liveness keeps passing after preemption" code 200 localhost:8111/liveness
  check "preemption logged" has_log 'Node is preempted'
  waitcheck "-envoy-drain calls ENVOY_DRAIN_URL" 10 eval '[ "$(docker exec pw-fake curl -s localhost:18080/drains)" -ge 1 ]'
  check "preempted 1 pushed to VMAgent" eval 'vmagent | grep "^preempted{" | grep -q " 1$"'
  stop_with TERM >/dev/null
  check "termination event" eval 'events | grep "\"name\":\"termination\"" | grep -q "\"signal\":\"termination\""'
  finish
  finish pw-fake
  STARTED=${STARTED/ fake / }
}

suite_credential() {
  # the wrapper as root (with the capabilities to switch users and signal them)
  start cred test/e2e/configs/credential.yaml --user 0 --cap-add SETUID --cap-add SETGID --cap-add KILL
  waitcheck "ready" 15 ready
  check "process runs with sysProcAttr.credential" file_is /tmp/cred.proc 1010
  check "exec probe runs with the process credential" file_is /tmp/cred.probe 1010
  check "postStart hook runs with the process credential" file_is /tmp/cred.hook 1010
  check "wrapper itself runs as root" eval '[ "$(x sh -c "stat -c %u /proc/1")" = 0 ]'
  stop_with TERM >/dev/null
  check "root wrapper stops the process" exit_code 0
  finish
}

suite_hardening() {
  start hard test/e2e/configs/hardening.yaml
  sleep 6
  check "wrapper survives a command that can't start" running
  check "start failure retried with backoff" eval '[ $(count_log "Retry command start") -ge 2 ]'
  check "broken command not ready, healthy one runs" eval 'get localhost:8111/status | grep -q "\"broken\":false" && get -f "localhost:8111/pidof?name=healthy"'
  check "start_failure metric" metric 'status="start_failure"'
  check "long line logged in 64KiB parts" eval 'logs | grep name=jobs | grep -q xxxxxxxx && ! logs | awk "length > 70000" | grep -q .'
  x touch /tmp/jobs.ok
  waitcheck "flooding postStart hook killed" 20 has_log 'hook was killed, output exceeds'
  local old
  old=$(x cat /tmp/jobs.child)
  waitcheck "command restarted after the hook failure" 30 eval "[ \"\$(x cat /tmp/jobs.child)\" != $old ]"
  check "background job of the old run killed" x sh -c "! kill -0 $old 2>/dev/null || grep -q '^State:.*Z' /proc/$old/status"
  finish

  # with --init the killed jobs are reaped
  start hard-init test/e2e/configs/hardening.yaml --init
  x touch /tmp/jobs.ok
  waitcheck "flooding postStart hook killed" 25 has_log 'hook was killed, output exceeds'
  sleep 3
  check "--init: no zombies left" eval '! x sh -c "grep -l \"^State:.*Z\" /proc/[0-9]*/status" | grep -q .'
  finish
}

suite_lifecycle() {
  local secs
  start job test/e2e/configs/job.yaml
  waitcheck "isJob: completed job logged" 10 has_log 'Job completed'
  sleep 3
  check "isJob: completed job not restarted" eval '[ $(count_log "msg=\"migrated") = 1 ]'
  check "isJob: wrapper keeps running" eval 'running && get -f "localhost:8111/pidof?name=app"'
  finish

  start job-only test/e2e/configs/job-only.yaml
  waitcheck "only a job: completed" 10 has_log 'Job completed'
  sleep 2
  check "only a job: wrapper keeps running" eval 'running && has_log "waiting for a signal"'
  check "only a job: ready" code 200 localhost:8111/readiness
  docker kill -s TERM "$CUR" >/dev/null
  waitcheck "only a job: SIGTERM stops the wrapper" 10 stopped
  check "only a job: exit code 0" exit_code 0
  finish

  start job-fail test/e2e/configs/job-fail.yaml
  waitcheck "isJob: failed job stops the wrapper" 15 stopped
  check "isJob: exit code 1" exit_code 1
  check "isJob: failure logged" has_log 'Job failed'
  finish

  start never test/e2e/configs/restart-never.yaml
  waitcheck "restartPolicy: never logged" 10 has_log "Restart policy is 'never'"
  sleep 3
  check "restartPolicy: never not restarted" eval '[ $(count_log "msg=\"once-ran") = 1 ] && running'
  finish

  start backoff test/e2e/configs/backoff.yaml
  waitcheck "restartBackoff grows exponentially up to maxDelay" 15 eval \
    'has_log "Restart command in 200ms (failures=1)" && has_log "Restart command in 400ms (failures=2)" &&
     has_log "Restart command in 800ms (failures=3)" && has_log "Restart command in 1s (failures=4)"'
  check "restart back-off metric" metric 'sidecar_cmd_restart_backoff_seconds{process_name="crash"'
  secs=$(stop_with TERM)
  check "SIGTERM interrupts the back-off ($secs s)" eval "[ $secs -le 3 ] && exit_code 0"
  finish
}

suite_sql() {
  need mysql
  start sql examples/sql-probe.yaml
  waitcheck "sql probe passes" 30 ready
  check "DSN password not logged" no_log 'root:secret'
  docker exec pw-mysql mysql -uroot -psecret -e "SET GLOBAL super_read_only=1" 2>/dev/null
  waitcheck "fails when the query result doesn't contain the value" 20 unready
  docker exec pw-mysql mysql -uroot -psecret -e "SET GLOBAL super_read_only=0; SET GLOBAL read_only=0" 2>/dev/null
  waitcheck "passes again" 20 ready
  finish
  start sql-hook test/e2e/configs/sql-hook.yaml
  waitcheck "sql postStart hook" 20 eval 'docker exec pw-mysql mysql -uroot -psecret -N -e "SHOW DATABASES LIKE \"e2e_hook\"" 2>/dev/null | grep -q e2e_hook'
  waitcheck "ready after the sql hook" 10 ready
  finish
}

suite_amqp() {
  need rabbitmq
  start amqp examples/amqp.yaml
  waitcheck "amqp probe passes" 30 ready
  check "amqp password not logged" no_log 'guest:guest'
  finish
  local tpl='amqp://%v:%v@localhost:5672/'
  start amqp-env test/e2e/configs/amqp-env.yaml -e AMQP_PROBE_RABBITMQ_DSN_TEMPLATE=$tpl \
    -e AMQP_PROBE_RABBITMQ_USER=guest -e AMQP_PROBE_RABBITMQ_PASS=guest
  waitcheck "AMQP_PROBE_RABBITMQ_* from the wrapper env" 20 ready
  finish
  start amqp-badpass test/e2e/configs/amqp-env.yaml -e AMQP_PROBE_RABBITMQ_DSN_TEMPLATE=$tpl \
    -e AMQP_PROBE_RABBITMQ_USER=guest -e AMQP_PROBE_RABBITMQ_PASS=wrong
  sleep 6
  check "wrong password fails" unready
  finish
  start amqp-reuse test/e2e/configs/amqp-reuse.yaml
  waitcheck "reuseConn: ready" 20 ready
  check "reuseConn: one connection kept" eval '[ "$(docker exec -u rabbitmq pw-rabbitmq rabbitmqctl -q list_connections | grep -c .)" -ge 1 ]'
  docker exec -u rabbitmq pw-rabbitmq rabbitmqctl -q close_all_connections e2e >/dev/null
  waitcheck "reuseConn: closed connection detected" 10 has_log 'reset amqp probe connection'
  waitcheck "reuseConn: reconnects" 10 ready
  finish
}

suite_lock() {
  # the fake shared lock server (API of github.com/tentens-tech/shared-lock) listens
  # on the default -shared-lock-url http://localhost:8191 and on :18080
  need fake
  start lock-a examples/shared-lock.yaml; local a=$CUR
  waitcheck "first instance acquires the lock and is ready" 30 ready
  check "acquire lock metric" metric 'sidecar_cmd_acquire_lock_total'
  start lock-b examples/shared-lock.yaml -e HEALTH_SOCKET=:8112; local b=$CUR
  sleep 8
  # a standby instance is excluded from readiness (pod stays Ready), but its process must not run
  check "second instance waits for the lock" eval 'has_log "Wait a lock" && code 404 "localhost:8112/pidof?name=singleton"'
  check "standby instance stays ready" code 200 localhost:8112/readiness
  docker kill -s TERM "$a" >/dev/null
  waitcheck "second instance takes over" 40 eval 'get -f "localhost:8112/pidof?name=singleton"'
  finish "$a"; finish "$b"
  start lockprom examples/prommerge-shared-lock.yaml
  waitcheck "prommerge + lock" 30 eval 'get localhost:8111/prommerge | grep -q "process_name=\"singleton\""'
  finish
  start lock-url test/e2e/configs/lock-url.yaml -- -shared-lock-url http://127.0.0.1:18080
  waitcheck "-shared-lock-url" 30 ready
  finish
  finish pw-fake
  STARTED=${STARTED/ fake / }
}

# ---------------------------------------------------------------- main

if [ "${1:-}" = -l ]; then printf '%s\n' "${SUITES[@]}"; exit 0; fi
selected=("$@"); [ $# -eq 0 ] && selected=("${SUITES[@]}")
for s in "${selected[@]}"; do
  declare -F "suite_$s" >/dev/null || { echo "unknown suite: $s (see -l)"; exit 2; }
done

rm -rf "$LOGDIR"; mkdir -p "$LOGDIR"
cleanup
trap teardown EXIT

SUITE=build
echo "### build"
if [ -z "${E2E_NO_BUILD:-}" ]; then
  docker build -q -t $IMG -f "$REPO/deployment/docker/Dockerfile" "$REPO" >/dev/null || { echo "image build failed"; exit 1; }
  docker build -q -t $TIMG --build-arg IMAGE=$IMG -f "$E2E/tools.Dockerfile" "$E2E" >/dev/null || { echo "tools image build failed"; exit 1; }
fi
docker run -d --name pw-netns alpine:3.24 sleep 86400 >/dev/null

for SUITE in "${selected[@]}"; do
  echo "### $SUITE"
  t0=$SECONDS
  "suite_$SUITE"
  echo "  ($((SECONDS - t0))s)"
done

SUITE=logs
check "no panics in any log" eval "! grep -l 'panic\|fatal error' $LOGDIR/*.log"

echo
echo "RESULT: $PASS passed, $FAIL failed (logs: $LOGDIR)"
for f in ${FAILED[@]+"${FAILED[@]}"}; do echo "  - $f"; done
[ $FAIL = 0 ]
