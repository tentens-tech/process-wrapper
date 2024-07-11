package config

import (
	"context"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	"process-wrapper/pkg/process"
	"process-wrapper/pkg/signals"
	"sync"
	"syscall"
	"testing"
	"time"
)

const ()

/*
### Test cases
- Probes
  - MySQL probe
  - TCP probe
  - Exec probe
  - File probe
  - FloatingPid/CrashLoopBackoff
  - Env probe
  - PostStart Hook
- Signals
- Metrics
  - MetricsServer
- Replicas

*/

func GetTestCases() map[string]string {
	var testCase = map[string]string{
		"Envs": `
- name: "envs"
  entrypoint: "/bin/sleep"
  args: ["1"]
  envs:
    SERVER: "1"
  envsFile: "../../vault"
`,

		"EnvsFile:": ``,

		"MysqlProbe": `
- name: "http-8122"
  entrypoint: "./failure-http"
  envs:
    MYSQL_DSN: "admin:admin@tcp(127.0.0.1:6032)/stats"
    LISTEN_SOCKET: ":8122"
    FAILURE_RATIO: "0"
  readiness:
    delay: "5s"
    failureThreshold: 1
    tcp:
      socket: ":8122"
    sql:
      queries:
        - "SELECT hg.comment FROM runtime_mysql_servers ms inner join runtime_mysql_replication_hostgroups hg on ms.hostgroup_id=hg.writer_hostgroup WHERE ms.status = 'ONLINE'"
        - "SELECT hg.comment FROM runtime_mysql_servers ms inner join runtime_mysql_replication_hostgroups hg on ms.hostgroup_id=hg.reader_hostgroup WHERE ms.status = 'ONLINE'"
      contains:
        - "monolith"
        - "payments"
`,
		"TCPProbe": ``,

		"ExecProbe": ``,

		"FileProbe": ``,

		"FloatingPid": `
- name: "restarts"
  entrypoint: "sh"
  args:
    - "-c"
    - "go run cmd/http/main.go"
  readiness:
    #delay: "5s"
    #period: "5s"
    #failureThreshold: 3
    floatingPid: {}
      #period: "10s"
      #threshold: 1 # must be <10
`,
		"EnvProbe": ``,

		"PostStartHook": ``,

		"Signals": ``,

		"MetricsServer": ``,

		"Raplicas": ``,

		"MemoryLimit": ``,
	}
	return testCase
}

func TestHelloAll(t *testing.T) {
	var err error
	for tCase, config := range GetTestCases() {
		t.Run(tCase, func(t *testing.T) {
			var commands []process.Command
			if tCase == "Envs" {
				t.Logf("%+v", config)
				err = yaml.Unmarshal([]byte(config), &commands)
				if err != nil {
					t.Errorf("Failed to parse test case, %v", err)
				}
				t.Logf("%+v", commands)
				wg := sync.WaitGroup{}
				var gracefulShutdownDeadline = time.Second * 5
				ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownDeadline)
				//process.NewSchedule(ctx, commands, nil, process.NewState(""))

				s := process.NewState("./test_"+tCase, false)

				n := signals.NewNotifier(cancel, gracefulShutdownDeadline, s)
				go n.Notify()

				for i := range commands {
					wg.Add(1)
					t.Logf("Start %v command", commands[i].Name)
					t.Logf("%+v", commands[i].Envs)
					commands[i].SetState(s)
					envs := commands[i].MergeEnvs()
					t.Logf("Envs: %+v", envs)
					go commands[i].Run(ctx, &wg)
				}
				go func() {
					for {
						pids := s.GetPids()
						select {
						case <-ctx.Done():
							t.Errorf("Context deadline %v", tCase)
							if len(pids) == 0 {
								t.Errorf("Unknown pid, %v", err)
							}
							return
						default:
							if len(pids) > 0 {
								t.Logf("Pids: %+v", pids)

								for _, pid := range pids {
									err = syscall.Kill(pid, syscall.SIGTERM)
									if err != nil {
										log.Errorf("%v", err)
									}
								}
								return
							}
							continue
						}
					}
				}()

				//log.SetLevel(log.DebugLevel)

				wg.Wait()

			}

			/*
				msg, _ := hello.Hello(c.name)

				if msg != c.want {
					t.Errorf("expected: %v, got: %v", c.want, msg)
				}
			*/
		})
	}
}
