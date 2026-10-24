# Multi-source mapping (roadmap B5)

Status: design, from a spike. No production code yet.

Goal: map drift of a multi-source Application whose Helm chart comes from a
Helm repository (HTTP or OCI) or from another Git repository, with its value
files in a Git "ref" source (`$values/...`), to a merge request on those value
files, verified by render through Argo CD as for single-source Helm.

Today such an Application is skipped: `trackedResources` returns "several
sources" and says so once per revision (`UnsupportedSource` Event), the policy
status records only the first source, and Helm mapping refuses `spec.sources`
(`UnsupportedSourceType`).

## Spike

Argo CD v3.5.3 on the kind cluster (`./hack/dev-up.sh`), throwaway
Applications in `argocd` and a throwaway namespace `spike`. Public charts:
`podinfo` 6.7.1 from `https://stefanprodan.github.io/podinfo` (HTTP) and
`ghcr.io/stefanprodan/charts` (OCI, repository URL without `oci://`). The
values repository and a Git chart repository were served by a `git daemon` pod
in the cluster, because pushing scratch branches to gitlab.com needs the
GitLab token, which the spike must not use. Everything was deleted afterwards
(Applications, namespace, the RBAC edit below).

The three Applications had `sources: [chart or path with
helm.valueFiles: [$values/env/values.yaml], {repoURL: <values repo>, ref: values,
targetRevision: main}]`. All three compared and synced.

### S1. `GetManifestsWithFiles` against a multi-source Application

Argo CD accepts the call, but it renders **the first source only** and treats
the upload as that source's checkout:

| Upload | Result |
|---|---|
| only `env/values.yaml` (chart from a Helm repository) | `error reading helm chart from /tmp/<id>/Chart.yaml: no such file or directory`: the upload root is the chart directory, and Argo CD does not pull the chart |
| the chart files at the root, no values file | `helm template . ... --values /tmp/<id>/env/values.yaml` fails: `no such file or directory`; `$values/` resolves to the **upload root** |
| the chart files at the root plus `env/values.yaml` | renders; the Deployment has the uploaded `replicaCount` (7). Same for the OCI chart |
| Git chart: the chart under `charts/podinfo/` plus `env/values.yaml` | renders; `$values` again resolves to the upload root |

So the upload works only if Backflow supplies **the chart itself**, laid out
as Argo CD expects, and both repositories share one path namespace: a values
repository with a file at a path the chart also has (`values.yaml`,
`templates/`, `Chart.yaml` at the root of a Helm-repository chart) cannot be
represented. Argo CD offers no API that returns a chart's files (see S3), so
Backflow would have to implement the Helm index, tarball and OCI registry
protocols itself, against "reuse Argo CD's work".

### S2. Unary `GetManifests` with `revisions` and `sourcePositions`

`GET /api/v1/applications/{name}/manifests?appNamespace=argocd&revisions=<chart
version or SHA>&revisions=<values SHA>&sourcePositions=1&sourcePositions=2`
renders any commit by SHA, including one that exists only on a pushed
non-default branch, and for a chart from HTTP, OCI or Git alike:

- a commit on a pushed branch `bf-1` renders with its values (replicas 9, 11,
  12, ...); the pull is by SHA, tags are not needed;
- a commit under a **hidden ref** (`refs/backflow/x`) also renders: Argo CD
  fetches the SHA when it does not have it;
- after the branch is deleted, a commit Argo CD already fetched still renders
  (it stays in the repo-server's clone), a commit it never fetched fails with
  `Failed to checkout revision <sha>: git fetch origin <sha>...`. Deleting the
  scratch branch right after the renders is therefore safe, and the failure
  mode of a missing commit is explicit.

Latency, measured per call through the API server:

- repo-server work (`GenerateManifest` in its log): 0.1 to 0.3 s per render;
- first calls, HTTP chart: 2.3 s cold, 0.3 to 0.4 s with a fresh values SHA;
- later calls on the same cluster, for every chart type and for
  `GetManifestsWithFiles`: 10.0 to 10.9 s, a constant spent in the API server
  (the repo-server log of the same calls shows 0.1 to 0.3 s). The kind
  cluster's DNS was flaky (`lookup registry-1.docker.io: i/o timeout` while
  pulling), so this is probably a lookup timeout and not Argo CD's cost, but it
  is **unexplained**. See the open questions; budgets must not assume 0.3 s.
