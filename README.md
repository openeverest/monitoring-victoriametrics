# monitoring-victoriametrics

PoC of an **ExtensionManaged** `MonitoringClass` controller for spec 014 (extensible
monitoring). It claims the `victoriametrics` class and, for every Accepted
`MonitoringBinding` of that class, creates one VictoriaMetrics Operator
`VMPodScrape` per endpoint published in `Instance.status.monitoring.sources.metrics`.

It never writes Instances: it owns only the bindings' `Configured` condition
and the destinations' `Ready` condition. VMPodScrapes are owned by the binding, so
removing the destination from the Instance garbage-collects them.

```sh
# Installs the controller and claims the `victoriametrics` MonitoringClass. Like a
# provider, it is its own chart, so it can ship as a hub extension or as a
# conditional subchart of the OpenEverest chart.
helm install monitoring-victoriametrics charts/monitoring-victoriametrics -n openeverest-system
```

For development against the current kubeconfig: `make build && IMG=... make docker-build`, or run the controller out of cluster with `kubectl apply -f <(helm template x charts/monitoring-victoriametrics -s templates/monitoringclass.yaml)` and `go run ./cmd/controller`.

`MonitoringDestination.spec.parameters`:

```yaml
vmPodScrapeLabels: {openeverest.io/vmagent: default}   # how your VMAgent selects VMPodScrapes
interval: 30s
```
