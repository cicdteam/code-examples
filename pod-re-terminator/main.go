package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-co-op/gocron"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	defaultPodsLabelSelector = "neon/component=compute-node"
	defaultPodsNamespace     = "default"
	defaultStuckTimeout      = "30s"
)

var (
	namespace     string
	labelSelector string
	timeout       string
	dryRun        bool
)

func main() {

	flag.StringVar(&namespace, "namespace", defaultPodsNamespace, "namespace")
	flag.StringVar(&labelSelector, "selector", defaultPodsLabelSelector, "selector (label query) to filter on")
	flag.StringVar(&timeout, "timeout", defaultStuckTimeout, "duration after which to delete pods in terminating state")
	flag.BoolVar(&dryRun, "dryrun", false, "skip forced pod removal")

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
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "can't create kubernetes client")
		os.Exit(1)
	}

	// define stuck timeout
	stuckTimeout, err := time.ParseDuration(timeout)
	if err != nil {
		logger.Error(err, "can't parse timeout")
		os.Exit(1)
	}

	// cron job task
	// - find pods in terminating state
	// - check if pod in termination state longer then terminationGracePeriodSeconds or stuckTimeout
	// - delete stuck pods
	task := func() {
		listOpts := metav1.ListOptions{
			LabelSelector: labelSelector,
			FieldSelector: "status.phase=Running",
		}
		podlist, err := c.CoreV1().Pods(namespace).List(ctx, listOpts)
		if err != nil {
			logger.Error(err, "can't get pods list")
			return
		}

		stuckness := []corev1.Pod{}
		for _, pod := range podlist.Items {
			if pod.DeletionTimestamp != nil {
				// this pod in termination state, check how long
				t := stuckTimeout
				if pod.Spec.TerminationGracePeriodSeconds != nil {
					// pod has termination grace period in spec
					grace, _ := time.ParseDuration(fmt.Sprintf("%ds", *pod.Spec.TerminationGracePeriodSeconds))
					if grace > stuckTimeout {
						// respect grace period as it longer then stuck timeout form args
						t = grace
					}
				}
				// check if pod in termination state too long
				if time.Since(pod.DeletionTimestamp.Time) > t {
					// seems this pod stuck
					stuckness = append(stuckness, pod)
				}
			}
		}

		if len(stuckness) > 0 {
			for _, pod := range stuckness {
				logger.Info("candidate for forced removal", "name", pod.Name, "terminating", time.Since(pod.DeletionTimestamp.Time))

				if !dryRun {
					if err := c.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
						logger.Error(err, "pod deletion error")
					} else {
						logger.Info("pod was forcibly removed", "name", pod.Name)
					}
				}
			}
		} else {
			logger.Info("there are no pods stuck in termination")
		}
	} // task

	s := gocron.NewScheduler(time.UTC)
	s.SingletonModeAll()
	_, err = s.Cron("*/1 * * * *").Do(task) // every minute
	if err != nil {
		logger.Error(err, "scheduling failed")
		os.Exit(1)
	}
	s.StartBlocking()
}