- an identical request is answered from Argo CD's manifest cache (0.04 s).

GitLab behaviour could not be tested (no token). From GitLab's documentation,
to be confirmed on the demo project before relying on it: the push option
`ci.skip` (and `[ci skip]` / `[skip ci]` in the commit message) suppresses the
branch pipeline; webhooks for push events still fire, so a project with a push
webhook (Argo CD's own refresh webhook, say) is notified of the scratch branch;
GitLab accepts pushes only under `refs/heads/` and `refs/tags/`, so a hidden ref
is not an option there. go-git can send push options and several refs in one
push.

### S3. Chart defaults without Git

`POST /api/v1/repositories/{url-encoded repo}/appdetails` with the chart source
returns, for HTTP and for OCI (no registry credentials needed for a public
chart):

- `helm.values`: the **text** of the chart's root `values.yaml` with its comments
  (4300 bytes for podinfo; 48 KiB for `prometheus`);
- `helm.valueFiles`: the chart's value files, including `charts/<dep>/values.yaml`
  paths of subcharts;
- `helm.parameters`: the flattened scalars (not needed).

Subchart **defaults are not returned**, only the file names. This is the same
shape as today's "dependency defaults are not in the repository"
(`UnreadDependencies`), and is handled the same way.

Authorization is stricter than the docs suggest. Argo CD authorises the call
in one of two ways, and the error message names the check that failed:

- with `appProject` only: `applications, create, <project>/`: too much for
  Backflow;
- with `appName`, `appNamespace` and `appProject`: the source in the request
  must **equal** one of the Application's sources, `helm.*` settings included
  (a source without `helm.valueFiles` was refused as `permission denied`, even
  for the admin), and the account needs `applications, get` (Backflow has it)
  and **`repositories, get, *`** (the object is the bare repository URL, not
  `<project>/<url>`; `default/*` is not enough, the exact URL is).

So Backflow's Argo CD account needs one more policy line:
`p, role:backflow, repositories, get, *, allow` (or one per chart repository).
`hack/dev-up.sh` and the chart's docs must add it, and a missing permission must
surface as a condition on the policy and as a retried (not final) reason.

### S4. Chart in Git, `$values` in another Git repository

Same as S1 and S2 with `path: charts/podinfo`: the upload works with the chart
under `charts/podinfo/` and `env/values.yaml` at the upload root; the unary call
by SHA works with `revisions=[<chart repo SHA>, <values repo SHA>]`. The two
repositories again share the upload's single path namespace, but here the chart
is under its own path, so a collision needs the values repository to have files
under that path.

### S5. Status fields per source

From a synced multi-source Application:

- `status.sync.revisions`: positional, one per source: the chart **version**
  (`6.7.1`, also for OCI) or the resolved commit SHA. `status.sync.revision` is
  **empty**;
- `status.operationState.syncResult.revisions` and `.sources`: the same, per
  source, after a sync; `syncResult.revision` is empty; `history[].revisions` /
  `.sources` likewise;
- `status.sourceTypes`: positional: `["Helm", ""]`. A `ref` source has the
  empty type, so the type of a source is not "Directory" there;
- `status.sync.comparedTo.sources` repeats the sources, with `ref`, `chart`,
  `path`, `helm.valueFiles`;
- the first drift sync of a synced Application with a changed chart version
  shows as a different `revisions[i]`, which is how a chart upgrade appears.

`gitAhead` already reads `revisions` lists (it joins them) and `sourceChange`
already compares `spec.sources` with `syncResult.sources`; per source
comparison is what is missing.

## Render path

Options:

- **A. Upload** (`GetManifestsWithFiles`) with the chart files Backflow
  supplies.
- **B. Scratch commits**: commit the edit on a temporary branch of the values
  repository and render by SHA through unary `GetManifests` with `revisions`.
- **C. Unsupported.**

**Chosen: B**, with C where B is not possible.

Why not A: it needs Backflow to download and unpack charts from HTTP and OCI
registries (index parsing, credentials, digest resolution), which is exactly the
work Argo CD already does and Backflow must not copy; the render could then
differ from Argo CD's (chart version resolution, `helm.version`, plugins), the
single path namespace can collide (S1), and only the first source is rendered.
B renders what Argo CD would render, with Argo CD's own chart handling and
caches, and `$values` resolves per repository.

What B costs:

- **Write access for verification.** Backflow already needs `write_repository`
  to open the merge request on the same repository, so no new permission is
  needed, but the repository now receives scratch refs *before* any human
  decision. "Git always wins" holds (a branch is not a change of the source of
  truth, and it is removed), but it must be said in the README and the policy
  docs, and with `spec.mapping.scratchBranches: Auto` a policy in `ReportOnly` mode
  does not push anything: its multi-source proposals stay `Unmapped` with
  `ScratchBranchesDisabled` (see Decisions).
- **Branch budget and cleanup.** One push per verification round, not per
  render: Backflow builds all the candidate commits locally (go-git, from the
  tree of `spec.source` revision of the values repository), gives each its own
  ref `backflow/scratch/<proposal>/<n>` under one push (with `ci.skip`), renders
  them one by one, and deletes all refs in one push in a `defer`, whatever the
  outcome. Confirmation renders are a second round with the same shape. A
  leftover branch (operator crash) is found by the prefix and removed at the
  start of the next verification of that proposal and by a periodic sweep of
  refs older than an hour that carry the prefix. Limits: at most
  `maxScratchBranches` (default 40) refs per push, the existing render budget
  for the whole verification, and a push that is rejected (protected branch
  pattern, quota) makes the proposal `Unmapped/ScratchBranchRejected` and is
  retried only on a change of the connection.
- **CI.** The push carries `ci.skip`; the commit message carries `[skip ci]`
  as a second line of defence. If the project's pipelines or webhooks still
  run, that is visible and bounded by the branch budget. To verify on the demo
  project in the e2e section; until then it is an assumption.
- **Latency** per render is Argo CD's, with the unexplained 10 s of the spike
  (open question 1).

### Where each case gets its renders

| Case | Source 1 | Source 2 (ref) | Render | Engine input |
|---|---|---|---|---|
| Helm-repository chart (HTTP/OCI) + `$values` in Git | `chart`, `targetRevision` = version | Git `ref` | B: `revisions=[version, scratch SHA]`, `sourcePositions=[1,2]` | `Files` = the values repository tree at the synced SHA (value files and, for `$values/...` paths, resolved to repository paths); defaults from `appdetails` (below) |
| Git chart + `$values` in another Git repository | Git, `path` | Git `ref` | B: `revisions=[chart SHA, scratch SHA]` | `Files` = both trees, each file keyed by repository **and** path; the chart's `values.yaml` is a default |
| Git chart + `$values` in the same repository | Git, `path` | Git `ref` (same URL) | B on that one repository (the chart and values SHAs are the same commit) | as the single-repository case |
| Directory or Kustomize first source | | | unchanged (single-source rules) | |

`rendermap` already has the seams (`Renderer`, `Provider`, `Writer`). The
changes:

- `rendermap.Renderer.Render(ctx, app, files)` takes files keyed by path. A
  multi-source renderer needs the repository too. Add a `rendermap.Tree` key,
  `FileKey{Repo, Path}`, behind a small adapter: the Helm provider keeps
  working with plain paths when `Repo` is empty. The scratch renderer
  (`internal/rendermap/scratch.go`, new) implements `Renderer`: it turns an
  edited file set into commits on the values repository and renders by SHA. The
  engine's render cache keys on the file set, so identical edit sets share
  commits and renders as today.
- `ArgoRenderer` gets a unary `GetManifests` method on `internal/argocd.Client`
  (`ManifestsByRevisions(ctx, app, revisions, sourcePositions)`), REST, bearer
  token, same error mapping (`ErrRenderFailed`, `ErrNotFound`).
- `internal/gitrepo` gets the write side it lacks for scratch refs: build a
  commit from a tree plus edits without a working copy (it creates new files
  already), push several refs with push options, delete refs.
- The baseline render (twice, to find nondeterministic fields) also goes through
  scratch commits? No: the baseline is the **unmodified** state, which is the
  synced commit itself; it renders by SHA with no push at all.

## Chart defaults when the chart is not in Git

Single-source Helm reads defaults from `values.yaml` and from unpacked
subcharts in the repository (`ChartDefault`, `defaultOf`). For a chart from a
Helm repository the `Provider` takes them from `appdetails` (S3):

- `helm.values` is the chart's `values.yaml`, parsed with the same code as
  today (`parseValues`, `Layer` with `IsDefault: true`, `Name` =
  `<chart>@<version> values.yaml`). It gives **A1**, overrides (a key the value
  files do not set is added to the last value file) and **A2**, structured values
  (maps and lists, `treeLayers` and the structured rules) exactly as with a
  chart in Git: the engine only sees layers.
- Subchart defaults are not available: a key under a dependency that the
  parent's value files do not set stays `Unmapped` with the existing
  `UnreadDependencies` explanation, extended with "the chart comes from a Helm
  repository".
- `appdetails` is fetched once per (chart, version, source settings) and cached
  by the version in `status.sync.revisions[i]`, with the same TTL discipline as
  the other caches; a failed call is retried, never final (RBAC or the network).
- Templates are not available either, so the "which keys do any template read"
  proof (`valueRefs`) has nothing to scan: `References` returns "everything"
  (`all = true`), exactly as for a chart without templates today, and the
  confirmation renders (`MaxConfirmations`) carry the load. Ambiguity cannot be
  excluded statically, so more proposals end in `TooManyCandidates`, and that
  is the correct, safe outcome.

For a Git chart the defaults and templates come from the chart's repository
tree as today, read at `revisions[chart position]`.

## API, policy and proposals

### BackflowPolicy status: per source

`ApplicationSummary` keeps the current fields (they describe the **first**
source, for compatibility and for printer columns) and gains:

```go
// Sources lists every source of a multi-source Application, in order.
Sources []SourceSummary `json:"sources,omitempty"`

type SourceSummary struct {
    // Role: Chart (helm chart from a Helm repository), Git (a Git source that
    // renders), Ref (a ref source, referenced as $name/...).
    Role           string `json:"role"`
    Ref            string `json:"ref,omitempty"`   // the name of a ref source
    RepoURL        string `json:"repoURL"`
    Path           string `json:"path,omitempty"`  // chart name for a Helm repository
    TargetRevision string `json:"targetRevision,omitempty"`
    SyncedRevision string `json:"syncedRevision,omitempty"`
    SourceType     string `json:"sourceType,omitempty"`
    ScmConnection  string `json:"scmConnection,omitempty"`
    Reason         string `json:"reason,omitempty"`
}
```

`Sources` is filled from `spec.sources` with `status.sourceTypes` and
`status.sync.revisions` (positional). `ScmConnection` is matched **per
repository by host**, as today, for every source that is a Git repository: a
values repository on another host than the chart repository has its own
connection, and the one that must exist and have write access is the
connection of the repository that is **written to**. A missing connection on a
source that is only read (a public chart repository) is not a problem; one on
the values repository is `Reason: NoScmConnection` as for a single source.

### Which sources Backflow writes to

A proposal edits the **ref** source(s) that hold the value files the Helm source
names in `helm.valueFiles` (`$values/...`). Everything else is read-only. The
write target of an edit is the repository of the ref source that
`$ref/path` resolves to. Several ref sources may feed the value files; an edit
goes to the file that defines the key (as today, value files are layered); if
the same key is defined in files of two different ref repositories, the
proposal is `Unmapped` (`AmbiguousCandidates`-like, reason below), because the
merge request would have to be split and the outcome ordered across two
repositories.

### DriftProposal spec and status

`spec.source` stays the Application's primary source (compatibility, printer
columns, the existing consumers) and gains a list for the others:

