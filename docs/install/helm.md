# Install with Helm

Backflow runs in the cluster as one Deployment. Install it with a Helm chart (Helm 3.8 or later), or, on OpenShift, from
OperatorHub through the catalog of the project ([Install from OperatorHub (OpenShift)](openshift.md#install-from-operatorhub-openshift-olm)).

With Helm:

```bash
helm install backflow oci://ghcr.io/sserkanml/charts/backflow \
  --version 1.0.0 --namespace backflow-system --create-namespace
```

The chart installs the CRDs (`ScmConnection`, `BackflowPolicy`,
`DriftProposal`), a ClusterRole, the Deployment and, with
`leaderElection: true` (the default), a leader election Role. The image is
`ghcr.io/sserkanml/backflow`, for linux/amd64 and linux/arm64.

The rest of the chart's settings are the flags of the operator; the table of [Configuration](../configuration.md) lists every
flag with its `BACKFLOW_*` variable and its Helm value. To upgrade a release see [Upgrade and uninstall](../operations/upgrade.md);
**apply the CRDs of the new version first**, because Helm never upgrades them.

## Values

The most used values of the chart; all of them are in
[charts/backflow/values.yaml](../../charts/backflow/values.yaml).

| Value | Default | |
|---|---|---|
| `image.repository`, `image.tag` | `ghcr.io/sserkanml/backflow`, the chart's `appVersion` | |
| `imagePullSecrets` | `[]` | for a mirror that needs credentials |
| `replicaCount`, `leaderElection` | `1`, `true` | more than one replica needs leader election |
| `mergeRequestPollInterval` | `2m` | how often an open merge request is polled |
| `resources` | 50m/128Mi requests, 500m/512Mi limits | tune to the number of Applications |
| `argocd.defaultAccess` | `[]` | which Applications (Argo CD projects) the policies of which namespaces may match through the default connection; empty: only the operator's namespace (see [Multi-tenancy and access](../security.md#multi-tenancy-and-access)) |
| `argocd.allowedServers` | `[]` | the Argo CD servers a policy may name in `spec.argoCD.server.url`, besides `argocd.server`, which is always allowed; another server makes the policy not `Ready` (`ServerNotAllowed`) and nothing is requested from it (see [Allowed servers and hosts](../configuration.md#allowed-servers-and-hosts)) |
| `scm.allowedHosts` | `[gitlab.com, github.com]` | the Git hosts a `ScmConnection` may name in `spec.url`; another host makes it not `Ready` (`HostNotAllowed`); set your self-hosted GitLab or GitHub here |
| `actorWebhook.enabled` | `false` | the observe-only admission webhook that names the user of a change (see [The actor webhook](../who-changed-it.md#the-actor-webhook-who-it-really-was)); needs Kubernetes 1.28 and, outside OpenShift, `actorWebhook.networkPolicy.apiserverCIDRs` |
| `actorWebhook.recordUsername`, `.usernameInMergeRequest` | `true`, `false` | store user names and groups (false: only person or controller); put the name into merge requests and commits |
| `actorWebhook.retention`, `.tolerance` | `2h`, `10s` | how long a ChangeRecord is kept; how far its time may be from the managedFields entry it is checked against |
| `secretNamespaces` | `[]` | namespaces where the operator may read Secrets; empty means every namespace (see [Permissions](../security.md#permissions)) |
| `scmRecheckInterval` | `5m` | how often a working ScmConnection is verified again; also how long a rotated token goes unnoticed |
| `secretCacheTTL` | `30s` | how long a Secret that was read stays in memory |
| `repoCache.sizeLimit` | `2Gi` | the `emptyDir` Git repositories are cached on |
| `extraArgs` | `[]` | extra operator flags |
| `podSecurityContext`, `securityContext` | restricted, no fixed IDs | see [Helm on OpenShift](openshift.md#helm-on-openshift) |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | empty | |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, generated, `{}` | |
| `metrics.enabled`, `metrics.certSecret` | `false`, none | HTTPS on 8443 behind Kubernetes authn/authz |
| `argocd.server`, `argocd.tokenSecret.{namespace,name,key}` | empty, `apiToken` | the Argo CD connection of every policy without `spec.argoCD.server` (see [Install on OpenShift GitOps](openshift.md#install-on-openshift-gitops)) |
| `unpackFileDependencies` | `true` | upload `file://` chart dependencies unpacked where that renders the same (see [Helm sources](../mapping/helm.md#helm-sources)) |
| `argocd.serviceCA`, `argocd.caFile` | `false`, empty | OpenShift: verify the Argo CD server with the injected service CA, or with a CA file you mount |
| `trustedCABundle` | `false` | OpenShift: ConfigMap with the cluster's trusted CA bundle, trusted for ScmConnections without `caSecretRef` |
| `extraVolumes`, `extraVolumeMounts` | `[]` | e.g. a CA bundle for `argocd.caFile` |
| `serviceMonitor.enabled` | `false` | needs `metrics.enabled` and the Prometheus Operator |
| `networkPolicy.enabled` | `false` | needs `metrics.enabled`; allows metrics ingress only |
| `prometheusRule.enabled`, `.labels`, `.alertLabels` | `false`, `{}`, `{}` | the alerts of [Metrics and alerts](../operations/metrics.md#metrics-and-alerts); needs the Prometheus Operator |
| `grafanaDashboard.enabled`, `.labels`, `.namespace` | `false`, `grafana_dashboard: "1"`, release namespace | a ConfigMap with the dashboard, for the Grafana sidecar |
