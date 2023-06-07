package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

const (
	vmImage      = "andreyneon/vm-postgres:15-bullseye"
	vmNamePrefix = "vm-stress-"
	vmNamespace  = "default"

	vmLoopDelayMS = 1000

	postgresqlConf = `
listen_addresses = '*'
shared_preload_libraries = 'pg_stat_statements'

max_connections = 64
shared_buffers = 256MB
effective_cache_size = 1536MB
maintenance_work_mem = 128MB
checkpoint_completion_target = 0.9
wal_buffers = 16MB
default_statistics_target = 100
random_page_cost = 1.1
effective_io_concurrency = 200
work_mem = 4MB
min_wal_size = 1GB
max_wal_size = 4GB
max_worker_processes = 4
max_parallel_workers_per_gather = 2
max_parallel_workers = 4
max_parallel_maintenance_workers = 2
`
)

var (
	configMapName   = fmt.Sprintf("%sconfig", vmNamePrefix)
	vmCount         int
	pgbenchDuration int
	load            bool
	autoscale       bool
	metricsAddr     string
)

type stats struct {
	VmStartDurations        []int64
	PgbenchStartDurations   []int64
	MigrationDurations      []int64
	MigrationStartDurations []int64
}

var counters stats

type performanceMetrics struct {
	VmTotal                 *metrics.Counter
	VmStartFails            *metrics.Counter
	VmExecutionFails        *metrics.Counter
	VmStartDuration         *metrics.Summary
	PgbenchTotal            *metrics.Counter
	PgbenchStartFails       *metrics.Counter
	PgbenchExecutionFails   *metrics.Counter
	PgbenchStartDuration    *metrics.Summary
	MigrationTotal          *metrics.Counter
	MigrationStartFails     *metrics.Counter
	MigrationExecutionFails *metrics.Counter
	MigrationCompletions    *metrics.Counter
	MigrationTimeouts       *metrics.Counter
	MigrationStartDuration  *metrics.Summary
	MigrationDuration       *metrics.Summary
}

var (
	performance = performanceMetrics{
		VmTotal:                 metrics.NewCounter(`loadtest_vm_running_total`),
		VmStartFails:            metrics.NewCounter(`loadtest_vm_start_fails_total`),
		VmExecutionFails:        metrics.NewCounter(`loadtest_vm_execution_fails_total`),
		VmStartDuration:         metrics.NewSummary(`loadtest_vm_startup_duration_seconds`),
		PgbenchTotal:            metrics.NewCounter(`loadtest_pgbench_running_total`),
		PgbenchStartFails:       metrics.NewCounter(`loadtest_pgbench_start_fails_total`),
		PgbenchExecutionFails:   metrics.NewCounter(`loadtest_pgbench_execution_fails_total`),
		PgbenchStartDuration:    metrics.NewSummary(`loadtest_pgbench_startup_duration_seconds`),
		MigrationTotal:          metrics.NewCounter(`loadtest_migration_running_total`),
		MigrationStartFails:     metrics.NewCounter(`loadtest_migration_start_fails_total`),
		MigrationExecutionFails: metrics.NewCounter(`loadtest_migration_execution_fails_total`),
		MigrationCompletions:    metrics.NewCounter(`loadtest_migration_completions_total`),
		MigrationTimeouts:       metrics.NewCounter(`loadtest_migration_timeouts_total`),
		MigrationStartDuration:  metrics.NewSummary(`loadtest_migration_startup_duration_seconds`),
		MigrationDuration:       metrics.NewSummary(`loadtest_migration_duration_total`),
	}
)