```go
type SourceRef struct { ...existing... }

// Sources are the Application's sources when it has several, in order,
// at the revisions Argo CD last synced.
Sources []SourceRef `json:"sources,omitempty"`   // in spec: immutable like the rest
```

with `SourceRef` extended by `Role`, `Ref` (the `$name`) and `Position`.
`status.mapping` records the repository written to:

- `status.mapping.repoURL` and `status.mapping.revision`: the repository and
  the commit the edits apply to (the values repository at its synced SHA);
- `SourceEdit.RepoURL`: each edit names its repository (several repositories
  are possible in principle; the first version writes to exactly one and uses
  the rest only for reading);
- `MergeRequestRef` names the repository as well (`repoURL`), and the merge
  request is opened there with the connection that matches **that** host.

### Git-ahead per source

`gitAhead` compares positions: `status.sync.revisions[i]` against
`syncResult.revisions[i]`. For each source:

- a **ref** source that is a Git repository: `RelevantFiles` is the set of
  value files the Helm source names under `$ref/...` (globs expanded at that
  revision, like single-source value files) and any file parameters read from
  it; a commit that touches none of them does not pause;
- a **Git chart**: `helmmap.RelevantFiles` for the chart directory, as today;
- a **Helm-repository chart**: there are no files to compare; a different
  chart version (`revisions[i]` differs from `syncResult.revisions[i]`) **is an
  Application change** and pauses detection until a sync, exactly like a
  changed `spec.source` today (`sourceChange`);
