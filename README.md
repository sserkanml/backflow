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

`applications, get` is also the permission Backflow needs to render Helm charts
through Argo CD (see Helm sources below); nothing more is required.

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

### Upgrading: apply the CRDs first

**Before every `helm upgrade`, apply the CRDs of the version you upgrade to:**

```bash
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <version> | kubectl apply --server-side -f -
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <version> -n backflow-system
```

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

- 0.5.0 adds `spec.helm.addOverrides` to the `BackflowPolicy`. With the old CRD
  the field is dropped and the default (on) applies, so you cannot turn
  overrides off until the CRDs are applied. **This changes what proposals
  become:** a Helm change whose old value only the chart's defaults hold used to
  be `Unmapped` and now becomes a merge request that adds a key to your last
  value file. Set `spec.helm.addOverrides: false` on the policy to keep the old
  behaviour.

Applying the CRDs afterwards fixes new proposals; a proposal that failed is not
retried.

## Development

```bash
./hack/dev-up.sh        # kind cluster "backflow", Argo CD, CRDs, demo-app
make run                # run the operator locally against the current kube context
make test               # unit and envtest tests
make lint               # golangci-lint
make coverage           # how much real-chart and overlay drift maps back to Git (needs helm; installs kustomize; see test/coverage/REPORT.md)
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
IN_CLUSTER=true ./hack/test-kustomize.sh    # needs kustomize/ in the demo repository (dev-up.sh copies it)
```

Releases are made by [release-please](https://github.com/googleapis/release-please)
from Conventional Commits on `main`; the tag it creates publishes the image and
the chart.

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
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
spec:
  helm:
    addOverrides: true        # the default; false leaves such drift Unmapped
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
  stays `Unmapped`; so does an Application without any value file.
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
needs `spec.helm.addOverrides`. Maps and lists are written in block style, the
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
apiVersion: backflow.io/v1alpha1
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
  `spec.helm.addOverrides: false` it is `Unmapped` (`NoVerifiedCandidate`,
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
   | 4 | A file outside the path (a shared base or component), **only with `spec.kustomize.editBases`** | `Direct` |
   | 5 | A **new patch**: `<overlay>/backflow-patches/<kind>-<name>.yaml` with only the drifted fields (a removed list item as `$patch: delete`, a removed key as `null`), listed under `patches:` in the overlay's `kustomization.yaml`; an existing file of that name is extended and keeps its comments. Only with `spec.kustomize.addPatches` | `KustomizePatch` |

   Comments, key order and quoting of the edited files are kept.
4. The merge request and the commit say what kind of edit each one is, that it was
   verified by rendering, and, for a new patch file, that the file was added. The
   new file and the edited `kustomization.yaml` are in the same commit.

```yaml
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
spec:
  kustomize:
    editBases: false     # default; true allows edits of files outside the Application's path
    addPatches: true     # default; false leaves what no setting, patch or file holds Unmapped
```

`spec.mapping.allowPartial` and `spec.helm.allowCollateralChanges` apply to
Kustomize sources the same way as to Helm ones.

### Shared bases

A base is usually shared by several environments, so editing it changes them too.
Backflow works out which other Applications build a file outside the Application's
path (the files each Application of **any** BackflowPolicy reads):

- If **no other Application** builds the file, tier 4 comes before tier 5.
- If **others do**, a new patch (tier 5) is tried first; the shared file is edited
  only when that cannot be verified or `addPatches` is false. The merge request and
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
  Git cannot change it: `SetByApplication`, naming the option.
- **Remote bases, `helmCharts`, plugins and KRM functions:** the output is not a
  function of the files Backflow can edit and render.
- **Secrets**, as for Helm (`SecretNotVerifiable`).
- A value no setting, patch or file holds, with `addPatches: false`
  (`NoVerifiedCandidate`); the message names a file that holds the value but is
  outside the path while `editBases` is off.
- Changing the **merge key** of a list item (a Service's `port`, a container's `name`)
  cannot be expressed as a patch; it needs `editBases` to edit the file itself.

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
| `NoVerifiedCandidate` | no value file holds the value, or it is set in the Application (`spec.sources[N].helm.values`, `valuesObject`, `parameters`); Backflow does not map values set in the Application yet |
| `UnsupportedSourceType` | the Application is outside the supported shape |

Detection while Git is ahead is per source: a commit pauses the Application only
when it changes a value file of a ref source (globs expanded at both revisions) or,
for a chart in Git, a file the chart reads. A **new chart version** (in the
Application, or a range that resolved to one) is a change of the Application, not
Git being ahead: an `ApplicationChanged` Event says so, and detection is paused
until Argo CD syncs the Application.

## Partial proposals

By default a proposal is all or nothing: when one drifted field cannot be traced
back to Git, the whole proposal is `Unmapped`, even though the other fields
could be proposed. A policy can opt in to proposing what can be proven:

```yaml
apiVersion: backflow.io/v1alpha1
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
