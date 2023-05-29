package main

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	neonvmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

const (
	milliCPUsMin = 1000 // 1 core
	milliCPUsMax = 4000 // 4 cores

	memorySlotSize = "1Gi"
	memorySlotsMin = 4
	memorySlotsMax = 16

	autoscalingBounds = `{ "min": { "cpu": "1", "mem": "4Gi" }, "max": {"cpu": 4, "mem": "16Gi" } }`

	neonvmCacheSize = "1Gi"

	vmCreationTimeout = 60 // seconds
)

func startVM(ctx context.Context, vmName string, vmClient *neonvm.Clientset) (string, error) {
	var ip string
	var err error
	// check if vm exists already
	_, err = vmClient.NeonvmV1().VirtualMachines(vmNamespace).Get(ctx, vmName, metav1.GetOptions{})
	if err == nil {
		return ip, fmt.Errorf("vm %s already exists", vmName)
	} else if err != nil && !apierrors.IsNotFound(err) {
		return ip, err
	}

	// define vm spec
	vm := neonvmapi.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        vmName,
			Namespace:   vmNamespace,
			Labels:      labels(),
			Annotations: annotations(),
		},
		Spec: neonvmapi.VirtualMachineSpec{
			QMP:                           20183,
			RunnerPort:                    25183,
			RestartPolicy:                 neonvmapi.RestartPolicyNever,
			TerminationGracePeriodSeconds: &[]int64{1}[0],
			SchedulerName:                 "autoscale-scheduler",
			Guest: neonvmapi.Guest{
				CPUs: neonvmapi.CPUs{
					Min: &[]neonvmapi.MilliCPU{milliCPUsMin}[0],
					Max: &[]neonvmapi.MilliCPU{milliCPUsMax}[0],
					Use: &[]neonvmapi.MilliCPU{milliCPUsMax}[0],
				},
				MemorySlotSize: resource.MustParse(memorySlotSize),
				MemorySlots: neonvmapi.MemorySlots{
					Min: &[]int32{memorySlotsMin}[0],
					Max: &[]int32{memorySlotsMax}[0],
					Use: &[]int32{memorySlotsMin}[0],
				},
				RootDisk: neonvmapi.RootDisk{
					Image: vmImage,
				},
				Args: []string{"-c", "config_file=/etc/postgresql/postgresql.conf"},
				Env: []neonvmapi.EnvVar{
					{
						Name:  "POSTGRES_HOST_AUTH_METHOD",
						Value: "trust",
					},
				},
				Ports: []neonvmapi.Port{
					{
						Name:     "postgres",
						Port:     5432,
						Protocol: neonvmapi.ProtocolTCP,
					},
					{
						Name:     "host-metrics",
						Port:     9100,
						Protocol: neonvmapi.ProtocolTCP,
					},
					{
						Name:     "informant",
						Port:     10301,
						Protocol: neonvmapi.ProtocolTCP,
					},
				},
			},
			Disks: []neonvmapi.Disk{
				{
					Name:      configMapName,
					MountPath: "/etc/postgresql",
					DiskSource: neonvmapi.DiskSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: configMapName,
							},
							Items: []corev1.KeyToPath{
								{
									Key:  "postgresql.conf",
									Path: "postgresql.conf",
								},
							},
						},
					},
				},
				{
					Name:      "cache",
					MountPath: "neonvm/cache",
					DiskSource: neonvmapi.DiskSource{
						Tmpfs: &neonvmapi.TmpfsDiskSource{
							Size: resource.MustParse(neonvmCacheSize),
						},
					},
				},
			},
			ExtraNetwork: &neonvmapi.ExtraNetwork{
				Enable:    true,
				Interface: "net1",
			},
			EnableAcceleration: true,
		},
	}

	// create vm
	_, err = vmClient.NeonvmV1().VirtualMachines(vmNamespace).Create(ctx, &vm, metav1.CreateOptions{})
	if err != nil {
		return ip, err
	}

	// get vm status and wait for Running phase, then retrive overlay IP address
	var created *neonvmapi.VirtualMachine
	timeout := time.After(vmCreationTimeout * time.Second)
	tick := time.Tick(time.Second)
LOOP:
	for {
		select {
		case <-tick:
			created, err = vmClient.NeonvmV1().VirtualMachines(vmNamespace).Get(ctx, vmName, metav1.GetOptions{})
			if err != nil {
				break
			}
			if created.Status.Phase == neonvmapi.VmRunning && created.Status.ExtraNetIP != "" {
				break LOOP
			}
		case <-timeout:
			err = fmt.Errorf("got a timeout while waiting for vm to start")
			break LOOP
		}
	}

	return created.Status.ExtraNetIP, err
}

func stopVM(ctx context.Context, vmName string, vmClient *neonvm.Clientset) error {
	var err error
	for try := 1; try <= 10; try++ {
		err = vmClient.NeonvmV1().VirtualMachines(vmNamespace).Delete(ctx, vmName, metav1.DeleteOptions{})
		if err == nil || apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(time.Second)
	}

	// ensure vm was deleted
	if err == nil || apierrors.IsNotFound(err) {
		for try := 1; try <= 10; try++ {
			_, err = vmClient.NeonvmV1().VirtualMachines(vmNamespace).Get(ctx, vmName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			time.Sleep(time.Second)
		}
	} else {
		return err
	}

	return fmt.Errorf("can't delete vm")
}

func getVmPhase(ctx context.Context, vmName string, vmClient *neonvm.Clientset) (neonvmapi.VmPhase, error) {
	// get vm
	vm, err := vmClient.NeonvmV1().VirtualMachines(vmNamespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return vm.Status.Phase, nil
}

func labels() map[string]string {
	l := map[string]string{}
	l["autoscaling.neon.tech/enabled"] = "true"
	return l
}

func annotations() map[string]string {
	a := map[string]string{}
	a["autoscaling.neon.tech/bounds"] = autoscalingBounds
	return a
}
