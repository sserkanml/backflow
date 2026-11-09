# Upgrade and uninstall

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

## Upgrading to 1.0 (`backflow.io/v1`)

The API is `backflow.io/v1` from 1.0 (`ScmConnection`, `BackflowPolicy`, `DriftProposal`). Its schema is the one of `v1beta1`, which 0.18 shipped,
without a change; `v1beta1` stays served, deprecated, until 2.0 ([compatibility policy](../compatibility.md)). `v1alpha1`, the API of 0.17 and
earlier, is gone. What the fields of `v1beta1` are, old to new from `v1alpha1`, is in
[docs/design/api-v1beta1.md](../design/api-v1beta1.md). Things you will notice when you come from 0.17:

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
(`manager migrate convert-files`), how to check the result and how to roll back is in **[Upgrading to 1.0](upgrade-1.0.md)**.

Never edit a Backflow object through `v1alpha1` after the CRD stores `v1`: the write is pruned to the `v1` schema and loses fields. On a cluster
with no Backflow objects, delete the CRDs (and `changerecords.backflow.io`) and apply the new ones.

## Upgrading: apply the CRDs first

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
  [Partial proposals](../mapping/partial-proposals.md)). With the old CRDs the policy field is dropped, so the option stays
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
