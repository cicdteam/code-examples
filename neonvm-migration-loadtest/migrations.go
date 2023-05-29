package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-co-op/gocron"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	neonvmapi "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	neonvm "github.com/neondatabase/autoscaling/neonvm/client/clientset/versioned"
)

const (
	createMigrationEvery       = 300 // seconds
	migrationCreationTimeout   = 60  //seconds
	migrationCompletionTimeout = 240 //seconds
)

var (
	migrationSpec = neonvmapi.VirtualMachineMigrationSpec{
		PreventMigrationToSameHost: false,
		Incremental:                true,
		AllowPostCopy:              false,
		AutoConverge:               true,
		MaxBandwidth:               resource.MustParse("1Gi"),
		CompletionTimeout:          migrationCompletionTimeout,
	}
)

func doMigrations(ctx context.Context, vmName string, vmClient *neonvm.Clientset, wg *sync.WaitGroup, migrationsStop, migrationsDone chan struct{}) {
	defer wg.Done()
	var err error

	log := log.FromContext(ctx)

	task := func(vmName string, job gocron.Job) {
		vmmName := fmt.Sprintf("%s-%03d", vmName, job.RunCount())

		if err = startMigration(ctx, vmName, vmmName, vmClient); err != nil {
			log.Error(err, "migration start failed", "vmm", vmmName)
			return
		}
		log.Info("migration started", "vmm", vmmName)

		// status check managed by ticker and timeout timer
		timeout := time.After(migrationCompletionTimeout * time.Second)
		tick := time.Tick(10 * time.Second)
		for {
			select {
			case <-tick:
				// check migration phase
				phase, err := getMigrationPhase(ctx, vmmName, vmClient)
				if err != nil {
					log.Error(err, "migration check failed", "vmm", vmmName)
					break
				}
				switch phase {
				case neonvmapi.VmmSucceeded:
					log.Info("migration completed", "vmm", vmmName)
					if err := stopMigration(ctx, vmmName, vmClient); err != nil {
						log.Error(err, "migration deletion failed", "vmm", vmmName)
					}
					return
				case neonvmapi.VmmFailed:
					log.Info("migration failed", "vmm", vmmName)
					return
				}
			case <-timeout:
				log.Info("migration timed out", "vmm", vmmName)
				return
			}
		}
	}

	s := gocron.NewScheduler(time.UTC)
	s.SingletonModeAll()
	_, err = s.Every(createMigrationEvery).Second().DoWithJobDetails(task, vmName)
	if err != nil {
		log.Error(err, "migration scheduling failed", "vmm", vmName)
		return
	}
	s.StartAsync()

	<-migrationsStop
	s.Stop()
	close(migrationsDone)
}

func startMigration(ctx context.Context, vmName, vmmName string, vmClient *neonvm.Clientset) error {
	var err error

	// check if migration exists already
	_, err = vmClient.NeonvmV1().VirtualMachineMigrations(vmNamespace).Get(ctx, vmmName, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("migration already exists")
	} else if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// define vmm
	migrationSpec.VmName = vmName
	vmm := neonvmapi.VirtualMachineMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmmName,
			Namespace: vmNamespace,
		},
		Spec: migrationSpec,
	}

	// create vmm
	_, err = vmClient.NeonvmV1().VirtualMachineMigrations(vmNamespace).Create(ctx, &vmm, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	// get vmm status and wait for Running phase
	timeout := time.After(migrationCreationTimeout * time.Second)
	tick := time.Tick(time.Second)
LOOP:
	for {
		select {
		case <-tick:
			created, err := vmClient.NeonvmV1().VirtualMachineMigrations(vmNamespace).Get(ctx, vmmName, metav1.GetOptions{})
			if err != nil {
				break
			}
			if created.Status.Phase == neonvmapi.VmmRunning {
				break LOOP
			}
		case <-timeout:
			err = fmt.Errorf("migration start timed out")
			// destroy vmm as it not needed
			stopMigration(ctx, vmmName, vmClient)
			break LOOP
		}
	}

	return err
}

func getMigrationPhase(ctx context.Context, vmmName string, vmClient *neonvm.Clientset) (neonvmapi.VmmPhase, error) {
	// get migration
	vmm, err := vmClient.NeonvmV1().VirtualMachineMigrations(vmNamespace).Get(ctx, vmmName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	return vmm.Status.Phase, nil
}

func stopMigration(ctx context.Context, vmmName string, vmClient *neonvm.Clientset) error {
	var err error
	for try := 1; try <= 10; try++ {
		err = vmClient.NeonvmV1().VirtualMachineMigrations(vmNamespace).Delete(ctx, vmmName, metav1.DeleteOptions{})
		if err == nil || apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(time.Second)
	}

	// ensure vmm was deleted
	if err == nil || apierrors.IsNotFound(err) {
		for try := 1; try <= 10; try++ {
			_, err = vmClient.NeonvmV1().VirtualMachineMigrations(vmNamespace).Get(ctx, vmmName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			time.Sleep(time.Second)
		}
	} else {
		return err
	}

	return fmt.Errorf("can't delete vm migration")
}
