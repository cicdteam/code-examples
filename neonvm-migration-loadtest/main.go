package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	//	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

const (
	vmImage      = "andreyneon/vm-postgres:15-bullseye"
	vmNamePrefix = "vm-stress-"
	vmNamespace  = "default"

	vmLoopDelayMS = 300

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
	kconfig         = flag.String("kube-config", "~/.kube/config", "Path to kuberenetes config. Only required if out-of-cluster.")
	configMapName   = fmt.Sprintf("%sconfig", vmNamePrefix)
	vmCount         = flag.Int("vm-count", 3, "number of virtual machines")
	pgbenchDuration = flag.Int("duration", 600, "duration of benchmark test in seconds")
	noLoad          = flag.Bool("no-load", false, "do not run pgbench workload")
)

type stats struct {
	VmStartFails            int64
	VmExecutionFails        int64
	PgbenchStartFails       int64
	PgbenchExecutionFails   int64
	MigrationStartFails     int64
	MigrationExecutionFails int64
	MigrationCompletions    int64
	MigrationTimeouts       int64
	MigrationDurations      []int64
}

var counters stats

func main() {
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

	// resolve tilda in kubeconfig path
	kcfg := *kconfig
	if strings.HasPrefix(kcfg, "~/") {
		dirname, _ := os.UserHomeDir()
		kcfg = filepath.Join(dirname, kcfg[2:])
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kcfg)
	if err != nil {
		klog.Fatal(err)
	}
	// tune Kubernetes client perfomance
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

	if err := createConfigMap(ctx, kClient); err != nil {
		logger.Error(err, "configmap with postgresql.conf create/update failed")
		os.Exit(255)
	}

	// vm starts loop
	var wg sync.WaitGroup
	for loop := 1; loop <= *vmCount; loop++ {
		// delay between vm starts
		time.Sleep(time.Millisecond * vmLoopDelayMS)

		wg.Add(1)
		go func(loop int) {
			defer wg.Done()
			// generate vm name
			vmName := fmt.Sprintf("%s%04d", vmNamePrefix, loop)

			// start vm
			var vmIP string
			vmIP, err = startVM(ctx, vmName, vmClient)
			if err != nil {
				counters.VmStartFails++
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

			if !*noLoad {
				// start pgbench
				if err := startPgbenchPod(ctx, vmName, vmIP, kClient); err != nil {
					counters.PgbenchStartFails++
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
			}

			// run migrations loop
			migrationsStop := make(chan struct{})
			migrationsDone := make(chan struct{})
			wg.Add(1)
			go doMigrations(ctx, vmName, vmClient, &wg, migrationsStop, migrationsDone)

			if !*noLoad {
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
								logger.Info("pgbench succeeded", "pod", fmt.Sprintf("%s-pgbench", vmName))
								if err := stopPgbenchPod(ctx, vmName, kClient); err != nil {
									logger.Error(err, "deleting pgbench failed", "pod", fmt.Sprintf("%s-pgbench", vmName))
								}
								close(pgbenchDone)
								return
							case corev1.PodFailed:
								counters.PgbenchExecutionFails++
								logger.Info("pgbench failed", "pod", fmt.Sprintf("%s-pgbench", vmName))
								close(pgbenchDone)
								return
							}
						}
					}
				}()
				<-pgbenchDone
			} else {
				time.Sleep(time.Duration(*pgbenchDuration) * time.Second)
			}

			// stopping migrations loop
			close(migrationsStop)
			<-migrationsDone

			// destroy vm, skip deletion if vm in Failed state (for further investigation)
			if vmWasFailed, err := deleteVMifNotFailed(ctx, vmName, vmClient); err != nil {
				logger.Error(err, "neonvm deletion failed", "vm", vmName)
			} else {
				logger.Info("neonvm deleted", "vm", vmName)
				if vmWasFailed {
					counters.VmExecutionFails++
				}
			}
		}(loop)
	} // vm starts loop

	wg.Wait()

	if err := deleteConfigMap(ctx, kClient); err != nil {
		logger.Error(err, "configmap with postgresql.conf deletion failed")
	}

	logger.Info("Statistics",
		"vm start fails", counters.VmStartFails,
		"vm execution fails", counters.VmExecutionFails,
		"pgbench start fails", counters.PgbenchStartFails,
		"pgbench execution fails", counters.PgbenchExecutionFails,
		"migration start fails", counters.MigrationStartFails,
		"migration execution fails", counters.MigrationExecutionFails,
		"migration completions", counters.MigrationCompletions,
		"migration min duration", migrationDurationString(durationMin(counters.MigrationDurations)),
		"migration max duration", migrationDurationString(durationMax(counters.MigrationDurations)),
		"migration average duration", migrationDurationString(durationAverage(counters.MigrationDurations)))
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

func durationAverage(durations []int64) int64 {
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
	min := durations[0]
	for _, value := range durations {
		if value < min {
			min = value
		}
	}
	return min
}

func durationMax(durations []int64) int64 {
	max := durations[0]
	for _, value := range durations {
		if value > max {
			max = value
		}
	}
	return max
}
