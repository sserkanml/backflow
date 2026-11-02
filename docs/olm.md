# Install from OperatorHub (OpenShift, OLM)

Backflow can be installed with the Operator Lifecycle Manager through a catalog of its own, alongside the Helm chart. The catalog
is not listed on OperatorHub.io or in `community-operators`: you add it to your cluster with a `CatalogSource`, and then
Backflow appears in **Operators → OperatorHub** (OpenShift) or can be subscribed to with `oc`.

The first version in the catalog is already on the final API (`backflow.io/v1beta1`).

| | |
|---|---|
| Package / channel | `backflow` / `stable` |
| Install mode | `AllNamespaces` only (it watches `ScmConnection`, `BackflowPolicy` and Argo CD `Application`s in every namespace) |
| Kubernetes / OpenShift | 1.30 / 4.17 or later (`minKubeVersion`) |
| Images | `ghcr.io/sserkanml/backflow` (the operator), `ghcr.io/sserkanml/backflow-bundle` (the bundle), `ghcr.io/sserkanml/backflow-catalog` (the file-based catalog; tags `<version>` and `stable`) |
| Cluster permissions | the ones of `config/rbac`: `get` on Secrets (never `list` or `watch`), the Backflow resources, Argo CD Applications (read), events, CRD `get` by name |

## 1. Add the catalog

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

## 2. Subscribe, and configure

The operator is configured by environment variables (`BACKFLOW_*`, one for every flag; the table is in the
[README](../README.md#configuration-without-helm-environment-variables)), which you set in `Subscription.spec.config.env`:

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
        value: backflow-local-user              # <user>-local-user, see the OpenShift GitOps section of the README
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

**What the Argo CD token Secret is, and the allow lists, are explained in the README** ([Allowed servers and hosts](../README.md#allowed-servers-and-hosts),
[Multi-tenancy and access](../README.md#multi-tenancy-and-access)); they are the same as the Helm chart's `scm.allowedHosts`,
`argocd.allowedServers` and `argocd.defaultAccess`. An `ArgoCD` CA bundle (`BACKFLOW_ARGOCD_CA_FILE`) and the trusted CA bundle
(`BACKFLOW_TRUSTED_CA_FILE`) are files in the pod: add them with `Subscription.spec.config.volumes` and `volumeMounts`.

Then make an `ScmConnection` and a `BackflowPolicy` as in the README ([samples](../config/samples)).

## Metrics

The CSV ships the metrics service (`:8443`, TLS, protected by token review). To scrape it, give the Prometheus of OpenShift access
(label the namespace `openshift.io/cluster-monitoring=true` and create a `ServiceMonitor`), as for any operator.

## The actor webhook (who made a change): not part of the CSV

The actor webhook is an observe-only admission webhook that tells the operator who changed a resource, so that a proposal can name the user.
In the Helm chart it is a `ValidatingWebhookConfiguration` with `matchConditions` (CEL) that keep out Secrets, dry runs, system
components and resources Argo CD does not track. **A ClusterServiceVersion's `webhookdefinitions` cannot express `matchConditions`**
(nor can they be optional): put in the CSV, the webhook would send *every update of every namespaced resource, Secrets included,* to the operator and
could not be switched off. So it is **off by default and a separate opt-in**, and the CSV does not declare it. OLM therefore does not
manage its certificate; on OpenShift the service CA does.

[`docs/olm/actor-webhook.yaml`](olm/actor-webhook.yaml) is made from the Helm chart (`make olm-actor-webhook`, checked in CI), so that
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
      # optional: BACKFLOW_ACTOR_RECORD_USERNAME=false keeps user names out of the records; see the table in the README
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

## Upgrading

Upgrades follow the channel: a new catalog image (`:stable`) with a newer bundle makes OLM propose an `InstallPlan` for the
subscription; `installPlanApproval: Automatic` applies it, `Manual` waits for `oc patch installplan ... approved=true`. Each version
in the channel `replaces` the one before it, so a subscription goes through them in order.

## Coming from the Helm chart

The first version in the catalog is on the final API, and OLM does not migrate objects. From **a Helm release of 0.18 or later**
(already `v1beta1`): `helm uninstall backflow -n backflow-system` (the CRDs and your objects stay), then subscribe. From **0.17 or
earlier** (`v1alpha1`):

1. Uninstall the Helm release: `helm uninstall backflow -n <namespace>`. The CRDs and the `ScmConnection`, `BackflowPolicy` and
   `DriftProposal` objects stay (Helm never deletes CRDs). The operator is gone, which `migrate` needs.
2. Convert the manifests in Git, if Argo CD applies your Backflow objects: `manager migrate convert-files` (see
   [the upgrade guide](upgrade-v1beta1.md#1-convert-the-manifests-in-git-gitops-users)).
3. Run the migration once, with the container image of the version you subscribe to, and your own kubeconfig. Keep the backup
   directory:
   ```bash
   podman run --rm --user "$(id -u)" -v "$HOME/.kube:/kube:ro" -v "$PWD/backflow-backup:/backup:Z" -e KUBECONFIG=/kube/config \
     ghcr.io/sserkanml/backflow:<version> migrate --backup-dir /backup
   ```
   (`--dry-run` first.) It adds `v1beta1` to the CRDs, rewrites the objects and removes `v1alpha1`. The details, the rollback and
   what the tool does are in [docs/upgrade-v1beta1.md](upgrade-v1beta1.md).
4. Subscribe (step 2 above), with the settings you had in the Helm values as `BACKFLOW_*` variables (the table in the README has
   the Helm value of each).

OLM takes over the existing CRDs when the bundle's CRDs are compatible, which the migrated ones are.

## For maintainers

* `make bundle` writes `bundle/` and `bundle.Dockerfile` from `config/` (with `operator-sdk`, pinned and checksummed in
  `hack/olm.mk`); `make bundle-validate` runs the validation of the bundle, the OperatorHub suite and the good practices; the
  descriptors of the fields come from `+operator-sdk:csv:customresourcedefinitions` markers on the API types, and the rest of
  the CSV (description, icon, links, install modes, `minKubeVersion`) from `config/manifests/bases`.
* `make catalog` writes the file-based catalog from `catalog/bundles.txt` (the versions published before, rendered from their
  images) and `bundle/`; `make catalog-build` builds the image.
* The release workflow pushes `backflow-bundle:<version>` and `backflow-catalog:<version>` and `:stable`; the workflow for `main`
  pushes `:main` of both (a bundle that names `backflow:main`). **After a release, add its version to `catalog/bundles.txt`**
  in the next change, or the next catalog will not have it and the channel will not replace it. The images are not signed
  (neither is the operator image).
* `hack/test-olm.sh` is the test on kind: it installs OLM (`operator-sdk olm install`, pinned), builds the images of the
  checkout, adds the catalog, subscribes, waits for the CSV and the operator, makes an `ScmConnection` and a `BackflowPolicy`,
  runs the scorecard and uninstalls. CI runs it (`.github/workflows/olm.yml`).
