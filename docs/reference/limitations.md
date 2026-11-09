# Limitations

- **One Helm source, and Git ref sources, for several sources.** An Application
  with `spec.sources` in any other shape gets one `UnsupportedSource` Event and no
  proposals. See [Helm charts from a Helm repository or a registry](../mapping/multi-source.md).
- **Directory, Helm and Kustomize sources only.** Plugin sources are detected
  but their proposals end `Unmapped`. See [Helm sources](../mapping/helm.md) and [Kustomize sources](../mapping/kustomize.md)
  for what mapping can and cannot do.
- **One operator instance is one trust domain.** Policies and connections of an
  instance share the repository cache and the operator can read the Secrets of
  every namespace in `secretNamespaces` (every namespace by default); run one
  instance per tenant that must be kept apart (see [Permissions](../security.md#permissions)).
- **YAML and JSON manifests.** Jsonnet cannot be edited, and Argo CD's Jsonnet
  output is not read at all, so a resource it generates is not found. See [JSON files](../mapping/json.md)
  for what is read as JSON.
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
- **A rotated token is noticed after a delay.** Secrets are not watched (the
  operator cannot list them), so an ScmConnection finds out at its next
  verification, at most `scmRecheckInterval` (5 minutes by default) later. See
  [Permissions](../security.md#permissions).
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
