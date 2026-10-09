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
- Argo CD is reached at `spec.argoCD.url` of the policy, by default
  `https://argocd-server.argocd.svc`. An Argo CD with a private CA needs
  `caSecretRef` (a Secret key holding the PEM bundle).

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

- **Directory sources only.** Drift in resources rendered by Kustomize or Helm
  is detected, but the proposal ends `Unmapped` with a reason; nothing is
  written to Git for it.
- **YAML manifests only.** A resource defined in a JSON manifest cannot be
  edited; its proposal is `Unmapped` (`UnsupportedFileFormat`).
- **Array style is re-rendered.** Changing a value inside a list re-renders
  the entry that holds it, so its indentation and flow or block style may
  differ from your file. Items cannot be added to or removed from lists.
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
  anything. For Helm and Kustomize sources, Applications with several sources,
  Applications that were never synced, directories that contain a symbolic
  link (at either revision), and when the repository cannot be read, every
  commit pauses detection, because the check cannot be trusted. One Event on
  the policy says so. Proposals that are already open are left as they are.
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
