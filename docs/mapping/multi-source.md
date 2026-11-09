# Helm charts from a Helm repository or a registry

For the other sources see [What maps](overview.md).

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

## `spec.mapping.scratchBranches`

```yaml
spec:
  mapping:
    scratchBranches: Auto     # Auto (default) | Always | Never
```

`Auto` allows scratch branches in `MergeRequest` and `DirectCommit` mode and not
in `ReportOnly`, whose mapping never writes to Git. When they are not allowed, a
proposal of such an Application is `Unmapped` with `ScratchBranchesDisabled`, and
the message names this field. `Always` allows them in every mode.

## Time

Every candidate is a render through Argo CD, which took seconds each when this
was measured. The time all renders of one verification may take together is the
operator's `--render-budget` (default `2m`; `renderBudget` in the chart), and the
operator logs at the end of each verification how many renders it made and how
long it took (`Verification finished`, with `renders`, `elapsed`, `scratchBranches`
and `pushes`). Past the budget a proposal is `Unmapped` (`RenderBudgetExceeded`).

## What stays Unmapped

| Reason | When |
|---|---|
| `ScratchBranchesDisabled` | scratch branches are not allowed (see above) |
| `ScratchBranchRejected` | the repository refused the scratch branches (a protected-branch rule, a hook) |
| `ArgoCDUnsupported` | this Argo CD cannot render by revision per source (it needs 2.6 or later; 3.5.3 is what the tests run against) |
| `ValueFileNotInGit` | a value file is inside a chart from a Helm repository or a registry (its own `values-prod.yaml`): it is not in Git, so it cannot be edited; the message names the file and the source |
| `MultipleValueRepositories` | the value files are in two Git repositories (a second ref source, or inside a chart in Git): a proposal is written to one repository |
| `SetByApplication` | the value is set in the Application (`spec.sources[N].helm.values`, `valuesObject`, `parameters`); see [Values set in the Application](application-values.md) |
| `NoVerifiedCandidate` | no value file holds the value |
| `UnsupportedSourceType` | the Application is outside the supported shape |

Detection while Git is ahead is per source: a commit pauses the Application only
when it changes a value file of a ref source (globs expanded at both revisions) or,
for a chart in Git, a file the chart reads. A **new chart version** (in the
Application, or a range that resolved to one) is a change of the Application, not
Git being ahead: an `ApplicationChanged` Event says so, and detection is paused
until Argo CD syncs the Application.