func main() {

	flag.IntVar(&vmCount, "vm-count", 3, "number of virtual machines")
	flag.IntVar(&pgbenchDuration, "duration", 600, "duration of benchmark test in seconds")
	flag.BoolVar(&load, "load", true, "run pgbench workload")
	flag.BoolVar(&autoscale, "autoscale", true, "use autoscling")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9090", "the address the metric endpoint binds to")

	// define logging options
	opts := zap.Options{
		Development:     true,
		StacktraceLevel: zapcore.Level(zapcore.PanicLevel),
		TimeEncoder:     zapcore.ISO8601TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// define logger
	logger := zap.New(zap.UseFlagOptions(&opts))

	//	// define klog settings (used in LeaderElector)
	klog.SetLogger(logger)

	// define context with logger
	ctx := log.IntoContext(context.Background(), logger)

	// get Kubernetes client and tune performance
	cfg, err := config.GetConfig()
	if err != nil {
		logger.Error(err, "kubernetes config not found")
		os.Exit(1)
	}
	cfg.QPS = 1000
	cfg.Burst = 2000

	// get k8s client
	kClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatal(err)
	}

	// get neonvm client
	vmClient, err := neonvm.NewForConfig(cfg)
	if err != nil {
		klog.Fatal(err)
	}

	// Expose the registered metrics at `/metrics` path.
	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		metrics.WritePrometheus(w, true)
	})
	go http.ListenAndServe(metricsAddr, nil)

	logger.Info("staring", "load", load, "autoscale", autoscale, "vm count", vmCount, "duration", pgbenchDuration)

	if err := createConfigMap(ctx, kClient); err != nil {
		logger.Error(err, "configmap with postgresql.conf create/update failed")
		os.Exit(255)
	}

	// chill out pause used to inspect VM after load test
	// just check jow many PostgreSQl instances running after stest and compare with VM count
	// e.g. if we start 100 VMs but after test 3 failed or stuck we will see only 97 running Postgreses in grafana
	chillOutPause := time.Millisecond*time.Duration(vmLoopDelayMS*vmCount) + time.Second*time.Duration(300)

	// vm starts loop
	var wg sync.WaitGroup
	for loop := 1; loop <= vmCount; loop++ {
		// delay between vm starts
		time.Sleep(time.Millisecond * vmLoopDelayMS)

		wg.Add(1)
		go func(loop int, chillout time.Duration) {
			defer wg.Done()
			// generate vm name
			vmName := fmt.Sprintf("%s%04d", vmNamePrefix, loop)

			// start vm
			var vmIP string
			t := time.Now()
			vmIP, err = startVM(ctx, vmName, vmClient)
			if err != nil {
				performance.VmStartFails.Inc()
				logger.Error(err, "neonvm start failed", "vm", vmName)
				// destroy vm, skip deletion if vm in Failed state (for further investigation)
				if _, err := deleteVMifNotFailed(ctx, vmName, vmClient); err != nil {
					logger.Error(err, "neonvm deletion failed", "vm", vmName)
				} else {
					logger.Info("neonvm deleted", "vm", vmName)
				}
				return
			}
			logger.Info("neonvm started", "vm", vmName)
			startDuration := int64(time.Now().Sub(t).Round(time.Second).Seconds())
			counters.VmStartDurations = append(counters.VmStartDurations, startDuration)
			performance.VmTotal.Inc()
			performance.VmStartDuration.UpdateDuration(t)
			if load {
				// start pgbench
				t := time.Now()
				if err := startPgbenchPod(ctx, vmName, vmIP, kClient); err != nil {
					performance.PgbenchStartFails.Inc()
					logger.Error(err, "pgbench start failed", "pod", fmt.Sprintf("%s-pgbench", vmName))
					// destroy vm as it not needed
					if err := deleteVM(ctx, vmName, vmClient); err != nil {
						logger.Error(err, "neonvm stop failed", "vm", vmName)
					} else {
						logger.Info("neonvm deleted", "vm", vmName)
					}
					return
				}
				logger.Info("pgbench started", "pod", fmt.Sprintf("%s-pgbench", vmName))
				startDuration := int64(time.Now().Sub(t).Round(time.Second).Seconds())
				counters.PgbenchStartDurations = append(counters.PgbenchStartDurations, startDuration)
				performance.PgbenchTotal.Inc()
				performance.PgbenchStartDuration.UpdateDuration(t)
			}

			// run migrations loop
			migrationsStop := make(chan struct{})
			migrationsDone := make(chan struct{})
			wg.Add(1)
			go doMigrations(ctx, vmName, vmClient, &wg, migrationsStop, migrationsDone)

			if load {
				// check pgbench finished
				pgbenchDone := make(chan struct{})
				wg.Add(1)
				go func() {
					defer wg.Done()
					// check pgbench status every 10 seconds
					ticker := time.NewTicker(10 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-ticker.C:
							phase, err := getPgbenchPodPhase(ctx, vmName, kClient)
							if err != nil {
								logger.Error(err, "pgbench get phase error", "pod", fmt.Sprintf("%s-pgbench", vmName))
								break
							}
							switch phase {
							case corev1.PodSucceeded:
								performance.PgbenchTotal.Dec()
								logger.Info("pgbench succeeded", "pod", fmt.Sprintf("%s-pgbench", vmName))
								if err := stopPgbenchPod(ctx, vmName, kClient); err != nil {
									logger.Error(err, "deleting pgbench failed", "pod", fmt.Sprintf("%s-pgbench", vmName))
								}
								close(pgbenchDone)
								return
							case corev1.PodFailed:
								performance.PgbenchTotal.Dec()
								performance.PgbenchExecutionFails.Inc()
								logger.Info("pgbench failed", "pod", fmt.Sprintf("%s-pgbench", vmName))
								close(pgbenchDone)
								return
							}
						}
					}
				}()
				<-pgbenchDone
			} else {
				time.Sleep(time.Duration(pgbenchDuration) * time.Second)
			}

			// stopping migrations loop
			close(migrationsStop)
			<-migrationsDone

			time.Sleep(chillout)

			// destroy vm, skip deletion if vm in Failed state (for further investigation)
			performance.VmTotal.Dec()
			if vmWasFailed, err := deleteVMifNotFailed(ctx, vmName, vmClient); err != nil {
				logger.Error(err, "neonvm deletion failed", "vm", vmName)
			} else {
				logger.Info("neonvm deleted", "vm", vmName)
				if vmWasFailed {
					performance.VmExecutionFails.Inc()
				}
			}
		}(loop, chillOutPause)
	} // vm starts loop

	wg.Wait()

	if err := deleteConfigMap(ctx, kClient); err != nil {
		logger.Error(err, "configmap with postgresql.conf deletion failed")
	}

	logger.Info("statistics",
		"vm start fails", performance.VmStartFails.Get(),
		"vm execution fails", performance.VmExecutionFails.Get(),
		"pgbench start fails", performance.PgbenchStartFails.Get(),
		"pgbench execution fails", performance.PgbenchExecutionFails.Get(),
		"migration start fails", performance.MigrationStartFails.Get(),
		"migration execution fails", performance.MigrationExecutionFails.Get(),
		"migration completions", performance.MigrationCompletions.Get())
	logger.Info("vm start durations",
		"vm start min", durationString(durationMin(counters.VmStartDurations)),
		"vm start max", durationString(durationMax(counters.VmStartDurations)),
		"vm start avg", durationString(durationAvg(counters.VmStartDurations)))
	logger.Info("pgbench start durations",
		"pgbench start min", durationString(durationMin(counters.PgbenchStartDurations)),
		"pgbench start max", durationString(durationMax(counters.PgbenchStartDurations)),
		"pgbench start avg", durationString(durationAvg(counters.PgbenchStartDurations)))
	logger.Info("migration start durations",
		"migration start min", durationString(durationMin(counters.MigrationStartDurations)),
		"migration start max", durationString(durationMax(counters.MigrationStartDurations)),
		"migration start avg", durationString(durationAvg(counters.MigrationStartDurations)))
	logger.Info("migration durations",
		"migration min duration", migrationDurationString(durationMin(counters.MigrationDurations)),
		"migration max duration", migrationDurationString(durationMax(counters.MigrationDurations)),
		"migration avg duration", migrationDurationString(durationAvg(counters.MigrationDurations)))
} // main

