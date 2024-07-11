package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/grafana/pyroscope-go"
	log "github.com/sirupsen/logrus"
	"github.com/username1366/prommerge"
	"net/http"
	"os"
	"process-wrapper/pkg/config"
	"process-wrapper/pkg/lease/handlers"
	"process-wrapper/pkg/lease/lock"
	"process-wrapper/pkg/metrics"
	"process-wrapper/pkg/process"
	"process-wrapper/pkg/signals"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	ConfigEnv               = "CONFIG"
	ReadinessFile           = "/tmp/started"
	WaitDeadlineSeconds     = 120
	GracefulShutdownSeconds = 10
	DefaultSharedLockURL    = "http://localhost:8191"
)

var (
	version                  = "0.0.0"
	configPath               string
	skipCommands             string
	readinessFile            string
	pyroscopeURL             string
	sharedLockURL            string
	debug                    bool
	wait                     bool
	sharedLock               bool
	sharedLockEtcdTLS        bool
	printVersion             bool
	envoyDrain               bool
	promMergeEmptyOnFailure  bool
	promMergeAsync           bool
	promMergeSort            bool
	promMergeOmitMeta        bool
	promMergeSupressErrors   bool
	waitDeadline             time.Duration
	gracefulShutdownDeadline time.Duration
)

func printEnv() {
	fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults() // Print default help
	fmt.Println(lock.DefaultClientCertPath)
	envs := `
Environment Variables:
  CONFIG: process-wrapper configuration in JSON format
  POD_NAMESPACE: must be set in config to handle fast sidecar termination, and to enrich loki events by namespace label
  NODE_NAME: must be set in config to enrich events with labels send to metrics-server
  HANDLE_PREEMPTION: handle google cloud preemption events gracefully
  HEALTH_SOCKET: http server socket which handles /readiness, /liveness, /prommerge endpoints, (default ":8111")
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
  SHARED_LEASE_ETCD_CA_CERT: path to CA certificate, (default "%v")
  SHARED_LEASE_ETCD_CLIENT_CERT: path to client certificate, (default "%v")
  SHARED_LEASE_ETCD_CLIENT_KEY: path to client key, (default "%v")
  AMQP_PROBE_RABBITMQ_DSN_TEMPLATE: amqp probe dsn template, e.g. 'amqp://%%v:%%v@localhost:5673'
  AMQP_PROBE_RABBITMQ_USER: amqp probe user 'guest'
  AMQP_PROBE_RABBITMQ_PASS: amqp probe password, e.g. 'guest'

`
	fmt.Printf("%v", fmt.Sprintf(envs, lock.DefaultCaCertPath, lock.DefaultClientCertPath, lock.DefaultClientKeyPath))
}

func initFlags() {
	flag.Usage = printEnv
	flag.StringVar(&configPath, "config", "", "config file path")
	flag.StringVar(&skipCommands, "skip-commands", "", "comma-separated list of ignored commands")
	flag.StringVar(&readinessFile, "readiness-file", ReadinessFile, "readiness file path")
	flag.StringVar(&pyroscopeURL, "pyroscope-url", "", "pyroscope profiler address, e.g. http://localhost:4040")
	flag.StringVar(&sharedLockURL, "shared-lock-url", DefaultSharedLockURL, "shared-lock server url, e.g. http://localhost:8191")
	flag.BoolVar(&debug, "debug", false, "enable debug")
	flag.BoolVar(&wait, "wait", false, "wait readiness")
	flag.BoolVar(&sharedLock, "shared-lock", false, "shared lock server")
	flag.BoolVar(&sharedLockEtcdTLS, "shared-lock-etcd-tls", false, "use tls transport for etcd")
	flag.BoolVar(&printVersion, "version", false, "prints version")
	flag.BoolVar(&envoyDrain, "envoy-drain", false, "drain envoy listeners in case of preemption")
	flag.BoolVar(&promMergeEmptyOnFailure, "prommerge-empty-on-failure", false, "return empty metric list if on of the target is failed")
	flag.BoolVar(&promMergeAsync, "prommerge-async", true, "collect all prometheus targets concurrently")
	flag.BoolVar(&promMergeSort, "prommerge-sort", true, "disable prommerge result sorting")
	flag.BoolVar(&promMergeOmitMeta, "prommerge-omit-meta", false, "disable prometheus metadata collection (HELP, TYPE strings)")
	flag.BoolVar(&promMergeSupressErrors, "prommerge-supress-errors", false, "do not print module errors")
	flag.DurationVar(&waitDeadline, "wait-deadline", time.Second*WaitDeadlineSeconds, "wait deadline duration")
	flag.DurationVar(&gracefulShutdownDeadline, "graceful-shutdown-deadline", time.Second*GracefulShutdownSeconds, "time between SIGTERM/SIGQUIT and SIGKILL")
	flag.Parse()

	if debug {
		log.SetLevel(log.DebugLevel)
	}
}

