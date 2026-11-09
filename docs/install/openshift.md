# Install on OpenShift

Backflow runs on OpenShift 4.17 or later (Kubernetes 1.30) under the `restricted-v2` security context constraint, with no changes
to it. There are three parts to this page:

- **[OperatorHub](#install-from-operatorhub-openshift-olm)**: install the operator with the Operator Lifecycle Manager from a
  catalog of the project, and configure it with `BACKFLOW_*` variables.
- **[OpenShift GitOps](#install-on-openshift-gitops)** (1.18 or later): let the `ArgoCD` resource manage the read-only account of
  Backflow, so that no token is made by hand, and verify the Argo CD server with the service CA.
- **[The Helm chart on OpenShift](#helm-on-openshift)**: the security context, and the manual account for an Argo CD that is not
  OpenShift GitOps.

The chart is described in [Install with Helm](helm.md); every setting is in [Configuration](../configuration.md).

## Install from OperatorHub (OpenShift, OLM)

Backflow can be installed with the Operator Lifecycle Manager through a catalog of its own, alongside the Helm chart. The catalog
is not listed on OperatorHub.io or in `community-operators`: you add it to your cluster with a `CatalogSource`, and then
Backflow appears in **Operators → OperatorHub** (OpenShift) or can be subscribed to with `oc`.

The first version in the catalog is 1.0.0, already on the final API (`backflow.io/v1`).

| | |
|---|---|
| Package / channel | `backflow` / `stable` |
| Install mode | `AllNamespaces` only (it watches `ScmConnection`, `BackflowPolicy` and Argo CD `Application`s in every namespace) |
| Kubernetes / OpenShift | 1.30 / 4.17 or later (`minKubeVersion`) |
| Images | `ghcr.io/sserkanml/backflow` (the operator), `ghcr.io/sserkanml/backflow-bundle` (the bundle), `ghcr.io/sserkanml/backflow-catalog` (the file-based catalog; tags `<version>` and `stable`) |
| Cluster permissions | the ones of `config/rbac`: `get` on Secrets (never `list` or `watch`), the Backflow resources, Argo CD Applications (read), events, CRD `get` by name |

### 1. Add the catalog

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: backflow-catalog
  namespace: openshift-marketplace        # olm, on a cluster with a plain OLM
spec:
  sourceType: grpc
  image: ghcr.io/sserkanml/backflow-catalog:stable
  displayName: Backflow
  publisher: sserkanml
  updateStrategy:
    registryPoll:
      interval: 30m                       # how often OLM looks for a newer catalog image: upgrades follow the channel
```

```bash
oc apply -f catalogsource.yaml
oc -n openshift-marketplace get catalogsource backflow-catalog -o jsonpath='{.status.connectionState.lastObservedState}{"\n"}'   # READY
oc get packagemanifest backflow -n openshift-marketplace
```

On a cluster that cannot reach ghcr.io, mirror the three images (`oc adm catalog mirror`, or `oc-mirror`) and point `image:` and an
`ImageContentSourcePolicy`/`ImageDigestMirrorSet` at the mirror.

### 2. Subscribe, and configure

The operator is configured by environment variables (`BACKFLOW_*`, one for every flag; the table is in the
[Configuration](../configuration.md#flags-environment-variables-and-helm-values)), which you set in `Subscription.spec.config.env`:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: backflow
  namespace: openshift-operators          # the global OperatorGroup of OpenShift: AllNamespaces
spec:
  channel: stable
  name: backflow
  source: backflow-catalog
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic          # Manual: every upgrade waits for an approval
  config:
    env:
      # The Git hosts a ScmConnection may name. The default is gitlab.com,github.com: LIST YOUR SELF-HOSTED GITLAB OR GITHUB.
      - name: BACKFLOW_SCM_ALLOWED_HOSTS
        value: gitlab.example.com,gitlab.com
      # The Argo CD of OpenShift GitOps, for the policies that name none, and who may use it (policy namespaces : projects).
      - name: BACKFLOW_ARGOCD_SERVER
        value: https://openshift-gitops-server.openshift-gitops.svc
      - name: BACKFLOW_ARGOCD_TOKEN_SECRET_NAMESPACE
        value: openshift-gitops
      - name: BACKFLOW_ARGOCD_TOKEN_SECRET_NAME
        value: backflow-local-user              # <user>-local-user, see Install on OpenShift GitOps below
      - name: BACKFLOW_ARGOCD_DEFAULT_ACCESS
        value: "team-a,team-b:team-a;platform:*"   # several values: separated by ; (or new lines)
      # Where the operator reads Secrets (its own namespace and the Argo CD token's are always included). The CSV grants
      # get on Secrets in every namespace; this keeps the operator out of the others. Empty: any.
      - name: BACKFLOW_SECRET_NAMESPACES
        value: team-a,team-b
```

```bash
oc apply -f subscription.yaml
oc -n openshift-operators get csv -w            # backflow.v<version>: Succeeded
oc -n openshift-operators get deploy backflow-controller-manager
```

A variable is read when the operator starts; changing `config.env` restarts it. A flag given as an `args` entry would win over
a variable, but a Subscription cannot add arguments: use the variables.

**What the Argo CD token Secret is, and the allow lists, are explained in the configuration and security guides** ([Allowed servers and hosts](../configuration.md#allowed-servers-and-hosts),
[Multi-tenancy and access](../security.md#multi-tenancy-and-access)); they are the same as the Helm chart's `scm.allowedHosts`,
`argocd.allowedServers` and `argocd.defaultAccess`. An `ArgoCD` CA bundle (`BACKFLOW_ARGOCD_CA_FILE`) and the trusted CA bundle
(`BACKFLOW_TRUSTED_CA_FILE`) are files in the pod: add them with `Subscription.spec.config.volumes` and `volumeMounts`.

Then make an `ScmConnection` and a `BackflowPolicy` as in [Getting started](../getting-started.md) ([samples](../../config/samples)).

### Metrics

The CSV ships the metrics service (`:8443`, TLS, protected by token review). To scrape it, give the Prometheus of OpenShift access
(label the namespace `openshift.io/cluster-monitoring=true` and create a `ServiceMonitor`), as for any operator.

### The actor webhook (who made a change): not part of the CSV

The actor webhook is an observe-only admission webhook that tells the operator who changed a resource, so that a proposal can name the user.
In the Helm chart it is a `ValidatingWebhookConfiguration` with `matchConditions` (CEL) that keep out Secrets, dry runs, system
components and resources Argo CD does not track. **A ClusterServiceVersion's `webhookdefinitions` cannot express `matchConditions`**
(nor can they be optional): put in the CSV, the webhook would send *every update of every namespaced resource, Secrets included,* to the operator and
could not be switched off. So it is **off by default and a separate opt-in**, and the CSV does not declare it. OLM therefore does not
manage its certificate; on OpenShift the service CA does.

[`docs/olm/actor-webhook.yaml`](../olm/actor-webhook.yaml) is made from the Helm chart (`make olm-actor-webhook`, checked in CI), so that
its rules, `matchConditions`, timeout (1 s) and `failurePolicy: Ignore` are the chart's. To use it:

```bash
NS=openshift-operators
WEBHOOK_PATH="observe-$(openssl rand -hex 16)"        # the secret of the webhook
# 1. the Service (the service CA writes the certificate Secret) and the webhook configuration
sed -e "s/OPERATOR_NAMESPACE/$NS/g" -e "s#WEBHOOK_PATH#$WEBHOOK_PATH#g" docs/olm/actor-webhook.yaml | oc apply -f -
oc -n "$NS" get secret backflow-controller-manager-actor-webhook-tls        # wait until it exists
# 2. the operator: the Subscription gets the variables and the certificate volume
```

```yaml
spec:
  config:
    env:
      - {name: BACKFLOW_ACTOR_WEBHOOK, value: "true"}
      - {name: BACKFLOW_ACTOR_WEBHOOK_PATH, value: "/observe-0123...(the same path)"}
      - {name: BACKFLOW_WEBHOOK_CERT_PATH, value: /var/run/actor-webhook-cert}
      # optional: BACKFLOW_ACTOR_RECORD_USERNAME=false keeps user names out of the records; see the table in docs/configuration.md
    volumes:
      - name: actor-webhook-cert
        secret: {secretName: backflow-controller-manager-actor-webhook-tls}
    volumeMounts:
      - {name: actor-webhook-cert, mountPath: /var/run/actor-webhook-cert, readOnly: true}
```

To turn it off, delete the webhook configuration and remove the variables. Add the Argo CD application controller of your
cluster to the users the webhook ignores by editing the `matchConditions` of the file before you apply it (the chart's
`actorWebhook.excludeUsers`; the file has the chart's default). **This path has not been tested on an OpenShift cluster; the kind test
does not cover it** (there is no service CA on kind). The generated file is checked against the chart.

### Upgrading

Upgrades follow the channel: a new catalog image (`:stable`) with a newer bundle makes OLM propose an `InstallPlan` for the
subscription; `installPlanApproval: Automatic` applies it, `Manual` waits for `oc patch installplan ... approved=true`. Each version
in the channel `replaces` the one before it, so a subscription goes through them in order.

### Uninstall

Delete the `Subscription` and the `ClusterServiceVersion` (`oc -n openshift-operators delete subscription backflow` and
`oc -n openshift-operators delete csv backflow.v<version>`). The operator and the permissions OLM made for it are removed. The CRDs
and your objects stay (OLM does not delete CRDs, and deleting a CRD deletes its objects). The end-user roles of the bundle
(`backflow-*-admin-role`, `-editor-role`, `-viewer-role`, `backflow-metrics-reader`) also stay, unbound; remove them with
`oc delete clusterrole -l olm.owner=backflow.v<version>`.

### Coming from the Helm chart

The first version in the catalog is 1.0.0, on the final API (`backflow.io/v1`), and OLM does not migrate objects: do it with `manager migrate`
(the way from either source version is in [docs/upgrade-1.0.md](../operations/upgrade-1.0.md)). Start with `helm uninstall backflow -n <namespace>`: the
CRDs and your `ScmConnection`, `BackflowPolicy` and `DriftProposal` objects stay (Helm never deletes CRDs), and the operator is gone.

* **From a Helm release of 0.18.x** (objects stored as `v1beta1`): subscribe, then run `migrate` (no backup, no stop: it stores each object again as
  `v1` with an update that changes nothing). Or run it before you subscribe; the order does not matter. Until it has run the operator works and
  says so in its log and in the metric `backflow_storage_migration_pending`.
* **From 0.17 or earlier** (`v1alpha1`): `migrate` first, **then** subscribe; the operator of 1.0 refuses to start on `v1alpha1` objects.
  1. Convert the manifests in Git, if Argo CD applies your Backflow objects: `manager migrate convert-files` (see
     [the upgrade guide](../operations/upgrade-1.0.md#1-convert-the-manifests-in-git-gitops-users)).
  2. Run the migration once, with the container image of the version you subscribe to, and your own kubeconfig. Keep the backup directory:
     ```bash
     podman run --rm --user "$(id -u)" -v "$HOME/.kube:/kube:ro" -v "$PWD/backflow-backup:/backup:Z" -e KUBECONFIG=/kube/config        ghcr.io/sserkanml/backflow:<version> migrate --backup-dir /backup
     ```
     (`--dry-run` first.) It adds `v1` to the CRDs, converts the objects and removes `v1alpha1`. The details, the rollback and what the
     tool does are in [docs/upgrade-1.0.md](../operations/upgrade-1.0.md).
  3. Subscribe, with the settings you had in the Helm values as `BACKFLOW_*` variables (the table in [Configuration](../configuration.md) has the Helm value of each).

OLM takes over the existing CRDs when the bundle's CRDs are compatible, which the migrated ones are.

### For maintainers

* `make bundle` writes `bundle/` and `bundle.Dockerfile` from `config/` (with `operator-sdk`, pinned and checksummed in
  `hack/olm.mk`); `make bundle-validate` runs the validation of the bundle, the OperatorHub suite and the good practices; the
  descriptors of the fields come from `+operator-sdk:csv:customresourcedefinitions` markers on the API types, and the rest of
  the CSV (description, icon, links, install modes, `minKubeVersion`) from `config/manifests/bases`.
* `make catalog` writes the file-based catalog from the releases (the git tags `vX.Y.Z` from 1.0.0 on, each rendered from its image
  `backflow-bundle:X.Y.Z`) and `bundle/`; `make catalog-build` builds the image. **There is no list to edit after a release.** The
  tags are the record of what was released, and `hack/olm-catalog.sh` renders every one of them from its published image, so a
  release whose bundle was never pushed makes the catalog of the next release, and that release, fail (a catalog without it
  would upgrade nobody to the new version), and a clone without the tags is refused, because it would leave versions out
  without a word. The release workflow therefore checks out all the tags.
* The release workflow pushes `backflow-bundle:<version>` and `backflow-catalog:<version>` and `:stable`; the workflow for `main`
  pushes `:main` of both (a bundle that names `backflow:main`). The images are not signed (neither is the operator image).
* `hack/test-olm.sh` is the test on kind: it installs OLM (`operator-sdk olm install`, pinned), builds the images of the
  checkout, adds the catalog, subscribes, waits for the CSV and the operator, makes an `ScmConnection` and a `BackflowPolicy`,
  runs the scorecard and uninstalls. CI runs it (`.github/workflows/olm.yml`).

## Install on OpenShift GitOps

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
[Multi-tenancy and access](../security.md#multi-tenancy-and-access).)

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
described under [Install](../getting-started.md); a policy that has none and an operator without
`argocd.server` detects no drift. The `ScmConnection` for GitLab or GitHub is made as
in step 2 of Install.

**Token renewal.** OpenShift GitOps writes the new token into the same Secret before the old
one expires. The operator reads the Secret by name when it needs it and keeps it for
`secretCacheTTL` (30s). When Argo CD answers 401, it drops that copy, reads the Secret
again and repeats the call once with what is there now; a proposal being mapped retries
the same way. So the renewal is picked up within the TTL whether or not Argo CD rejects the
old token at once, and an Argo CD that really refuses the token is reported as before
(Event `ArgoCDUnauthorized`).

## Helm on OpenShift

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
of the `ArgoCD` resource) for the account of [Getting started](../getting-started.md#2-a-read-only-argo-cd-account). The server certificate is
signed by the cluster's service CA: mount it in a Secret and set
`argoCD.caSecretRef`.

With OpenShift GitOps 1.18 or later you can skip the account, the token Secret and the CA
Secret: see [Install on OpenShift GitOps](#install-on-openshift-gitops). The manual way
(an account made by hand, a policy with `spec.argoCD.server`) keeps working, and so does any
upstream Argo CD.
