# API review for v1beta1 and the migration plan

Status: **decided, being implemented in stages** (see [Decisions](#decisions)). It is the review that had to happen
once, before `backflow.io/v1beta1` exists: after that, field names and meanings are a contract.

Scope: `ScmConnection`, `BackflowPolicy`, `DriftProposal`, `ChangeRecord`, all `backflow.io/v1alpha1`
today (operator 0.17.0). Read from `api/v1alpha1/*_types.go` on `main` at `e762456`, and from the
controllers where a claim depends on behaviour. Claims that I could not confirm in the code are marked
**(unverified)** and collected in [section 7](#7-open-decisions-and-what-was-not-verified).

Reference: the Kubernetes
[API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md).

Contents: [Decisions](#decisions) · [1 Findings](#1-the-ten-findings-that-matter-most) ·
[2 Cross-cutting](#2-cross-cutting-findings) · [3 Audit by kind](#3-audit-by-kind) ·
[4 Proposed v1beta1](#4-proposed-v1beta1) · [5 Migration](#5-migration) ·
[6 What v1beta1 promises](#6-what-v1beta1-promises) · [7 Open decisions](#7-open-decisions-and-what-was-not-verified)

## Decisions

Taken by the maintainer after the review; they override the recommendations below where they differ. The
sections after this one keep the original analysis, with its numbering, as the reason for each.

| # | Decision | Detail that follows from it |
|---|---|---|
| 1 | **Take every proposed rename and move now**, one breaking change before 1.0 | tables in [4.1](#41-old-to-new-per-kind) are the target |
| 2 | **Migration: option (b)**, a one-shot `backflow migrate`, no conversion webhook | stage 2; the operator **refuses to start** while `v1alpha1` is in `status.storedVersions` of a Backflow CRD (stage 1) |
| 3 | **`ChangeRecord` moves to `internal.backflow.io/v1alpha1`** | Go package `api/records/v1alpha1`; the old CRD `changerecords.backflow.io` is deleted by the user (and by the tool); records are not migrated |
| 4 | **More than 256 changes**: the proposal is created `Unmapped`, reason `TooManyChanges`, with the first 256 changes and `spec.truncated: true` | `spec.changes` has `maxItems: 256`; `Mapped=False/TooManyChanges`; the proposal controller never maps a truncated proposal |
| 5 | **Finished proposals are deleted after 14 days** (`spec.retention.finishedProposals`, default `336h`); open and `Unmapped` proposals never | finished = `Merged`, `Rejected`, `Reverted`, `Superseded`, `Failed`; the age counts from the new `status.finishedAt`; `0s` keeps them for ever (replaces D4 below) |
| 6 | **Minimum Kubernetes 1.30 (OpenShift 4.17)** | CRD validation ratcheting (beta in 1.30) lets objects that predate a rule stay valid until they change; the CEL used is limited to `size`, `has`, `matches`, `duration` and comparison, all available since 1.29, so the URL library is **not** needed and the Kubernetes version check in the chart (`kubeVersion: >=1.30.0-0`) is the one requirement |
| 7 | **SSRF allowlists, installer-set, checked by the controllers** | `--argocd-allowed-servers` / chart `argocd.allowedServers`, `--scm-allowed-hosts` / chart `scm.allowedHosts`. The operator's own default Argo CD server is always allowed. Not allowed: `Ready=False`, reason `ServerNotAllowed` (policy) or `HostNotAllowed` (connection), **no request is made**. Not in the CRD. Empty list: nothing but the default is allowed; the chart defaults `scm.allowedHosts` to `gitlab.com`, `github.com` |
| 8 | **Controller state leaves the annotations** (superseded, reverted, live-reverted, access revoked, rejection reported) | `status.signals` of the proposal. Three controllers produce these facts (policy, drift, proposal), and the repository rule is that one controller owns a status, so the facts live in one **disjoint sub-object written only by the drift and policy controllers with a merge patch of that object**; the proposal controller reads it and writes every other status field. Users cannot write status (the subresource) |
| 9 | **The drift controller's working set does not depend on the bounded `status.applications`** | the controller resolves the allowed Applications itself (the same resolution the policy controller uses); the status shows `applicationCount` and `applicationsTruncated` |
| 10 | **`assignActor` is a `*bool`** (default `true`) | |
| 11 | **Deleting a policy keeps today's behaviour**: its proposals are deleted by the owner reference; merge requests that are open on the Git host **stay open and are no longer tracked** | documented in the field docs and the upgrade guide (stage 3) |

Additions the decisions imply, all in v1beta1: `spec.truncated` and `status.finishedAt` on `DriftProposal`,
`status.signals`, `status.applicationCount` and `status.applicationsTruncated` on `BackflowPolicy`.

---

## 1. The ten findings that matter most

Ordered by how much they would hurt after v1beta1, not by how many fields they touch.

1. **Controller state lives in annotations of `DriftProposal`.** `backflow.io/superseded-by`,
   `reverted`, `live-reverted`, `access-revoked` and `rejection-reported` are written by the operator
   (`drift_controller.go:48-56` and `:670`). A user with `update` on a proposal can
   edit them, they race with spec/status writes, and `kubectl get -o yaml` shows them outside the documented
   schema. This is state; it belongs in `status` (conditions or fields) so that it is schema, versioned and
   RBAC-split by the status subresource.
2. **Several lists have no bound and can push an object past etcd's ~1.5 MiB limit.**
   `BackflowPolicy.status.applications` (one summary with all sources per matched Application; a policy that
   selects by label on a big Argo CD has hundreds), `DriftProposal.spec.changes` (`MinItems=1`, no max, each
   `desired`/`live` an unbounded string), `status.mapping.edits`, and most `[]string` in the specs. A
   bound is a *contract* and has to be chosen now; adding a `maxItems` later rejects objects that exist.
3. **The first source is stored twice, with two meanings of "revision".** `ApplicationSummary` repeats the first
   source's `repoURL/path/targetRevision/syncedRevision/sourceType` and also lists it in `sources[]`;
   `DriftProposal.spec.source` does the same against `spec.sources[]`. In `SourceRef.revision` means "the
   commit Argo CD last synced", in `ApplicationSource.revision` "the revision Argo CD resolved and compared
   against". One list, always filled, removes the question.
4. **`Actor` mixes identity with evidence and has conditional semantics.** `username/groups/kind` come from the
   actor webhook; `manager/operation/time` from `managedFields`; `fields` is "set only when other managers wrote the
   rest"; `others[]` repeats the shape without the identity; `userAgent` is deprecated and never filled. `kind`
   (`person|controller`) and `operation` (a free string with an enum) are inconsistent with every other enum.
5. **Enums are inconsistent and many "enums" are free strings.** Lower case (`gitlab|github`,
   `person|controller`), upper case (`UPDATE`), CamelCase (everything else). Free strings that are really
   closed sets: `status.excludedReason`, `status.applications[].reason`, `status.mapping.reason`,
   `unmapped[].reason`, `applications[].sourceType` (documented as including `Plugin`; the `DriftProposal` enum
   `SourceType` has no `Plugin`).
6. **Validation is prose and controller code.** "At least one of `names` or `selector`" is a comment; `Add`
   needs an empty `desired` and `Remove` an empty `live` by comment only; URLs, hosts, durations and JSON
   pointers have no format; almost nothing has `maxItems`/`maxLength`. The one CEL rule (`spec` immutable) is
   good and stays.
7. **`assignActor` cannot be set to `false` from Go.** `AssignActor bool json:"assignActor,omitempty"` with
   `+kubebuilder:default=true`: a Go client (the operator's own tests, the migration tool, any controller)
   that sets `false` omits the field and the API server defaults it to `true`. Defaults that are `true` need a
   `*bool` (as `addPatches` and `addOverrides` already are) or an enum.
8. **Naming and grouping grew feature by feature.** `argoCDNamespace` is outside `argoCD`; the switches are
   `editBases`, `addPatches`, `addOverrides`, `fallbackToApplicationOverride`, `allowPartial`,
   `allowCollateralChanges` (five verbs); the block `mergeRequest` also holds `branchPrefix` and `targetBranch`,
   which apply to `DirectCommit` too (`driftproposal_propose.go:211` reads `TargetBranch` before the mode
   switch **(unverified for DirectCommit end to end)**; references are `application`, `resource` (no `Ref`),
   `policyName`, `scmConnection` (a name, no suffix).
9. **Types from other projects and unmarked lists.** `corev1.SecretKeySelector` is used for tokens and CA
   bundles; it carries `optional`, which the operator ignores, and its schema belongs to Kubernetes.
   `metav1.Condition` lists have no `+listType=map`/`listMapKey=type`, so server-side apply replaces the
   whole array. `ObservedGeneration` is in `BackflowPolicy.status` only.
10. **`ChangeRecord` is not an API, and `DriftProposal` has no lifecycle end.** The record holds user names, is
    rewritten on every flush, has second precision (the cause of the same-second scale bug fixed in
    `6feec99`) and is meant for the operator alone, but sits in the public group where `backflow.io/*` RBAC
    grants it to tenants. Separately, **nothing ever deletes a finished `DriftProposal`** (no TTL or history
    limit found in `internal/controller`); they accumulate for the life of the policy, and deleting the policy
    cascades to all of them through the owner reference, open merge requests included.

Not an API shape problem, but found on the way and worth a ticket: `spec.argoCD.url` is a free URL together with
`insecureSkipTLSVerify` and a token from the policy's own namespace, and nothing allow-lists it; a tenant who can
create a `BackflowPolicy` can point the operator's network position at any URL. It is bounded by what the
token proves, but it is a server-side request primitive.

---

## 2. Cross-cutting findings

| # | Topic | Today | Proposal |
|---|---|---|---|
| X1 | Group/version | one group, four kinds at `v1alpha1` | keep `backflow.io`; three kinds to `v1beta1`; `ChangeRecord` leaves the group ([section 3.4](#34-changerecord)) |
| X2 | Conditions | `[]metav1.Condition`, no list markers, types/reasons only in Go constants | add `+listType=map +listMapKey=type`; publish types and reasons per kind as part of the API ([6](#6-what-v1beta1-promises)) |
| X3 | `observedGeneration` | `BackflowPolicy.status` only (per condition everywhere) | in `status` of every kind with a mutable spec (`ScmConnection`, `BackflowPolicy`); not on `DriftProposal` (immutable spec, the generation is always 1) |
| X4 | Enum case | `gitlab`, `person`, `UPDATE` vs CamelCase | CamelCase everywhere: `GitLab`, `Person` |
| X5 | Free strings that are enums | see finding 5 | typed enums; where the set is open (provider-reported MR `state`) say so in the doc and keep a string |
| X6 | Durations | `batchWindow` is `metav1.Duration` (string) | keep; add `format: duration` and a CEL range (0s to 24h) |
| X7 | Secret references | `corev1.SecretKeySelector` | local `SecretKeyReference {name, key}`, both required; the namespace is always the object's own and is not expressible, which *is* the access boundary and stays so |
| X8 | Default-true booleans | `*bool` for two, plain `bool` for `assignActor` | `*bool` for all default-true ones; no new default-true booleans (prefer an enum) |
| X9 | Printer columns | per kind, `Actor`/`Changed by` read `spec.actor.*` | follow the new paths; add `Applications` to the policy; mark detail columns `priority=1` |
| X10 | Short names | `bfp`, `scm`, `dp`, `chr` | keep `bfp`, `scm`; `dp` may collide with other CRDs that use it (a cluster with DataPlane-like CRDs): keep it, add `drift`; add category `backflow` to the three kinds (only `ChangeRecord` has it now) |
| X11 | Bounds | most lists unbounded | every list gets `maxItems`, every string that is stored gets `maxLength` (table in section 4) |
| X12 | Owner reference | `DriftProposal` owned by its policy, `blockOwnerDeletion` set | keep (a proposal without its policy has no meaning) but document the cascade; the migration must never delete a policy |

CEL needs bounded input for its cost estimate: every rule below is paired with the `maxItems`/`maxLength` it
needs. The functions used (`isURL`, `duration`, `url`) need a Kubernetes with the CEL URL library;
check the chart's minimum Kubernetes version before adopting them **(unverified)**.

---

## 3. Audit by kind

Verdicts: **keep**, **rename**, **move**, **remove**, **validate** (same name, stricter schema), **add**.

### 3.1 ScmConnection

| Field | Finding | Verdict |
|---|---|---|
| `spec.provider` | enum `gitlab\|github`, lower case | rename values to `GitLab`, `GitHub` |
| `spec.url` | pattern `^https?://.+`, unbounded | validate: `maxLength: 2048`, CEL `isURL(self) && url(self).getScheme() in ['http','https']` |
| `spec.hosts` | "Defaults to the host of URL" is controller behaviour; unbounded, no format, order irrelevant | validate: `maxItems: 16`, `+listType=set`, hostname pattern; add `status.hosts` with the effective list |
| `spec.tokenSecretRef` | `corev1.SecretKeySelector` (`optional` is ignored) | keep name; type becomes `SecretKeyReference` |
| `spec.caSecretRef` | same | keep name, same type |
| `spec.committer` | `{name, email}`, `format=email` | validate: `name` `minLength: 1` |
| `status.conditions` | no map markers | add markers; types: `Ready` (reasons: `Authenticated`, plus the failure reasons) |
| `status.authenticatedAs` | fine | keep |
| `status.lastCheckTime` | fine | keep |
| `status.observedGeneration` | missing | add |
| printer columns, `scm` | fine | keep |

Namespace rule: the policy controller lists `ScmConnection` in the policy's namespace only
(`backflowpolicy_controller.go:105`), and a connection reads its Secrets from its own namespace. Both are
structural today. Keep, and state them in the field docs.

### 3.2 BackflowPolicy

| Field | Finding | Verdict |
|---|---|---|
| `spec.argoCDNamespace` | the Argo CD connection is `argoCD`, its namespace is a sibling | move to `spec.argoCD.namespace` |
| `spec.argoCD` (`ArgoCDServer`) | optional, absent means "the operator's default connection" | move to `spec.argoCD.server`; absent still means the operator default; `url` validated like `ScmConnection.url` |
| `…tokenSecretRef`, `caSecretRef` | `SecretKeySelector` | `SecretKeyReference` |
| `…insecureSkipTLSVerify` | matches the Go/kubeconfig name | keep |
| `spec.applications` | "at least one of `names`/`selector`" only in a comment; how both combine is not said | validate: CEL `size(self.names) > 0 \|\| has(self.selector)`, `names` `maxItems: 100` `+listType=set`; document the combination (AND) |
| `spec.mode` | CamelCase enum, default `MergeRequest` | keep |
| `spec.include`, `spec.exclude` | `KindSelector{group, kind}`; `"*"` in free strings; Secrets excluded unless included is hidden in a comment; unbounded | validate: `maxItems: 64`, `kind` `minLength: 1`; document the Secret rule on the field |
| `spec.ignoreFields` | list of rules (`group, kind, name, jsonPointers`) | validate: `maxItems: 64`, pointers `maxItems: 32`, pattern `^/`; name stays |
| `spec.batchWindow` | duration, pointer because `0s` is valid and the default is `30s` | validate: `format: duration`, CEL range |
| `spec.helm.mappingMode` | enum | keep |
| `spec.helm.targetValuesFile` | free string | keep, `maxLength: 512` |
| `spec.helm.mappings[]` | `{group, kind, name, fieldPointer, valuesPath}` | validate: `maxItems: 64`, pointer pattern, `valuesPath` `maxLength: 256` |
| `spec.helm.fallbackToApplicationOverride` | verb differs from its siblings | rename `allowApplicationOverride` |
| `spec.helm.allowCollateralChanges` | enum set, good | keep |
| `spec.helm.addOverrides` | `*bool`, default true | rename `allowValueOverrides` (`*bool`, default true) |
| `spec.kustomize.editBases` | bool, default false | rename `allowBaseEdits` |
| `spec.kustomize.addPatches` | `*bool`, default true | rename `allowNewPatches` |
| `spec.mapping.allowPartial` | bool, default false | keep |
| `spec.mapping.scratchBranches` | enum `Auto\|Always\|Never` | keep; document that it only matters for multi-source Helm |
| `spec.mergeRequest` | holds `branchPrefix`, `targetBranch` (also used by `DirectCommit`) and the MR-only `labels`, `reviewers`, `assignActor` | split into `spec.git.{branchPrefix, targetBranch}` and `spec.git.mergeRequest.{labels, reviewers, assignActor}` |
| `…labels`, `reviewers` | unbounded strings | validate: `maxItems: 16`, `+listType=set`, `maxLength: 128` |
| `…assignActor` | plain bool with default true (finding 7) | `*bool` |
| *(none)* | no way to bound the number of finished proposals (finding 10) | add `spec.retention.finishedProposals` (duration, default `336h`; `0s` keeps for ever) |
| `status.observedGeneration` | present | keep |
| `status.conditions` | `Ready`; reasons only in Go (`Resolved`, `NoApplicationsMatched`, `ApplicationConflict`, `MissingScmConnection`, `ApplicationVisibilityUnknown`, `ArgoCDDefaultNotAllowed`, …) | add markers; publish the list |
| `status.matchedApplications` | the names again, already in `applications[].name` | remove |
| `status.applications[]` | unbounded; repeats the first source (finding 3); `sourceType`, `reason` free strings; `scmConnection` is a name | bound `maxItems: 100` plus `status.applicationCount`; `sources[]` always filled and the first-source fields removed; `type` enum; `reason` becomes `untrackedReason` enum; `scmConnection` becomes `scmConnectionName` inside the source |
| `status.openProposals` | count | keep |
| `status.argoCD` | `{source, url}`; the exclusion count and reason are siblings of it | keep `source`, `url`; absorb the exclusions (below) |
| `status.excludedApplications`, `excludedReason` | one count and **one** reason, although both `ProjectNotAllowed` and `NotVisibleToToken` can apply at once; reason is a free string | replace by `status.argoCD.hiddenApplications: [{reason, count}]`, reason an enum; never a name |

### 3.3 DriftProposal

| Field | Finding | Verdict |
|---|---|---|
| `spec` | immutable by `self == oldSelf` (good); `spec` is an observation, not a desired state, which is unusual but deliberate (like an Event) | keep rule and document |
| `spec.policyName` | a name | rename `spec.policyRef {name}` (same namespace) |
| `spec.application` | `ObjectRef {name, namespace}`, no `Ref` suffix | rename `spec.applicationRef` |
| `spec.resource` | `ResourceRef` | rename `spec.resourceRef` |
| `spec.source` | first source, duplicates `sources[0]` (finding 3) | remove; `sources` is always filled |
| `spec.sources[]` | `ApplicationSource`, same name as Argo CD's unrelated type; `role` `Render\|Values\|Reference`; edits go to `Values` | keep list as `spec.sources`, `maxItems: 16`; type renamed `ProposalSource`; for a single source `role: Render`, which is then the edit target; `type` enum gains `Plugin` |
| `spec.changes[]` | `MinItems=1` only; `desired`/`live` JSON strings unbounded; `Add`/`Remove` rules in prose; `path` unchecked | validate: `maxItems: 256`, `maxLength` 16 KiB per value, `path` pattern `^/`, CEL `self.op != 'Add' \|\| !has(self.desired)` and `self.op != 'Remove' \|\| !has(self.live)`. What the controller does above 256 changes (an `Unmapped` proposal with reason `TooManyChanges`) is a decision: [section 7](#7-open-decisions-and-what-was-not-verified) |
| `spec.actor` | see finding 4 | reshape: `actor {kind, username, groups}` and `managers[]` |
| `spec.actor.userAgent` | deprecated, never filled | remove |
| `spec.actor.manager/operation/time/fields/others` | evidence from `managedFields` | move to `spec.managers[]` `{manager, operation, time, fields}`, `maxItems: 5`, most recent first, the first being the latest writer; `operation` typed enum `Update\|Apply`; `time` stays |
| `spec.actor.kind` | `person\|controller` | `Person\|Controller`; unset means unknown |
| `spec.detectedAt` | when the drift was first observed | keep |
| `status.phase` | the conventions deprecate phases; here it is a summary of conditions that is also a printer column and a state machine documented in `CLAUDE.md` | keep, document the machine as part of the API, and never put information in `phase` that is not in a condition |
| `status.conditions` | `Mapped`, `Proposed`, `LiveReverted`, `CleanedUp`; reasons only in Go | add markers; publish types and reasons |
| `status.mapping.strategy` | enum includes `Unmapped`, which is also a phase, a `Mapped=False` condition and `mapping.reason` | remove `Unmapped` from the enum (no strategy when unmapped) |
| `status.mapping.reason` | free string, duplicates the condition reason | remove (the `Mapped` condition says it) |
| `status.mapping.repoURL` | "when several sources" | move to `edits[].repoURL` (required) |
| `status.mapping.edits[]` | unbounded | validate: `maxItems: 64`, `value` `maxLength` 16 KiB |
| `status.mapping.verified`, `diff` | bool and a 32 KiB diff | keep |
| `status.mapping.collateral[]` | resource identity without version, values unbounded | keep; type `ObjectIdentity`; `maxLength` on `current`/`new` |
| `status.mapping.unmapped[]` | `reason` free string (`UnsupportedChange`, `NoVerifiedCandidate`, …) | `reason` becomes an enum |
| `status.mapping.application` (`ApplicationValue`) | `application` is `"namespace/name"` while every other reference is an object; nested `parent` has a 7-value state enum and a second 32 KiB diff | rename `status.mapping.applicationOption`; `application` becomes `applicationRef`; keep the state enum |
| `status.mergeRequest`, `branch`, `commitSHA` | `branch` is in the status *and* in `mergeRequest.branch` | group as `status.git {branch, commitSHA, mergeRequest {url, number, state}}`; drop `mergeRequest.branch` |
| `status.mergeRequest.state` | provider-reported string | keep a string, document it as open |
| `status.supersededBy` | a name, duplicated by an annotation | keep as `status.supersededBy` (a name); drop the annotation |
| `status.pushRejections`, `lastPushRejectedAt` | retry bookkeeping in the public surface | move to `status.git.push {rejections, lastRejectedAt}` |
| `status.message` | summary of the phase; duplicates a condition message | keep as "the message of the condition that caused the current phase" until phases are fully explained by conditions |
| annotations | five controller-owned annotations (finding 1) | replace by status: `status.conditions` (`LiveReverted`, `CleanedUp` exist), plus `status.accessRevoked`-style reasons on `Mapped`/`Proposed`; `rejection-reported` becomes a field of `status.git.push` |
| printer columns | `Actor` reads `spec.actor.username`; `Changed by` reads `spec.actor.manager` | `Actor` → `.spec.actor.username`, `Changed by` → `.spec.managers[0].manager` |

### 3.4 ChangeRecord

Is it public API? **No.** It is written by the operator's webhook, read by the operator, holds user names and
groups, lives two hours, is rewritten on every flush and was shaped by an admission request. Treating it as
public would freeze fields (`resource` as a plural, `subResource`, `operation: UPDATE`) that exist only because
an AdmissionReview has them.

Options considered:

| Option | Cost | Verdict |
|---|---|---|
| Keep in `backflow.io`, promote to `v1beta1` with the rest | freezes an internal shape; keeps `backflow.io/*` RBAC granting user names to tenants | no |
| Keep in `backflow.io/v1alpha1` forever | works, but `kubectl get backflow` and wildcard grants still include it | no |
| **Move to a separate group, `internal.backflow.io/v1alpha1`, never graduated** | a new CRD and an RBAC rule; nothing to convert (records expire in 2 h) | **yes** |
| Replace by an in-memory/other store | a different design (restarts, replicas) | out of scope |

Field changes, since the type is rewritten anyway: `kind` becomes `actorKind` (it collides with the usual meaning of
`kind`) with values `Person|Controller`; `subResource` becomes `subresource`; `operation` is removed (the only
value is `UPDATE`); `firstSeen`/`lastSeen` become `metav1.MicroTime` (the second precision of the current
fields is the cause of `6feec99`); `paths` items get `maxLength: 1024`. The chart's `actor-webhook-role.yaml`
and the operator's `kubebuilder:rbac` markers move to the new group.

---

## 4. Proposed v1beta1

### 4.1 Old to new, per kind

**ScmConnection**

| v1alpha1 | v1beta1 | Change |
|---|---|---|
| `spec.provider: gitlab\|github` | `spec.provider: GitLab\|GitHub` | renamed values |
| `spec.url` | `spec.url` | validated, `maxLength` |
| `spec.hosts` | `spec.hosts` | `maxItems 16`, set, hostname pattern |
| `spec.tokenSecretRef` | `spec.tokenSecretRef` | `SecretKeyReference` |
| `spec.caSecretRef` | `spec.caSecretRef` | `SecretKeyReference` |
| `spec.committer` | `spec.committer` | `name` `minLength 1` |
| `status.conditions` | `status.conditions` | list markers |
| — | `status.observedGeneration` | added |
| — | `status.hosts` | added (effective hosts) |

**BackflowPolicy**

| v1alpha1 | v1beta1 | Change |
|---|---|---|
| `spec.argoCDNamespace` | `spec.argoCD.namespace` | moved |
| `spec.argoCD.{url, tokenSecretRef, caSecretRef, insecureSkipTLSVerify}` | `spec.argoCD.server.{…}` | moved |
| `spec.applications` | `spec.applications` | CEL: one of `names`, `selector` |
| `spec.helm.addOverrides` | `spec.helm.allowValueOverrides` | renamed |
| `spec.helm.fallbackToApplicationOverride` | `spec.helm.allowApplicationOverride` | renamed |
| `spec.kustomize.editBases` | `spec.kustomize.allowBaseEdits` | renamed |
| `spec.kustomize.addPatches` | `spec.kustomize.allowNewPatches` | renamed |
| `spec.mergeRequest.branchPrefix`, `targetBranch` | `spec.git.branchPrefix`, `spec.git.targetBranch` | moved |
| `spec.mergeRequest.{labels, reviewers, assignActor}` | `spec.git.mergeRequest.{…}` | moved; `assignActor` is `*bool` |
| — | `spec.retention.finishedProposals` | added |
| `status.matchedApplications` | — | removed |
| `status.applications[].{repoURL, path, targetRevision, syncedRevision, sourceType}` | `status.applications[].sources[0].{repoURL, path, targetRevision, syncedRevision, type}` | moved (`sources` always filled) |
| `status.applications[].scmConnection`, `sources[].scmConnection` | `status.applications[].sources[].scmConnectionName` | renamed |
| `status.applications[].reason` | `status.applications[].untrackedReason` | renamed, enum |
| — | `status.applicationCount` | added (`applications` is capped at 100) |
| `status.excludedApplications`, `status.excludedReason` | `status.argoCD.hiddenApplications[{reason, count}]` | merged, list of reasons |
| everything else | unchanged | |

**DriftProposal** (spec is immutable: every spec row below is a *recreate* in the migration)

| v1alpha1 | v1beta1 | Change |
|---|---|---|
| `spec.policyName` | `spec.policyRef.name` | renamed |
| `spec.application` | `spec.applicationRef` | renamed |
| `spec.resource` | `spec.resourceRef` | renamed |
| `spec.source` + `spec.sources` | `spec.sources` | merged, always filled |
| `spec.changes[]` | `spec.changes[]` | bounds, CEL on `op` |
| `spec.actor.{username, groups, kind}` | `spec.actor.{username, groups, kind}` | `kind` values `Person\|Controller` |
| `spec.actor.userAgent` | — | removed |
| `spec.actor.{manager, operation, time, fields}` + `spec.actor.others[]` | `spec.managers[]` | merged, the first is the latest writer |
| `status.mapping.strategy: Unmapped` | (no strategy) | enum value removed |
| `status.mapping.reason` | — | removed |
| `status.mapping.repoURL` | `status.mapping.edits[].repoURL` | moved |
| `status.mapping.application` | `status.mapping.applicationOption` | renamed; `application` is an object ref |
| `status.mergeRequest`, `status.branch`, `status.commitSHA` | `status.git.{mergeRequest, branch, commitSHA}` | grouped |
| `status.pushRejections`, `lastPushRejectedAt` | `status.git.push.{rejections, lastRejectedAt}` | grouped |
| five `backflow.io/*` annotations | `status` | moved out of metadata |
| everything else | unchanged | |

**ChangeRecord** → `internal.backflow.io/v1alpha1` (see 3.4). No conversion: the old objects are deleted with the
old CRD.

### 4.2 Examples

#### ScmConnection

```yaml
apiVersion: backflow.io/v1beta1
kind: ScmConnection
metadata:
  name: gitlab
  namespace: team-a
spec:
  provider: GitLab
  url: https://gitlab.com
  hosts:
    - gitlab.com
  tokenSecretRef:
    name: gitlab-token
    key: token
  caSecretRef:
    name: gitlab-ca
    key: ca.crt
  committer:
    name: Backflow
    email: backflow@example.com
status:
  observedGeneration: 2
  hosts:
    - gitlab.com
  authenticatedAs: backflow-bot
  lastCheckTime: "2026-11-02T09:10:11Z"
  conditions:
    - type: Ready
      status: "True"
      reason: Authenticated
      message: The token authenticated as backflow-bot.
      observedGeneration: 2
      lastTransitionTime: "2026-11-02T09:00:00Z"
```

#### BackflowPolicy

```yaml
apiVersion: backflow.io/v1beta1
kind: BackflowPolicy
metadata:
  name: demo
  namespace: team-a
spec:
  argoCD:
    namespace: argocd
    server:                       # absent: the operator's default connection
      url: https://argocd-server.argocd.svc
      tokenSecretRef:
        name: argocd-token
        key: token
      caSecretRef:
        name: argocd-ca
        key: ca.crt
      insecureSkipTLSVerify: false
  applications:
    names:
      - demo-app
    selector:
      matchLabels:
        env: test
  mode: MergeRequest
  include:
    - group: apps
      kind: Deployment
    - group: ""
      kind: ConfigMap
  exclude:
    - group: ""
      kind: ServiceAccount
  ignoreFields:
    - group: apps
      kind: Deployment
      name: demo
      jsonPointers:
        - /spec/replicas
  batchWindow: 30s
  helm:
    mappingMode: Auto
    targetValuesFile: values-prod.yaml
    mappings:
      - group: apps
        kind: Deployment
        fieldPointer: /spec/replicas
        valuesPath: api.replicaCount
    allowCollateralChanges:
      - Labels
    allowValueOverrides: true
    allowApplicationOverride: false
  kustomize:
    allowBaseEdits: false
    allowNewPatches: true
  mapping:
    allowPartial: false
    scratchBranches: Auto
  git:
    branchPrefix: backflow/
    targetBranch: main
    mergeRequest:
      labels:
        - backflow
      reviewers:
        - alice
      assignActor: true
  retention:
    finishedProposals: 336h
status:
  observedGeneration: 3
  openProposals: 1
  applicationCount: 1
  argoCD:
    source: Policy
    url: https://argocd-server.argocd.svc
    hiddenApplications:
      - reason: ProjectNotAllowed
        count: 1
  applications:
    - name: demo-app
      managedResources: 3
      sources:
        - position: 1
          repoURL: https://gitlab.com/sserkanml/backflow-demo.git
          path: apps/demo
          targetRevision: main
          revision: b93fd8cbb32e052554ea40659bf291672f95945c
          syncedRevision: b93fd8cbb32e052554ea40659bf291672f95945c
          type: Directory
          scmConnectionName: gitlab
  conditions:
    - type: Ready
      status: "True"
      reason: Resolved
      message: 1 Application matched.
      observedGeneration: 3
      lastTransitionTime: "2026-11-02T09:00:00Z"
```

#### DriftProposal

```yaml
apiVersion: backflow.io/v1beta1
kind: DriftProposal
metadata:
  name: demo-app-deployment-demo-df95acbd
  namespace: team-a
  labels:
    backflow.io/policy: demo
    backflow.io/application: demo-app
    backflow.io/resource: 1f0c5c2e
  ownerReferences:
    - apiVersion: backflow.io/v1beta1
      kind: BackflowPolicy
      name: demo
      uid: 6b1d2f0e-0000-0000-0000-000000000001
      controller: true
      blockOwnerDeletion: true
spec:
  policyRef:
    name: demo
  applicationRef:
    name: demo-app
    namespace: argocd
  resourceRef:
    group: apps
    version: v1
    kind: Deployment
    namespace: demo
    name: demo
  sources:
    - position: 1
      role: Render
      repoURL: https://gitlab.com/sserkanml/backflow-demo.git
      revision: b93fd8cbb32e052554ea40659bf291672f95945c
      targetRevision: main
      path: apps/demo
      type: Directory
  changes:
    - path: /spec/replicas
      op: Replace
      desired: "1"
      live: "2"
  actor:
    kind: Person
    username: alice
    groups:
      - devs
      - system:authenticated
  managers:
    - manager: kubectl
      operation: Update
      time: "2026-11-02T09:01:58Z"
      fields:
        - /spec/replicas
  detectedAt: "2026-11-02T09:02:03Z"
status:
  phase: Proposed
  message: Merge request !12 is open.
  conditions:
    - type: Mapped
      status: "True"
      reason: Mapped
      message: One edit in apps/demo/deployment.yaml, verified by a render.
      lastTransitionTime: "2026-11-02T09:02:10Z"
    - type: Proposed
      status: "True"
      reason: MergeRequestOpened
      message: Merge request !12 was opened.
      lastTransitionTime: "2026-11-02T09:02:14Z"
  mapping:
    strategy: Direct
    verified: true
    edits:
      - repoURL: https://gitlab.com/sserkanml/backflow-demo.git
        file: apps/demo/deployment.yaml
        location: /spec/replicas
        value: "2"
        changeIndex: 0
    diff: |
      --- a/apps/demo/deployment.yaml
      +++ b/apps/demo/deployment.yaml
      @@ -6,1 +6,1 @@
      -  replicas: 1
      +  replicas: 2
  git:
    branch: backflow/demo-app-deployment-demo-df95acbd
    mergeRequest:
      url: https://gitlab.com/sserkanml/backflow-demo/-/merge_requests/12
      number: 12
      state: opened
```

#### ChangeRecord (internal)

```yaml
apiVersion: internal.backflow.io/v1alpha1
kind: ChangeRecord
metadata:
  name: cr-d0c72f7b8bdb299c
  namespace: backflow-system
  labels:
    backflow.io/object-uid: d791a0ca-00e3-4c2e-9033-fe93e366ebd1
spec:
  uid: d791a0ca-00e3-4c2e-9033-fe93e366ebd1
  group: apps
  resource: deployments
  subresource: scale
  namespace: demo
  name: demo
  resourceVersion: "4821"
  actorKind: Person
  username: alice
  groups:
    - devs
  paths:
    - /spec/replicas
  firstSeen: "2026-11-02T09:01:58.231904Z"
  lastSeen: "2026-11-02T09:01:58.231904Z"
  count: 1
```

---

## 5. Migration

### 5.1 What constrains the choice

These are facts about this project, found in the repository, that decide between the options.

1. **The chart's CRDs are in `crds/` and are not templated or upgraded by Helm** (`README.md:395-405`,
   `NOTES.txt:117`). A conversion webhook needs `spec.conversion.webhook.clientConfig` (service name,
   namespace, `caBundle`) *inside the CRD*. With `crds/` that cannot name the release's Service or carry a
   generated CA; it would have to be injected (cert-manager `inject-ca-from`, OpenShift `inject-cabundle`), which
   exists for two of the five TLS modes (`serviceCA`, `certManager`) and not for `helm` and `secret`
   (`actor-webhook.md`, TLS row), or the CRDs have to move into `templates/`, which breaks the
   "apply the CRDs first" procedure users follow today.
2. **The webhook server is optional today.** It runs only with `actorWebhook.enabled`
   (default off). A conversion webhook would make a TLS endpoint, a Service and a NetworkPolicy from the
   kube-apiserver mandatory for every install.
3. **The operator is a single replica by default** (`replicaCount: 1`). Anything the API server needs from it to read a CR
   is unavailable during every rollout.
4. **`DriftProposal.spec` is immutable by CEL** (`self == oldSelf`). A tool cannot rewrite the spec of an existing
   proposal to new field names: the update is rejected. With a conversion webhook the stored object is
   converted before validation, so a storage migration passes; without one, proposals are
   deleted and created again.
5. **`DriftProposal` is owned by its policy.** Deleting a `BackflowPolicy` deletes its proposals. The migration may
   rewrite a policy; it must not delete one.
6. **The state that matters is small.** `ScmConnection` and `BackflowPolicy` are written by users, usually from
   Git (Argo CD users, in particular); `ChangeRecord` expires in two hours; the only state that cannot be
   rebuilt is `DriftProposal.status` of *open* proposals (merge request URL and number, branch, phase,
   `pushRejections`), because the operator tracks the merge request by it.
7. **`conversion: None` is lossy for renames.** The API server relabels `apiVersion` and prunes fields the
   target schema does not have. A renamed field is silently dropped. So with `None`, the two versions must never be
   served side by side if their schemas differ.
8. **The operator already has a CRD freshness check** (`CRDsOutdated` event, `driftproposal_propose.go:812`),
   so a "refuse to run against the wrong CRD" start-up check has a precedent.

### 5.2 Option (a): conversion webhook, both versions served

The CRDs serve `v1alpha1` and `v1beta1`, storage `v1beta1`, `conversion.strategy: Webhook`; the operator
converts on request; a storage-version migration (or a plain rewrite) moves etcd to `v1beta1`; later
`v1alpha1` is dropped.

*Upgrade procedure for a user*
1. Apply the new CRDs (`helm show crds … | kubectl apply --server-side --force-conflicts`). Requires a CA
   injected or a `caBundle` in the CRD.
2. `helm upgrade` to the operator version that serves the webhook.
3. Wait until the operator is ready. Until then **every read of the three kinds fails**.
4. Optionally run the storage-version migration; edit manifests in Git at leisure; later upgrade to the release that
   drops `v1alpha1`, and remove `v1alpha1` from `status.storedVersions`.

*Cost*
- A mandatory TLS endpoint on every install; the CA injection problem of constraint 1.
- Conversion code for every renamed or reshaped field, in both directions, kept until `v1alpha1` is removed. The
  reshapes (`actor` → `actor` + `managers`, `source` + `sources` → `sources`, annotations → status) are lossy in
  the reverse direction, so round-trip tests are needed.

*Failure modes*
- **Operator down, crash-looping or rolling means the CRs are unreadable** by anyone: `kubectl get bfp`, Argo CD
  comparing these kinds, `helm` hooks. Namespace deletion hangs while it cannot list them. There is no
  `failurePolicy` for conversion.
- **Start-up cycle:** the operator's own informers list `v1beta1`; if any object is still stored as `v1alpha1`,
  that list needs the operator's own webhook, which must be serving before the caches start.
- A wrong or expired CA fails every request until fixed; the actor webhook has `failurePolicy: Ignore` to avoid
  exactly this class of outage, and conversion has no equivalent.
- The API server calls the webhook only when the stored version differs from the requested one, so after the
  storage migration the webhook is rarely hit, which makes a broken endpoint easy to miss until the next
  `v1alpha1` apply from an old manifest.

*What it buys:* old manifests in Git keep working for a while, objects convert on read, no downtime window at
upgrade for users whose Git cannot change atomically with the operator.

### 5.3 Option (b): one-shot migration with a tool (recommended shape)

No webhook. `v1beta1` is the only served version; the data is rewritten once by a tool, with the operator stopped.

**Tool.** `backflow migrate` as a subcommand of the existing binary and image (so it ships with the chart and
needs no new artefact), run with the user's own kubeconfig (they are applying CRDs, so they are cluster admin
already). The operator does **not** get write access to CRDs or to `customresourcedefinitions/status`: a
start-up step inside the operator would need exactly that, cluster-wide, to run once.

| Subcommand | What it does |
|---|---|
| `migrate plan` | reads every object of the three kinds as `v1alpha1`, reports what it would write (counts, the open proposals, anything it cannot convert), writes nothing |
| `migrate backup --dir D` | `v1alpha1` YAML of every object, `ChangeRecord` excluded, plus a manifest with `resourceVersion`s |
| `migrate apply --dir D` | converts and writes, in the order below; idempotent (an object already in `v1beta1` is skipped) |
| `migrate convert -f path…` | rewrites manifests (GitOps repos) from `v1alpha1` to `v1beta1`, as a diff; no cluster access |
| `migrate finish` | removes `v1alpha1` from `status.storedVersions` after checking that no object is stored in it |

**Order of `apply`.**
1. `ScmConnection` and `BackflowPolicy`: read, convert, `PUT` (same name, so the **uid is kept** and the owner
   references of proposals stay valid). Policy `status` is not written: the operator rebuilds it.
2. `DriftProposal`: for each, delete and create again with the same name, labels, **new owner reference with the
   policy's (unchanged) uid**, converted spec; then write `status` through the status subresource (phase,
   mapping, `git.*`, conditions with their times). The original `creationTimestamp` cannot be kept; it is kept as
   the annotation `migrate.backflow.io/created: <RFC3339>`. Terminal proposals follow the same path (they are
   history; `--skip-finished` drops them instead and says how many).
3. `ChangeRecord` is not migrated.

The five controller annotations (finding 1) are converted into the status fields that replace them in the same
step.

**Why delete and create for `DriftProposal`.** Constraint 4: the spec is immutable, and with `None` the old stored
object has the old field names. A `PUT` of a renamed spec is rejected. This is also the only step where a crash
can lose data: between the delete and the create. It is why `backup` is required before `apply`, why `apply` is
idempotent, and why the operator is stopped throughout (it would otherwise see a proposal vanish and, for an
open one, could treat the merge request as orphaned).

**The CRD side.** The new CRD files list `v1beta1` (served, storage) and keep `v1alpha1` with `served: false`,
`storage: false`, so that `status.storedVersions` may still name it until `finish` runs. The old CRD of
`ChangeRecord` is deleted; the new one is `internal.backflow.io`.

**Fail-closed start-up check in the new operator.** At start it reads the CRDs: if `v1beta1` is not the storage
version, or `status.storedVersions` still contains `v1alpha1`, it does not start its controllers, sets
readiness to false, and logs and emits an event naming `backflow migrate`. This removes the silent-loss
case where someone runs `helm upgrade` and forgets the CRDs or the tool.

*Upgrade procedure for a user* (what the README would say)
1. Upgrade to the last `v1alpha1` release (it contains `migrate plan`, so a user can see the effect before any
   change).
2. `backflow migrate plan`; read it. If Argo CD manages the policies, pause the sync of those Applications (or
   disable auto-prune for them) so it does not fight the rewrite.
3. `kubectl -n <ns> scale deploy/backflow --replicas=0`.
4. `backflow migrate backup --dir ./backflow-backup`.
5. Apply the new CRDs (`helm show crds … --version <new> | kubectl apply --server-side --force-conflicts -f -`).
   From here `v1alpha1` is not served; the tool reads the backup, not the API.
6. `backflow migrate apply --dir ./backflow-backup`.
7. `helm upgrade` to the new operator; it starts, passes the check, tracks the open merge requests again.
8. `backflow migrate finish`; then update the manifests in Git (`backflow migrate convert -f manifests/`) and
   re-enable the sync. Until the manifests change, Argo CD sync of the old `apiVersion` fails loudly (no matching
   version), which is the intended behaviour.
9. Delete the old `changerecords.backflow.io` CRD. (It is the only manual deletion.)

*Failure modes*
- A crash in step 6 leaves some objects converted and some not. Re-running `apply` converts the rest. The
  backup and the old objects' `resourceVersion`s in its manifest let the tool refuse to overwrite an object that
  somebody changed after the backup.
- An open merge request whose proposal was recreated is found again by branch name and URL in status; the operator
  does not need anything else. Proposals that were `Mapping` (not yet proposed) are re-mapped by the controller
  like any `Mapping` proposal after a restart.
- Downgrade after step 6 is not possible; the backup restores the old state only together with the old CRDs
  (`kubectl delete crd` first, which deletes all objects, then apply the old CRDs and the backup). It is
  documented, not automated.
- Downtime: the operator is stopped from step 3 to step 7 (minutes). Drifts that appear in this time are
  detected afterwards (the operator reads Argo CD state, it does not rely on events); the actor webhook, being
  in the same pod, is off, so changes made in this window have no `ChangeRecord` and their proposals get no user
  name (the hold added in `db0ff4f` does not help: the record never existed).

### 5.4 Option (c): the simpler options, and why not

**(c1) Rename in place in `v1alpha1`, relabel later.** Add the new names next to the old ones for a release or
two (controller reads the new one first), then publish `v1beta1` with a schema identical to the last
`v1alpha1` and `conversion: None`; the move is then a pure relabel and storage-version migration needs no tool.
This is the Kubernetes-approved way for a schema-identical version bump, and it is the right way for
`ScmConnection` and `BackflowPolicy`. It does not work for `DriftProposal`: the spec is immutable, so existing
objects keep the old names for ever, and the controller would have to read both shapes for as long as any such
object exists (until the retention in finding 10 removes them, which does not exist yet). It also cannot
express a *removal* (the first-source duplication, the annotations) without the same dual reading.

**(c2) Do not migrate `DriftProposal` at all.** Finished proposals are history, and open ones are short-lived (a
merge request is merged or closed within days). Ship `v1beta1` with a new `DriftProposal` schema, delete the old
CRD (which deletes the old proposals), accept losing the tracking of merge requests that were open at the
upgrade. Simple, and honest for 0.x, but it orphans real merge requests on the Git host and loses the history that
`status.phase` and the audit trail represent. Offered as the fallback if the `apply` step of the tool turns out
too costly to maintain; not recommended.

**(c3) Delete and recreate everything from a backup (no in-place rewrite).** The same as (b) but with the CRDs
deleted first. Simpler tool, but `kubectl delete crd` is destructive and also removes any object the tool failed to
export; (b) writes in place and keeps users' objects (and their uids) unless a step specifically needs a
recreate.

### 5.5 Recommendation

**Option (b)**, with the conversion webhook (a) rejected and (c1) used where it is free.

Reasons, in order of weight:

1. **The conversion webhook's failure mode is wrong for this operator.** Unreadable CRs whenever a single-replica
   operator rolls, and no `failurePolicy`, are a worse outage than anything this migration fixes. The project made
   the opposite choice for the actor webhook (`failurePolicy: Ignore`, handler answers in milliseconds) and the
   same reasoning applies.
2. **The chart cannot carry a conversion CA** (constraint 1) without moving the CRDs into `templates/` and
   changing the upgrade procedure, or restricting TLS modes. That is a larger change than the migration.
3. **The data to preserve is small and well-understood** (constraint 6): two user-written kinds, one ephemeral
   kind, and the status of open proposals. A tool that does exactly that is easier to reason about, test and
   reverse than conversion code that has to be correct in both directions for lossy reshapes.
4. **v1beta1 should be the last breaking change.** If the review is done properly, `v1beta1` → `v1` is a relabel
   with identical schema and `conversion: None`, with no tool and no webhook. The cost of (b) is paid once.
5. **Fail closed.** The start-up check turns "forgot a step" into a refusal with an instruction, not silent field
   loss (constraint 7).

What (b) costs: a short window with the operator stopped; manual steps for GitOps users (the manifests must change
in Git, and Argo CD will say so loudly); no rollback after `apply` without the backup; and `migrate apply` must be
tested against real upgrades (kind, 0.17 objects with open proposals and a merge request) in CI before release.

When to revisit: if a significant number of users keep policies in Git that cannot change atomically with the
operator, offer (a) for **`BackflowPolicy` and `ScmConnection` only** (no reshapes of immutable data), as a
separate decision, after the CRD packaging question of constraint 1 is solved.

*Release plan:* `0.18.0` ships `migrate plan/backup` only (on `v1alpha1`), so users can check beforehand;
`0.19.0` is the first `v1beta1` release with the full tool (`feat(api)!:`); `v1alpha1` stays in the CRD,
unserved, for one more minor, then is removed. `v1` follows after at least two minors without schema change.

---

## 6. What v1beta1 promises

Once published:

- No field is removed, renamed or changes meaning. Fields are added as optional with a default that keeps behaviour.
- No enum value is removed or renamed. Values may be added; **consumers must tolerate unknown values** of
  `phase`, `strategy`, every `reason`, `type` and `state`.
- Conditions: the *type* names and the *reason* strings per kind are part of the API and listed in the generated
  reference. A new reason may appear; an existing one keeps its meaning.
- Bounds (`maxItems`, `maxLength`) may be raised, never lowered.
- Defaults do not change. `retention.finishedProposals` defaults to keep-forever so that adding it changes
  nothing; a changed default would be a new field.
- Status is additive. A status field is not removed without the same rule.
- `internal.backflow.io` carries no promise.
- Labels `backflow.io/policy|application|resource|object-uid` are part of the contract (selectors in the wild),
  the annotations are not, and after this change the operator sets none that matter.

---

## 7. Open decisions and what was not verified

**Decisions for the maintainer** (my recommendation first):

| # | Decision | Recommendation |
|---|---|---|
| D1 | How many renames. The table in 4.1 lists them all; the cheap-to-skip ones are `allow*` on `helm`/`kustomize`, `policyRef/applicationRef/resourceRef`, and `spec.git` | take them: the tool recreates proposals anyway, and a rename is free only once |
| D2 | `ChangeRecord` group name (`internal.backflow.io`) | take it |
| D3 | `status.phase` stays | keep |
| D4 | `retention.finishedProposals` default | **decided: 336h** (14 days), see Decisions |
| D5 | Above 256 `changes` | **decided**: `Unmapped`, `TooManyChanges`, first 256 kept, `spec.truncated: true`; nothing is proposed to Git for it |
| D6 | Whether `migrate` also converts GitOps manifests | yes, as a diff; it is the part users feel |

**Not verified** (to check before implementing, none changes the shape above):

- Whether the controller ever creates a `DriftProposal` for an Application whose `sourceType` is `Plugin`. The policy
  status copies Argo CD's value (`backflowpolicy_controller.go:654-662`); the `DriftProposal` enum has no
  `Plugin`. If the proposal can be created, the create fails today.
- That `targetBranch` and `branchPrefix` apply to `DirectCommit` end to end (read before the mode switch in
  `driftproposal_propose.go`, not run).
- The minimum Kubernetes version of the chart against the CEL functions in section 2 (`isURL`, `url`, `duration`).
- That `kubectl get`/server-side apply on a CRD with `v1alpha1` `served: false` but still in
  `status.storedVersions` behaves as described in 5.3 on the Kubernetes versions the chart supports. This is the
  first thing to test in the migration CI.
- That nothing outside this repository reads the removed annotations, `status.matchedApplications`,
  `status.message`-like fields or `spec.actor.userAgent` (no search outside the repository was possible).
- The size of a `BackflowPolicy` status for a policy that matches hundreds of Applications (no measurement; the
  cap of 100 is a judgement, to be set from one).
- Secret values in `spec.changes` are masked by `internal/argocd/mask.go` before they reach a proposal; I read
  that this runs on the manifest response, not that every code path that builds `changes` goes through it.

---

## Implementation notes (stage 1)

Where the implementation made a choice the analysis above left open, or differs from it:

- **`status.signals`** is a sub-object of the proposal status that only the drift and policy controllers write (a merge
  patch of that object), and the proposal controller reads. It holds `supersededBy`, `reverted`, `liveRevertedAtRevision`,
  `accessRevoked` and `rejectionReported`. The informer of the proposal controller reacts to a change of it
  (status changes do not bump the generation).
- **Reasons that are open sets are not enums.** `status.mapping.unmapped[].reason` is a CamelCase string (pattern, `maxLength`),
  because new reasons appear with new mapping rules and an enum would make a status write fail. `untrackedReason` and the
  `hiddenApplications[].reason` (`ProjectNotAllowed`, `NotVisibleToToken`, `VisibilityUnknown`) are closed enums.
- **The allowlists** are package-level settings of the controllers (`SetArgoCDAllowedServers`, `SetScmAllowedHosts`), set once at
  start-up from the flags. They are checked where a request would be made: the resolution of a policy's Applications and
  `argoCDConfig` for a server (before any Secret is read), the verification of a `ScmConnection` and `scmAccessByName` for a host.
- **The drift controller resolves Applications itself** (`appResolver`), the same way the policy controller does, and builds
  the summaries from the live Application. `status.applications` is capped at 100 and is for people. The ScmConnection
  of a repository is found from the `ScmConnection` objects, not from the policy status. Two places still read the capped
  list: the scratch-branch sweeper and the list of other Applications that use a shared Kustomize base in a merge request
  text; for a policy beyond 100 Applications they cover the first 100 (a follow-up).
- **The start-up check** (`internal/crdcheck`) needs `get` on the three CRDs (a rule with `resourceNames` in the ClusterRole).
  It fails when a CRD does not serve and store `v1beta1`, and when `status.storedVersions` has anything else.
- **Retention** is a decision of the proposal controller (`retain`), counted from `status.finishedAt`, which `patchStatus` sets
  when the phase becomes a final one; a proposal that finished before the field existed is counted from its last condition
  change, the first time it is looked at.
- **Kubernetes 1.30** is the chart's `kubeVersion`. The CEL used needs only `size`, `has`, `matches` and `duration`.
