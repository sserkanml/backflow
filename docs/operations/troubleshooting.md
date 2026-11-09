# Troubleshooting

When something does not work, the answer is almost always in a **condition** of one of the three resources, and the
condition has a machine-readable **reason**. This page lists every reason the operator sets, what it means, and what to do.
A test keeps it complete: it fails when the code has a reason that is not in a table below, or a table has a reason that
the code does not set.

## Where to look

```bash
kubectl -n <ns> get scm,bfp,dp                       # one line each: Ready, phase, ...
kubectl -n <ns> describe scm <name>                  # conditions and Events
kubectl -n <ns> get bfp <name> -o jsonpath='{.status.conditions}'
kubectl -n <ns> get dp <name> -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}'
kubectl -n <ns> get dp <name> -o yaml                # spec.changes, status.mapping, status.git, status.conditions
kubectl -n backflow-system logs deploy/backflow-controller-manager
```

| Resource | Condition | Look here when |
|---|---|---|
| `ScmConnection` | `Ready` | a proposal waits with `MissingScmConnection`, `ScmAuthFailed` or `ScmForbidden`, or a policy is not `Ready` for a repository |
| `BackflowPolicy` | `Ready` | nothing is detected for an Application |
| `DriftProposal` | `Mapped` | the proposal is `Unmapped`, or stays in `Mapping` |
| `DriftProposal` | `Proposed` | the proposal is mapped but has no merge request |
| `DriftProposal` | `CleanedUp`, `LiveReverted` | a merge request is still open after the drift ended |

The `message` of a condition has the details (it names the Secret, the namespace, the flag or the chart value), and never a token.
Reasons may be added in a minor release; do not fail on one you do not know.

## ScmConnection: `Ready`