- the Application is paused when any source says so; the Event names the source
  (`ref values: 2 file(s) changed`).

The aheadKey/cache fingerprint gains the per-source sets. `lookAtHelm` becomes
a loop over the Git sources with `countRelevant`, which already takes a
per-revision callback.

## What stays Unmapped, and the reason

| Situation | Reason (machine-readable) | Note |
|---|---|---|
| single-source Helm-repository chart with inline `helm.values` / `valuesObject` | `UnsupportedSourceType` (as today) | roadmap B6 |
| the drifted value comes from `helm.values`, `valuesObject`, `parameters` or `fileParameters` of the Application | `NoVerifiedCandidate` with the existing "set by ... in the Application" text | B6; already reported for single-source |
| several ref sources define the same key | `AmbiguousCandidates` | message names both repositories |
| the value file is in a repository Backflow cannot write to (no connection, read-only token) | `RepositoryAuthFailed` / `NoScmConnection`, **retried** | like single-source |
| scratch branches are off (`spec.mapping.scratchBranches: Never`, or `Auto` with `ReportOnly`) | `ScratchBranchesDisabled` | mapping "never writes to Git"; verification of a multi-source proposal does, so it needs the switch; the message names the field |
| `revisions`/`sourcePositions` not supported by the Argo CD | `ArgoCDUnsupported` | minimum Argo CD 2.6 |
| the push of the scratch branches is rejected | `ScratchBranchRejected` | final until the connection changes |
| `$values` resolves to a path that does not exist at the synced revision | `MissingValueFile` | as today |
| plugin source (CMP) among the sources, or a Kustomize/Directory first source | `UnsupportedSourceType` | |
| the chart repository needs credentials the Argo CD account cannot use for `appdetails` | `ChartDefaultsUnavailable`, **retried**; the candidates in the value files are still tried, only overrides need defaults | |
| `appdetails` RBAC missing (`repositories, get`) | condition on the policy, retried | S3 |
| more than 40 candidates (scratch branch bound) | `TooManyCandidates` | existing reason |

