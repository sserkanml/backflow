# Upgrading to `backflow.io/v1beta1`

This version replaces the API `backflow.io/v1alpha1` with `backflow.io/v1beta1` (`ScmConnection`, `BackflowPolicy`,
`DriftProposal`). `v1alpha1` is no longer in the CRDs. The fields that were renamed or moved are listed, old to new, in
[docs/design/api-v1beta1.md](design/api-v1beta1.md); the tool below does the conversion for you.

**Everyone who has Backflow objects has to migrate them once, in this order.** The operator of this version does not start
while an object is still stored as `v1alpha1`; it says so and names the command.

| | Step | Who |
|---|---|---|
| 1 | [Convert the manifests in Git](#1-convert-the-manifests-in-git-gitops-users) and merge them | those who keep `ScmConnection`/`BackflowPolicy` in Git |
| 2 | [Set the new allow lists](#2-set-the-new-allow-lists) in your chart values | everyone |
| 3 | [Migrate the objects](#3-migrate-the-objects): stop the operator, back up, migrate | everyone |
| 4 | [Upgrade the chart or the manifests](#4-upgrade-the-chart-or-the-manifests) and start the operator | everyone |
| 5 | [Verify](#5-verify) | everyone |

[Rolling back](#rolling-back) needs the backup of step 3.

## What to know first

- **Run `migrate` before you apply the CRDs of this version.** It adds `v1beta1` to the CRDs that are installed, rewrites the
  objects, and removes `v1alpha1` from the CRDs when none is left. The API server refuses a CRD that drops `v1alpha1` while
  objects are stored in it (`Error from server (Invalid)`), so applying the new CRDs first fails (nothing is lost). This is the one upgrade where the
  "apply the CRDs first" advice of the README is the other way round.
- **A write through `v1alpha1` after the CRD stores `v1beta1` loses fields**: the API server prunes it to the `v1beta1`
  schema. That is why the old manifests in Git must be converted (step 1), and why the tool never writes through
  `v1alpha1`.
- A `DriftProposal` cannot be changed in place (its spec is immutable), so the tool deletes each one and creates it again, with
  its status. The new object has a new `creationTimestamp` (the old one is in the annotation `migrate.backflow.io/created`) and
  a new uid. Merge requests on the Git host are not touched.
- **Finished proposals are deleted after 14 days** (`spec.retention.finishedProposals`), counted from `status.finishedAt`, which the
  tool sets from the last change of their conditions. A finished proposal older than 14 days is therefore deleted when the new
  operator starts. `--skip-finished` backs up and deletes the finished ones during the migration instead.
- `ChangeRecord`s (what the actor webhook buffers, for two hours) are not migrated and not backed up (they hold user names). The
  tool deletes them; the new ones are in the CRD `changerecords.internal.backflow.io`, which the tool creates. Delete the old CRD
  afterwards: `kubectl delete crd changerecords.backflow.io`.
- Kubernetes **1.30** or later (OpenShift 4.17).

## 1. Convert the manifests in Git (GitOps users)

If Argo CD (or anything else) applies your `ScmConnection` and `BackflowPolicy` from Git, convert those files and merge the
change. After the migration the old manifests either fail to sync (`v1alpha1` is not served) or, written through `v1alpha1`,
lose fields.

```bash
manager migrate convert-files path/to/gitops/repo          # rewrites the files in place
manager migrate convert-files --check path/to/gitops/repo   # changes nothing; exit 1 if a file would change (for CI)
```

(`manager` is `/manager` in the image: `docker run --rm -v "$PWD:/repo" ghcr.io/sserkanml/backflow:<new> migrate convert-files /repo`.)

It reads `.yaml` and `.yml` files, converts the `ScmConnection` and `BackflowPolicy` documents of `backflow.io/v1alpha1`, and leaves
everything else as it is, byte for byte: other documents, other files, comments outside the converted documents. A converted
document keeps its comments and the order of its keys and is written again with two spaces of indentation. Documents it cannot
convert safely (a Helm template, a YAML anchor, an unknown `provider`) are named and left alone; `--check` fails for them too.
Helm and Kustomize sources are not rewritten: a template that builds a Backflow object needs the same renames by hand.

Merge the change when you are ready for step 3. Until the objects are migrated, the Application that applies them reports a sync
error ("no matches for kind ... in version backflow.io/v1beta1"); nothing is written. If you prefer, switch off its automatic
sync until step 3 is done.

## 2. Set the new allow lists

Three settings of the chart decide what the operator may call and see. They are the installer's, not the tenants', and the defaults
are strict:

- `scm.allowedHosts`: the Git hosts a `ScmConnection` may name. **The default is `gitlab.com` and `github.com`: a self-hosted
  GitLab or GitHub must be listed** (`scm.allowedHosts: [gitlab.example.com]`, or `*.example.com`), or its connection is not
  `Ready` (`HostNotAllowed`) and its proposals are held.
- `argocd.allowedServers`: the Argo CD servers a policy may name in `spec.argoCD.server.url`, besides `argocd.server`. Policies
  that name another server are not `Ready` (`ServerNotAllowed`).
- `argocd.defaultAccess`: which policies (by namespace) see which Argo CD projects through the operator's own connection
  (`argocd.server`). Empty: only the namespace the operator is installed in, every project. A policy elsewhere without its own
  `spec.argoCD.server` is not `Ready` (`ArgoCDDefaultNotAllowed`) and its open proposals are retired. A single-tenant cluster
  keeps the old behaviour with `[{policyNamespaces: ["*"], projects: ["*"]}]`.

Put them in your values before step 4. See [Allowed servers and hosts](../README.md#allowed-servers-and-hosts) and
[Multi-tenancy and access](../README.md#multi-tenancy-and-access).

## 3. Migrate the objects

Pause whatever applies Backflow objects from Git (or merge step 1 first and accept the sync error until now), then take one of
the two ways. Both stop the operator, write a backup of every `v1alpha1` object (one YAML file each, never overwritten) before
anything changes, and run `manager migrate`.

### With the chart: a hook Job

The chart can run the migration as part of `helm upgrade`. A pre-upgrade hook Job (the new image, `manager migrate --stop-operator`)
scales the operator down, backs up and migrates; a post-upgrade hook scales the operator back to what it was. It needs a
`PersistentVolumeClaim` for the backup, because the backup is what a rollback uses:

```bash
kubectl -n backflow-system apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: backflow-migration-backup}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 100Mi}}}
EOF

helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <new> -n backflow-system \
  --reuse-values --set migration.enabled=true --set migration.backup.existingClaim=backflow-migration-backup \
  --set 'scm.allowedHosts={gitlab.example.com}'            # step 2: your values
```

Do **not** apply the CRDs of the new version first. The Job does nothing when no object is stored as `v1alpha1`, so the setting
can stay on, but turn it off afterwards. Its permissions (to rewrite CRDs) exist only while the hooks run. If the Job fails, the
upgrade fails and the operator **stays stopped**: read the Job's log, fix what it names (see [Troubleshooting](#troubleshooting)),
and run `helm upgrade` again, or scale the operator back with `manager migrate --restore-operator`. Whatever the Job did is in the
backup. Skip to [step 5](#5-verify).

### By hand

```bash
kubectl -n backflow-system scale deployment/<operator> --replicas=0      # or: manager migrate --stop-operator
manager migrate --dry-run                                               # what would change; changes nothing
manager migrate --backup-dir ./backflow-backup                          # the real run
```

Run the binary of the **new** version with your own kubeconfig (it needs the rights to update CRDs, so cluster-admin): from the
image, or in the cluster as a Job.

```bash
# from the image, on your machine (the backup goes to ./backflow-backup)
docker run --rm --user "$(id -u)" -v "$HOME/.kube:/kube:ro" -v "$PWD/backflow-backup:/backup" -e KUBECONFIG=/kube/config \
  ghcr.io/sserkanml/backflow:<new> migrate --backup-dir /backup
```

`--dry-run` prints, for every object, every field that moves, is renamed or dropped. It changes nothing and writes no file. The real
run:

1. refuses to run while the operator runs (a Deployment with replicas, or a held leader-election lease); `--stop-operator` scales it
   down first (and `--restore-operator` scales it back);
2. reads every `v1alpha1` object and writes the backups; if that fails, nothing is changed;
3. adds `v1beta1` (from the CRD of this release, inside the binary) to the installed CRDs, as the stored version, with `v1alpha1`
   still served for reading;
4. converts `ScmConnection` and `BackflowPolicy` in place (uid, labels and status kept where they still apply), and each
   `DriftProposal` by delete and create, with its status;
5. when no object of a CRD is left, sets `status.storedVersions` to `[v1beta1]` and removes `v1alpha1` from the CRD. A CRD with an
   object it could not convert keeps `v1alpha1` (not served) and the tool says which object and why;
6. creates the CRD `changerecords.internal.backflow.io`.

Exit status 0 means everything is done; 1 means an error or an object that could not be migrated. Run it again after fixing: it does
only what is left. If it is stopped in the middle (a crash, Ctrl-C), run it again with the same `--backup-dir`: a proposal that was
deleted and not created again is created from its backup.

If the API server refuses an object (the `v1beta1` schema is stricter than `v1alpha1`: patterns, bounds), the tool writes the
`v1beta1` object it tried to `<backup-dir>/rejected/<Kind>/` and leaves the original. Correct the file and `kubectl replace -f` it.
**Do not edit the original object through `v1alpha1`**: it would lose its `v1alpha1` fields.

## 4. Upgrade the chart or the manifests

Apply the CRDs of the new version (a no-op after the migration, and the right habit for every later upgrade), then the chart:

```bash
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <new> | kubectl apply --server-side --force-conflicts -f -
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <new> -n backflow-system --reuse-values \
  --set 'scm.allowedHosts={gitlab.example.com}'
kubectl -n backflow-system scale deployment/<operator> --replicas=1      # if you stopped it by hand and the chart did not
```

With kustomize (`config/default`), build and apply the manifests of the new version the same way:
`kustomize build config/default | kubectl apply --server-side --force-conflicts -f -`. Re-enable the sync of the Application that
applies your Backflow objects, now that its manifests are `v1beta1` (step 1).

## 5. Verify

```bash
kubectl get crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io \
  -o custom-columns='NAME:.metadata.name,STORED:.status.storedVersions,SERVED:.spec.versions[*].name'
# STORED and SERVED are [v1beta1] for all three

kubectl get scm,bfp -A                       # connections and policies: READY True once the operator has checked them
kubectl get dp -A                            # proposals: PHASE as before
kubectl get dp -A -o json | jq '[.items[] | select(.metadata.annotations["migrate.backflow.io/migrated-from"])] | length'
kubectl -n backflow-system rollout status deployment/<operator>
kubectl -n backflow-system logs deployment/<operator> | head
```

The operator is running and `Ready`, and a proposal that had an open merge request still has `.status.git.mergeRequest`. The
operator does not rewrite it; it polls the merge request again. A connection or policy that is not `Ready` names the setting
(step 2) in its condition message.

Keep the backup directory until you are sure.

## Rolling back

The backup is a `v1alpha1` YAML file for every object. To go back to the last release:

```bash
kubectl -n backflow-system scale deployment/<operator> --replicas=0
# 1. delete the v1beta1 CRDs. This deletes every Backflow object: check that the backup is complete first
ls backflow-backup/*/ | head
kubectl delete crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io
# 2. install the CRDs and the chart of the release you come from
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version <old> | kubectl apply --server-side -f -
# 3. create the objects again, as v1alpha1 (the owner references of the proposals get the new uid of their policy)
manager migrate restore --backup-dir ./backflow-backup        # the binary of the OLD or the new version both work
helm upgrade backflow oci://ghcr.io/sserkanml/charts/backflow --version <old> -n backflow-system --reuse-values
```

`restore` leaves an object that exists as it is, so it can be run again. Proposals that the new operator deleted for their
retention are in the backup too, and come back.

## Troubleshooting

- **"the operator is running"**: scale it down, or use `--stop-operator`. An operator that runs outside the cluster
  (`make run`) cannot be seen; stop it yourself.
- **A hook Job failed**: `kubectl -n backflow-system logs job/backflow-migrate-pre`. The usual cause is an object the `v1beta1`
  schema refuses (the Job's log names it and the file in `rejected/`). With a `emptyDir` backup that file is gone with the pod:
  use a claim.
- **The CRD still has `v1alpha1` after the run**: an object could not be converted; the output names it. Fix it (see above) and run
  again.
- **`kubectl apply` of the new CRDs fails with `Error from server (Invalid): CustomResourceDefinition ... is invalid`**: you applied them before the migration (nothing was changed). Run the
  migration (step 3); it adds what is missing.
- **The operator starts and exits with "still has objects stored as v1alpha1"**: the migration did not finish; run it again.