| Reason | Meaning | What to do |
|---|---|---|
| `Authenticated` | `True`. The host accepted the token. `status.authenticatedAs` is the user. | Nothing. |
| `InvalidURL` | `spec.url` is not a URL the operator can use. | Fix `spec.url` (`https://gitlab.example.com`, no path). |
| `UnsupportedProvider` | `spec.provider` is not `GitLab` or `GitHub`. | Fix `spec.provider`. |
| `SecretError` | The token Secret is missing, has no such key, or is empty. | Create the Secret in the namespace of the connection, with the key `tokenSecretRef.key`. |
| `SecretForbidden` | The operator may not read Secrets in that namespace. | Add the namespace to `secretNamespaces` (chart) or `BACKFLOW_SECRET_NAMESPACES`; see [Security](../security.md#permissions). |
| `HostNotAllowed` | The host of `spec.url` is not in `scm.allowedHosts`. No request was made and the Secret was not read. | Add the host to `scm.allowedHosts` (`--scm-allowed-hosts`); see [Configuration](../configuration.md#allowed-servers-and-hosts). |
| `Unauthorized` | The host refused the token (HTTP 401 or 403). | Create a new token with the scopes `api` and `write_repository` (GitLab) or `contents` and `pull_requests` (GitHub), put it in the Secret. A rotated token is noticed within `scmRecheckInterval` (5 minutes). |
| `Unreachable` | The host could not be reached (DNS, network, TLS). | Check the URL, the network policy, a proxy, and the CA: `caSecretRef` or the trusted CA bundle. |
| `UnexpectedResponse` | The host answered something that is not its API (a login page, a proxy error). | Check the URL, in particular the base path of a self-hosted instance. |

## BackflowPolicy: `Ready`

A policy that is not `Ready` detects nothing. Open proposals of a policy that becomes not `Ready` are left as they are.

| Reason | Meaning | What to do |
|---|---|---|
| `Resolved` | `True`. The policy resolves its Applications; the message says how many and which Argo CD connection it uses. | Nothing. |
| `NoApplicationsMatched` | No Argo CD Application in `spec.argoCD.namespace` matches `spec.applications`. | Check the names, the label selector and the namespace (`openshift-gitops` on OpenShift GitOps). |
| `ApplicationConflict` | Another, older policy already manages an Application this policy selects. An Application is managed by one policy only. | Narrow one of the two selectors, or delete one policy. |
| `MissingScmConnection` | In a mode that writes to Git, no `ScmConnection` of the namespace matches the host of a selected Application's repository. | Create the `ScmConnection` for that host, or use `ReportOnly`. |
| `InvalidSpec` | `spec.applications` has neither `names` nor `selector`, or the selector is not valid. | Fix the selector. |
| `ArgoCDNotFound` | The Argo CD `Application` CRD is not installed in the cluster. | Install Argo CD; the policy recovers by itself. |
| `ArgoCDCredentials` | The Argo CD token Secret of the policy is missing, empty or unreadable. | Create the Secret named by `spec.argoCD.server.tokenSecretRef` in the namespace of the policy. |
| `ServerNotAllowed` | `spec.argoCD.server.url` is not allowed by the installer. Nothing is requested from it. | Add the URL to `argocd.allowedServers` (`--argocd-allowed-servers`). |
| `ArgoCDDefaultNotAllowed` | The policy uses the operator's default Argo CD connection, and `argocd.defaultAccess` gives its namespace no project. Its proposals are retired. | Add a rule for the namespace to `argocd.defaultAccess`, or give the policy its own `spec.argoCD.server`; see [Multi-tenancy and access](../security.md#multi-tenancy-and-access). |
| `SecretForbidden` | The operator may not read the Argo CD token Secret (or the CA Secret) of the policy. | As for the `ScmConnection`: `secretNamespaces`. |
| `ApplicationVisibilityUnknown` | With the policy's own token, Argo CD could not be asked which of the selected Applications the token may see, so none is shown yet. It is asked again every 30 seconds. | Check that Argo CD is reachable from the operator. |

## DriftProposal: `Mapped`

`Mapped` is `True` when the change was mapped to Git. `False` has two kinds of reason: **final** ones end the proposal in the phase
`Unmapped` (`status.mapping.reason` has the explanation, and a person decides), and **retried** ones keep the phase `Mapping` and
are tried again with a delay of up to ten minutes, forever. "We could not look" is not "we looked and could not map".

### Mapped

| Reason | Meaning | What to do |
|---|---|---|
| `Mapped` | `True`. The verified edit is in `status.mapping`. | Nothing. |
| `PartiallyMapped` | `True`. Some of the drifted fields are mapped and verified, the others are in `status.mapping.unmapped` (the policy has `allowPartial`). | See [Partial proposals](../mapping/partial-proposals.md). |

### Unmapped: the source cannot be traced to a file (final)

| Reason | Meaning | What to do |
|---|---|---|
| `NotFound` | No YAML or JSON document in the Application's path defines the resource. | Check `directory.recurse`, `include` and `exclude`; the resource may be generated (Jsonnet, a plugin, an ApplicationSet) or defined in another repository. |
| `Ambiguous` | More than one document defines the resource. | Remove the duplicate, or narrow `directory.include`/`exclude`. |
| `VerificationFailed` | An edit was made, but rendering it did not reproduce the live state exactly. | Read the message; a value that a webhook or a controller changes after Git cannot be proposed. Change Git by hand. |
| `CannotApply` | The change cannot be applied to the document (for example an item of a list cannot be added on its own). | Change Git by hand; the diff of the proposal shows what. |
| `UnsupportedSourceType` | The source is not Directory, Helm or Kustomize, is a plugin, a chart that is not in Git, or an Application with sources in a shape that is not supported. | See [What maps](../mapping/overview.md). |
| `UnsupportedFileFormat` | The file that defines the resource is not a format that can be edited (for example Jsonnet). | Change Git by hand. |
| `InvalidSourceOptions` | The Application's directory options (`include`, `exclude`) are not valid globs. | Fix the Application. |
| `RevisionNotFound` | The commit Argo CD synced is not in the repository any more (a force push, a deleted branch). | Sync the Application at an existing commit; the drift is proposed again. |
| `RepositoryURLHasCredentials` | The repository URL of the Application contains a user name or password, which Backflow never uses. | Remove them from the Application and give Backflow access with an `ScmConnection`. |
| `TooManyChanges` | The drift has more than 256 field changes. The first 256 are listed (`spec.truncated`). | Look at what rewrites the resource; ignore fields that are noise (`ignoreFields`). |
| `ApplicationValue` | An Event, not a condition: the diagnosis of a value that the Application sets (`SetByApplication`). | See [Values set in the Application](../mapping/application-values.md). |

### Unmapped: verification by render

| Reason | Meaning | What to do |
|---|---|---|
| `NoVerifiedCandidate` | No place was found whose edit reproduces the drifted field when rendered. | Read the message; the value may come from a template, a default or a function. Change Git by hand. |
| `RenderMismatch` | An edit changes something other than the drifted fields, or not the value that is live. | The message names the difference; usually a value that is derived from another. |
| `NondeterministicField` | The field changes at every render (a generated certificate, a random name), so no edit can be shown to reproduce it. | Leave it to the chart, or ignore the field (`ignoreFields`). |
| `SecretNotVerifiable` | The drifted field is in a Secret. Argo CD masks Secret data, so no render can verify it. | Change Git by hand. Secrets are excluded from policies unless included explicitly. |
| `ValueAffectsOtherFields` | Every key that reproduces the field also changes other fields, which the policy does not allow. | Allow it with `spec.helm.allowCollateralChanges` for labels and annotations, or change the chart. See [Helm](../mapping/helm.md). |
| `AmbiguousCandidates` | More than one edit verifies equally well. | Change Git by hand, or remove the ambiguity (the message names the candidates). |
| `EditsInteract` | The edits for several drifted fields verify one by one, but not together. | Split the change, or change Git by hand. |
| `TooManyCandidates` | There are more candidate edits than Backflow tries. | Change Git by hand. |
| `RenderBudgetExceeded` | All the renders of one verification took longer than `renderBudget` (2 minutes). | Raise `renderBudget` if Argo CD is slow, or reduce the number of candidates. |
| `RenderFailed` | The source does not render with the edit (or at all). | Fix the chart or the overlay; the message has the Argo CD error. |
| `FilesTooLarge` | The files to upload to Argo CD exceed its limits. | Narrow the source directory. |
| `UnsupportedChange` | The kind of change cannot be written as an edit. | Change Git by hand. |
| `SetByApplication` | The value is set by an option of the Application (`helm.parameters`, `kustomize.images`, ...), which Argo CD applies on top of Git. | Change the option in the Application; `status.mapping.application` says where. See [Values set in the Application](../mapping/application-values.md). |

### Unmapped: Helm and Kustomize sources

| Reason | Meaning | What to do |
|---|---|---|
| `MissingValueFile` | A value file does not exist and the Application does not set `ignoreMissingValueFiles`. | Add the file or the setting. |
| `ValueFileOutsideRepository` | A value file is outside the repository. | Fix the path. |
| `SymlinkedValueFile` | A value file is a symbolic link, which is neither followed nor sent. | Use the real file. |
| `UnsupportedValueFile` | A value file is a reference to another source or a URL scheme Argo CD does not allow. | Use a file of the repository. |
| `InvalidInlineValues` | `helm.values` or `helm.valuesObject` of the Application is not valid YAML. | Fix the Application. |
| `SymlinkInChart` | A chart directory contains a symbolic link, so the render would not be the one Argo CD does. | Remove the link. |
| `DependencyOutsideRepository` | A `file://` dependency leaves the repository or does not exist. | Fix the dependency. |
| `ArgoSourceOverrides` | The chart directory has `.argocd-source*.yaml`, which Argo CD merges into the source and Backflow does not model. | Remove the override file, or change Git by hand. |
| `KustomizeSourceNotClosed` | The kustomization reads something that cannot be uploaded (a remote base, `helmCharts`, a symbolic link, a missing file), so a render of edited files would not be Argo CD's render. | The message names the reference. See [Kustomize](../mapping/kustomize.md). |
| `BaseNotEditable` | The value comes from a file outside the Application's path and base edits are off. | Allow them with `spec.kustomize.allowBaseEdits`, if the base is not shared with others you do not want to change. |

### Unmapped: charts from a Helm repository or a registry

| Reason | Meaning | What to do |
|---|---|---|
| `MultipleValueRepositories` | The value files are in more than one Git repository. | Change Git by hand; see [Charts from a Helm repository or a registry](../mapping/multi-source.md). |
| `ValueFileNotInGit` | A value file of the Application is not in the Git repository of its `ref` source. | Fix the path or the `ref`. |
| `ScratchBranchesDisabled` | The policy forbids the scratch branches that verification of this Application needs (`spec.mapping.scratchBranches: Never`). | Allow them (`Auto`), or accept `Unmapped`. |
| `ScratchBranchRejected` | The Git host refused the scratch branches (`backflow/scratch/*`): a protected-branch rule or a push hook. | Allow Backflow to push them, or set `scratchBranches: Never`. |
| `ArgoCDUnsupported` | This Argo CD cannot render a multi-source Application by revision. | Upgrade Argo CD (the message names the version). |

### Retried: Backflow could not look

The phase stays `Mapping` and the `Mapped` condition has one of these reasons until the problem is gone.

| Reason | Meaning | What to do |
|---|---|---|
| `RepositoryUnavailable` | The repository could not be read (network, timeout, the host is down). | Wait, or check the network and the CA. |
| `RepositoryAuthFailed` | The Git host refused the token that reads the repository. | Fix the token of the `ScmConnection`. |
| `RepositoryNotFound` | GitLab and GitHub answer 404 for a private repository the token cannot see: usually "no access". | Create an `ScmConnection` with access, or give the token access. |
| `ApplicationNotFound` | The Argo CD Application of the proposal does not exist (yet). | Nothing if it is being created; else the proposal is stale. |
| `ApplicationUnavailable` | The Application could not be read from the cluster. | Check the operator's permissions and logs. |
| `PolicyNotFound` | The policy of the proposal was deleted. | Recreate it, or delete the proposal. |
| `ArgoCDNotConfigured` | The policy has no Argo CD connection and the operator has no default. | Set `spec.argoCD.server`, or install the operator with `argocd.server`. |
| `ArgoCDUnauthorized` | Argo CD refused the token. | Renew the token; the account needs `applications, get` (and `repositories, get` for charts). A renewed token in the same Secret is picked up within 30 seconds. |
| `ArgoCDApplicationNotFound` | Argo CD does not know the Application. | Check `spec.argoCD.namespace` and the project. |
| `ArgoCDUnreachable` | Argo CD could not be reached. | Check the URL, the CA and the network policy. |
| `ChartDefaultsUnavailable` | The account cannot read the chart's own `values.yaml` through Argo CD. | Add `p, role:backflow, repositories, get, *, allow` to the Argo CD RBAC; the object is the bare repository URL. |

## DriftProposal: `Proposed`

| Reason | Meaning | What to do |
|---|---|---|
| `ReportOnly` | `False`. The policy is in `ReportOnly`: the edit is verified and kept in the proposal, and nothing is written to Git. | Change `mode` to `MergeRequest` when you want merge requests. |
| `Proposing` | The proposal is mapping the change again at the head of the target branch, committing and pushing. This can take minutes. | Wait. |
| `MergeRequestOpened` | `True`. The merge request is open (`status.git`). | Review it. |
| `MergeRequestMerged` | The merge request was merged; the phase is `Merged`. | Nothing. |
| `MergeRequestClosed` | The merge request was closed without merging; the phase is `Rejected`. | Nothing; Argo CD can revert the cluster. |
| `Committed` | `True`. In `DirectCommit` mode the commit was made. | Nothing. |
| `MissingScmConnection` | No `ScmConnection` of the namespace matches the host of the repository. The proposal is held and tried again. | Create one. |
| `ScmAuthFailed` | The Git host refused the token. Held and retried. | Fix the token. |
| `ScmForbidden` | The token may not push or open a merge request. Held and retried. | Give the token the scopes and the role (Developer or write access). |
| `ScmProjectNotFound` | The Git host does not know the project, or the token cannot see it. Held and retried. | Check the repository URL and the token's access. |
| `ScmUnavailable` | The Git host is down, rate limited or answers unexpectedly. Held and retried with the host's advice. | Wait. |
| `MergeRequestRejected` | The Git host refused the merge request as invalid. The proposal fails. | Read the message (a missing label, a reviewer that does not exist). |
| `UnsupportedProvider` | The provider of the connection is not supported. The proposal fails. | Use `GitLab` or `GitHub`. |
| `InvalidRepositoryURL` | The repository URL of the Application is not usable. The proposal fails. | Fix the Application. |
| `TargetNotABranch` | The target branch (the Application's `targetRevision`, or `HEAD`) is not a branch of the repository: a tag, a SHA, or a branch that does not exist. The proposal fails. | Set `spec.git.targetBranch` on the policy. |
| `BranchConflict` | The proposal branch already exists and is not this proposal's work, or it is the target branch. It was left untouched. The proposal fails. | Delete the branch, or change `spec.git.mergeRequest.branchPrefix`. |
| `PushRejected` | The Git server declined the push three times (a protected branch, a hook, or an outage). The proposal fails. | Allow the token to push `backflow/*` branches; see the alert [BackflowPushRejected](metrics.md#backflowpushrejected). |
| `NonFastForward` | In `DirectCommit` mode the target branch moved again while pushing, even after a retry. The proposal fails. | Retry later; busy branches suit `MergeRequest`. |
| `HistoryTooLong` | There are more than 500 commits on the target branch since the revision of the proposal, so Backflow cannot tell whether it was already committed (`DirectCommit`). The proposal is held. | Look at the branch: if the change is there, delete the proposal. |
| `SourceChanged` | A change in Git since the proposal was made makes it obsolete: the proposal is superseded (the message says why). | Nothing; the drift is proposed again if it remains. |
| `LiveReverted` | The live change went back to what Git says. | Nothing. |
| `SupersededByNewerDrift` | A newer drift of the same resource replaced this proposal. | Nothing. |
| `AccessRevoked` | The policy can no longer see the Application (a rule removed, a project changed, a token that lost the permission). The proposal ends `Superseded`. | Restore the access. |

## DriftProposal: `LiveReverted` and `CleanedUp`

`LiveReverted` is a condition of a proposal whose merge request is still open: Argo CD reset the cluster to Git, but the change is not lost,
it waits in the merge request.

| Reason | Meaning | What to do |
|---|---|---|
| `SyncedNewRevision` | `True`. An Argo CD sync reset the cluster to Git while the merge request is open; the change waits in the merge request. | Merge it if the change should stay, or close it. |
| `LiveChangeReturned` | `False`. The change is live again; the merge request proposes it. | Nothing. |


After a proposal is `Superseded` or `Reverted`, its merge request is closed and its branch deleted.

| Reason | Meaning | What to do |
|---|---|---|
| `Closed` | `True`. The merge request was closed (with a comment that says why) and the branch deleted. | Nothing. |
| `AlreadyClosed` | The merge request was already closed. | Nothing. |
| `AlreadyMerged` | The merge request was merged before it could be closed. | Nothing; Git has the change. |
| `Commented` | The comment was added; the merge request is being closed. | Wait. |
| `BranchKept` | The branch was kept because it holds commits that are not Backflow's. | Delete it yourself if you do not need it. |
| `CleanupPending` | Closing the merge request or deleting the branch failed; it is tried again. | Check the Git host and the token. |

## Status values that are not conditions

| Value | Where | Meaning |
|---|---|---|
| `ProjectNotAllowed` | `status.argoCD.hiddenApplications[].reason` | The Application's Argo CD project is not allowed for the policy's namespace by `argocd.defaultAccess`. |
| `NotVisibleToToken` | `status.argoCD.hiddenApplications[].reason` | The policy's own token may not get the Application, or it does not exist (Argo CD answers 403 for both). |
| `VisibilityUnknown` | `status.argoCD.hiddenApplications[].reason` | Argo CD could not be asked; the Application is not shown until it answers. |
| `CredentialsInRepoURL` | `status.applications[].reason` | A repository URL of the Application contains credentials; the Application is not tracked. |
| `Located` | `status.mapping.application.parent.state` | The parent Application, the file and the location are found and a render with the edit changes exactly that option. |
| `NotManagedFromGit` | `parent.state` | No Application manages the child. |
| `GeneratedByApplicationSet` | `parent.state` | An ApplicationSet generated it; change its template or generator. |
| `OutOfSync` | `parent.state` | The parent is not synced, so what Git says is not what runs. |
| `Ambiguous` | `parent.state` | More than one Application manages it. |
| `NotVerifiable` | `parent.state` | The parent has several sources, no Git source, another source type, or no verified edit shows the option. |
| `NotVisible` | `parent.state` | The parent Application is not visible to this policy. |

## It does nothing at all

1. `kubectl -n backflow-system get pods`, and the log of the manager: it must be running and ready.
2. `kubectl get crd | grep backflow.io`: the CRDs of the **same version** as the operator (Helm never upgrades them; see
   [Upgrade and uninstall](upgrade.md)).
3. A `BackflowPolicy` that is `Ready` and says `Watching N application(s)`.
4. The Application is `OutOfSync` in Argo CD, and the batch window (30 seconds) and the Git-ahead pause
   (see [Concepts](../concepts.md#git-ahead)) have passed.
5. The kind is not excluded: Secrets are never captured unless included, and `include`/`exclude` apply.
6. Argo CD's automated sync may have reset the drift before Backflow saw it: see [Limitations](../reference/limitations.md).
