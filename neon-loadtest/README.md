## Neon loadtest tool

What this tool do:

- create Neon projects (computes) wtih `k8s-neonvm` provisioner (Virtaul Machine)
- for each project periodiacall run heavy SQL queries that triger scale up and down compute

### TL;DR

1. Obtain [Neon API Key](https://neon.tech/docs/manage/api-keys)
2. Run

```console
NEON_API_KEY=your_key_here go run main.go
```

get usage info

```console
go run main.go -h
```

create 100 projects and place workload  during 1 hour

```console
NEON_API_KEY=your_key_here go run main.go -count 100 -duration 1h
```

### Run as kubernetes job

create 100 projects and place workload  during 1 hour

```console
kubectl create secret generic neon-loadtest --from-literal=apikey=your_key_here
kubectl apply -f manifests/loadtest.yaml
```

tear down

```console
kubectl delete -f manifests/loadtest.yaml
kubectl delete secret neon-loadtest
```

>NOTE: edit `loadtest.yaml` (args for `loadtest` container) if you want change load test settings before run
