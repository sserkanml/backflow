# Backflow

Reverse GitOps for Kubernetes and OpenShift. Backflow detects changes made
directly in a cluster to resources that Argo CD manages, and proposes them back
to the Git source of truth as merge requests against the file that owns the
resource.

Backflow never writes to the cluster: Argo CD is the only thing that applies
manifests. Backflow reads the cluster and writes to Git.

Backflow 1.0 is the first stable release: `backflow.io/v1` gets no breaking changes in 1.x.

### Install from OperatorHub (OpenShift)

Backflow is also an OLM operator, from a catalog of its own (it is not listed on OperatorHub.io): add a `CatalogSource`, and
subscribe to the package `backflow`, channel `stable`, install mode AllNamespaces. The settings that are Helm values in the chart are
`BACKFLOW_*` environment variables in `Subscription.spec.config.env` (the
[table](#configuration-without-helm-environment-variables) has both). **Add your self-hosted GitLab or GitHub to
`BACKFLOW_SCM_ALLOWED_HOSTS`**; the default is `gitlab.com,github.com`.

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata: {name: backflow-catalog, namespace: openshift-marketplace}
spec:
  sourceType: grpc
  image: ghcr.io/sserkanml/backflow-catalog:stable
  displayName: Backflow
  publisher: sserkanml
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata: {name: backflow, namespace: openshift-operators}
spec:
  channel: stable
  name: backflow
  source: backflow-catalog
  sourceNamespace: openshift-marketplace
  config:
    env:
      - {name: BACKFLOW_SCM_ALLOWED_HOSTS, value: "gitlab.example.com,gitlab.com"}
```

The full guide ([docs/olm.md](docs/olm.md)): the Argo CD connection of OpenShift GitOps, upgrades along the channel, why the actor
webhook is a separate opt-in there, and how to come from the Helm chart (uninstall the release, run `manager migrate` once for a
`v1alpha1` installation, then subscribe).

### Permissions

The operator never writes to the cluster. Everything it is allowed to do, and why:

| Resource | Verbs | Scope | Why |
|---|---|---|---|
| `applications.argoproj.io` | `get`, `list`, `watch` | cluster | find the Applications a policy selects and see when they drift or sync |
| `scmconnections`, `backflowpolicies`, `driftproposals` (`backflow.io`) | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` | cluster | its own resources; it creates and updates DriftProposals |
| their `/status` | `get`, `update`, `patch` | cluster | the controller that owns a resource writes its status |
| their `/finalizers` | `update` | cluster | cleanup of branches and merge requests when a proposal is deleted |
| `events` | `create`, `patch` | cluster | Events on policies and proposals |
| `secrets` | **`get` only** | every namespace, or the namespaces of `secretNamespaces` and of `argocd.tokenSecret` | the token of an ScmConnection, its CA bundle, the Argo CD token and CA bundle of a policy, the Argo CD token of `argocd.tokenSecret` |
| `leases`, `configmaps`, `events` | leader election | release namespace | only with `leaderElection` |
| `tokenreviews`, `subjectaccessreviews` | `create` | cluster | only with `metrics.enabled` (authentication and authorization of the metrics endpoint) |

Secrets are read **by name, straight from the API server**: the operator holds
no `list` or `watch` on them, keeps no informer for them, and so does not hold
every Secret of the cluster in memory. A Secret read stays in memory for
`secretCacheTTL` (30s) and is never logged.

By default the `get` on Secrets is a ClusterRole rule. To narrow it to the
namespaces where your ScmConnections and policies live, list them:

```bash
helm upgrade --install backflow oci://ghcr.io/sserkanml/charts/backflow -n backflow-system \
  --set 'secretNamespaces={team-a,team-b}'
```

The cluster-wide rule is then gone: the chart creates a Role and a RoleBinding
that grant `get` on Secrets in each listed namespace and in the release
namespace. An ScmConnection whose token Secret is in another namespace is not
`Ready`, with the reason `SecretForbidden` and a message that names the namespace
and the chart value. A BackflowPolicy whose Argo CD token is in another namespace
cannot reach Argo CD; its proposals are held and the message names the namespace.

**Token rotation.** Secrets are not watched, so a changed Secret is noticed at
the next read, not at once:

- An ScmConnection is verified again every `scmRecheckInterval` (default `5m`,
  the operator flag `--scm-recheck-interval`). A rotated token or CA bundle is
  therefore picked up within that interval, and `Ready` turns False if the old
  token stops working. Changing the ScmConnection itself verifies it at once.
- A proposal that is held because its credentials were refused retries with a
  growing delay (5 seconds to 10 minutes) and reads the Secret again each time.
- The Argo CD token is read when it is used, and kept for `secretCacheTTL` (30s,
  `--secret-cache-ttl`), and dropped as soon as Argo CD answers 401, so a rotated
  token is used within about 30 seconds.

### Multi-tenancy and access

Backflow reads Applications, and the live values and diffs of what they manage, from
Argo CD. Who may *see* an Application through a BackflowPolicy is decided in two ways,
one for each way a policy reaches Argo CD. Anyone who can create a BackflowPolicy in a
namespace is limited by them; the policy itself cannot widen them.

**A policy with its own `spec.argoCD.server`** (a token Secret in its namespace). It sees the
Applications that token can get from Argo CD, and nothing else: for each selected
Application the operator asks Argo CD, with that token, whether it may get it (the answer
is kept for a minute per token and Application). Argo CD answers 403 for an Application
that exists and one that does not, so a refused one is hidden either way. Which
Applications the token may get is the Argo CD RBAC of that account.

If Argo CD cannot be reached, the policy fails closed: an Application whose visibility was
confirmed earlier stays visible, however old the answer, and one that was never confirmed
is not shown (it is counted in `status.argoCD.hiddenApplications` with the reason `VisibilityUnknown`,
and the `Ready` message says the check is retried, every 30 seconds). Only a definite
401, 403 or 404 hides an Application or retires its proposals; an outage never does. The
confirmed answers live in the operator's memory, so after a restart during an outage
nothing is shown until Argo CD answers.

**A policy without `spec.argoCD.server`** uses the operator's default connection, whose account
can usually read every Application. Which Applications such a policy may match is
defined by the administrator who installs the operator, never by a policy:

```yaml
# values.yaml of the chart (the flag --argocd-default-access, once per rule)
argocd:
  server: https://argocd-server.argocd.svc
  tokenSecret: {namespace: argocd, name: backflow-local-user}
  defaultAccess:
    - policyNamespaces: [team-a]      # the namespaces of the policies; "*" is any
      projects: [team-a, shared]      # Argo CD projects (spec.project); "*" is any
    - policyNamespaces: [platform]
      projects: ["*"]
```

- A policy may match an Application only if its Argo CD project is allowed for the
  policy's namespace by some rule. Rules add up; a rule allows the pairs of its
  namespaces and its projects. An Application without `spec.project` is in `default`.
- **No rules (the default):** only the policies in the namespace the operator is
  installed in may use the default connection, for every project. This is the
  single-installer setup of Install on OpenShift GitOps. With rules, only what they allow
  is allowed, the operator's own namespace included (add it to a rule if it needs one).
- A policy in a namespace no rule covers is not `Ready`: reason
  `ArgoCDDefaultNotAllowed`, and a message that names the chart value. It matches nothing,
  and the proposals it had are retired.
- **Single-tenant cluster** that wants every policy to see everything:
  `defaultAccess: [{policyNamespaces: ["*"], projects: ["*"]}]`.

**What a policy is not told.** An Application a policy may not see never appears by
name, repository, path or project in its status, in its proposals, in Events, in the
operator's log at the default level or in a metric label. The status holds a count and a
reason only:

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: Resolved
      message: "Watching 1 application(s); Argo CD API: operator default (https://argocd-server.argocd.svc); 2 selected application(s) excluded: their Argo CD project is not allowed for this namespace (argocd.defaultAccess)"
  applicationCount: 1
  applications:
    - name: team-a-web
      managedResources: 3
      sources: [{position: 1, repoURL: "https://gitlab.com/team-a/web.git", type: Directory}]
  argoCD:
    source: OperatorDefault
    url: https://argocd-server.argocd.svc
    hiddenApplications:
      - reason: ProjectNotAllowed       # or NotVisibleToToken, VisibilityUnknown
        count: 2
```

The count does show that a name in `spec.applications.names` or a label selector matched
something the policy may not see; it is the one thing a policy can learn. A parent
Application (the app-of-apps behind a value set in an Application) that the policy may not
see is reported as `NotVisible` with the sentence "the parent Application is not visible
to this policy", and nothing else about it.

**Existing proposals.** When an Application stops being visible to its policy (a rule
removed, a project changed, a token that lost the permission), its open proposals end as
`Superseded` with the reason `AccessRevoked`, without a call to Argo CD. The rule is also
checked again before drift detection and before every proposal reads from Argo CD with the
default token, whatever the policy's status says.

### Upgrade and uninstall

```bash
# CRDs are not touched by helm upgrade: apply the ones of the new version first
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <new> | kubectl apply --server-side --force-conflicts -f -
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <new> -n backflow-system

helm uninstall backflow -n backflow-system
# the CRDs, and with them every ScmConnection, BackflowPolicy and DriftProposal, stay:
kubectl delete crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io
```

Helm installs the files in `crds/` once and never upgrades or removes them, so
the upgrade step above is yours to run. Deleting a CRD deletes all its objects.

**Upgrading to the version that adds `argocd.defaultAccess` (breaking).** Policies without
`spec.argoCD.server` used to see every Application through the operator's default connection. From
this version the default connection needs `argocd.defaultAccess` unless the policies live in
the operator's own namespace: a policy in any other namespace is not `Ready`
(`ArgoCDDefaultNotAllowed`) and its open proposals are retired until you allow it. Set the
rules before you upgrade, for example `--set 'argocd.defaultAccess[0].policyNamespaces={team-a}'
--set 'argocd.defaultAccess[0].projects={team-a}'`, or `*`/`*` to keep the old behaviour in a
single-tenant cluster. Policies with their own `spec.argoCD.server` keep working if their token can
get the Applications they select; the ones it cannot get disappear from their status
(`status.argoCD.hiddenApplications` counts them).

### Upgrading to 1.0 (`backflow.io/v1`)

The API is `backflow.io/v1` from 1.0 (`ScmConnection`, `BackflowPolicy`, `DriftProposal`). Its schema is the one of `v1beta1`, which 0.18 shipped,
without a change; `v1beta1` stays served, deprecated, until 2.0 ([compatibility policy](docs/compatibility.md)). `v1alpha1`, the API of 0.17 and
earlier, is gone. What the fields of `v1beta1` are, old to new from `v1alpha1`, is in
[docs/design/api-v1beta1.md](docs/design/api-v1beta1.md). Things you will notice when you come from 0.17:

- `spec.argoCDNamespace` and `spec.argoCD` are `spec.argoCD.namespace` and `spec.argoCD.server`; `spec.mergeRequest` is
  `spec.git` (and `spec.git.mergeRequest`); `provider` is `GitLab` or `GitHub`.
- A `DriftProposal` has `spec.policyRef`, `applicationRef`, `resourceRef`, one list `spec.sources` (a single-source
  Application has one entry), `spec.actor` for the user and `spec.managers` for the field managers; its status groups
  what it did in Git under `status.git`.
- Finished proposals (merged, rejected, reverted, superseded, failed) are **deleted after 14 days**
  (`spec.retention.finishedProposals`, `0s` keeps them); open and `Unmapped` proposals never are. Deleting a policy deletes its
  proposals; a merge request that is open on the Git host stays open and is no longer tracked.
- A drift with more than 256 field changes becomes an `Unmapped` proposal (`TooManyChanges`) that lists the first 256
  (`spec.truncated: true`).
- `ChangeRecord` is `internal.backflow.io/v1alpha1`, not part of the API.
- Kubernetes **1.30** (OpenShift 4.17) is the minimum: the CRDs use CEL validation and rely on ratcheting of existing objects.

Objects stored in an older version are moved to `v1` once with `manager migrate`:

- **From 0.18.x** (`v1beta1`): each object is stored again as `v1` with an update that changes nothing. This is safe **with the operator running**,
  needs no backup, and can be done after the upgrade; until then the operator works, logs a warning and sets the metric
  `backflow_storage_migration_pending`.
- **From 0.17 and earlier** (`v1alpha1`): the operator must be stopped, a backup is written, and **the migration runs before you apply the CRDs of
  this version** (it adds `v1` to the installed CRDs itself; the API server refuses the new CRDs while objects are stored as `v1alpha1`).
  The operator of 1.0 refuses to start on `v1alpha1` objects.

The way, for Helm (with the chart's `migration.enabled` hook Job, or by hand), kustomize and OLM, how to convert the manifests in Git
(`manager migrate convert-files`), how to check the result and how to roll back is in **[docs/upgrade-1.0.md](docs/upgrade-1.0.md)**.

Never edit a Backflow object through `v1alpha1` after the CRD stores `v1`: the write is pruned to the `v1` schema and loses fields. On a cluster
with no Backflow objects, delete the CRDs (and `changerecords.backflow.io`) and apply the new ones.

### Upgrading: apply the CRDs first

(The one exception is an upgrade from 0.17 or earlier: migrate first, see above.)

**Before every `helm upgrade`, apply the CRDs of the version you upgrade to:**

```bash
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <version> | kubectl apply --server-side --force-conflicts -f -
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <version> -n backflow-system
```

`--server-side --force-conflicts` lets the apply take over fields that an earlier
`kubectl apply` or `kubectl replace` of the CRDs owns; without it the apply can
stop on a field-ownership conflict.

Helm installs the files in the chart's `crds/` once and never upgrades them, so
`helm upgrade` leaves the CRDs of the old version in place, and an API server
with an old CRD silently drops the fields it does not know. New versions add
fields, and the operator then runs with a CRD that cannot hold them:

- 0.2.0 added `status.pushRejections` and `status.lastPushRejectedAt` to the
  `DriftProposal` (the count of pushes a Git server declined). The operator
  notices: when it records a declined push and the count does not come back, it
  emits a Warning Event `CRDsOutdated` and fails that proposal with
  `PushRejected` instead of retrying for ever.
- 0.3.0 adds `spec.helm.allowCollateralChanges` to the `BackflowPolicy` and
  `status.mapping.collateral` to the `DriftProposal`. With the old CRDs the
  policy field is dropped, so label and annotation changes are never allowed and
  such proposals stay `Unmapped`, and a mapping would not keep its list of
  collateral changes.

- 0.4.0 adds `spec.mapping.allowPartial` to the `BackflowPolicy` and
  `status.mapping.unmapped` to the `DriftProposal` (partial proposals, see
  below). With the old CRDs the policy field is dropped, so the option stays
  off and a proposal with a field that cannot be mapped stays `Unmapped`, as
  before; nothing breaks, the feature is just unavailable.

- 0.5.0 adds `spec.helm.allowValueOverrides` to the `BackflowPolicy`. With the old CRD
  the field is dropped and the default (on) applies, so you cannot turn
  overrides off until the CRDs are applied. **This changes what proposals
  become:** a Helm change whose old value only the chart's defaults hold used to
  be `Unmapped` and now becomes a merge request that adds a key to your last
  value file. Set `spec.helm.allowValueOverrides: false` on the policy to keep the old
  behaviour.

Applying the CRDs afterwards fixes new proposals; a proposal that failed is not
retried.

## Architecture and security model

**What it is.** One Deployment (the operator, `/manager`) with three controllers and a few helpers, and three custom resources of the
group `backflow.io` (`v1`): `ScmConnection` (a Git host and the Secret with its token), `BackflowPolicy` (which Argo CD Applications to watch and how to
propose) and `DriftProposal` (one captured drift and its journey, made by the operator; its spec is an immutable observation). A fourth,
`ChangeRecord` of `internal.backflow.io`, is private to the optional actor webhook.

**What it does.** For an Application a policy selects, it reads the Application (`status.sync`) from the cluster and, through Argo CD's REST API, the
manifests Argo CD renders. A difference between the rendered (desired) state and the live one is a drift. It traces each changed field back to the
file that owns it in Git (plain manifests, Kustomize or Helm values, with several sources), writes the edit to a branch, and **verifies the mapping by
asking Argo CD to render the edited source again**: only an edit that reproduces the live state exactly becomes a merge request (or a direct
commit, if the policy says so). Anything else is `Unmapped` and left for a person. The result is a merge request; a person decides.

**What it never does.**

- **It never writes to the cluster.** Argo CD is the only thing that applies manifests. The operator's ClusterRole can read the cluster and write
  its own resources (`backflow.io`, `internal.backflow.io`), nothing else.
- **It never guesses a mapping** and never reaches a Git host the installer did not allow.
- It never lists or watches Secrets: it **gets** the few it needs **by name**, straight from the API server (so the only permission is `get`), and only
  in the namespace of the object that names them (and, for the Argo CD token of the installation, the one the installer named). `secretNamespaces`
  narrows even that.

**The trust boundaries.**

| Who | Can | Cannot |
|---|---|---|
| The installer (Helm values, flags) | set the Argo CD connection, the allow lists (`scm.allowedHosts`, `argocd.allowedServers`), who may use the default connection (`argocd.defaultAccess`), where Secrets are read | |
| A tenant (creates `ScmConnection` and `BackflowPolicy` in a namespace) | name a Git host and a token Secret of its namespace, select Applications | name a host or an Argo CD server the installer did not allow (the object is not `Ready`, no request is made, the Secret is not read); read a Secret of another namespace; see an Argo CD project `defaultAccess` does not give its namespace |
| Argo CD | apply manifests; decide what its token sees | be written to by Backflow |
| The Git host token | push a branch and open a merge request | do more than the token's scopes |
| The optional actor webhook | see updates of tracked resources (not Secrets, not dry runs, not system or GitOps users) and record who made them | deny anything (it is observe-only: failure policy `Ignore`, always answers allowed, a one second timeout) |

**Data it keeps.** Proposals hold the changed fields (desired and live values, which may be sensitive: Secrets are excluded by default), the
mapping and its status. With the actor webhook on, user names and groups go into `ChangeRecord`s for two hours (`actorWebhook.retention`) and, if
`recordUsername` is on, into the proposals; `usernameInMergeRequest` puts a name into the text of a merge request, which Git keeps for ever (off
by default). Finished proposals are deleted after 14 days by default. Tokens are never logged, put in a condition, an event, a metric or a commit.

**The pod.** Non-root, a read-only root file system, no capabilities, `seccompProfile: RuntimeDefault` (the Pod Security `restricted` profile); the
Git cache is an `emptyDir`. The metrics endpoint is served over TLS and protected by token review. A reported vulnerability goes through
[SECURITY.md](SECURITY.md); what the version number promises is in [docs/compatibility.md](docs/compatibility.md).

**Compatibility and support.** Semantic versioning from 1.0: `backflow.io/v1` gets no breaking change in 1.x, `v1beta1` is served but deprecated
until 2.0, and a deprecation is announced one minor release ahead. Backflow has a single maintainer: **only the latest minor release of 1.x gets fixes**
(no backports; upgrading within 1.x is safe by the rule above). A security report is acknowledged within five business days, fixed as soon as
reasonably possible, and disclosed in coordination within 90 days at most. The details, and the support matrix (Kubernetes, OpenShift, Argo CD, GitLab and GitHub,
with what CI tests), are in [docs/compatibility.md](docs/compatibility.md) and [SECURITY.md](SECURITY.md).

## Metrics and alerts

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
| `backflow_drifts_detected_total` | counter | `policy_namespace`, `policy`, `application`, `source_type`, `changed_by` | drifts that became a proposal; `changed_by` is `interactive` (a known tool a person runs), `controller` (a known controller), `unknown` (a manager that is neither) or `none` (no manager could be named); see [Who changed it](#who-changed-it) |
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

### Scraping

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

### Alerts

`prometheusRule.enabled=true` renders a PrometheusRule with these alerts. Put the
label your Prometheus selects rules by in `prometheusRule.labels`, and extra
labels for the alerts (a team, a routing key) in `prometheusRule.alertLabels`.
Every alert has a `summary`, a `description` and a `runbook` annotation that
points here.

#### BackflowScmConnectionNotReady

`backflow_scm_connection_ready == 0` for 15 minutes. The Git host does not accept
the token (revoked, expired, wrong scopes), the Secret cannot be read, or the host
is down. `kubectl -n <ns> describe scm <name>` shows the reason; Backflow checks
again by itself, and as soon as the Secret or the connection changes.

#### BackflowPolicyNotReady

`backflow_policy_ready == 0` for 15 minutes. No Application matches, an
`ScmConnection` is missing for a repository, two policies select the same
Application (`ApplicationConflict`), or the Argo CD token Secret is not readable.
The policy detects nothing meanwhile. `kubectl -n <ns> describe bfp <name>`.

#### BackflowArgoCDUnreachable

More than half of the requests to the Argo CD API got no answer for 10 minutes.
Check the URL in `spec.argoCD.server`, the CA, a NetworkPolicy and the Argo CD server.
Wrong credentials are a `4xx`, not an `error`: look at
`sum by (endpoint, code_class) (rate(backflow_argocd_requests_total[5m]))`.

#### BackflowRenderErrors

Renders through Argo CD ended in `result="error"` in the last 15 minutes: Argo CD
did not answer or refused (a token without `repositories, get`, a repo server that
is down). A chart that does not render is `result="failed"` and does not alert.
Proposals of Helm and Kustomize sources stay in `Mapping` and are retried.

#### BackflowDetectionPausedLong

Drift detection for an Application has been paused for an hour. `reason` says why:
`GitAhead` or `ApplicationChanged` (the source changed since the last sync; it
resumes after a complete, successful sync), `Settling` (a sync is running or not
yet compared). If nothing syncs the Application, nothing resumes: sync it, or
look at why the sync does not finish. The Events of the policy say it too.

#### BackflowPushRejected

A Git host declined a push of Backflow in the last 15 minutes: a protected branch
the token may not push to, a push hook, or the host having trouble. Backflow
retries, then marks the proposal `Failed` with `PushRejected`. Allow the token to
push the proposal branches (`git.branchPrefix`).

#### BackflowScratchBranchesSwept

The sweeper deleted scratch branches (`backflow/scratch/*`) that a verification
left behind: the operator was stopped, or lost the Git host, in the middle of one.
Now and then that is expected; a stream of them means verifications are being
cut short, look at the operator's restarts and at `backflow_scratch_branches_total{action="push_failed"}`.

### Dashboard

`grafanaDashboard.enabled=true` renders a ConfigMap with the label
`grafana_dashboard: "1"`, which the sidecar of the Grafana and kube-prometheus-stack
charts loads. Change the label in `grafanaDashboard.labels` if your sidecar looks
for another, and `grafanaDashboard.namespace` if it watches one namespace. The
dashboard has an overview (what is Ready, drifts, proposals, mapping results,
merge requests), latency (verification, render, Argo CD and Git host requests),
dependencies (request classes, renders, scratch branches, Secret reads) and state
(connections, policies, paused detection, version). The JSON is
[charts/backflow/dashboards/backflow.json](charts/backflow/dashboards/backflow.json);
import it by hand if you do not use the sidecar.

### PromQL

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

## Development

```bash
./hack/dev-up.sh        # kind cluster "backflow", Argo CD, CRDs, demo-app
make run                # run the operator locally against the current kube context
make test               # unit and envtest tests
make lint               # golangci-lint
make coverage           # how much real-chart and overlay drift maps back to Git (needs helm; installs kustomize; see test/coverage/REPORT.md)
make helm-sync          # copy the CRDs and RBAC rules into charts/backflow after make manifests
```

CI uses Helm v4.2.0 (`.github/workflows/test.yml`). The chart tests compare the
rendered objects, not the text, so they also pass with Helm 3.

To try a local build in the cluster instead of the released image:

```bash
make docker-build IMG=backflow:dev
kind load docker-image backflow:dev --name backflow
helm install backflow charts/backflow -n backflow-system \
  --set image.repository=backflow --set image.tag=dev --set image.pullPolicy=Never
```

The end-to-end scenarios run against a local `bin/manager`, or against the
operator deployed in the cluster with `IN_CLUSTER=true`:

```bash
IN_CLUSTER=true ./hack/test-scm.sh
IN_CLUSTER=true ./hack/test-backflow-policy.sh
IN_CLUSTER=true ./hack/test-drift.sh
IN_CLUSTER=true ./hack/test-mapping.sh
IN_CLUSTER=true ./hack/test-kustomize.sh    # needs kustomize/ in the demo repository (dev-up.sh copies it)
```

### End-to-end tests against GitLab or GitHub

The scenarios that push branches and merge merge requests
(`test-merge-request.sh`, `test-helm.sh`, `test-kustomize.sh`,
`test-multi-source.sh`) run against a demo repository on GitLab or on GitHub.
`SCM=gitlab` (the default) or `SCM=github` picks the host; `DEMO_REPO` is the
repository, as `owner/name` or an https URL. Both are read by every `hack/`
script, and `hack/scm.py` (tested by `python3 hack/test_scm.py`, which needs no
network) is the one place that knows the two APIs.

| | GitLab | GitHub |
|---|---|---|
| `DEMO_REPO` | `sserkanml/backflow-demo` | `sserkanml/backflow-demo-gh` |
| token | `GITLAB_TOKEN`: `api` and `write_repository` | `GITHUB_TOKEN`: fine-grained, the demo repository only, Contents: read and write, Pull requests: read and write, Metadata: read |
| kind cluster | `backflow` | `backflow-gh` |

To run them against GitHub:

1. Create an empty **public** repository (the Applications and the read-only
   scripts read it without credentials; do not add a README or a license) and
   a fine-grained token with the permissions above, for that repository only. The
   token needs *Actions: read* as well if you want the check that no workflow run
   starts for a scratch branch; without it that one check is skipped with a
   message.
2. `SCM=github GITHUB_TOKEN=... DEMO_REPO=<owner>/<repo> ./hack/dev-up.sh` creates
   the cluster `backflow-gh`, copies the content of the GitLab demo repository
   into the empty one (only files it lacks; set `SEED_SOURCE` to copy from another
   public GitLab project) and points the demo Applications at it. The
   Applications have the same names as on GitLab, so the two demos live in two
   clusters and coexist; set `CLUSTER_NAME` to put both in one (the second
   `dev-up.sh` then re-points the Applications).
3. `SCM=github GITHUB_TOKEN=... DEMO_REPO=<owner>/<repo> ./hack/test-merge-request.sh`
   (and the others). `SECTIONS=a,b` runs only some sections.

The scripts need write access to the demo repository, not Maintainer: unlike the
GitLab project, `main` of the GitHub demo repository is not protected. Tokens are
read from the environment only and are never printed or written down.

### End-to-end tests in CI

`.github/workflows/e2e.yml` runs the same scripts in GitHub Actions, once against
GitLab and once against GitHub (two jobs, each with its own kind cluster made by
`hack/dev-up.sh`). `hack/ci-e2e.sh` is what the jobs run, and it works locally too
(`SCM=github SCRIPTS=helm ./hack/ci-e2e.sh plan`). The kubebuilder scenario in
`test-e2e.yml` (the operator deployed from `config/`, no secrets) is a separate,
cheap check that runs on every push.

**How a run starts.** The scripts push branches and merge merge requests in the
demo repositories, so only these start them:

- a pull request **of this repository** (not a fork) that carries the label `e2e`;
  it runs again on every push to the branch while the label is there
- the nightly schedule (02:17 UTC, the default branch)
- by hand, for example against GitHub, one script and one section:

```bash
gh workflow run e2e.yml -f scm=github -f scripts=merge-request -f sections=a,b
gh workflow run e2e.yml -f scm=gitlab -f scripts=helm,kustomize
gh workflow run e2e.yml -f scm=github -f argocd_path=policy -f scripts=merge-request   # the policy path on GitHub
gh run watch
```

`argocd_path` picks how the operator reaches Argo CD in the run: `policy` (every policy has
`spec.argoCD.server`), `default` (the connection the installer gave the operator, as with the
OpenShift GitOps local user: `dev-up.sh` is run with `ARGOCD_LOCAL_USER_SECRET=true`, the
operator is started with `--argocd-server` and `--argocd-token-secret-*`, and the policies
have no `spec.argoCD.server`) or `matrix` (the default, and what the nightly run does: GitLab
keeps `policy`, GitHub uses `default`, so both paths are tested every night). Only
`merge-request`, `helm`, `kustomize` and `multi-source` can use `default`; the other
scripts keep the policy path, and the summary of the run says which path each script and
each job used. Locally: `ARGOCD_DEFAULT=true ARGOCD_LOCAL_USER_SECRET=true`.
`scm` is `both` (default), `gitlab` or `github`; `scripts` is a comma separated
list of `policy`, `drift`, `mapping`, `application-values`, `scm`, `merge-request`,
`helm`, `kustomize`, `multi-source` (empty: all). The first four need no token and
run in the GitLab job only. `sections` becomes `SECTIONS` and is used only when
exactly one script is selected. Every script runs even when an earlier one failed;
the job fails at the end. A script that skips itself because the token is missing
counts as failed.

**Where the results are.** The summary of the run has a table per host with the
passed and failed checks and the duration of every script (and the time per
section). The logs of the scripts and of the operator are in the artifact
`e2e-logs-gitlab` or `e2e-logs-github`, kept for 7 days. Before they are uploaded
they are scrubbed, and the job fails if the value of a token (or its base64) was
in any of them.

**Two runs never touch one demo repository**: the jobs share a concurrency group
per host (`e2e-gitlab`, `e2e-github`) and wait for each other; a run that waits is
not cancelled by a newer one, but only one run can wait, so a third replaces the
second. Before and after the scripts (also when the run fails or is cancelled) the
job closes the open merge requests that carry the label `backflow-e2e` or come
from a `backflow/` branch and deletes the `backflow/` and `backflow/scratch/`
branches (`hack/scm.py cleanup`); `main` is never touched, and the scripts restore
its content themselves.

**What it needs.** The repository environment `e2e` with two secrets:

| Secret | Is | Scopes |
|---|---|---|
| `GITLAB_TOKEN` | a token for `gitlab.com/sserkanml/backflow-demo` | `api` and `write_repository`; its user is Maintainer of the project, because `main` is protected and the scripts restore it |
| `E2E_GITHUB` | a fine-grained token for `github.com/sserkanml/backflow-demo-gh` only | Contents: read and write, Pull requests: read and write, Metadata: read; add Actions: read to also check that no workflow run starts for a scratch branch |

GitHub does not allow secret names that start with `GITHUB_`, so the workflow
maps `E2E_GITHUB` to `GITHUB_TOKEN`, the name the scripts read, only in the `env`
of the steps that run the scripts, the cleanup, the scrub and `dev-up.sh`. The
secret of the other host is never read in a job, and no step passes a token to
`gh` or to another action. The workflow itself has `contents: read` and nothing else.
Because a pull request of this repository can change the scripts that then run with
the secrets, give the label `e2e` only to changes you have read, and protect the
environment (required reviewers, and the branches and pull requests that may
deploy to it).

**Rotating a token.** Create the new token with the same scopes, replace the
secret in Settings, Environments, `e2e` (or `gh secret set GITLAB_TOKEN --env e2e`),
start `gh workflow run e2e.yml -f scm=<host> -f scripts=scm` to check it (`scm` is the
cheapest script that uses it), and then revoke the old token. GitLab project tokens
and GitHub fine-grained tokens expire; put the expiry date in your calendar.

Releases are made by [release-please](https://github.com/googleapis/release-please)
from Conventional Commits on `main`; the tag it creates publishes the image and
the chart.

## Who changed it

`kubectl get dp` has a column **Changed by**: the field manager that wrote the drifted
fields last. Backflow reads it from `metadata.managedFields` of the live object that Argo CD
returns for the drift (Argo CD v3.5 includes `managedFields` there), so it needs no extra
permission on the cluster and no webhook. `spec.managers` holds it, the latest writer first:

```yaml
spec:
  managers:
    - manager: kubectl-patch
      operation: Update
      time: "2026-11-01T13:26:26Z"
      fields: [/data/LOG_LEVEL]
```

The merge request says it (`Changed with: kubectl-patch (Update) at 2026-11-01 13:26:26 UTC`)
and so does the commit (a `Changed-with:` trailer).

| What changed the resource | Manager |
|---|---|
| `kubectl patch`, `edit`, `set image`, `label`, `annotate` | `kubectl-patch`, `kubectl-edit`, `kubectl-set`, `kubectl-label`, `kubectl-annotate` |
| `kubectl scale` | `kubectl` (Update, on the `scale` subresource) |
| `kubectl apply` | `kubectl-client-side-apply` (Update); with `--server-side` `kubectl` (Apply) |
| a controller such as the HPA | for example `kube-controller-manager` |
| `oc`, a browser console | `oc` and the like, or the browser (`Mozilla`) — expected, not verified |
| Argo CD's sync | `argocd-controller` — never named as the actor |

The `kubectl` rows were observed on a kind cluster (kubectl 1.32). `oc`, the console and the HPA were
not: the names in those two rows are what the clients are expected to send. A manager that is not
recognised is not called a person and not called a controller either (see below).

How it is decided. For every drifted field the entry of `managedFields` that owns it is found
(list elements are matched by their merge key). Argo CD's managers (`argocd-*`) and Backflow
are left out, and the most recent remaining entry is the actor. When several managers wrote
different drifted fields, the latest is the first of `spec.managers`, with its paths in `fields`, and the
others (at most four more) follow it. A manager is put in one of three classes, by its name only:

- **interactive**: a tool a person runs: `kubectl*`, `oc` and `oc-*`, browsers (`Mozilla`, `Chrome`, ...),
  `k9s`, `lens`, `helm`, `kustomize`, `curl`, ...
- **controller**: a known controller: `kube-controller-manager` (which is what the HPA scales
  through), `kube-scheduler`, `kubelet`, `kube-apiserver`, `cloud-controller-manager`,
  `cluster-autoscaler`, and names that end in `-controller`, `-operator` or `-controller-manager`
  (`cert-manager-controller`, Flux's `kustomize-controller`, `openshift-controller-manager`,
  `cluster-version-operator`). Only for these does the merge request carry a note that the change was
  not made by a person and that a controller that keeps doing it will undo the merge (an HPA changing
  `spec.replicas`, say).
- **unknown**: every other name. The merge request only says `Changed with: <manager>`, with no
  note either way, because a wrong "not a person" would mislead the reviewer.

The metric `backflow_drifts_detected_total` counts the class in `changed_by` (`interactive`, `controller`,
`unknown`), and `none` for a drift for which no manager could be named.

What it cannot tell:

- **No username, without the actor webhook.** `managedFields` do not hold one, so `spec.actor`
  is not set, "Changed by: unknown" stays in the merge request and nobody is assigned
  (`git.mergeRequest.assignActor`). A username needs the identity of the request, which the optional
  [actor webhook](#the-actor-webhook-who-it-really-was) records.
- **A removed field has no owner.** When the change is a field that is gone from the live object,
  there is no entry to read, and the proposal has no actor.
- **It is a hint, not proof.** The name is set by the client (`--field-manager`, or the user agent): it can
  be anything, and anyone who can write to the object can pick any name, or the name of a controller. Read
  "Changed with" as where to look first. Who it really was needs the identity of the request, which the
  [actor webhook](#the-actor-webhook-who-it-really-was) records.
- **It is set when the proposal is created** and never changes (spec is immutable): a later change
  of the same field by someone else shows in a new proposal.
- **Nothing blocks on it.** An object without `managedFields`, or with ones that cannot be read, gives a
  proposal without an actor (logged at V(1)), counted as `changed_by="none"`.

### The actor webhook: who it really was

Off by default. With `actorWebhook.enabled=true` the chart installs an **observe-only admission
webhook** and the operator names the user of a change in `spec.actor` (`username`, `groups`, `kind`).
The apiserver sends it the request, including the identity it authenticated; the webhook
**always answers "allowed"**, keeps the paths of the fields that changed (never a value), and the
operator pairs that with the drift when it makes the proposal.

```bash
helm upgrade --install backflow oci://ghcr.io/sserkanml/charts/backflow -n backflow-system \
  --set actorWebhook.enabled=true \
  --set 'actorWebhook.networkPolicy.apiserverCIDRs={10.0.0.4/32}'   # not on OpenShift
```

```yaml
spec:
  actor:
    username: alice            # from the webhook, only when a record is confirmed (below)
    groups: [devs, system:authenticated]
    kind: person               # or controller (a service account)
    manager: kubectl-patch     # from managedFields, as before
    operation: Update
    time: "2026-11-01T13:26:26Z"
```

**It cannot block or slow the cluster.** The webhook is `UPDATE` only, `failurePolicy: Ignore`,
`sideEffects: None`, `timeoutSeconds: 1`, and its handler does no I/O: it parses the review, keeps the
changed paths in memory and answers. What reaches it is narrowed by the apiserver first
(`matchConditions`, Kubernetes 1.28 or later; the chart refuses older ones unless
`actorWebhook.allowUnfiltered=true`): only updates of resources Argo CD tracks (the
`argocd.argoproj.io/tracking-id` annotation or the `app.kubernetes.io/instance` label) and `scale`;
never Secrets, dry runs, status writes, the controllers of `kube-system`, nodes, the controller manager,
the Argo CD application controller (`actorWebhook.excludeUsers`) or Backflow itself. In a busy namespace
that was 9 calls out of 215 in the spike. A webhook that is down or unreachable costs nothing; one that is
*up but hung* costs the timeout (1 s) on each matched request, which is why the timeout is short and the
match narrow. The numbers and the evidence are in
[docs/design/actor-webhook.md](docs/design/actor-webhook.md).

**Where the records are kept.** Each replica serves the webhook (the Service reaches any ready pod) and
flushes what it saw every 2 seconds to `ChangeRecord` objects in the operator's namespace: one per
(object uid, user, field manager, subresource), the changed paths merged. They hold user names, so they are
for administrators, not for tenants (no tenant role is created for them; the operator has a Role in its own
namespace). The leader deletes them after `actorWebhook.retention` (2 hours) and the oldest beyond
`actorWebhook.maxRecords` (5000), counted in `backflow_actor_records_dropped_total`.

**A record is believed only if the cluster agrees.** When a proposal is made, the operator takes the
records of the object (by uid) and keeps one only if the live `managedFields` agree: the same field
manager, an entry written within `actorWebhook.tolerance` (10 s) of the record, and an entry that owns
the drifted path. A review nobody made, or one that names someone else, fails that and sets no name
(`backflow_actor_matches_total{result="manager_mismatch"|"time_mismatch"|"path_mismatch"}`). Without a
matching record the actor is what managedFields say, as without the webhook. The webhook never delays or
fails a proposal.

**Who can call it.** The webhook cannot tell who calls it (the apiserver does not authenticate to
admission webhooks unless the control plane is configured for it, which a chart cannot do). Three layers
instead: a NetworkPolicy that admits the API server only (on OpenShift the `host-network` policy group,
elsewhere `actorWebhook.networkPolicy.apiserverCIDRs`, which is required; a CNI that does not enforce
NetworkPolicy gives no protection); a random URL path, kept across upgrades; and the cross-check above,
which makes a forged record useless unless the forger also knows a real change's manager and time.
**Without the network policy any pod that can reach the port can post a forged review**, and could put a
different user name on a real change.

**TLS** (`actorWebhook.tls.mode`): `auto` uses the OpenShift service CA on OpenShift, else cert-manager if
its API is installed, else a certificate made by Helm (five years, kept across upgrades,
`actorWebhook.tls.renew=true` to replace it); or name `serviceCA`, `certManager`, `helm`, or `secret` (your
own `tls.secretName` and `tls.caBundle`). The webhook reads the certificate from files and picks up a
renewed one without a restart.

**Privacy.** User names are personal data.

- `actorWebhook.recordUsername=false` stores only `kind` (`person`, or `controller` for a service account):
  no name, no groups, in the records or in the proposal.
- `actorWebhook.usernameInMergeRequest` (default `false`) keeps the name out of the text of the merge
  request and the commit trailer, because Git keeps it for ever. The proposal still has it. A policy that
  sets `git.mergeRequest.assignActor` assigns the merge request to that user regardless, since that is what
  the option asks for.
- Names are never labels of a metric and are not logged at the default level. The records live two hours.

**What to watch.** The webhook fails open, so a broken one is silent. Alert on the apiserver's own metrics:

- `apiserver_admission_webhook_fail_open_count{name="<release>-backflow-controller-manager-actor-webhook"}`
  rising: the apiserver could not reach the webhook or it was too slow, and changes were not recorded.
- `apiserver_admission_match_condition_evaluation_errors_total` above zero for it: **a match condition
  failed to evaluate, which under `failurePolicy: Ignore` means the webhook is not called at all**. The
  chart's conditions are tested against sample requests (a field that is absent is not empty:
  `request.subResource == ''` is an error, `!has(request.subResource)` is not), but a custom
  `actorWebhook.excludeUsers` or namespace selector is yours to check.
- Backflow's own: `backflow_actor_webhook_calls_total` (`recorded`, `ignored`, `invalid`, `busy`),
  `backflow_actor_records_written_total`, `backflow_actor_records_dropped_total{reason}`,
  `backflow_actor_matches_total{result}`. No calls recorded while people patch tracked resources means the
  webhook is not reached.

**OpenShift.** On OpenShift 4.15 and later (Kubernetes 1.28, `matchConditions` in beta and on) the chart
uses the service CA (`service.beta.openshift.io/serving-cert-secret-name` on the Service and
`inject-cabundle` on the webhook configuration) and admits the API server through the
`policy-group.network.openshift.io/host-network` label; `matchConditions` is GA from 4.17. Add the
OpenShift GitOps application controller to `actorWebhook.excludeUsers`
(`system:serviceaccount:openshift-gitops:openshift-gitops-argocd-application-controller`). These OpenShift
details follow the documentation and were not run on a cluster; the kind spike and the chart tests were.

**What stays a limitation.**

- An admission request carries **no user agent** (the API server's own is all there is), so
  there is no user agent in the API (a field that was never filled was removed in v1beta1); the field manager is the hint for the tool.
- `kubectl scale` and `oc scale` go through the `scale` subresource. The request carries **no field
  manager**, and the managedFields entry the API server writes for it (manager from the user agent,
  `subresource: scale`) has **no time**, so a scale cannot be checked like other changes. A record without a
  manager is accepted only against such an entry: it has the record's subresource, belongs to the manager shown
  as the actor, owns the drifted path, and has the same object uid. When the entry has a time, the record must
  be within `actorWebhook.tolerance` of it, as usual. When it has none, **the history of the Application
  bounds the record instead**: only a record made after the last successful sync of the Application
  (`status.operationState.finishedAt` of a `Succeeded` operation, or the newest `status.history[].deployedAt`,
  whichever is later), which put the resource back to Git's state, and not after the proposal is made (plus the
  tolerance), is used. So a scale by Alice at 14:00 that Argo CD synced back at 14:05 cannot be attributed to a
  drift that appears at 15:00 when Bob's scale was not recorded. If no successful sync is known, nobody is named
  (`backflow_actor_matches_total{result="no_window"}`). If records of different users are in the window the
  result is `ambiguous` and nobody is named. The sync is the Application's last one, not necessarily one that
  touched the resource, so the window can start later than necessary; the error is always a name left out,
  never a wrong one. A record that has a manager keeps the strict check. This remains the weakest check of the
  feature: anyone who can post to the webhook port could name a user for a scale inside the window, which the
  NetworkPolicy is there to prevent. The Scale object has no annotations either, so a scale of any Deployment,
  StatefulSet or ReplicaSet in a watched namespace is recorded, tracked by Argo CD or not; only the records of a
  drifted, tracked object are ever read, and the others expire with the retention.
- A change made as a shared account (a CI token) is that account; impersonation shows the impersonated user.
- Changes made while the webhook, or the operator, is down are not named (fail open is the point).
- The records are in a pod's memory for up to the flush interval, so a crash can lose the last seconds.
- Kubernetes before 1.28 has no `matchConditions`; with `allowUnfiltered` every update of every namespaced
  resource reaches the webhook.

## Limitations

- **One Helm source, and Git ref sources, for several sources.** An Application
  with `spec.sources` in any other shape gets one `UnsupportedSource` Event and no
  proposals. See Helm charts from a Helm repository or a registry above.
- **Directory, Helm and Kustomize sources only.** Plugin sources are detected
  but their proposals end `Unmapped`. See Helm sources and Kustomize sources above
  for what mapping can and cannot do.
- **One operator instance is one trust domain.** Policies and connections of an
  instance share the repository cache and the operator can read the Secrets of
  every namespace in `secretNamespaces` (every namespace by default); run one
  instance per tenant that must be kept apart (see Permissions above).
- **YAML and JSON manifests.** Jsonnet cannot be edited, and Argo CD's Jsonnet
  output is not read at all, so a resource it generates is not found. See JSON files
  below for what is read as JSON.
- **Array style is re-rendered.** Changing a value inside a list re-renders
  the entry that holds it, so its indentation and flow or block style may
  differ from your file. Items cannot be added to or removed from lists.
- **Comments inside a re-rendered entry can be lost.** Only a changed scalar
  keeps its line and its trailing comment. When a change replaces a list or a
  map, adds a field to a flow map (`{}`) or removes a key from one, the whole
  entry is written again and comments between its lines are not kept.
- **Comments on removed keys are orphaned.** When a change removes a key, the
  comments attached to that key are left behind or dropped with it.
- **The repository cache is never pruned and holds full clones.** It lives in
  the volume mounted at `--repo-cache-dir` and only grows. Size it for your
  repositories, or restart the pod with an empty volume to clear it.
- **A rotated token is noticed after a delay.** Secrets are not watched (the
  operator cannot list them), so an ScmConnection finds out at its next
  verification, at most `scmRecheckInterval` (5 minutes by default) later. See
  Permissions above.
- **Open proposals of a blocked or not-Ready policy are left as they are.**
  When a policy stops matching an Application, conflicts with another policy
  or its connection is not Ready, proposals that are already open keep their
  phase; they are not closed or withdrawn.
- **While Git is ahead of the last sync of an Application (manual sync, or a
  failing sync), drift detection for that Application is paused, so Backflow
  never proposes to revert a commit.** For a Directory source, only commits
  that change files under the Application's directory count (respecting
  `directory.recurse`); a commit elsewhere in the repository does not pause
  anything. For a Helm source, only the files the render depends on count,
  evaluated at both revisions: the chart's files (`Chart.yaml`, `values.yaml`,
  `templates/`, `charts/`, `crds/`, ...), the Application's value files and
  `fileParameters` files wherever they are (globs expanded at each revision),
  `file://` dependency directories, and the other files of a chart directory
  only when a template uses `.Files` (honouring `.helmignore`). Another
  service's values file next to a shared chart, another environment's values
  file or a README does not pause detection. For Kustomize sources, and for
  Applications that were never synced, directories that contain a symbolic link
  or a Jsonnet file (or use `directory.jsonnet`) at either revision, a symbolic
  link in anything a Helm render reads, and when the repository cannot be read,
  every commit pauses detection, because the check cannot be trusted.
  When the last sync did not succeed or was selective (only some resources),
  only the resources it did not apply are held back: a resource counts as
  caught up when that sync was a full one that succeeded, or reports the
  resource itself as Synced. Detection for the whole Application is paused when
  its source (path, directory options, target revision, Helm, Kustomize,
  plugin) changed since the last sync. One Event on the policy says so, once
  per resource and revision. Proposals that are already open are left as they are.
  Detection is also held back while a sync is running and until Argo CD has
  compared again after it.
- **Argo CD's automated sync can reset a drift before Backflow sees it.** With
  automated sync on and `selfHeal` off, Argo CD syncs each new revision once, the
  first time the Application is OutOfSync at it. After a commit that does not
  change an Application (a file elsewhere in the repository, or another
  environment's values file), the Application stays Synced, but its last sync
  is still of an older revision. The next live change then makes it OutOfSync,
  and Argo CD's automated sync applies the new revision, which also resets that
  live change, usually within a second. Backflow sees the drift disappear and
  makes no proposal, by design: there is nothing left to propose. In the
  operator log (V(1)) this looks like the Application "ahead of the last sync"
  and a resource OutOfSync with nothing that differs. The Application's
  `status.history` shows the automated sync at that moment. A manual sync of the
  current revision (or the next drift) is what makes later drifts stay.
- **A drift becomes a proposal only after it has been stable.** It must be
  seen with the same changes at the same synced and compared revision for the
  policy's `spec.batchWindow` (default 30s), across at least two observations.
  A drift that disappears within the window is never proposed, and several quick
  edits to a resource give one proposal for the final state. This also covers
  the moment after a sync, when Argo CD can still show a resource as OutOfSync.
  What was seen is kept in memory; after a restart the window starts again,
  which only delays a proposal.
- **A merged change is not proposed again until Argo CD sees the merge.** Argo CD
  polls Git (about every three minutes) and until it compares against the merge
  commit the resource still looks OutOfSync with the same changes at the same
  revision. A `Merged` proposal blocks an identical one at that revision, as a
  `Rejected` one does; a new revision lifts the block.
