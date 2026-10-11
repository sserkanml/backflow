# Backflow

Reverse GitOps for Kubernetes and OpenShift. Backflow detects changes made
directly in a cluster to resources that Argo CD manages, and proposes them back
to the Git source of truth as merge requests against the file that owns the
resource.

Backflow never writes to the cluster: Argo CD is the only thing that applies
manifests. Backflow reads the cluster and writes to Git.

## Install

Backflow runs in the cluster as one Deployment and is installed with a Helm
chart (Helm 3.8 or later):

```bash
helm install backflow oci://ghcr.io/sserkanml/charts/backflow \
  --version 0.1.0 --namespace backflow-system --create-namespace
```

The chart installs the CRDs (`ScmConnection`, `BackflowPolicy`,
`DriftProposal`), a ClusterRole, the Deployment and, with
`leaderElection: true` (the default), a leader election Role. The image is
`ghcr.io/sserkanml/backflow`, for linux/amd64 and linux/arm64.

### 1. A read-only Argo CD account

Backflow reads the Applications and their managed resources through the Argo CD
API. Give it an account that can do nothing else:

```bash
kubectl -n argocd patch configmap argocd-cm --type merge \
  -p '{"data":{"accounts.backflow":"apiKey"}}'
kubectl -n argocd edit configmap argocd-rbac-cm      # add the two lines below to policy.csv
```

```
p, role:backflow, applications, get, */*, allow
g, backflow, role:backflow
```

```bash
argocd login <argocd-server> --username admin
TOKEN="$(argocd account generate-token --account backflow)"
kubectl -n backflow-system create secret generic argocd-token --from-literal=token="$TOKEN"
```

### 2. A token for the Git host, and a policy

Create the token with the scopes `api` and `write_repository` (GitLab) or
`contents` and `pull_requests` (GitHub). It, the connection and the policy live
in the same namespace.

```bash
kubectl -n backflow-system create secret generic gitlab-token --from-literal=token="$GITLAB_TOKEN"

kubectl -n backflow-system apply -f - <<'YAML'
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata:
  name: gitlab
spec:
  provider: gitlab                  # or github
  url: https://gitlab.com           # your server for a self-hosted one
  tokenSecretRef: {name: gitlab-token, key: token}
---
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata:
  name: my-apps
spec:
  argoCDNamespace: argocd
  applications:
    names: [my-app]                 # or selector: {matchLabels: {env: test}}
  mode: ReportOnly                  # MergeRequest and DirectCommit write to Git
  argoCD:
    url: https://argocd-server.argocd.svc
    tokenSecretRef: {name: argocd-token, key: token}
YAML

kubectl -n backflow-system get scm,bfp,dp
```

With `ReportOnly` Backflow finds the file that defines each drifted resource and
shows the verified edit in the `DriftProposal` (`kubectl get dp -o yaml`), but
writes nothing to Git. Change `mode` to `MergeRequest` to get a merge request
per drift. More options (ignored fields, included and excluded kinds, branch
prefix, labels, reviewers, batch window) are in
[config/samples](config/samples).

### Values

The most used values of the chart; all of them are in
[charts/backflow/values.yaml](charts/backflow/values.yaml).

| Value | Default | |
|---|---|---|
| `image.repository`, `image.tag` | `ghcr.io/sserkanml/backflow`, the chart's `appVersion` | |
| `imagePullSecrets` | `[]` | for a mirror that needs credentials |
| `replicaCount`, `leaderElection` | `1`, `true` | more than one replica needs leader election |
| `mergeRequestPollInterval` | `2m` | how often an open merge request is polled |
| `resources` | 50m/128Mi requests, 500m/512Mi limits | tune to the number of Secrets and Applications |
| `repoCache.sizeLimit` | `2Gi` | the `emptyDir` Git repositories are cached on |
| `extraArgs` | `[]` | extra operator flags |
| `podSecurityContext`, `securityContext` | restricted, no fixed IDs | see OpenShift below |
| `nodeSelector`, `tolerations`, `affinity`, `podAnnotations` | empty | |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, generated, `{}` | |
| `metrics.enabled`, `metrics.certSecret` | `false`, none | HTTPS on 8443 behind Kubernetes authn/authz |
| `serviceMonitor.enabled` | `false` | needs `metrics.enabled` and the Prometheus Operator |
| `networkPolicy.enabled` | `false` | needs `metrics.enabled`; allows metrics ingress only |

### OpenShift

The chart works under the `restricted-v2` SCC as it is, with no SCC changes and
no `anyuid`: the pod runs as non-root with a read-only root file system, no
capabilities, the default seccomp profile, and no fixed user or group ID, so
OpenShift assigns them. The repository cache is an `emptyDir`, which is
writable for any assigned ID. If your cluster policy requires fixed IDs, set
them in `podSecurityContext` (`runAsUser`, `fsGroup`).

Argo CD on OpenShift (the GitOps operator) usually runs in `openshift-gitops`:
use `argoCDNamespace: openshift-gitops` in the policy, its server URL
(`https://openshift-gitops-server.openshift-gitops.svc`) in `argoCD.url`, and
the `argocd-cm` and `argocd-rbac-cm` of that namespace (or the matching fields
of the `ArgoCD` resource) for the account above. The server certificate is
signed by the cluster's service CA: mount it in a Secret and set
`argoCD.caSecretRef`.

### Upgrade and uninstall