func Pyroscope() {
	// These 2 lines are only required if you're using mutex or block profiling
	// Read the explanation below for how to set these rates:
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(5)

	_, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: "process-wrapper",

		// replace this with the address of pyroscope server
		ServerAddress: pyroscopeURL,

		// you can disable logging by setting this to nil
		Logger: pyroscope.StandardLogger,

		// optionally, if authentication is enabled, specify the API key:
		// AuthToken:    os.Getenv("PYROSCOPE_AUTH_TOKEN"),

		// you can provide static tags via a map:
		Tags: map[string]string{"hostname": os.Getenv("HOSTNAME")},

		ProfileTypes: []pyroscope.ProfileType{
			// these profile types are enabled by default:
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,

			// these profile types are optional:
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})
	if err != nil {
		log.Errorf("Failed to start pyroscope profiler, %v", err)
	}
}

func main() {
	initFlags()
	if wait {
		log.Printf("Wait all process readiness")
		err := WaitReadiness()
		if err != nil {
			log.Errorf("Wait readiness failure, %v", err)
			os.Exit(1)
		}
		return
	}
	if printVersion {
		fmt.Println(version)
		os.Exit(0)
	}
	if pyroscopeURL != "" {
		Pyroscope()
	}
	if sharedLock {
		err := SharedLockServer()
		if err != nil {
			log.Fatalln(err)
		}
		os.Exit(0)
	}
	StartProcessWrapper()
}

func SharedLockServer() error {
	socket := os.Getenv("SHARED_LEASE_SOCKET")
	etcdEndpoints := os.Getenv("SHARED_LEASE_ETCD_ENDPOINTS")
	etcdCaCert := os.Getenv("SHARED_LEASE_ETCD_CA_CERT")
	etcdClientCert := os.Getenv("SHARED_LEASE_ETCD_CLIENT_CERT")
	etcdClientKey := os.Getenv("SHARED_LEASE_ETCD_CLIENT_KEY")
	err := handlers.LeaseController(socket, etcdEndpoints, etcdCaCert, etcdClientCert, etcdClientKey, sharedLockEtcdTLS)
	return err
}

func WaitReadiness() error {
	log.Printf("Wait deadline: %v", waitDeadline)
	t := time.NewTimer(waitDeadline)
	for {
		select {
		case <-t.C:
			return fmt.Errorf("wait deadline is exceeded")
		default:
			_, err := os.Stat(readinessFile)
			if err == nil {
				return nil
			}
			time.Sleep(time.Millisecond * 500)
		}
	}
}

func StartProcessWrapper() {
	pid := os.Getpid()
	if pid != 1 {
		log.WithFields(log.Fields{"name": "main", "pid": pid}).Warnf("Main process pid is not 1, signal notifier may work unproperly")
		err := os.WriteFile("pid", []byte(fmt.Sprintf("%v", pid)), 0600)
		if err != nil {
			log.Errorf("Failed to create pid file, %v", err)
		}
	}
	state := process.NewState(readinessFile, envoyDrain)
	ctx, cancel := context.WithCancel(context.Background())
	n := signals.NewNotifier(cancel, gracefulShutdownDeadline, state)
	go n.Notify()
	podStatus := process.NewPodStatus(cancel)
	go podStatus.ReadStatusController()

	var cfg config.Config
	if configPath != "" {
		cfg = config.File{Path: configPath}
	} else {
		cfg = config.JSONEnv{Env: ConfigEnv}
	}

	commands := cfg.Load()
	PrintConfigJson(config.ToJSON(commands))
	go metrics.PrometheusHandler()

	opts := prommerge.PromDataOpts{
		EmptyOnFailure: promMergeEmptyOnFailure,
		Async:          promMergeAsync,
		Sort:           promMergeSort,
		OmitMeta:       promMergeOmitMeta,
		SupressErrors:  promMergeSupressErrors,
		HTTPClient: &http.Client{
			Timeout: time.Second * 30, // Set a total timeout for the request
			Transport: &http.Transport{
				MaxIdleConns:        500,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
				MaxIdleConnsPerHost: 200,
			},
		},
	}
	go state.PromMergeHandler(config.PromTargets(commands), opts)

	wg := sync.WaitGroup{}
	go process.NewSchedule(ctx, commands, strings.Split(skipCommands, ","), sharedLockURL, state, &wg)
	wg.Add(1)
	wg.Wait()
	log.Printf("All commands are completed")
}

func PrintConfigJson(data []byte, err error) {
	if err != nil {
		log.Errorf("Failed to print CONFIG JSON, %v", err)
		return
	}
	log.Debugf("CONFIG='%v'", string(data))
}