## Test plan

- **Unit**
  - `argocd`: `ManifestsByRevisions` with `httptest` (revisions, positions,
    errors); `appdetails` client (source equality, the three denial shapes).
  - `gitrepo`: scratch commit from a tree plus edits, multi-ref push with push
    options, ref deletion, against local repositories.
  - `rendermap`: the scratch renderer with a fake `Git` and a fake
    `ManifestsByRevisions`: identical edit sets share a commit, cleanup on
    success, error and context cancel, the branch bound.
  - `helmmap`: a Provider with defaults from appdetails text (overrides and
    structured), `UnreadDependencies` text for a Helm-repository chart,
    `$values` path resolution (ref name, nested `$values/a/b.yaml`).
  - controller: `ApplicationSummary.Sources` from a multi-source fixture,
    per-source Git-ahead (ref source touched, not touched, chart version
    changed), ScmConnection per host, proposal spec/status naming the
    repository.
- **Coverage fixtures** (`test/coverage`): a multi-source variant of the
  existing chart set, rendered with `helm template` against a local directory
  chart with a `$values` file, to keep golden outcomes for the single-source
  charts unchanged and add new rows (`values-ref` flavour of the same
  scenarios). The default renderer there is `helm template`, so the scratch
  path is replaced by a renderer that writes the files to a temp directory;
  the Git push is exercised in the unit and e2e tests.