func createConfigMap(ctx context.Context, kClient *kubernetes.Clientset) error {
	// try to get confogMap form k8s
	_, err := kClient.CoreV1().ConfigMaps(vmNamespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: vmNamespace,
		},
		Data: map[string]string{
			"postgresql.conf": postgresqlConf,
		},
	}

	if err == nil {
		// already present ? update then
		_, err = kClient.CoreV1().ConfigMaps(vmNamespace).Update(ctx, &cm, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	} else {
		// create
		_, err = kClient.CoreV1().ConfigMaps(vmNamespace).Create(ctx, &cm, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func deleteConfigMap(ctx context.Context, kClient *kubernetes.Clientset) error {
	err := kClient.CoreV1().ConfigMaps(vmNamespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

func durationAvg(durations []int64) int64 {
	if len(durations) == 0 {
		return 0
	}
	var s int64
	for _, d := range durations {
		s += d
	}
	return s / int64(len(durations))
}

func durationMin(durations []int64) int64 {
	if len(durations) == 0 {
		return 0
	}
	min := durations[0]
	for _, value := range durations {
		if value < min {
			min = value
		}
	}
	return min
}

func durationMax(durations []int64) int64 {
	if len(durations) == 0 {
		return 0
	}
	max := durations[0]
	for _, value := range durations {
		if value > max {
			max = value
		}
	}
	return max
}

func durationString(duration int64) string {
	d := time.Duration(duration) * time.Second
	return d.String()
}