```bash
# CRDs are not touched by helm upgrade: apply the ones of the new version first
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <new> | kubectl apply --server-side -f -
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <new> -n backflow-system

helm uninstall backflow -n backflow-system
# the CRDs, and with them every ScmConnection, BackflowPolicy and DriftProposal, stay:
kubectl delete crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io
```

Helm installs the files in `crds/` once and never upgrades or removes them, so
the upgrade step above is yours to run. Deleting a CRD deletes all its objects.
Branches and merge requests that Backflow opened stay in Git.

### Running in a cluster

- The container root file system is read-only. Git repositories are cached in
  an `emptyDir` (2Gi) mounted at `/var/cache/backflow` and passed with
  `--repo-cache-dir`. The operator checks at startup that the directory is
  writable and exits with an error if it is not.
- Memory requests/limits (128Mi/512Mi) are a starting point; tune them to the
  number of Secrets and Applications in the cluster and the size of the
  repositories.
- `--merge-request-poll-interval` (default `2m`) is how often the state of an
  open merge request is polled. A shorter interval notices a merge or a close
  sooner and costs more calls to the Git host's API.
- Argo CD is reached at `spec.argoCD.url` of the policy, by default
  `https://argocd-server.argocd.svc`. An Argo CD with a private CA needs
  `caSecretRef` (a Secret key holding the PEM bundle).
- Without the Argo CD Application CRD the operator starts anyway; policies are
  `Ready=False` with the reason `ArgoCDNotFound` until Argo CD is installed.

### Trust

**One operator instance is a single trust domain.** The repository cache is
shared by every `BackflowPolicy` and `ScmConnection` the instance serves: a
repository fetched with one connection's token stays in the cache and can be
read for another policy that names the same URL. All Secrets are also readable
by the operator cluster-wide. Run a separate instance (own namespace, own cache
volume) for teams or tenants that must not see each other's repositories.

An Argo CD `repoURL` that carries credentials (`https://user:token@host/...`,
`ssh://user:password@host/...`) is never used or stored. The policy does not
track that Application: its entry in `status.applications` has the reason
`CredentialsInRepoURL` and only the redacted URL, one Event says so, and the
Application is skipped by drift detection, so the URL never reaches a
DriftProposal, the repository cache or a log. (A plain user name in an ssh URL,
`git@host:path`, is not a credential.) Give Backflow access with an
`ScmConnection` instead.

## Development

```bash
./hack/dev-up.sh        # kind cluster "backflow", Argo CD, CRDs, demo-app
make run                # run the operator locally against the current kube context
make test               # unit and envtest tests
make lint               # golangci-lint
make helm-sync          # copy the CRDs and RBAC rules into charts/backflow after make manifests
```

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
```

Releases are made by [release-please](https://github.com/googleapis/release-please)
from Conventional Commits on `main`; the tag it creates publishes the image and
the chart.

## Limitations

- **One source per Application.** An Application with `spec.sources` gets one
  `UnsupportedSource` Event and no proposals.
- **Directory sources only: no Helm or Kustomize mapping yet.** Drift in
  resources rendered by Kustomize or Helm is detected, but the proposal ends
  `Unmapped` with a reason; nothing is written to Git for it.
- **One operator instance is one trust domain.** Policies and connections of an
  instance share the repository cache and the operator can read every Secret;
  run one instance per tenant that must be kept apart (see Trust above).
- **YAML manifests only.** A resource defined in a JSON manifest cannot be
  edited; its proposal is `Unmapped` (`UnsupportedFileFormat`).
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
- **All Secrets are cached cluster-wide.** The operator reads token Secrets
  through an informer that watches every Secret in the cluster, which costs
  memory in clusters with many of them.
- **Open proposals of a blocked or not-Ready policy are left as they are.**
  When a policy stops matching an Application, conflicts with another policy
  or its connection is not Ready, proposals that are already open keep their
  phase; they are not closed or withdrawn.
- **While Git is ahead of the last sync of an Application (manual sync, or a
  failing sync), drift detection for that Application is paused, so Backflow
  never proposes to revert a commit.** For a Directory source, only commits
  that change files under the Application's directory count (respecting
  `directory.recurse`); a commit elsewhere in the repository does not pause
  anything. For Helm and Kustomize sources, Applications that were never
  synced, directories that contain a symbolic link or a Jsonnet file (or use
  `directory.jsonnet`) at either revision, and when the repository cannot be
  read, every commit pauses detection, because the check cannot be trusted.
  When the last sync did not succeed or was selective (only some resources),
  only the resources it did not apply are held back: a resource counts as
  caught up when that sync was a full one that succeeded, or reports the
  resource itself as Synced. Detection for the whole Application is paused when
  its source (path, directory options, target revision, Helm, Kustomize,
  plugin) changed since the last sync. One Event on the policy says so, once
  per resource and revision. Proposals that are already open are left as they are.
  Detection is also held back while a sync is running and until Argo CD has
  compared again after it.
- **A drift becomes a proposal only after it has been stable.** It must be
  seen with the same changes at the same synced and compared revision for the
  policy's `spec.batchWindow` (default 30s), across at least two observations.
  A drift that disappears within the window is never proposed, and several quick
  edits to a resource give one proposal for the final state. This also covers
  the moment after a sync, when Argo CD can still show a resource as OutOfSync.
  What was seen is kept in memory; after a restart the window starts again,
  which only delays a proposal.
