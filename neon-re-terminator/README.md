## neon-re-terminator

### algorithm

- get list of pods in a specific namespace (`default` by default) by label selector(`neon/component=compute-node` by default)
- check if pod has `DeletionTimestamp` (meaning the pod in `terminating` state)
- check if pod in terminating state longer than timeout (`30s` by default) or longer than `.spec.terminationGracePeriodSeconds`
- check if the `compute-node` container is already terminated (container status `terminated`)
- if all confitions passed - force delete pod

### usage

build

```console
go build
```

get help

```console
./neon-re-terminator -help
```

### run in kuberntes cluster

```console
kubectl apply -f manifests
```
