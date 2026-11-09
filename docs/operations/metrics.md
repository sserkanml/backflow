# Metrics and alerts

The operator serves Prometheus metrics next to those of controller-runtime
(`controller_runtime_*`, `workqueue_*`, `rest_client_*`). The ones of Backflow
start with `backflow_`. Their labels are of low cardinality: a policy, an
application, a source type, a reason, a phase, a provider, a host, an endpoint, a
status class. Never the name of a resource or a proposal, a file, a branch, a
repository URL, a token or a Secret, and a test fails when one of them shows up.
The series of a policy, a connection or an Application that is deleted go away
with it. A metric never blocks or fails a reconcile.

Counters start at zero when the operator starts, so use `rate` and `increase`.

The names of a policy and of a connection are always two labels, never one joined
value: `policy_namespace` and `policy` on every metric that refers to a
BackflowPolicy, `scm_namespace` and `scm` on the one that refers to an
ScmConnection. (They are not called `namespace` and `name`: Prometheus gives those
to the scrape target, and renames the label of the metric to `exported_namespace`.)
Metric names and labels are a contract; a change to one is a breaking change.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `backflow_storage_migration_pending` | gauge | | the number of Backflow CRDs whose objects may still be stored as `v1beta1` (the operator works meanwhile, since `v1` has the same schema); 0 once `manager migrate` has run, which is checked every 5 minutes; `v1beta1` can go in 2.0 only when it is 0 |
| `backflow_build_info` | gauge | `version`, `commit` | always 1; the version and the commit of the binary: for a release image the tag without its `v` and the commit of the tag, for an image of `main` `main` and its commit, for `make build` the output of `git describe` and the short commit, else what the Go toolchain recorded (`-dirty` for a modified tree), else `dev` and `unknown` |
| `backflow_scm_connection_ready` | gauge | `scm_namespace`, `scm` | 1 when an ScmConnection is Ready (its token is accepted), else 0 |
| `backflow_policy_ready` | gauge | `policy_namespace`, `policy` | 1 when a BackflowPolicy is Ready, else 0 |
| `backflow_policy_applications` | gauge | `policy_namespace`, `policy` | Applications a policy selects |
| `backflow_detection_paused` | gauge | `policy_namespace`, `policy`, `application`, `reason` | 1 while drift detection for an Application is paused; `reason` is `GitAhead`, `ApplicationChanged` or `Settling` |
| `backflow_proposals` | gauge | `policy_namespace`, `policy`, `phase` | DriftProposals as they are now, read on every scrape; a proposal without a phase is `Pending` |
| `backflow_drifts_detected_total` | counter | `policy_namespace`, `policy`, `application`, `source_type`, `changed_by` | drifts that became a proposal; `changed_by` is `interactive` (a known tool a person runs), `controller` (a known controller), `unknown` (a manager that is neither) or `none` (no manager could be named); see [Who changed it](../who-changed-it.md#who-changed-it) |
| `backflow_mapping_results_total` | counter | `source_type`, `strategy`, `reason` | outcomes of mapping: `Mapped` (or `PartiallyMapped`), or the reason a proposal is `Unmapped`; a mapping that is retried later is not counted |
| `backflow_merge_requests_total` | counter | `provider`, `action` | `opened`, `merged`, `closed`, and `push_rejected` (a Git push the server declined; `provider` is `git` for it) |
| `backflow_verification_duration_seconds` | histogram | `source_type`, `result` | time to map and verify a proposal by rendering; `result` is `mapped`, `unmapped` or `retry` |
| `backflow_renders_total` | counter | `renderer`, `deps`, `result` | renders through Argo CD; `renderer` is `upload` (files sent to Argo CD) or `revisions` (a commit, multi-source), `deps` says how the `file://` dependencies of the chart were provided: `none` (it has none), `built` (Argo CD ran `helm dependency build`) or `unpacked` (uploaded unpacked), `result` is `ok`, `failed` (the source does not render: a verdict) or `error` (Argo CD did not answer) |
| `backflow_render_duration_seconds` | histogram | `renderer`, `deps` | time of one render |
| `backflow_deps_equivalence_checks_total` | counter | `result` | checks that a chart renders the same with its `file://` dependencies unpacked as with Argo CD's build: `identical` (the quick way is used), `different` or `error` (it is not) |
| `backflow_scratch_branches_total` | counter | `action` | scratch branches `pushed`, `deleted`, `swept` (found left behind by a crashed verification and deleted), `push_failed` |
| `backflow_argocd_requests_total` | counter | `endpoint`, `code_class` | requests to the Argo CD API by endpoint and `2xx`, `4xx`, `5xx` or `error` (no answer) |
| `backflow_argocd_request_duration_seconds` | histogram | `endpoint` | duration of those requests |
| `backflow_scm_requests_total` | counter | `provider`, `host`, `code_class` | requests to the API of a Git host |
| `backflow_scm_request_duration_seconds` | histogram | `provider`, `host` | duration of those requests |
| `backflow_secret_reads_total` | counter | `result` | reads of a Secret from the API server (the short memory of the operator is not counted): `ok`, `forbidden`, `notfound`, `error` |

## Scraping

The endpoint is off until you set `metrics.enabled=true`: HTTPS on port 8443
of the Service `<release>-backflow-controller-manager-metrics`, with a
self-signed certificate unless `metrics.certSecret` names one, behind Kubernetes
authentication and authorization. A scraper needs the ClusterRole
`...-metrics-reader` (`get` on `/metrics`):

```bash
kubectl create clusterrolebinding backflow-metrics-reader \
  --clusterrole=<release>-backflow-controller-manager-metrics-reader \
  --serviceaccount=<prometheus namespace>:<prometheus service account>
```

- **Prometheus Operator:** `serviceMonitor.enabled=true`. Put the label your
  Prometheus selects ServiceMonitors by in `serviceMonitor.labels`. It scrapes
  with the token of its own ServiceAccount and does not verify the certificate
  (`serviceMonitor.tlsConfig`).
- **OpenShift:** enable user workload monitoring
  (`enableUserWorkload: true` in the `cluster-monitoring-config` ConfigMap of
  `openshift-monitoring`); it reads a ServiceMonitor and a PrometheusRule in the
  namespace of the operator, and its Prometheus ServiceAccount
  (`prometheus-user-workload` in `openshift-user-workload-monitoring`) is the one
  to bind to the metrics-reader role. If you also set `networkPolicy.enabled`,
  label that namespace with what `networkPolicy.metricsNamespaceSelector` selects.
- **Without the operator:** `curl -k -H "Authorization: Bearer $TOKEN"
  https://<pod>:8443/metrics` with a token that may `get` `/metrics`. For a
  local run, `go run ./cmd/main.go --metrics-bind-address=:8443 --metrics-secure=false`
  serves plain HTTP.

## Alerts

`prometheusRule.enabled=true` renders a PrometheusRule with these alerts. Put the
label your Prometheus selects rules by in `prometheusRule.labels`, and extra
labels for the alerts (a team, a routing key) in `prometheusRule.alertLabels`.
Every alert has a `summary`, a `description` and a `runbook` annotation that
points here.

### BackflowScmConnectionNotReady

`backflow_scm_connection_ready == 0` for 15 minutes. The Git host does not accept
the token (revoked, expired, wrong scopes), the Secret cannot be read, or the host
is down. `kubectl -n <ns> describe scm <name>` shows the reason; Backflow checks
again by itself, and as soon as the Secret or the connection changes.

### BackflowPolicyNotReady

`backflow_policy_ready == 0` for 15 minutes. No Application matches, an
`ScmConnection` is missing for a repository, two policies select the same
Application (`ApplicationConflict`), or the Argo CD token Secret is not readable.
The policy detects nothing meanwhile. `kubectl -n <ns> describe bfp <name>`.

### BackflowArgoCDUnreachable

More than half of the requests to the Argo CD API got no answer for 10 minutes.
Check the URL in `spec.argoCD.server`, the CA, a NetworkPolicy and the Argo CD server.
Wrong credentials are a `4xx`, not an `error`: look at
`sum by (endpoint, code_class) (rate(backflow_argocd_requests_total[5m]))`.

### BackflowRenderErrors

Renders through Argo CD ended in `result="error"` in the last 15 minutes: Argo CD
did not answer or refused (a token without `repositories, get`, a repo server that
is down). A chart that does not render is `result="failed"` and does not alert.
Proposals of Helm and Kustomize sources stay in `Mapping` and are retried.

### BackflowDetectionPausedLong

Drift detection for an Application has been paused for an hour. `reason` says why:
`GitAhead` or `ApplicationChanged` (the source changed since the last sync; it
resumes after a complete, successful sync), `Settling` (a sync is running or not
yet compared). If nothing syncs the Application, nothing resumes: sync it, or
look at why the sync does not finish. The Events of the policy say it too.

### BackflowPushRejected

A Git host declined a push of Backflow in the last 15 minutes: a protected branch
the token may not push to, a push hook, or the host having trouble. Backflow
retries, then marks the proposal `Failed` with `PushRejected`. Allow the token to
push the proposal branches (`git.branchPrefix`).

### BackflowScratchBranchesSwept

The sweeper deleted scratch branches (`backflow/scratch/*`) that a verification
left behind: the operator was stopped, or lost the Git host, in the middle of one.
Now and then that is expected; a stream of them means verifications are being
cut short, look at the operator's restarts and at `backflow_scratch_branches_total{action="push_failed"}`.

## Dashboard

`grafanaDashboard.enabled=true` renders a ConfigMap with the label
`grafana_dashboard: "1"`, which the sidecar of the Grafana and kube-prometheus-stack
charts loads. Change the label in `grafanaDashboard.labels` if your sidecar looks
for another, and `grafanaDashboard.namespace` if it watches one namespace. The
dashboard has an overview (what is Ready, drifts, proposals, mapping results,
merge requests), latency (verification, render, Argo CD and Git host requests),
dependencies (request classes, renders, scratch branches, Secret reads) and state
(connections, policies, paused detection, version). The JSON is
[charts/backflow/dashboards/backflow.json](../../charts/backflow/dashboards/backflow.json);
import it by hand if you do not use the sidecar.

## PromQL

```promql
# How long verification takes, by source type (p95)
histogram_quantile(0.95, sum by (le, source_type) (rate(backflow_verification_duration_seconds_bucket[15m])))

# What share of proposals ends Unmapped, and why
sum by (reason) (increase(backflow_mapping_results_total{reason!~"Mapped|PartiallyMapped"}[1d]))
  / scalar(sum(increase(backflow_mapping_results_total[1d])))

# Proposals waiting for a human
sum by (policy_namespace, policy) (backflow_proposals{phase=~"Proposed|Unmapped"})

# Drifts per application over a week
sum by (application) (increase(backflow_drifts_detected_total[7d]))

# Drifts that a controller made, not a person
sum by (application) (increase(backflow_drifts_detected_total{changed_by="controller"}[7d]))

# Merge requests opened and merged per day
sum by (action) (increase(backflow_merge_requests_total{action=~"opened|merged"}[1d]))

# Share of Argo CD requests that failed, by endpoint
sum by (endpoint) (rate(backflow_argocd_requests_total{code_class=~"5xx|error"}[5m]))
  / sum by (endpoint) (rate(backflow_argocd_requests_total[5m]))

# Slowest Git host API calls (p99)
histogram_quantile(0.99, sum by (le, host) (rate(backflow_scm_request_duration_seconds_bucket[15m])))

# Applications whose detection has been paused, and why
backflow_detection_paused == 1

# Which version runs
backflow_build_info
```
