# Upgrading to 1.0

Backflow 1.0 serves its API as **`backflow.io/v1`** (`ScmConnection`, `BackflowPolicy`, `DriftProposal`). The schema is the one of
`v1beta1`, which 0.18 shipped, without a change; `v1` is the storage version, and `v1beta1` stays served, deprecated, for all of
1.x (see the [compatibility policy](../compatibility.md)). `v1alpha1`, the API of 0.17 and earlier, is gone.

Objects stored in an older version have to be moved to `v1` once, with `manager migrate`. Which way depends on where you come from:

| You run | What `migrate` does | The operator | A backup |
|---|---|---|---|
| **0.18.x** (objects stored as `v1beta1`) | stores each object again as `v1` with an update that changes nothing, `DriftProposal`s included (no delete, no create) | **keeps running** | not needed |
| **0.17 and earlier** (`v1alpha1`) | converts `ScmConnection` and `BackflowPolicy` in place, deletes and creates each `DriftProposal` again with its status | **stopped** | **required** |

0.18.0 cannot be installed from its chart (its CRDs had no group); if you run it from raw manifests, treat it as 0.18.x.

Nothing breaks for a 0.18.x user who does not migrate: the operator of 1.0 reads and writes `v1beta1` objects as `v1` (the schemas are
the same, the CRD has no conversion), starts with a warning in its log, and sets the metric `backflow_storage_migration_pending`
to the number of CRDs that still hold `v1beta1` objects until `migrate` has run. `migrate` is needed before `v1beta1` can be dropped
in 2.0, and it is cheap, so do it with the upgrade. The operator of 1.0 **refuses to start** while objects are stored as `v1alpha1`.

## What to know first

- **For 0.17 and earlier: run `migrate` before you apply the CRDs of this version.** It adds `v1` to the CRDs that are installed,
  converts the objects, and removes `v1alpha1` when none is left. The API server refuses a CRD that drops `v1alpha1` while objects
  are stored in it (`Error from server (Invalid)`), so applying the new CRDs first fails; nothing is lost. For **0.18.x** the
  order does not matter: you can apply the CRDs first (the usual way), and `migrate` then needs nothing but the cluster.
- **A write through `v1alpha1` after the CRD stores `v1` loses fields**: the API server prunes it to the `v1` schema. Convert the manifests in
  Git (below) before they are applied again; the tool never writes through `v1alpha1`.
- A `DriftProposal` of 0.17 cannot be changed in place (its spec is immutable and has other field names), so `migrate` deletes and
  creates each one again, with its status; the new object has a new `creationTimestamp` (the old one is in the annotation
  `migrate.backflow.io/created`) and a new uid. Merge requests on the Git host are not touched. **Finished proposals are deleted after 14
  days** (`spec.retention.finishedProposals`), counted from `status.finishedAt`, which the tool sets from the last change of their conditions:
  one that finished more than 14 days ago is deleted when the operator starts. `--skip-finished` backs up and deletes the finished ones instead.
- `ChangeRecord`s (what the optional actor webhook buffers, for two hours) are not migrated and not backed up from 0.17 (they hold user
  names). They live in `changerecords.internal.backflow.io`, which the tool creates. Delete the old CRD afterwards: `kubectl delete crd changerecords.backflow.io`.
- Kubernetes **1.30** or later (OpenShift 4.17).

## 1. Convert the manifests in Git (GitOps users)

If Argo CD (or anything else) applies your `ScmConnection` and `BackflowPolicy` from Git, convert those files and merge the change.
`v1alpha1` manifests **must** be converted (after the migration they fail to sync or lose fields); `v1beta1` manifests keep working
until 2.0, and converting them only changes the `apiVersion` line.

```bash
manager migrate convert-files path/to/gitops/repo          # rewrites the files in place
manager migrate convert-files --check path/to/gitops/repo   # changes nothing; exit 1 if a file would change (for CI)
```

(`manager` is `/manager` in the image: `docker run --rm -v "$PWD:/repo" ghcr.io/sserkanml/backflow:<version> migrate convert-files /repo`.)

It reads `.yaml` and `.yml` files, converts the `ScmConnection` and `BackflowPolicy` documents of `backflow.io/v1alpha1` and `v1beta1` to
`v1`, and leaves everything else as it is, byte for byte: other documents, other files, comments outside the converted documents. A
`v1beta1` document gets the new `apiVersion` and nothing else changes. A `v1alpha1` document keeps its comments and the order of its
keys and is written again with two spaces of indentation. Documents it cannot convert safely (a Helm template, a YAML anchor, an unknown
`provider`) are named and left alone; `--check` fails for them too. Helm and Kustomize sources are not rewritten: a template that builds a
Backflow object needs the same renames by hand.

Merge it when you are ready for step 3. Until the objects are migrated, the Application that applies a `v1` manifest reports a sync error
("no matches for kind ... in version backflow.io/v1") on a cluster that has not got the `v1` CRDs yet; nothing is written. If you prefer,
switch off its automatic sync until step 3 is done.

## 2. Set the allow lists

