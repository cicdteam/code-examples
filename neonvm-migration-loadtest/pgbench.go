package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	pgbenchInitTimeout = 120 // seconds
	pgbenchInitCommand = `set -e
until pg_isready --dbname=postgres; do sleep 1; done
createdb bench || true
pgbench -i --init-steps=tGp -s 140 bench
`
)

var (
	pgbenchCommand = fmt.Sprintf("pgbench -n -c 16 -j 8 -S -P 10 -T %d bench", *pgbenchDuration)
)

func startPgbenchPod(ctx context.Context, vmName string, vmIP string, kClient *kubernetes.Clientset) error {
	podName := fmt.Sprintf("%s-pgbench", vmName)
	var err error
	// check pod already exists
	_, err = kClient.CoreV1().Pods(vmNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		// pbench pod exist, do nothing
		return nil
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: vmNamespace,
			Annotations: map[string]string{
				"k8s.v1.cni.cncf.io/networks":             "neonvm-system/neonvm-overlay-for-pods",
				"kubectl.kubernetes.io/default-container": "pgbench",
			},
		},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &[]int64{1}[0],
			RestartPolicy:                 corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{
				{
					Name:  "init",
					Image: "postgres:15-alpine",
					Env: []corev1.EnvVar{
						{
							Name:  "PGHOST",
							Value: vmIP,
						},
						{
							Name:  "PGUSER",
							Value: "postgres",
						},
					},
					Args: []string{"/bin/sh", "-c", pgbenchInitCommand},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "pgbench",
					Image: "postgres:15-alpine",
					Env: []corev1.EnvVar{
						{
							Name:  "PGHOST",
							Value: vmIP,
						},
						{
							Name:  "PGUSER",
							Value: "postgres",
						},
					},
					Args: []string{"/bin/sh", "-c", pgbenchCommand},
				},
			},
		},
	}

	// create
	_, err = kClient.CoreV1().Pods(vmNamespace).Create(ctx, &pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// get pod status and wait for Running phase
	timeout := time.After(pgbenchInitTimeout * time.Second)
	tick := time.Tick(time.Second)
LOOP:
	for {
		select {
		case <-tick:
			p, err := kClient.CoreV1().Pods(vmNamespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				break
			}
			if p.Status.Phase == corev1.PodRunning {
				break LOOP
			}
		case <-timeout:
			err = fmt.Errorf("got a timeout while waiting for pod to start")
			break LOOP
		}
	}

	return err
}

func stopPgbenchPod(ctx context.Context, vmName string, kClient *kubernetes.Clientset) error {
	podName := fmt.Sprintf("%s-pgbench", vmName)

	var err error
	for try := 1; try <= 10; try++ {
		err = kClient.CoreV1().Pods(vmNamespace).Delete(ctx, podName, metav1.DeleteOptions{})
		if err == nil || apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(time.Second)
	}

	// ensure pod was deleted
	if err == nil || apierrors.IsNotFound(err) {
		for try := 1; try <= 10; try++ {
			_, err = kClient.CoreV1().Pods(vmNamespace).Get(ctx, podName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			time.Sleep(time.Second)
		}
	} else {
		return err
	}

	return fmt.Errorf("can't delete pgbench pod")
}

func getPgbenchPodPhase(ctx context.Context, vmName string, kClient *kubernetes.Clientset) (corev1.PodPhase, error) {
	// get pod
	podName := fmt.Sprintf("%s-pgbench", vmName)
	pod, err := kClient.CoreV1().Pods(vmNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return pod.Status.Phase, nil
}