- **E2E** (`hack/test-multi-source.sh`, own namespace, `SECTIONS` letters,
  bash 3.2, needs the GitLab token like the other scripts): the public chart
  `podinfo` 6.7.1 from `https://stefanprodan.github.io/podinfo` (HTTP) and from
  `ghcr.io/stefanprodan/charts` (OCI), a `$values` file in the demo repository
  (`apps/podinfo/values.yaml`, committed by the script, restored on exit):
  a. HTTP chart: scale the Deployment, merge request on `apps/podinfo/values.yaml`,
     scratch branches gone afterwards, no pipeline created;
  b. OCI chart: the same;
  c. an override of a chart default (a key only the chart's `values.yaml`
     defines, `ui.color`, from `appdetails`) is added to the value file;
  d. Git-ahead: a commit to the values file pauses, a commit to another file
     does not, and a chart version bump in the Application pauses;
  e. `ReportOnly` stays `Unmapped/ScratchBranchesDisabled`;
  f. a rejected scratch push (a protected branch rule on `backflow/scratch/*`)
     leaves the proposal `Unmapped` and no branches behind.
  Argo CD needs the one extra RBAC line from S3; `hack/dev-up.sh` adds it.

## Decisions

These settle the open questions of the spike.

1. **Render budget.** Configurable: the manager flag `--render-budget`
   (default 2m, today's `DefaultBudget`) and, only if cheap, no policy field.
   Every `Verify` logs, at V(0) and on completion, the number of renders and the
   wall time (`Verification finished`, `renders`, `elapsed`, `mapped`), so the
   unexplained 10 s of the spike shows up in real runs.
2. **Scratch pushes skip CI.** GitLab: the push option `ci.skip` and `[skip ci]`
   in the commit message. GitHub: `[skip ci]` in the commit message (there is
   no push option). Whether GitLab then really runs no pipeline is checked by the
   e2e script (section e), not assumed.
3. **Branches only**, named `backflow/scratch/<proposal-uid8>-<n>`. The parent
   of every scratch commit is the synced commit of the ref source
   (`status.sync.revisions[i]`). One push creates all candidate branches, one
   push deletes them (in a `defer`). A sweeper removes `backflow/scratch/`
   branches older than one hour, that prefix only, never another branch.
4. **Two repositories**: a proposal that would write to two repositories is
   `Unmapped` (or partial, when `allowPartial` and the other repository's fields
   can be left out).
5. **`spec.mapping.scratchBranches`**: `Auto` (default: off for `ReportOnly`,
   on for `MergeRequest` and `DirectCommit`), `Always`, `Never`. When it is off,
   the proposal is `Unmapped` with reason `ScratchBranchesDisabled` and a message
   that names the field.
6. **`appdetails`** requests are built from `spec.sources[i]` unmodified (S3).
7. **Pinned revisions.** Base and candidate renders pin every source to the
   resolved revision in `status.sync.revisions` (the chart version, the Git SHA),
   never to a range or a branch name.
8. **Argo CD version.** `revisions` and `sourcePositions` need Argo CD 2.6 or
   later; the behaviour was observed on 3.5.3 and that is the version the
   end-to-end tests run against. An "unknown field" or unsupported error from
   those parameters makes the proposal `Unmapped` with reason
   `ArgoCDUnsupported`.

## Still open

- **The 10 s in the API server** (S2): unexplained; the per-`Verify` log line of
  decision 1 and the budget flag exist to live with it. Re-measure on another
  cluster.
- **GitLab behaviour of scratch branches**: `ci.skip`, push webhooks, protected
  branch rules for `backflow/*`, push rate. Checked by the e2e script on the demo
  project, which needs the GitLab token.
- **Chart version ranges** (`targetRevision: 18.*`): renders pin the resolved
  version of `status.sync.revisions` (decision 7); unary `GetManifests` with
  `revisions` was only tried with an exact version, which is what pinning gives.
