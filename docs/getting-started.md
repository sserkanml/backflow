# Getting started

This page takes you from nothing to your first merge request: install Backflow, give it a read-only Argo CD account and a
token for your Git host, watch it in `ReportOnly` mode, then let it open merge requests. For what the parts are, see
[Concepts](concepts.md).

## Before you start

- A Kubernetes cluster (1.30 or later) or an OpenShift cluster (4.17 or later) with **Argo CD 3.x** and at least one
  Application whose source is in a Git repository on GitLab or GitHub (a plain-manifest directory is the simplest to try).
- `kubectl`, and Helm 3.8 or later for the commands below.
- Rights to create a token on the Git host: the scopes `api` and `write_repository` on GitLab, or `contents` and
  `pull_requests` on GitHub. Use a token of a service account that can push a branch and open a merge request, and nothing more.
- On OpenShift with OpenShift GitOps 1.18 or later you can skip steps 2 and the Argo CD part of step 3: see
  [Install on OpenShift](install/openshift.md#install-on-openshift-gitops).

## 1. Install Backflow

```bash
helm install backflow oci://ghcr.io/sserkanml/charts/backflow \
  --version 1.0.0 --namespace backflow-system --create-namespace
```

The chart installs the CRDs (`ScmConnection`, `BackflowPolicy`, `DriftProposal`), a ClusterRole, the Deployment and, with
`leaderElection: true` (the default), a leader election Role. If your Git host is self-hosted, add it to the allowed hosts now
(`--set 'scm.allowedHosts={gitlab.example.com,gitlab.com}'`; the default is `gitlab.com` and `github.com`): see
[Configuration](configuration.md#allowed-servers-and-hosts). Other ways to install, and every value, are in
[Install with Helm](install/helm.md), [Install on OpenShift](install/openshift.md) and [Install with kustomize](install/kustomize.md).

## 2. A read-only Argo CD account

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

`applications, get` is also the permission Backflow needs to render Helm charts
through Argo CD (see [Helm mapping](mapping/helm.md)); nothing more is required.

For Applications with several sources (a chart from a Helm repository or an OCI
registry, and its value files in a Git repository, `$values/...`) Backflow also
reads the chart's own `values.yaml` through Argo CD, which needs one more line,
for the repositories the charts come from (`*` for all of them):

```
p, role:backflow, repositories, get, *, allow
```

Without it such a proposal stays in `Mapping` with the reason
`ChartDefaultsUnavailable` and is tried again. The object of that permission is
the bare repository URL, not `<project>/<url>`; `default/*` does not match.

```bash
argocd login <argocd-server> --username admin
TOKEN="$(argocd account generate-token --account backflow)"
kubectl -n backflow-system create secret generic argocd-token --from-literal=token="$TOKEN"
```

## 3. A token for the Git host, a connection and a policy

Create the token with the scopes `api` and `write_repository` (GitLab) or
`contents` and `pull_requests` (GitHub). It, the connection and the policy live
in the same namespace.

```bash
kubectl -n backflow-system create secret generic gitlab-token --from-literal=token="$GITLAB_TOKEN"

kubectl -n backflow-system apply -f - <<'YAML'
apiVersion: backflow.io/v1
kind: ScmConnection
metadata:
  name: gitlab
spec:
  provider: GitLab                  # or GitHub
  url: https://gitlab.com           # your server for a self-hosted one
  tokenSecretRef: {name: gitlab-token, key: token}
---
apiVersion: backflow.io/v1
kind: BackflowPolicy
metadata:
  name: my-apps
spec:
  argoCD:
    namespace: argocd
    server:                         # leave it out to use the operator's own connection
      url: https://argocd-server.argocd.svc
      tokenSecretRef: {name: argocd-token, key: token}
  applications:
    names: [my-app]                 # or selector: {matchLabels: {env: test}}
  mode: ReportOnly                  # MergeRequest and DirectCommit write to Git
YAML

kubectl -n backflow-system get scm,bfp,dp
```

A policy that names its own Argo CD server needs that server in `argocd.allowedServers` (or to be `argocd.server`), and the host
of a `ScmConnection` has to be in `scm.allowedHosts` (`gitlab.com` and `github.com` are by default); see
[Allowed servers and hosts](configuration.md#allowed-servers-and-hosts).

With `ReportOnly` Backflow finds the file that defines each drifted resource and
shows the verified edit in the `DriftProposal` (`kubectl get dp -o yaml`), but
writes nothing to Git. Change `mode` to `MergeRequest` to get a merge request
per drift. More options (ignored fields, included and excluded kinds, branch
prefix, labels, reviewers, batch window) are in
[config/samples](../config/samples).

## 4. Watch a drift in `ReportOnly`

Change something in the cluster that Argo CD manages, for example a value of a ConfigMap of the Application:

```bash
kubectl -n demo patch configmap demo-config --type merge -p '{"data":{"LOG_LEVEL":"debug"}}'
```

Argo CD shows the Application as `OutOfSync`. Backflow waits until the change has been stable for the policy's batch window
(30 seconds by default, see [Batching](concepts.md#batching)) and then creates a `DriftProposal`:

```bash
kubectl -n backflow-system get dp -w
kubectl -n backflow-system get dp <name> -o yaml      # spec.changes, status.mapping.diff, status.conditions
```

The proposal goes from `Detected` to `Mapping` and then to `Proposed`; in `ReportOnly` it keeps the verified edit in
`status.mapping` (the file, the lines, a unified `diff`) and writes nothing to Git. If it ends `Unmapped`, the `Mapped`
condition and `status.mapping.reason` say why, and [Troubleshooting](operations/troubleshooting.md) says what to do.

## 5. Open merge requests

When the proposals look right, let the policy write to Git:

```bash
kubectl -n backflow-system patch bfp my-apps --type merge -p '{"spec":{"mode":"MergeRequest"}}'
```

Make a change as in step 4 again. Backflow pushes a branch `backflow/<proposal>-<uid>` and opens a merge request (a pull request
on GitHub) that shows the change, the file it edits, and that Argo CD rendered the edit and got the live state. The link is in
`status.git` of the proposal. Merge it, and within the poll interval (`mergeRequestPollInterval`, 2 minutes by default) the
proposal becomes `Merged`; Argo CD syncs the new commit and the Application is `Synced` again. Close it instead, and the
proposal becomes `Rejected` and Argo CD can revert the cluster to what Git says.

## 6. Where next

- Choose what is watched and what is ignored (`include`, `exclude`, `ignoreFields`), set labels and reviewers
  ([config/samples](../config/samples)), and the other [modes](concepts.md#modes).
- Helm and Kustomize Applications: [What maps](mapping/overview.md).
- More than one team on a cluster: [Security](security.md) and [Configuration](configuration.md).
- Metrics, alerts and a dashboard: [Metrics](operations/metrics.md). A proposal that does not do what you expect:
  [Troubleshooting](operations/troubleshooting.md).
