## NeonVM live migration loadtest tool

What this tool do:

- run [NeonVM](https://github.com/neondatabase/autoscaling/tree/main/neonvm) virtual machines in k8s cluster, each machine ased on `postgres:15-bullseye` docker image
- for each VM run separate pod with `pgbench` configured to attack PostgreSQL running inside VM via overlay network
- randomly trigger VM live migrations (each ~5 min in average) for each VM

### TL;DR

build

```console
go build
```

get usage info

```console
./neonvm-migration-loadtest -help
```

simple run

```console
./neonvm-migration-loadtest
```

>NOTE: tool will use default kubernetes config (`~/.kube/config`) and your current kubernetes context, you can override it by `-kubeconfig` arg

run 100 virtual machines during 1 hour without autoscale feature

```console
./neonvm-migration-loadtest -vm-count=100 -duration=3600 -autoscale=false
```

### Run as kubernetes job

apply RBAC

```console
kubectl apply -f manifests/rbac.yaml
```

run 100 virtual machines during 1 hour without autoscale feature

```console
kubectl apply -f manifests/loadtest.yaml
```

>NOTE: edit `loadtest.yaml` (args for `loadtest` container) if you want change load test settings before run

### Monitoring

>NOTE: manifests adapted for Prometheus managed by [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)

- [manifests/metrics.yaml](manifests/metrics.yaml) - used to configure Prometheus scrape metrics from load test tool
- [manifests/neonvm-metrics.yaml](manifests/neonvm-metrics.yaml) - used to configure Prometheus scrape metrics from NeonVM controller
- [dashboard/load-test.json](dashboard/load-test.json) - Grafana dashboard
