# Backflow

Reverse GitOps for Kubernetes and OpenShift. Backflow detects changes made
directly in a cluster to resources that Argo CD manages, and proposes them back
to the Git source of truth as merge requests against the file that owns the
resource.

Backflow never writes to the cluster: Argo CD is the only thing that applies
manifests. Backflow reads the cluster and writes to Git.

## In-cluster

```bash
make docker-build IMG=backflow:dev
kind load docker-image backflow:dev --name backflow     # kind only
make deploy IMG=backflow:dev                            # CRDs, RBAC, Deployment in backflow-system
git checkout config/manager/kustomization.yaml          # `make deploy` rewrites the image there
```

Then create the Secrets and resources from [config/samples](config/samples):

- an `ScmConnection` for the Git host, with a token Secret (GitLab: `api` +
  `write_repository`; GitHub: `contents` + `pull_requests`);
- a `BackflowPolicy` that selects the Argo CD Applications, with a Secret
  holding an Argo CD API token.

Things to know when running the operator in a cluster:

- The container root file system is read-only. Git repositories are cached in
  an `emptyDir` (2Gi) mounted at `/var/cache/backflow` and passed with
  `--repo-cache-dir`. The operator checks at startup that the directory is
  writable and exits with an error if it is not.
- Memory requests/limits (128Mi/512Mi) are a starting point; tune them to the
  number of Secrets and Applications in the cluster and the size of the
  repositories.
- `--merge-request-poll-interval` (default `2m`) is how often the state of an
  open merge request is polled. A shorter interval notices a merge or a close
  sooner and costs more calls to the Git host's API.
- Argo CD is reached at `spec.argoCD.url` of the policy, by default
  `https://argocd-server.argocd.svc`. An Argo CD with a private CA needs
  `caSecretRef` (a Secret key holding the PEM bundle).

### Trust

**One operator instance is a single trust domain.** The repository cache is
shared by every `BackflowPolicy` and `ScmConnection` the instance serves: a
repository fetched with one connection's token stays in the cache and can be
read for another policy that names the same URL. All Secrets are also readable
by the operator cluster-wide. Run a separate instance (own namespace, own cache
volume) for teams or tenants that must not see each other's repositories.

An Argo CD `repoURL` that carries credentials (`https://user:token@host/...`)
is not used: its proposals end `Unmapped` with the reason
`RepositoryURLHasCredentials`, and the URL is redacted in every message. Give
Backflow access with an `ScmConnection` instead.

To run the end-to-end scenarios against the deployed operator instead of a
local `bin/manager`:

```bash
IN_CLUSTER=true ./hack/test-drift.sh
IN_CLUSTER=true ./hack/test-mapping.sh
```

## Development

```bash
./hack/dev-up.sh        # kind cluster "backflow", Argo CD, CRDs, demo-app
make run                # run the operator locally against the current kube context
make test               # unit and envtest tests
```

## Limitations

- **One source per Application.** An Application with `spec.sources` gets one
  `UnsupportedSource` Event and no proposals.
- **Directory sources only.** Drift in resources rendered by Kustomize or Helm
  is detected, but the proposal ends `Unmapped` with a reason; nothing is
  written to Git for it.
- **YAML manifests only.** A resource defined in a JSON manifest cannot be
  edited; its proposal is `Unmapped` (`UnsupportedFileFormat`).
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
- **All Secrets are cached cluster-wide.** The operator reads token Secrets
  through an informer that watches every Secret in the cluster, which costs
  memory in clusters with many of them.
- **Open proposals of a blocked or not-Ready policy are left as they are.**
  When a policy stops matching an Application, conflicts with another policy
  or its connection is not Ready, proposals that are already open keep their
  phase; they are not closed or withdrawn.
- **While Git is ahead of the last sync of an Application (manual sync, or a
  failing sync), drift detection for that Application is paused, so Backflow
  never proposes to revert a commit.** For a Directory source, only commits
  that change files under the Application's directory count (respecting
  `directory.recurse`); a commit elsewhere in the repository does not pause
  anything. For Helm and Kustomize sources, Applications that were never
  synced, directories that contain a symbolic link or a Jsonnet file (or use
  `directory.jsonnet`) at either revision, and when the repository cannot be
  read, every commit pauses detection, because the check cannot be trusted.
  Detection is also paused, whatever the revisions, when the last sync did not
  succeed or was selective (only some resources), and when the Application's
  source (path, directory options, target revision, Helm, Kustomize, plugin)
  changed since the last sync. One Event on the policy says so. Proposals that are already open are left as they are.
  Detection is also held back while a sync is running and until Argo CD has
  compared again after it.
- **A drift becomes a proposal only after it has been stable.** It must be
  seen with the same changes at the same synced and compared revision for the
  policy's `spec.batchWindow` (default 30s), across at least two observations.
  A drift that disappears within the window is never proposed, and several quick
  edits to a resource give one proposal for the final state. This also covers
  the moment after a sync, when Argo CD can still show a resource as OutOfSync.
  What was seen is kept in memory; after a restart the window starts again,
  which only delays a proposal.
