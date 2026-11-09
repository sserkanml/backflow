# Backflow

Reverse GitOps for Kubernetes and OpenShift. Backflow detects changes made
directly in a cluster to resources that Argo CD manages, and proposes them back
to the Git source of truth as merge requests against the file that owns the
resource.

Backflow never writes to the cluster: Argo CD is the only thing that applies
manifests. Backflow reads the cluster and writes to Git.

Backflow 1.0 is the first stable release: `backflow.io/v1` gets no breaking changes in 1.x.

## Install

Backflow runs in the cluster as one Deployment. Install it with a Helm chart (Helm 3.8 or later), or, on OpenShift, from
OperatorHub through the catalog of the project ([Install from OperatorHub (OpenShift)](#install-from-operatorhub-openshift)).

With Helm:

```bash
helm install backflow oci://ghcr.io/sserkanml/charts/backflow \
  --version 0.1.0 --namespace backflow-system --create-namespace
```

The chart installs the CRDs (`ScmConnection`, `BackflowPolicy`,
`DriftProposal`), a ClusterRole, the Deployment and, with
`leaderElection: true` (the default), a leader election Role. The image is
`ghcr.io/sserkanml/backflow`, for linux/amd64 and linux/arm64.

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

### Install on OpenShift GitOps

OpenShift GitOps (1.18 or later) can manage a local Argo CD user from the `ArgoCD`
resource: it creates the account, a token, and the Secret `<name>-local-user` (key
`apiToken`) in the Argo CD namespace, and renews the token before it expires. With it
there is no token to generate by hand and no CA to copy.

**1. The account.** Add this to the `ArgoCD` resource (the default instance is
`openshift-gitops` in the namespace `openshift-gitops`). The `rbac.policy` lines are
added to the ones you have: do not replace a policy that is already there.

```yaml
apiVersion: argoproj.io/v1beta1
kind: ArgoCD
metadata:
  name: openshift-gitops
  namespace: openshift-gitops
spec:
  localUsers:
    - name: backflow
      apiKey: true
      tokenLifetime: 720h
      autoRenewToken: true
  rbac:
    policy: |
      p, role:backflow, applications, get, */*, allow
      p, role:backflow, repositories, get, *, allow
      g, backflow, role:backflow
```

`applications, get` is what drift detection needs; `repositories, get` lets Backflow
read the values of a chart in a registry for Applications with several sources.
Wait for the Secret: `kubectl -n openshift-gitops get secret backflow-local-user`.

**2. The operator.** One install; the connection is given to the *operator*, not to each
policy:

```bash
helm upgrade --install backflow oci://ghcr.io/sserkanml/charts/backflow \
  -n backflow-system --create-namespace \
  --set argocd.server=https://openshift-gitops-server.openshift-gitops.svc \
  --set argocd.tokenSecret.namespace=openshift-gitops \
  --set argocd.tokenSecret.name=backflow-local-user \
  --set argocd.serviceCA=true \
  --set trustedCABundle=true
```

- `argocd.*` become the flags `--argocd-server`, `--argocd-token-secret-namespace`,
  `--argocd-token-secret-name`, `--argocd-token-secret-key` (default `apiToken`) and
  `--argocd-ca-file`. They are set by whoever installs the operator and by nobody else: a
  policy can only name Secrets of its own namespace, so a user who may create policies
  cannot make the operator read a Secret in another one.
- With `secretNamespaces` set, the chart also grants `get` on Secrets in the namespace of
  `argocd.tokenSecret` (here `openshift-gitops`), next to the release namespace.
- `argocd.serviceCA=true` creates a ConfigMap annotated
  `service.beta.openshift.io/inject-cabundle: "true"`, which OpenShift fills with the
  service CA, mounts it, and verifies the server with it (`openshift-gitops-server` has a
  certificate signed by the service CA). No Secret holds a CA.
- `trustedCABundle=true` creates a ConfigMap labelled
  `config.openshift.io/inject-trusted-cabundle: "true"`, filled with the cluster's trusted
  CA bundle. Its roots are added for the connection to GitLab and GitHub of every
  `ScmConnection` that has no `caSecretRef` (one that has keeps its own). It helps when your
  Git host or proxy uses a company CA **and that CA is in the cluster's trusted CA bundle**
  (`spec.trustedCA` of the cluster `Proxy`, `oc get proxy/cluster -o yaml`). It does not
  add a CA by itself.
- Both files are read again when they change (within ten seconds); a changed CA needs no
  restart. A bundle that is missing or holds no certificate is an error, not the old CA.

**3. A policy without any Argo CD field.** (In the namespace the operator is installed in,
as here; in any other namespace the administrator allows it with `argocd.defaultAccess`, see
[Multi-tenancy and access](#multi-tenancy-and-access).)

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
metadata:
  name: my-apps
  namespace: backflow-system
spec:
  argoCD:
    namespace: openshift-gitops
  applications:
    names: [my-app]
  mode: ReportOnly
```

`kubectl -n backflow-system get bfp my-apps -o jsonpath='{.status.argoCD}'` says where
the connection comes from (`source: OperatorDefault`, and the URL, never a token), and the
`Ready` message says it too. A policy with its own `spec.argoCD.server` keeps using that, as
described under [Install](#install); a policy that has none and an operator without
`argocd.server` detects no drift. The `ScmConnection` for GitLab or GitHub is made as
in step 2 of Install.

**Token renewal.** OpenShift GitOps writes the new token into the same Secret before the old
one expires. The operator reads the Secret by name when it needs it and keeps it for
`secretCacheTTL` (30s). When Argo CD answers 401, it drops that copy, reads the Secret
again and repeats the call once with what is there now; a proposal being mapped retries
the same way. So the renewal is picked up within the TTL whether or not Argo CD rejects the
old token at once, and an Argo CD that really refuses the token is reported as before
(Event `ArgoCDUnauthorized`).

### Values

The most used values of the chart; all of them are in
[charts/backflow/values.yaml](charts/backflow/values.yaml).

| Value | Default | |
|---|---|---|
| `image.repository`, `image.tag` | `ghcr.io/sserkanml/backflow`, the chart's `appVersion` | |
| `imagePullSecrets` | `[]` | for a mirror that needs credentials |
| `replicaCount`, `leaderElection` | `1`, `true` | more than one replica needs leader election |
| `mergeRequestPollInterval` | `2m` | how often an open merge request is polled |
| `resources` | 50m/128Mi requests, 500m/512Mi limits | tune to the number of Applications |
| `argocd.defaultAccess` | `[]` | which Applications (Argo CD projects) the policies of which namespaces may match through the default connection; empty: only the operator's namespace (see Multi-tenancy and access) |
| `argocd.allowedServers` | `[]` | the Argo CD servers a policy may name in `spec.argoCD.server.url`, besides `argocd.server`, which is always allowed; another server makes the policy not `Ready` (`ServerNotAllowed`) and nothing is requested from it (see [Allowed servers and hosts](#allowed-servers-and-hosts)) |
| `scm.allowedHosts` | `[gitlab.com, github.com]` | the Git hosts a `ScmConnection` may name in `spec.url`; another host makes it not `Ready` (`HostNotAllowed`); set your self-hosted GitLab or GitHub here |
| `actorWebhook.enabled` | `false` | the observe-only admission webhook that names the user of a change (see [The actor webhook](#the-actor-webhook-who-it-really-was)); needs Kubernetes 1.28 and, outside OpenShift, `actorWebhook.networkPolicy.apiserverCIDRs` |
| `actorWebhook.recordUsername`, `.usernameInMergeRequest` | `true`, `false` | store user names and groups (false: only person or controller); put the name into merge requests and commits |
| `actorWebhook.retention`, `.tolerance` | `2h`, `10s` | how long a ChangeRecord is kept; how far its time may be from the managedFields entry it is checked against |
| `secretNamespaces` | `[]` | namespaces where the operator may read Secrets; empty means every namespace (see Permissions) |
| `scmRecheckInterval` | `5m` | how often a working ScmConnection is verified again; also how long a rotated token goes unnoticed |
| `secretCacheTTL` | `30s` | how long a Secret that was read stays in memory |
| `repoCache.sizeLimit` | `2Gi` | the `emptyDir` Git repositories are cached on |
| `extraArgs` | `[]` | extra operator flags |
| `podSecurityContext`, `securityContext` | restricted, no fixed IDs | see OpenShift below |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | empty | |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, generated, `{}` | |
| `metrics.enabled`, `metrics.certSecret` | `false`, none | HTTPS on 8443 behind Kubernetes authn/authz |
| `argocd.server`, `argocd.tokenSecret.{namespace,name,key}` | empty, `apiToken` | the Argo CD connection of every policy without `spec.argoCD.server` (see [Install on OpenShift GitOps](#install-on-openshift-gitops)) |
| `unpackFileDependencies` | `true` | upload `file://` chart dependencies unpacked where that renders the same (see [Helm sources](#helm-sources)) |
| `argocd.serviceCA`, `argocd.caFile` | `false`, empty | OpenShift: verify the Argo CD server with the injected service CA, or with a CA file you mount |
| `trustedCABundle` | `false` | OpenShift: ConfigMap with the cluster's trusted CA bundle, trusted for ScmConnections without `caSecretRef` |
| `extraVolumes`, `extraVolumeMounts` | `[]` | e.g. a CA bundle for `argocd.caFile` |
| `serviceMonitor.enabled` | `false` | needs `metrics.enabled` and the Prometheus Operator |
| `networkPolicy.enabled` | `false` | needs `metrics.enabled`; allows metrics ingress only |
| `prometheusRule.enabled`, `.labels`, `.alertLabels` | `false`, `{}`, `{}` | the alerts of [Metrics and alerts](#metrics-and-alerts); needs the Prometheus Operator |
| `grafanaDashboard.enabled`, `.labels`, `.namespace` | `false`, `grafana_dashboard: "1"`, release namespace | a ConfigMap with the dashboard, for the Grafana sidecar |

### OpenShift

The chart works under the `restricted-v2` SCC as it is, with no SCC changes and
no `anyuid`: the pod runs as non-root with a read-only root file system, no
capabilities, the default seccomp profile, and no fixed user or group ID, so
OpenShift assigns them. The repository cache is an `emptyDir`, which is
writable for any assigned ID. If your cluster policy requires fixed IDs, set
them in `podSecurityContext` (`runAsUser`, `fsGroup`).

Argo CD on OpenShift (the GitOps operator) usually runs in `openshift-gitops`:
use `argoCD.namespace: openshift-gitops` in the policy, its server URL
(`https://openshift-gitops-server.openshift-gitops.svc`) in `argoCD.url`, and
the `argocd-cm` and `argocd-rbac-cm` of that namespace (or the matching fields
of the `ArgoCD` resource) for the account above. The server certificate is
signed by the cluster's service CA: mount it in a Secret and set
`argoCD.caSecretRef`.

With OpenShift GitOps 1.18 or later you can skip the account, the token Secret and the CA
Secret: see [Install on OpenShift GitOps](#install-on-openshift-gitops). The way above
(an account made by hand, a policy with `spec.argoCD.server`) keeps working, and so does any
upstream Argo CD.

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

### Configuration without Helm: environment variables

Every setting of the operator is a flag of `/manager`, and every flag can also be set by an environment variable: `BACKFLOW_`
and the name of the flag in capitals with `_` for `-` (`--argocd-server` is `BACKFLOW_ARGOCD_SERVER`). This is how an installation
without Helm (OLM, see below: `Subscription.spec.config.env`) is configured. A flag given on the command line wins over the
variable; an empty variable is the same as none. A flag that can be repeated (`--argocd-default-access`,
`--argocd-allowed-servers`, `--scm-allowed-hosts`) takes several values in its variable, separated by `;` or new lines (the
comma-separated ones also by commas): `BACKFLOW_ARGOCD_DEFAULT_ACCESS="team-a,team-b:team-a;platform:*"`. A value that does not
parse stops the operator at start-up and names the variable.

| Flag | Environment variable | Helm value | |
|---|---|---|---|
| `--argocd-server` | `BACKFLOW_ARGOCD_SERVER` | `argocd.server` | the default Argo CD connection |
| `--argocd-token-secret-namespace` | `BACKFLOW_ARGOCD_TOKEN_SECRET_NAMESPACE` | `argocd.tokenSecret.namespace` | |
| `--argocd-token-secret-name` | `BACKFLOW_ARGOCD_TOKEN_SECRET_NAME` | `argocd.tokenSecret.name` | |
| `--argocd-token-secret-key` | `BACKFLOW_ARGOCD_TOKEN_SECRET_KEY` | `argocd.tokenSecret.key` | default `apiToken` |
| `--argocd-ca-file` | `BACKFLOW_ARGOCD_CA_FILE` | `argocd.caFile` (`argocd.serviceCA`) | a file in the pod; under OLM mount it yourself |
| `--argocd-insecure-skip-tls-verify` | `BACKFLOW_ARGOCD_INSECURE_SKIP_TLS_VERIFY` | | development only |
| `--argocd-allowed-servers` | `BACKFLOW_ARGOCD_ALLOWED_SERVERS` | `argocd.allowedServers` | URLs a policy may name |
| `--argocd-default-access` | `BACKFLOW_ARGOCD_DEFAULT_ACCESS` | `argocd.defaultAccess` | `ns[,ns]:project[,project]`, `*` matches any |
| `--scm-allowed-hosts` | `BACKFLOW_SCM_ALLOWED_HOSTS` | `scm.allowedHosts` | **must name a self-hosted GitLab/GitHub** |
| `--secret-namespaces` | `BACKFLOW_SECRET_NAMESPACES` | `secretNamespaces` | the operator reads Secrets only there (its own namespace and the Argo CD token's are always included); the chart enforces it with RBAC, OLM grants `get` everywhere and the operator enforces it |
| `--operator-namespace` | `BACKFLOW_OPERATOR_NAMESPACE` | (the release namespace) | default: the namespace of the service account |
| `--trusted-ca-file` | `BACKFLOW_TRUSTED_CA_FILE` | `trustedCABundle` | |
| `--merge-request-poll-interval` | `BACKFLOW_MERGE_REQUEST_POLL_INTERVAL` | `mergeRequestPollInterval` | default `2m` |
| `--scm-recheck-interval` | `BACKFLOW_SCM_RECHECK_INTERVAL` | `scmRecheckInterval` | default `5m` |
| `--secret-cache-ttl` | `BACKFLOW_SECRET_CACHE_TTL` | `secretCacheTTL` | default `30s` |
| `--storage-check-interval` | `BACKFLOW_STORAGE_CHECK_INTERVAL` | | how often the operator checks that `manager migrate` has stored every object as `v1`; default `5m` |
| `--render-budget` | `BACKFLOW_RENDER_BUDGET` | `renderBudget` | default `2m` |
| `--unpack-file-dependencies` | `BACKFLOW_UNPACK_FILE_DEPENDENCIES` | `unpackFileDependencies` | default `true` |
| `--repo-cache-dir` | `BACKFLOW_REPO_CACHE_DIR` | `repoCache.*` | |
| `--leader-elect` | `BACKFLOW_LEADER_ELECT` | `leaderElection` | |
| `--metrics-bind-address` | `BACKFLOW_METRICS_BIND_ADDRESS` | `metrics.enabled` (`:8443`) | |
| `--metrics-secure` | `BACKFLOW_METRICS_SECURE` | | default `true` |
| `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key` | `BACKFLOW_METRICS_CERT_PATH`, `BACKFLOW_METRICS_CERT_NAME`, `BACKFLOW_METRICS_CERT_KEY` | `metrics.certSecret` | |
| `--health-probe-bind-address` | `BACKFLOW_HEALTH_PROBE_BIND_ADDRESS` | | default `:8081` |
| `--actor-webhook` | `BACKFLOW_ACTOR_WEBHOOK` | `actorWebhook.enabled` | see the actor webhook under OLM |
| `--actor-webhook-path` | `BACKFLOW_ACTOR_WEBHOOK_PATH` | (generated by the chart) | |
| `--actor-retention` | `BACKFLOW_ACTOR_RETENTION` | `actorWebhook.retention` | |
| `--actor-flush-interval` | `BACKFLOW_ACTOR_FLUSH_INTERVAL` | `actorWebhook.flushInterval` | |
| `--actor-max-records` | `BACKFLOW_ACTOR_MAX_RECORDS` | `actorWebhook.maxRecords` | |
| `--actor-tolerance` | `BACKFLOW_ACTOR_TOLERANCE` | `actorWebhook.tolerance` | |
| `--actor-record-username` | `BACKFLOW_ACTOR_RECORD_USERNAME` | `actorWebhook.recordUsername` | |
| `--username-in-merge-request` | `BACKFLOW_USERNAME_IN_MERGE_REQUEST` | `actorWebhook.usernameInMergeRequest` | |
| `--webhook-port` | `BACKFLOW_WEBHOOK_PORT` | (the webhook port) | default `9443`, `-1` off |
| `--webhook-cert-path`, `--webhook-cert-name`, `--webhook-cert-key` | `BACKFLOW_WEBHOOK_CERT_PATH`, `BACKFLOW_WEBHOOK_CERT_NAME`, `BACKFLOW_WEBHOOK_CERT_KEY` | (the webhook certificate) | |
| `--enable-http2` | `BACKFLOW_ENABLE_HTTP2` | | default `false` |
| `--zap-devel`, `--zap-log-level`, `--zap-encoder`, `--zap-stacktrace-level`, `--zap-time-encoding` | `BACKFLOW_ZAP_*` | `extraArgs` | logging |
| `--kubeconfig` | `BACKFLOW_KUBECONFIG` | | outside a cluster only |

### Allowed servers and hosts

A `BackflowPolicy` names an Argo CD server (`spec.argoCD.server.url`) and a `ScmConnection` a Git host (`spec.url`),
and the operator then sends requests there, one of them with the token the tenant supplied. A tenant who may create these
objects must not be able to point the operator at any address in its network, so the **installer** decides which ones are
allowed, in the chart values and not in the objects:

- `argocd.allowedServers` (`--argocd-allowed-servers`): URLs such as `https://argocd.team-a.example.com` or
  `https://*.argo.corp:8443`. Scheme, host and port must match; the path does not count. The server of
  `argocd.server`, the operator's own connection, is always allowed.
- `scm.allowedHosts` (`--scm-allowed-hosts`): host names, or `*.example.com` for any subdomain. The default is
  `gitlab.com` and `github.com`. An empty list allows no host.

An object that names something else is not `Ready` (`ServerNotAllowed` for a policy, `HostNotAllowed` for a connection), the
message names the flag, **no request is made** and the token Secret is not read. A proposal of such a connection is held.

**Without Helm** (kustomize: `config/default`, `make deploy`; OLM: the bundle made from it) there is no chart value: the lists are
flags of the manager, or the `BACKFLOW_*` environment variables
([the table](#configuration-without-helm-environment-variables)). **The defaults are the chart's**: the manifests set
`BACKFLOW_SCM_ALLOWED_HOSTS=gitlab.com,github.com` (the Deployment in `config/manager`, so the OLM bundle has it too), and no extra
Argo CD server is allowed (the operator's own `--argocd-server` always is). A self-hosted GitLab or GitHub, or a server a policy
names in `spec.argoCD.server.url`, has to be added, or the `ScmConnection` is `HostNotAllowed` and the policy `ServerNotAllowed`.

* With **OLM**, set the variables in `Subscription.spec.config.env` ([docs/olm.md](docs/olm.md)); a variable of the Subscription
  replaces the default of the Deployment.
* With **kustomize**, patch the Deployment of your overlay, either with environment variables or with arguments, as `config/e2e`
  does for the server of the samples. **A flag wins over the variable and replaces it**: `--scm-allowed-hosts=gitlab.example.com`
  as an argument means that `gitlab.com` is no longer allowed, which the default variable would have allowed. So list every host you want:

```yaml
# kustomization.yaml of your overlay
resources:
- github.com/sserkanml/backflow/config/default?ref=v0.18.1
patches:
# an environment variable: merged by name, so it replaces the default value
- target: {kind: Deployment}
  patch: |-
    apiVersion: apps/v1
    kind: Deployment
    metadata: {name: backflow-controller-manager}
    spec:
      template:
        spec:
          containers:
          - name: manager
            env:
            - {name: BACKFLOW_SCM_ALLOWED_HOSTS, value: "gitlab.example.com,gitlab.com"}
            - {name: BACKFLOW_ARGOCD_ALLOWED_SERVERS, value: "https://argocd.example.com"}
# or an argument, once for each entry (config/e2e does this)
- target: {kind: Deployment}
  patch: |-
    - {op: add, path: /spec/template/spec/containers/0/args/-, value: "--argocd-allowed-servers=https://argocd.example.com"}
```

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

## Helm sources

For an Application with a Helm source (a chart in the Git repository), Backflow
traces a drifted value back to the **value file** that feeds it and proposes
the change to that file. It does not guess: a candidate key is only used after
Argo CD itself has rendered the chart with the edit.

How a proposal is made:

1. Backflow finds the keys in the Application's value files (`helm.valueFiles`,
   in Argo CD's order and with its globbing, `$ARGOCD_APP_*` variables and
   `ignoreMissingValueFiles`) whose effective value equals the value Git had
   for the drifted field. A key that a later file overrides is not a candidate.
   When no edit of such a key verifies, Backflow also tries the keys that hold
   a **part** of the value: after `kubectl set image` an image
   `nginxinc/nginx-unprivileged:1.27-alpine` became `...:1.28-alpine`, and the
   key `image.tag` holds `1.27-alpine`. What changed is the part of the old
   string that is left after the common beginning and end of the two strings
   are taken away; a key whose value occurs in the old string and covers that
   whole part is a candidate, and its new value is the same stretch cut out of
   the new string (so a registry change finds `image.registry`, and a value that
   occurs twice gives two candidates). The keys that hold the whole value are
   always tried first.
2. It renders the chart through Argo CD (`GetManifestsWithFiles`) with the files
   of the proposal's revision, once for the unmodified files (twice, to find
   fields that change on every render) and once for each candidate edit.
3. An edit is accepted only when every field it changes, in **all** rendered
   resources, is one of the drifted fields and now has its live value. When
   several drifted fields are fed by the same key, one edit covers all of them;
   when the fields need different keys, the edits are combined and rendered once
   more, and the result must differ from the unmodified render by exactly the
   drifted fields. Edits in several value files go into one commit.
4. The merge request and the commit message list the value file, each key path
   with its old and new value, and say that the edit was verified by rendering
   through Argo CD.

**Charts with `file://` dependencies are rendered faster.** When a chart depends on
another chart in the repository (`repository: file://../common`), Argo CD's repo server
renders what it receives with `helm template` and, only when Helm says a dependency is
"missing in charts/", runs `helm dependency build` and renders again. An upload is a
fresh directory every time, so it builds on every render. Backflow therefore uploads the
dependencies already unpacked under `charts/<chart name>/` (aliases stay in the parent
`Chart.yaml`; a dependency that is already in `charts/` as a directory or a `.tgz` is
never added a second time; remote dependencies are left alone, and a chart with a remote
dependency that is not in `charts/` is rendered as before). A render went from about
150 ms to about 65 ms on a kind cluster with Argo CD v3.5.3.

It never changes a result. The first render of a chart (identified by the content of
the chart and its `file://` dependencies, not of its value files, and by the version of
Argo CD) is made both ways and the resources are compared; the quick way is used from
then on only if they are identical. Otherwise the render stays the one Argo CD does by
itself, the reason is logged once, and the decision is kept in memory. For example a
dependency of a dependency that is not committed in its `charts/` is built by Argo CD
without its own dependency (Helm does not build those), so the unpacked set would
render more than Argo CD does, and it is not used. `--unpack-file-dependencies=false`
(chart value `unpackFileDependencies: false`) switches the whole thing off. The metrics
`backflow_renders_total` and `backflow_render_duration_seconds` have a `deps` label
(`none`, `built`, `unpacked`), and `backflow_deps_equivalence_checks_total` counts the
checks (`identical`, `different`, `error`).

**Hooks and names chosen at random.** Resources with a `helm.sh/hook` or
`argocd.argoproj.io/hook` annotation (a chart's test Pods, `PreSync` Jobs) are not
what the Application manages, so verification leaves them out of every comparison,
for Helm, Kustomize, Directory and multi-source Applications alike. A resource that
is not a hook but is named at random by the chart (`randAlphaNum` in its name) is
found by the two renders of the unmodified files: a resource that exists in one
render only is a name that is not stable, and adding or removing a resource of its
kind in that namespace is not counted as a change. The two renders are real renders:
for a multi-source Application, whose renders Argo CD answers from its manifest cache
for a revision it has rendered before, they are asked for with `noCache=true`, so the
cache cannot make them one (no scratch branch is needed for them). Uploaded files
(single-source Helm and Kustomize) are never cached.

Like Directory proposals, a Helm proposal is made against the head of the target
branch. When the head is the revision the proposal was mapped at, or only files
the mapping does not depend on changed since (the files a render reads for Helm,
everything below the directory for a Directory source), the mapping is reused
and nothing is rendered again; the operator log says which ("Reusing the mapping"
or "Mapping again", with the reason). Otherwise the head is mapped again, and
when it no longer renders the old values the proposal is `Superseded`.

When the change cannot be traced back with certainty the proposal is `Unmapped`,
and the `Mapped` condition says why (`NoVerifiedCandidate`,
`AmbiguousCandidates`, `EditsInteract`, `RenderMismatch`,
`NondeterministicField`, `SecretNotVerifiable`, `TooManyCandidates`,
`RenderBudgetExceeded`, `RenderFailed`, `SymlinkedValueFile`, `MissingValueFile`
and others). When a key reproduces the drifted field but also changes other
fields, the reason is `ValueAffectsOtherFields`, even if other keys failed for
other reasons: the message names the nearest key with the fields it would move
(path, current value, value after the edit) and says how many other candidates
were rejected, so a person can decide.

Argo CD must allow the `applications, get` permission to Backflow's account (the
read-only account in the install steps has it) and must be reachable over
**HTTP/2**: the render is a gRPC call on the same port as the API, so a proxy
that only speaks HTTP/1.1 in front of Argo CD makes mapping wait and retry
(`ArgoCDUnreachable`). Nothing is applied to the cluster.

### Defaults that only the chart defines

An environment often changes a value it never set: the replica count of a chart
that nobody overrides, a timeout, the pull policy. The old value then comes only
from the chart's own defaults (its `values.yaml`, or that of a dependency under
its name or alias, and `global.*`), and that file is not the Application's
configuration, so Backflow does not edit it. Instead it **adds an override**: the
key, with the live value, is appended to the Application's last value file (the
one with the highest precedence), where it wins over the defaults.

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  helm:
    allowValueOverrides: true        # the default; false leaves such drift Unmapped
```

- The key goes to the last value file of the Application, or to the file named in
  `spec.helm.targetValuesFile`. That file must be one of the Application's
  resolved value files (a repository path, relative to the chart directory, or
  as the Application writes it); otherwise the proposal stays `Unmapped` and the
  message says which file was named and which ones the Application uses.
- Missing intermediate maps are created, an existing parent map gets the entry
  appended in its own indentation, and the rest of the file (comments, quoting,
  key order) is not touched. The new value keeps the type of the default.
- Edits of keys a value file already defines are always tried first; an override
  is only tried when none of them verifies. It is verified by rendering through
  Argo CD like any other edit: it must change exactly the drifted fields, and the
  collateral and partial rules apply unchanged.
- Whole values and parts of a value (the image tag of `repository:tag`) both
  work. A key under a list in the defaults is out of scope: a list is replaced
  as a whole, and the proposal stays `Unmapped` with that reason.
- A key that the Application sets inline (`helm.values`, `helm.parameters`, ...)
  stays `Unmapped` (`SetByApplication`; see Values set in the Application, which says
  where it is defined); so does an Application without any value file.
- The merge request body has an **Overrides a chart default** table (value file,
  key added, new value, chart default, and the file that defines it), the commit
  message says the same, and the table of Helm values shows the old value as
  `(not set; chart default ...)`.

### Maps and lists

Annotations, labels, tolerations, a node selector, env vars and resources are
maps and lists in the rendered resource, and the value files hold them under
keys such as `podAnnotations`, `tolerations`, `extraEnvVars` or `resources`.
Which key feeds which field is written in the templates, not in the values, so
Backflow compares: it merges the layers of values the way Helm does, and looks
for keys whose content fits what the resource has in Git (its base render).
Every candidate is then verified by a render, exactly like a scalar edit: it
must produce the live state of the field and change nothing else.

What is found:

- **A key that holds what Git renders**: `tolerations` that equals the rendered
  list is set to the live list.
- **A stretch of a longer list**: a chart builds `env` from entries of its own
  and the user's `extraEnvVars`. The values list is the stretch of the rendered
  list it feeds (an empty one sits at the end or at the start), and the edit
  changes only that stretch: adding or removing an env var edits `extraEnvVars`.
- **A key added to or removed from a map**: a pod annotation or label, a node
  selector. The key is added to the values map that is a part of the rendered
  map (`podAnnotations`, `podLabels`, `nodeSelector`), or removed from it when a
  value file defines it.
- **An empty key that takes the place of what the chart generates**: `resources:
  {}` next to `resourcesPreset`. A drifted limit is brought to the live state by
  setting the whole `resources` map to what is live.

Where the key goes follows the scalar rules: edits of keys a value file already
defines come first; a key that only the chart's defaults hold is an override in
the Application's last value file (or `spec.helm.targetValuesFile`), which
needs `spec.helm.allowValueOverrides`. Maps and lists are written in block style, the
rest of the file (comments, order, siblings, quoting) stays, and the result is
checked to parse back to exactly the intended value. Strings that YAML 1.1
reads as other types (`yes`, `no`, `on`, `y`, `0777`, `1:30`) are quoted,
because Helm parses with a 1.1 parser.

Names decide which keys are rendered first, never which one is right: a key is
a candidate when its name fits the field (the words of the field and of the
resource's name, and `count` for replicas), and a field with more than ten
candidates is narrowed to the best-fitting ones only when that leaves a clear
tier. The keys that are left out are not forgotten, because a proposal is only
mapped when no other key could have worked too:

- a key that no template, helper or value file of the chart (and of its
  dependencies, packaged ones included) reads cannot be the one, and is dropped;
- when an edit verifies, the other keys that a template may read and that
  compete with it are rendered too, up to 30 renders and within the render
  budget; if one of them verifies as well, the proposal is `AmbiguousCandidates`;
- if there are more than can be rendered, or the render budget runs out, the
  proposal is `TooManyCandidates` and names the best keys that were not tried.

If two edits verify and give different files, the proposal is
`AmbiguousCandidates` however the keys were found; only edits that write the
same bytes to the same files are one mapping.

Limits:

- **Hardcoded in a template.** A list or map that a template writes itself, with
  no value behind it, has no key to edit (`NoVerifiedCandidate`).
- **Entries a helper generates.** An env var, annotation or label that the
  chart generates cannot be changed from the values: only the user's part of a
  list or map is editable.
- **A value that feeds more than the drifted field.** The same `containerPorts`
  key may feed a NetworkPolicy, or `master.count` the env of the replicas: the
  proposal is `Unmapped` with `ValueAffectsOtherFields`, naming what else moves.
- **A key removed that only the defaults define** cannot be removed from the
  Application; setting it to null is not proposed.
- **Ambiguous subtrees.** Two keys that both reproduce the change and give
  different files are never picked between.

### Label and annotation changes beside an edit

A chart often puts one value in more than one place: the image tag is also the
`app.kubernetes.io/version` label of the Deployment and of its pods. Editing the
tag then changes more than the field that drifted, and by default such an edit
is not accepted: the proposal is `Unmapped`, and its message names the nearest
edit and the other fields it would change. A policy can accept this for labels
and annotations:

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  helm:
    allowCollateralChanges: [Labels]        # and/or Annotations
```

With it, an edit may also change, on any rendered resource,
`metadata.labels` and `spec.template.metadata.labels` (`Labels`), and
`metadata.annotations` and `spec.template.metadata.annotations`
(`Annotations`); for a CronJob also the pod template of its jobs,
`spec.jobTemplate.spec.template.metadata.labels` and `.annotations`. The edit is still proven by rendering through Argo CD; the
strict rule (exactly the drifted fields) is tried first, and this is used only
when it finds nothing. **Never allowed, whatever the policy says:** any selector
(`spec.selector`, its `matchLabels` and `matchExpressions`, a Service's
`spec.selector`), because changing one can orphan the pods of a workload, and a
change to a field that drifted but does not end at its live value. A tag that
also feeds a selector label is therefore always `Unmapped`.

The changes that were accepted are recorded in `status.mapping.collateral` and
listed in the merge request body and in the commit message ("This edit also
changes: ..."), each with the resource, the path, the value now and the value
after the edit, so the reviewer sees them before merging.

Limits of Helm mapping:

- **Live-only additions are not reported by Argo CD.** A field that exists
  only in the live object (added with `kubectl edit` or `patch` and absent from
  Git) is not detected on its own, because Argo CD's diff preserves it. Such an
  addition is mapped when the same resource also has a reported drift. Removals
  and value changes are always detected.
- **Chart in Git, or a chart from a Helm repository or a registry with its
  value files in a Git ref source.** A chart from a Helm repository or an OCI
  registry with its values set only in the Application is `Unmapped`
  (`UnsupportedSourceType`); see the next section for the supported shape with
  several sources. Value files that are URLs are not edited; Backflow reports
  that a value may come from one.
- **The chart's own `values.yaml` is never edited.** A value that exists only
  there is overridden in the Application's last value file (see above); with
  `spec.helm.allowValueOverrides: false` it is `Unmapped` (`NoVerifiedCandidate`,
  "chart's own defaults"), as are keys under a list in the defaults.
- **Inline settings win and are not edited.** A key set by `helm.values`,
  `helm.valuesObject`, `helm.parameters` or `helm.fileParameters` on the
  Application overrides any value file, and that setting lives in the Argo CD
  Application, not in the chart repository. The proposal is `Unmapped` and
  says which layer sets it.
- **Derived fields.** Only values that equal the old value are candidates, so a
  field a template computes (`upper`, `printf`, a sum) has no candidate of its
  own. An edit of the key it derives from is accepted only when every field it
  moves is a drifted field; if the template also moves one that did not drift
  (a version label that follows `image.tag`), the proposal is `Unmapped` with
  that near miss. The reason is that merging the edit would make Git disagree
  with the cluster on a field nobody changed. Labels and annotations that follow
  the value can be accepted by the policy, see below.
- **Secret resources are never proposed.** Argo CD masks the data of Secrets,
  and a Secret value must not be put into Git (`SecretNotVerifiable`).
- **Fields that change on every render** (`randAlphaNum`, `now`) cannot be
  verified (`NondeterministicField`); resources with a random name are ignored
  when comparing renders.
- **Values keep their type.** An integer stays an integer, a boolean a boolean,
  a quoted string stays quoted, when the live value reads as that type and
  writing it that way renders back to exactly the same string; otherwise it is
  written as a quoted string. A tag `1.10` in a file is a number that renders
  as `1.1`, so the new tag `1.20` is written as `"1.20"`, not as `1.20`, which
  would render as `1.2`. YAML anchors and merge keys are read, but
  a key that exists only through a merge key (`<<: *base`) cannot be edited. A
  list is replaced by a later file as a whole, as Helm does; an item is added
  to or removed from a list by setting the list (see Maps and lists).
- **Bounded work.** At most 10 candidate keys per field and two minutes of
  rendering per proposal. A symbolic link in the chart, a value file or a
  `file://` dependency makes the Application unmappable and pauses detection
  while Git is ahead.
- **Argo CD renders from its own settings.** Backflow sends the files, not the
  Application: Helm version, `helm.releaseName`, `kubeVersion` and API versions
  are those of Argo CD. A `.argocd-source*.yaml` file in the chart directory is
  not modelled (`ArgoSourceOverrides`).

## Kustomize sources

For an Application with a Kustomize source (an overlay in the Git repository),
Backflow traces a drifted field back to the file or setting that decides it and
proposes the change there. Like Helm, nothing is guessed: an edit is only used
after Argo CD has rendered the edited files and the render reproduces the
drifted fields, and nothing else.

How a proposal is made:

1. **The closed file set.** Backflow follows the Application's `kustomization.yaml`
   through `resources`, `bases`, `components`, patches, generator files and envs,
   `replacements`, `openapi` and `crds` (`kustomize.RelevantFiles`), including
   bases outside the Application's path, and uploads exactly those files to Argo
   CD. A remote base or resource, `helmCharts`, a symbolic link or a reference
   that does not exist makes the set not closed: the proposal is `Unmapped`
   (`KustomizeSourceNotClosed`) and the message says which reference.
2. **Where each resource comes from.** One extra render of a copy whose overlay
   has `buildMetadata: [originAnnotations, transformerAnnotations]` tells which file
   declares each resource and which transformers touched it. This only orders and
   seeds the candidates; the annotations are never written to Git and never used
   as proof, and every verification render uses the unmodified files.
3. **Tiers, tried in this order** (a later tier is only tried when no edit of an
   earlier one verifies):

   | Tier | Where the edit goes | Strategy |
   |---|---|---|
   | 1 | A setting of the Application's own `kustomization.yaml`: `images` (`newName`, `newTag`, `digest`, also when the drift is only a part of the image string), `replicas`, `labels` / `commonLabels` / `commonAnnotations`, and the literals and env-file lines of a generator **without** a name hash | `KustomizePatch` |
   | 2 | A value inside a patch the overlay already has: a strategic merge patch file or inline patch (list items are matched by their merge key: containers and env vars by name, ports by `containerPort`, ...), or a JSON 6902 operation, as a file or as a string (kept in block style, or re-written as compact JSON when it was JSON) | `KustomizePatch` |
   | 3 | The resource file inside the Application's path | `Direct` |
   | 4 | A file outside the path (a shared base or component), **only with `spec.kustomize.allowBaseEdits`** | `Direct` |
   | 5 | A **new patch**: `<overlay>/backflow-patches/<kind>-<name>.yaml` with only the drifted fields (a removed list item as `$patch: delete`, a removed key as `null`), listed under `patches:` in the overlay's `kustomization.yaml`; an existing file of that name is extended and keeps its comments. Only with `spec.kustomize.allowNewPatches` | `KustomizePatch` |

   Comments, key order and quoting of the edited files are kept.
4. The merge request and the commit say what kind of edit each one is, that it was
   verified by rendering, and, for a new patch file, that the file was added. The
   new file and the edited `kustomization.yaml` are in the same commit.

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  kustomize:
    allowBaseEdits: false     # default; true allows edits of files outside the Application's path
    allowNewPatches: true     # default; false leaves what no setting, patch or file holds Unmapped
```

`spec.mapping.allowPartial` and `spec.helm.allowCollateralChanges` apply to
Kustomize sources the same way as to Helm ones.

### Shared bases

A base is usually shared by several environments, so editing it changes them too.
Backflow works out which other Applications build a file outside the Application's
path (the files each Application of **any** BackflowPolicy reads):

- If **no other Application** builds the file, tier 4 comes before tier 5.
- If **others do**, a new patch (tier 5) is tried first; the shared file is edited
  only when that cannot be verified or `allowNewPatches` is false. The merge request and
  the status list the others ("Merging this also changes: X, Y at their next sync").
- Applications that no policy covers cannot be known, so every merge request that
  edits a file outside the path says that other Applications outside Backflow's
  policies may use it.

### What is Unmapped, and why

- **Hashed generators.** `configMapGenerator` / `secretGenerator` without
  `disableNameSuffixHash` put a hash of the data in the name. Editing the data
  changes the name and every field that refers to it, so the proposal is
  `ValueAffectsOtherFields` and the message says so. Set `disableNameSuffixHash` (in
  `generatorOptions` or on the generator) to map the data.
- **Options of the Application.** What `spec.source.kustomize` sets (`images`,
  `replicas`, `namePrefix`, `nameSuffix`, `namespace`, `commonLabels`,
  `commonAnnotations`, `patches`) is applied by Argo CD on top of Git, so a change of
  Git cannot change it: `SetByApplication`, naming the option and where it is defined
  (see Values set in the Application).
- **Remote bases, `helmCharts`, plugins and KRM functions:** the output is not a
  function of the files Backflow can edit and render.
- **Secrets**, as for Helm (`SecretNotVerifiable`).
- A value no setting, patch or file holds, with `allowNewPatches: false`
  (`NoVerifiedCandidate`); the message names a file that holds the value but is
  outside the path while `allowBaseEdits` is off.
- Changing the **merge key** of a list item (a Service's `port`, a container's `name`)
  cannot be expressed as a patch; it needs `allowBaseEdits` to edit the file itself.

Git-ahead works as for Helm: a commit to any file the kustomization builds from, in
or outside the path, pauses detection for the Application until it is synced; a
commit to another file does not.

The render needs Argo CD's Kustomize (v5.8.1 at the time of writing); `make coverage`
and the render tests use the same version, installed by `make kustomize`.
`make test` installs it too, and with `CI=true` a missing binary fails the tests
instead of skipping them. `test/coverage/REPORT.md` has the coverage by source type.

## Helm charts from a Helm repository or a registry

An Application with **several sources** can take its chart from a Helm
repository (HTTP) or an OCI registry, and its value files from a Git repository
through a `ref` source (`$values/...`). Backflow maps a drift of such an
Application to a merge request on those value files, verified by rendering
through Argo CD like any Helm proposal:

```yaml
spec:
  sources:
    - repoURL: ghcr.io/stefanprodan/charts      # OCI registry
      chart: podinfo
      targetRevision: 6.7.1
      helm:
        valueFiles: [$values/env/values.yaml]
    - repoURL: https://gitlab.com/acme/gitops.git
      targetRevision: main
      ref: values                               # the repository the merge request is made in
```

The supported shape is **one Helm source plus Git ref sources**; the chart may
also be in another Git repository (`path:`). Any other combination (two sources
that render, a Kustomize or plugin source, no ref source) gets the
`UnsupportedSource` Event and no proposals, with the reason in its text.

How it differs from a chart in Git:

- **The chart is not in Git.** Its own defaults come from Argo CD (`appdetails`):
  the chart's `values.yaml`, as text. Overrides of a default (a key the value file
  never set) and maps and lists work as for a chart in Git. The defaults of
  **subcharts** are not available (Argo CD does not return them): a key under a
  subchart that the value file does not set stays `Unmapped`. The account needs
  one more Argo CD permission for this, `p, role:backflow, repositories, get, *,
  allow` (see the install steps); without it a proposal waits in `Mapping` with
  `ChartDefaultsUnavailable` and is tried again.
- **Argo CD renders from commits, so verification pushes scratch branches.** Each
  candidate edit is committed on top of the commit Argo CD synced the value file
  at, pushed to a branch `backflow/scratch/<proposal>-<n>` of the **repository of
  the ref source** (one push for all candidates, with the push option `ci.skip`
  on GitLab and `[skip ci]` in the commit message), rendered by its SHA with
  every source pinned to the revision Argo CD resolved (the chart version, never a
  range), and the branches are deleted again, in one push, whatever the outcome.
  No branch of yours is touched. A sweeper deletes `backflow/scratch/` branches
  older than an hour that a crash left behind, and no other branch. The token of
  the connection therefore needs the right to push branches there (the same it
  needs for the merge request), and a protected-branch rule must not forbid
  `backflow/scratch/*`.
- **Secrets in the render.** Argo CD returns the data of Secrets of a render by
  revision in the clear. Backflow masks it the moment the response is parsed
  (a keyed hash, so a change still shows as a change), and no Secret value
  reaches a log, a status, an Event or a merge request. Error texts of Argo CD
  are cut to their first line with quoted text removed.
- **Several repositories.** Each Git repository of the Application is matched to
  an `ScmConnection` by its own host (`status.applications[].sources[]` lists
  every source with its repository, revision and connection). The merge request
  is made in the repository of the ref source that holds the value files,
  with that repository's connection; the proposal's `spec.sources` and
  `status.mapping.repoURL` say which. A chart from a registry needs no connection.

### `spec.mapping.scratchBranches`

```yaml
spec:
  mapping:
    scratchBranches: Auto     # Auto (default) | Always | Never
```

`Auto` allows scratch branches in `MergeRequest` and `DirectCommit` mode and not
in `ReportOnly`, whose mapping never writes to Git. When they are not allowed, a
proposal of such an Application is `Unmapped` with `ScratchBranchesDisabled`, and
the message names this field. `Always` allows them in every mode.

### Time

Every candidate is a render through Argo CD, which took seconds each when this
was measured. The time all renders of one verification may take together is the
operator's `--render-budget` (default `2m`; `renderBudget` in the chart), and the
operator logs at the end of each verification how many renders it made and how
long it took (`Verification finished`, with `renders`, `elapsed`, `scratchBranches`
and `pushes`). Past the budget a proposal is `Unmapped` (`RenderBudgetExceeded`).

### What stays Unmapped

| Reason | When |
|---|---|
| `ScratchBranchesDisabled` | scratch branches are not allowed (see above) |
| `ScratchBranchRejected` | the repository refused the scratch branches (a protected-branch rule, a hook) |
| `ArgoCDUnsupported` | this Argo CD cannot render by revision per source (it needs 2.6 or later; 3.5.3 is what the tests run against) |
| `ValueFileNotInGit` | a value file is inside a chart from a Helm repository or a registry (its own `values-prod.yaml`): it is not in Git, so it cannot be edited; the message names the file and the source |
| `MultipleValueRepositories` | the value files are in two Git repositories (a second ref source, or inside a chart in Git): a proposal is written to one repository |
| `SetByApplication` | the value is set in the Application (`spec.sources[N].helm.values`, `valuesObject`, `parameters`); see Values set in the Application |
| `NoVerifiedCandidate` | no value file holds the value |
| `UnsupportedSourceType` | the Application is outside the supported shape |

Detection while Git is ahead is per source: a commit pauses the Application only
when it changes a value file of a ref source (globs expanded at both revisions) or,
for a chart in Git, a file the chart reads. A **new chart version** (in the
Application, or a range that resolved to one) is a change of the Application, not
Git being ahead: an `ApplicationChanged` Event says so, and detection is paused
until Argo CD syncs the Application.

## Values set in the Application

Some values are not in any file the source reads: the Application sets them itself,
with `helm.parameters`, `helm.values`/`valuesObject`, or `spec.source.kustomize`
(`images`, `replicas`, `namePrefix`, `nameSuffix`, `namespace`, `commonLabels`,
`commonAnnotations`, `patches`). Argo CD applies them on top of Git, so editing a
value file never changes the result. A drift of such a value ends `Unmapped` with the
reason **`SetByApplication`**, for Helm (single and multi-source) and Kustomize alike.

Backflow does not propose a merge request for it. **No Argo CD API renders an
Application with a changed option** (`GetManifests` and `GetManifestsWithFiles` use the
Application as it is stored; `sync` dry runs and the ApplicationSet generator do not
render, and the first writes to the Application), so a new value of the option cannot be
verified by a render, and Backflow only proposes what a render verifies. What it can prove
it tells you, in `status.mapping.application` and in one sentence, which is also
an Event on the proposal:

> Set by spec.source.helm.parameters[name=config.feature] of Application argocd/child-web,
> defined in apps/children/child.yaml (parent argocd/root). Set it to "gamma" there;
> Backflow cannot verify an Application option by rendering, so no merge request is made.

```yaml
status:
  mapping:
    strategy: Unmapped
    application:
      application: argocd/child-web
      option: spec.source.helm.parameters[name=config.feature]
      optionPath: /spec/source/helm/parameters/0/value
      current: '"beta"'
      suggested: '"gamma"'          # only when the option copies the field exactly
      parent:
        state: Located
        name: argocd/root
        repoURL: https://git.example/apps.git
        revision: 0f3c...
        file: apps/children/child.yaml
        location: /spec/source/helm/parameters/0/value
        diff: |                      # verified by a render of the parent, never applied
          ...
```

**Where the option is defined.** The Application is usually a resource of a parent
Application (app-of-apps). Backflow finds the parent in `status.resources` of the
Applications (the `argocd.argoproj.io/tracking-id` annotation is the fast path), and maps the
child's field to the parent's Git file with the same engines as for any resource
(Directory, Helm, Kustomize), verified by a render of the parent. For a Kustomize parent
it looks for the file that holds the value, not a place to add a patch.

| `parent.state` | Meaning |
|---|---|
| `Located` | The file and location are found and a render of the parent with the edit changes exactly that option. |
| `NotManagedFromGit` | No Application manages it. |
| `GeneratedByApplicationSet` | An ApplicationSet generated it (`name`); change its template or generator. |
| `OutOfSync` | The parent is not synced, so what Git says is not what runs. |
| `Ambiguous` | More than one Application manages it. |
| `NotVerifiable` | The parent has several sources, has no Git source, has another source type, or no verified edit shows the option (the message says which). |

The **suggested value** is given when it can be derived exactly: a parameter or a
`valuesObject` key (it keeps the type of the current value), an image tag of
`spec.source.kustomize.images`, a replica count, a label or annotation. For a string that holds
YAML or JSON (`helm.values`, a Kustomize `patches` entry) only the location is given.
When several options set the drifted fields, the first is in the status and the message
names the others. When only some of the unmapped fields are set by an option, the reason
stays the engine's, and the message adds the sentence.

Nothing is written for this: no commit, no branch, no merge request, no scratch
branch (a parent with several sources is `NotVerifiable` so that nothing needs
one). The parent is read with the `ScmConnection` of the proposal's namespace that
matches its host, or anonymously.

## JSON files

A manifest, a patch or a value file that is JSON is edited as JSON, **in place**: only
the bytes of the edit change. Indentation (spaces or tabs, any width), key order, the
text of numbers (`1.10` stays `1.10` unless the value changes), string escapes, line
endings (CRLF), a byte order mark and the final newline (or its absence) stay as they
are. A changed value is written with correct escaping; a new member goes after the last
one with the indentation of its siblings (or on the same line in a one-line object);
a removed member or element takes its comma with it and leaves no blank line; a map or
list that is replaced is written again in the surrounding style. Every result is parsed and
compared with the object the edit was meant to give, and a key defined twice on the path
of an edit (which JSON parsers read differently) is `Ambiguous`, never guessed.

What is JSON follows Argo CD (v3.5.3, `getObjsFromYAMLOrJSON`):

- **Directory sources.** A file whose name ends in `.json` holds **one object**; a
  second value in the file, or an array at the top, makes Argo CD fail, so Backflow
  reads such a file as defining nothing. An object with an `items` array is a list (any
  `kind`, `List` usually): its items are the resources, and `"items": null` is an empty
  list. A `.yaml` file with JSON in it is read as YAML and edited as YAML.
- **Kustomize sources.** `.json` resource files, strategic merge patches and JSON 6902
  patches (a JSON list) are edited as JSON, whichever tier they come in. Settings in
  `kustomization.yaml`, and new Backflow patches, stay YAML.
- **Helm sources.** A value file with a `.json` extension is read by Helm as YAML and
  edited by Backflow as JSON.

Limits: UTF-16 files are not edited (Argo CD reads them, Backflow refuses them), and
Jsonnet is not supported.

## Partial proposals

By default a proposal is all or nothing: when one drifted field cannot be traced
back to Git, the whole proposal is `Unmapped`, even though the other fields
could be proposed. A policy can opt in to proposing what can be proven:

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  mapping:
    allowPartial: true        # default false
```

With it, a proposal with at least one mappable field is mapped for those fields
only (Directory and Helm sources alike). Every field that is left out is listed
in `status.mapping.unmapped` (`path`, `reason`, `message`), the `Mapped`
condition is `True` with reason `PartiallyMapped`, and its message names both
sets. `spec.changes` is unchanged and still lists every change. A proposal where
nothing can be mapped is `Unmapped` as before.

Nothing is guessed: the edits of the mapped fields are verified together, they
must change exactly those fields to their live values, and every left-out field
must render exactly as it does in Git. An edit that would also change a left-out
field is not accepted, and the field it was for is left out as well. If the
verification of the combined edit fails, the whole proposal is `Unmapped`.

A partial merge request (or direct commit) is marked "(partial)" in its title
and commit subject, and its body has a **Not included in this merge request**
table: path, live value, Git value and the reason, followed by a warning (see
below). The changes table and the diff list the mapped fields only. The same
rule applies when the proposal is mapped again at a moved head of the target
branch, and `DirectCommit` mode honours it too.

> [!WARNING]
> **The fields left out are lost at the next Argo CD sync.** A sync applies the
> whole manifest, so it resets every field of the resource to its Git value,
> including the live changes that could not be mapped. With automated sync that
> happens right after the merge request is merged; with manual sync, when
> someone syncs. Either way, to keep one of those changes, port it to Git by
> hand, in the merge request or before it is merged. The merge request body and
> the commit message say so, in the same words for both sync modes.

Lifecycle: once the merge request is merged, the proposal is `Merged` and
stays so. The left-out fields do not become a new proposal: with manual sync,
Argo CD compares against the merge commit but has not synced it, so Backflow
pauses detection for the Application (Git is ahead, a `GitAhead` Event says so)
and makes no proposal; when the Application is synced, the left-out fields are
reset and there is nothing left to propose. With automated sync the sync comes
first. A new proposal for those fields appears only if somebody changes them in
the cluster again after the sync.

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