Three settings of the chart (or the `BACKFLOW_*` variables of an installation without Helm) decide what the operator may call and see. They are
the installer's, not the tenants', and the defaults are strict. If you came from 0.18 you have them already; from 0.17 they are new:

- `scm.allowedHosts`: the Git hosts a `ScmConnection` may name. **The default is `gitlab.com` and `github.com`: a self-hosted GitLab or
  GitHub must be listed** (`scm.allowedHosts: [gitlab.example.com]`, or `*.example.com`), or its connection is not `Ready` (`HostNotAllowed`).
- `argocd.allowedServers`: the Argo CD servers a policy may name in `spec.argoCD.server.url`, besides `argocd.server`. Others are `ServerNotAllowed`.
- `argocd.defaultAccess`: which policies (by namespace) see which Argo CD projects through the operator's own connection. Empty: only the namespace
  the operator is installed in. A policy elsewhere without its own `spec.argoCD.server` is not `Ready` (`ArgoCDDefaultNotAllowed`). A single-tenant
  cluster keeps the old behaviour with `[{policyNamespaces: ["*"], projects: ["*"]}]`.

See [Allowed servers and hosts](../configuration.md#allowed-servers-and-hosts) and [Multi-tenancy and access](../security.md#multi-tenancy-and-access).

## 3. Upgrade

### Helm, from 0.18.x

The usual order: the CRDs of the new version, then the chart, then `migrate`; the operator runs throughout.

```bash
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version 1.0.0 | kubectl apply --server-side --force-conflicts -f -
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version 1.0.0 -n backflow-system --reuse-values
manager migrate --dry-run        # what it would do: nothing is changed
manager migrate                  # stores every object again as v1; the operator keeps running
kubectl get crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.storedVersions}{"\n"}{end}'
```

Or let the chart do it, with the CRDs left to the tool: the hook Job of `helm upgrade` runs `migrate` first (nothing is stopped, nothing
backed up, so an `emptyDir` will do):

```bash
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version 1.0.0 -n backflow-system --reuse-values \
  --set migration.enabled=true --set migration.backup.emptyDir=true
```

The Job adds `v1` to the CRDs itself (do not apply them before it, or do; both work from 0.18). It does nothing when no object is stored as
`v1alpha1` or `v1beta1`, so the setting can stay on, but turn it off afterwards. If the Job fails, the upgrade fails and the operator of 0.18
is still running: read the Job's log, fix what it names, and run `helm upgrade` again.

### Helm, from 0.17 and earlier

The operator must be stopped, and the order is fixed: **migrate first, then the CRDs and the chart.**

With the chart: a pre-upgrade hook Job (the new image, `manager migrate --stop-operator`) scales the operator down, backs the objects up
and migrates; a post-upgrade hook scales it back to what it was. It needs a `PersistentVolumeClaim` for the backup, because the backup is
what a rollback uses:

```bash
kubectl -n backflow-system apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: backflow-migration-backup}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 100Mi}}}
EOF

helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version 1.0.0 -n backflow-system \
  --reuse-values --set migration.enabled=true --set migration.backup.existingClaim=backflow-migration-backup \
  --set 'scm.allowedHosts={gitlab.example.com}'            # step 2: your values
```

Do **not** apply the CRDs of the new version first. If the Job fails, the upgrade fails and the operator **stays stopped**: read the Job's
log (`kubectl -n backflow-system logs job/backflow-migrate-pre`), fix what it names (see [Troubleshooting](#troubleshooting)), and run
`helm upgrade` again, or scale the operator back with `manager migrate --restore-operator`. Whatever the Job did is in the backup.

By hand:

```bash
kubectl -n backflow-system scale deployment/<operator> --replicas=0      # or: manager migrate --stop-operator
manager migrate --dry-run                                               # what would change; changes nothing
manager migrate --backup-dir ./backflow-backup                          # the real run
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version 1.0.0 -n backflow-system --reuse-values
```

Run the binary of the **new** version with your own kubeconfig (it needs the rights to update CRDs, so cluster-admin): from the image, or in
the cluster as a Job.

```bash
# from the image, on your machine (the backup goes to ./backflow-backup)
docker run --rm --user "$(id -u)" -v "$HOME/.kube:/kube:ro" -v "$PWD/backflow-backup:/backup" -e KUBECONFIG=/kube/config \
  ghcr.io/sserkanml/backflow:1.0.0 migrate --backup-dir /backup
```

What the run does for objects stored as `v1alpha1`:

1. refuses to run while the operator runs (a Deployment with replicas, or a held leader-election lease); `--stop-operator` scales it down first;
2. reads every `v1alpha1` object and writes the backups; if that fails, nothing is changed;
3. adds `v1` (from the CRD inside the binary) to the installed CRDs as the stored version, with `v1alpha1` still served for reading;
4. converts `ScmConnection` and `BackflowPolicy` in place (uid, labels and status kept where they still apply), and each `DriftProposal` by delete and create, with its status;
5. when no object of a CRD is left, sets `status.storedVersions` to `[v1]` and removes `v1alpha1` from the CRD (`v1beta1` stays, served). A CRD with an
   object it could not convert keeps `v1alpha1` (not served) and the tool says which object and why;
6. creates the CRD `changerecords.internal.backflow.io`.

Exit status 0 means everything is done; 1 means an error or an object that could not be migrated. Run it again after fixing: it does only what is
left. If it is stopped in the middle (a crash, Ctrl-C), run it again with the same `--backup-dir`: a proposal that was deleted and not created
again is created from its backup.

If the API server refuses an object (the `v1` schema is stricter than `v1alpha1`: patterns, bounds), the tool writes the `v1` object it tried to
`<backup-dir>/rejected/<Kind>/` and leaves the original. Correct the file and `kubectl replace -f` it. **Do not edit the original object through
`v1alpha1`**: it would lose its `v1alpha1` fields.

### kustomize

Build and apply the manifests of the new version the same way, in the same order for the same source version:

```bash
# from 0.18: apply, then migrate (the operator runs)
kustomize build config/default | kubectl apply --server-side --force-conflicts -f -
manager migrate
# from 0.17: stop the operator, migrate, then apply
kubectl -n backflow-system scale deployment/backflow-controller-manager --replicas=0
manager migrate --backup-dir ./backflow-backup
kustomize build config/default | kubectl apply --server-side --force-conflicts -f -
```

`config/default` has the chart's defaults: `BACKFLOW_SCM_ALLOWED_HOSTS=gitlab.com,github.com` and no extra Argo CD server; see
[Allowed servers and hosts](../configuration.md#allowed-servers-and-hosts) for how to add yours.

### OLM (OpenShift)

The catalog starts with 1.0.0, so an installation from the Helm chart moves to OLM; see [Install on OpenShift](../install/openshift.md#coming-from-the-helm-chart):
uninstall the Helm release, run `manager migrate` once (as above; from 0.18 it can run any time, with the operator running or not), then subscribe.
Later versions upgrade along the channel and need no migration.

## 4. Verify

```bash
kubectl get crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io \
  -o custom-columns='NAME:.metadata.name,STORED:.status.storedVersions,SERVED:.spec.versions[*].name'
# STORED is [v1] for all three; SERVED is v1 and v1beta1

kubectl get scm,bfp -A                       # connections and policies: READY True once the operator has checked them
kubectl get dp -A                            # proposals: PHASE as before
kubectl -n backflow-system rollout status deployment/<operator>
kubectl -n backflow-system logs deployment/<operator> | head          # no "WARNING: objects may still be stored as backflow.io/v1beta1"
```

The metric `backflow_storage_migration_pending` is 0 (the operator looks again every five minutes, `--storage-check-interval`). A proposal
that had an open merge request still has `.status.git.mergeRequest`; the operator does not rewrite it, it polls the merge request again. A
connection or policy that is not `Ready` names the setting (step 2) in its condition message. Keep the backup directory until you are sure.

## Rolling back

**From 0.18.x:** nothing was changed in the objects, so the operator of 0.18.x works on them: downgrade the chart (`helm rollback`). The CRDs
keep `v1` and `v1beta1`; **do not** apply the CRDs of 0.18 over them (the API server refuses a CRD that drops `v1` while objects are stored in
it, and they are). The operator of 0.18 reads and writes through `v1beta1`, which is served.

**From 0.17 and earlier:** the backup is a `v1alpha1` YAML file for every object.

```bash
kubectl -n backflow-system scale deployment/<operator> --replicas=0
# 1. delete the Backflow CRDs. This deletes every Backflow object: check that the backup is complete first
ls backflow-backup/*/ | head
kubectl delete crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io
# 2. install the CRDs of the release you come from
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <old> | kubectl apply --server-side -f -
# 3. create the objects again, as v1alpha1 (the owner references of the proposals get the new uid of their policy)
manager migrate restore --backup-dir ./backflow-backup
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <old> -n backflow-system --reuse-values
```

`restore` leaves an object that exists as it is, so it can be run again. Proposals that the new operator deleted for their retention are in the
backup too, and come back.

## Troubleshooting

- **"the operator is running"** (from 0.17): scale it down, or use `--stop-operator`. An operator that runs outside the cluster (`make run`) cannot be
  seen; stop it yourself.
- **A hook Job failed**: `kubectl -n backflow-system logs job/backflow-migrate-pre`. The usual cause is an object the `v1` schema refuses (the Job's
  log names it and the file in `rejected/`). With an `emptyDir` backup that file is gone with the pod: use a claim when you come from 0.17.
- **The CRD still has `v1alpha1` after the run** (from 0.17): an object could not be converted; the output names it. Fix it (see above) and run again.
- **`kubectl apply` of the new CRDs fails with `Error from server (Invalid): CustomResourceDefinition ... is invalid`** (from 0.17): you applied them
  before the migration (nothing was changed). Run the migration; it adds what is missing.
- **The operator exits with "still has objects stored as v1alpha1"**: the migration did not finish; run it again.
- **The operator logs "WARNING: objects may still be stored as backflow.io/v1beta1"** (from 0.18): it works; run `manager migrate` (the operator may
  keep running).
