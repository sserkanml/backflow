# Development

```bash
./hack/dev-up.sh        # kind cluster "backflow", Argo CD, CRDs, demo-app
make run                # run the operator locally against the current kube context
make test               # unit and envtest tests
make lint               # golangci-lint
make coverage           # how much real-chart and overlay drift maps back to Git (needs helm; installs kustomize; see test/coverage/REPORT.md)
make helm-sync          # copy the CRDs and RBAC rules into charts/backflow after make manifests
```

CI uses Helm v4.2.0 (`.github/workflows/test.yml`). The chart tests compare the
rendered objects, not the text, so they also pass with Helm 3.

To try a local build in the cluster instead of the released image:

```bash
make docker-build IMG=backflow:dev
kind load docker-image backflow:dev --name backflow
helm install backflow charts/backflow -n backflow-system \
  --set image.repository=backflow --set image.tag=dev --set image.pullPolicy=Never
```

The end-to-end scenarios run against a local `bin/manager`, or against the
operator deployed in the cluster with `IN_CLUSTER=true`:

```bash
IN_CLUSTER=true ./hack/test-scm.sh
IN_CLUSTER=true ./hack/test-backflow-policy.sh
IN_CLUSTER=true ./hack/test-drift.sh
IN_CLUSTER=true ./hack/test-mapping.sh
IN_CLUSTER=true ./hack/test-kustomize.sh    # needs kustomize/ in the demo repository (dev-up.sh copies it)
```

## End-to-end tests against GitLab or GitHub

The scenarios that push branches and merge merge requests
(`test-merge-request.sh`, `test-helm.sh`, `test-kustomize.sh`,
`test-multi-source.sh`) run against a demo repository on GitLab or on GitHub.
`SCM=gitlab` (the default) or `SCM=github` picks the host; `DEMO_REPO` is the
repository, as `owner/name` or an https URL. Both are read by every `hack/`
script, and `hack/scm.py` (tested by `python3 hack/test_scm.py`, which needs no
network) is the one place that knows the two APIs.

| | GitLab | GitHub |
|---|---|---|
| `DEMO_REPO` | `sserkanml/backflow-demo` | `sserkanml/backflow-demo-gh` |
| token | `GITLAB_TOKEN`: `api` and `write_repository` | `GITHUB_TOKEN`: fine-grained, the demo repository only, Contents: read and write, Pull requests: read and write, Metadata: read |
| kind cluster | `backflow` | `backflow-gh` |

To run them against GitHub:

1. Create an empty **public** repository (the Applications and the read-only
   scripts read it without credentials; do not add a README or a license) and
   a fine-grained token with the permissions above, for that repository only. The
   token needs *Actions: read* as well if you want the check that no workflow run
   starts for a scratch branch; without it that one check is skipped with a
   message.
2. `SCM=github GITHUB_TOKEN=... DEMO_REPO=<owner>/<repo> ./hack/dev-up.sh` creates
   the cluster `backflow-gh`, copies the content of the GitLab demo repository
   into the empty one (only files it lacks; set `SEED_SOURCE` to copy from another
   public GitLab project) and points the demo Applications at it. The
   Applications have the same names as on GitLab, so the two demos live in two
   clusters and coexist; set `CLUSTER_NAME` to put both in one (the second
   `dev-up.sh` then re-points the Applications).
3. `SCM=github GITHUB_TOKEN=... DEMO_REPO=<owner>/<repo> ./hack/test-merge-request.sh`
   (and the others). `SECTIONS=a,b` runs only some sections.

The scripts need write access to the demo repository, not Maintainer: unlike the
GitLab project, `main` of the GitHub demo repository is not protected. Tokens are
read from the environment only and are never printed or written down.

## End-to-end tests in CI

`.github/workflows/e2e.yml` runs the same scripts in GitHub Actions, once against
GitLab and once against GitHub (two jobs, each with its own kind cluster made by
`hack/dev-up.sh`). `hack/ci-e2e.sh` is what the jobs run, and it works locally too
(`SCM=github SCRIPTS=helm ./hack/ci-e2e.sh plan`). The kubebuilder scenario in
`test-e2e.yml` (the operator deployed from `config/`, no secrets) is a separate,
cheap check that runs on every push.

**How a run starts.** The scripts push branches and merge merge requests in the
demo repositories, so only these start them:

- a pull request **of this repository** (not a fork) that carries the label `e2e`;
  it runs again on every push to the branch while the label is there
- the nightly schedule (02:17 UTC, the default branch)
- by hand, for example against GitHub, one script and one section:

```bash
gh workflow run e2e.yml -f scm=github -f scripts=merge-request -f sections=a,b
gh workflow run e2e.yml -f scm=gitlab -f scripts=helm,kustomize
gh workflow run e2e.yml -f scm=github -f argocd_path=policy -f scripts=merge-request   # the policy path on GitHub
gh run watch
```

`argocd_path` picks how the operator reaches Argo CD in the run: `policy` (every policy has
`spec.argoCD.server`), `default` (the connection the installer gave the operator, as with the
OpenShift GitOps local user: `dev-up.sh` is run with `ARGOCD_LOCAL_USER_SECRET=true`, the
operator is started with `--argocd-server` and `--argocd-token-secret-*`, and the policies
have no `spec.argoCD.server`) or `matrix` (the default, and what the nightly run does: GitLab
keeps `policy`, GitHub uses `default`, so both paths are tested every night). Only
`merge-request`, `helm`, `kustomize` and `multi-source` can use `default`; the other
scripts keep the policy path, and the summary of the run says which path each script and
each job used. Locally: `ARGOCD_DEFAULT=true ARGOCD_LOCAL_USER_SECRET=true`.
`scm` is `both` (default), `gitlab` or `github`; `scripts` is a comma separated
list of `policy`, `drift`, `mapping`, `application-values`, `scm`, `merge-request`,
`helm`, `kustomize`, `multi-source` (empty: all). The first four need no token and
run in the GitLab job only. `sections` becomes `SECTIONS` and is used only when
exactly one script is selected. Every script runs even when an earlier one failed;
the job fails at the end. A script that skips itself because the token is missing
counts as failed.

**Where the results are.** The summary of the run has a table per host with the
passed and failed checks and the duration of every script (and the time per
section). The logs of the scripts and of the operator are in the artifact
`e2e-logs-gitlab` or `e2e-logs-github`, kept for 7 days. Before they are uploaded
they are scrubbed, and the job fails if the value of a token (or its base64) was
in any of them.

**Two runs never touch one demo repository**: the jobs share a concurrency group
per host (`e2e-gitlab`, `e2e-github`) and wait for each other; a run that waits is
not cancelled by a newer one, but only one run can wait, so a third replaces the
second. Before and after the scripts (also when the run fails or is cancelled) the
job closes the open merge requests that carry the label `backflow-e2e` or come
from a `backflow/` branch and deletes the `backflow/` and `backflow/scratch/`
branches (`hack/scm.py cleanup`); `main` is never touched, and the scripts restore
its content themselves.

**What it needs.** The repository environment `e2e` with two secrets:

| Secret | Is | Scopes |
|---|---|---|
| `GITLAB_TOKEN` | a token for `gitlab.com/sserkanml/backflow-demo` | `api` and `write_repository`; its user is Maintainer of the project, because `main` is protected and the scripts restore it |
| `E2E_GITHUB` | a fine-grained token for `github.com/sserkanml/backflow-demo-gh` only | Contents: read and write, Pull requests: read and write, Metadata: read; add Actions: read to also check that no workflow run starts for a scratch branch |

GitHub does not allow secret names that start with `GITHUB_`, so the workflow
maps `E2E_GITHUB` to `GITHUB_TOKEN`, the name the scripts read, only in the `env`
of the steps that run the scripts, the cleanup, the scrub and `dev-up.sh`. The
secret of the other host is never read in a job, and no step passes a token to
`gh` or to another action. The workflow itself has `contents: read` and nothing else.
Because a pull request of this repository can change the scripts that then run with
the secrets, give the label `e2e` only to changes you have read, and protect the
environment (required reviewers, and the branches and pull requests that may
deploy to it).

**Rotating a token.** Create the new token with the same scopes, replace the
secret in Settings, Environments, `e2e` (or `gh secret set GITLAB_TOKEN --env e2e`),
start `gh workflow run e2e.yml -f scm=<host> -f scripts=scm` to check it (`scm` is the
cheapest script that uses it), and then revoke the old token. GitLab project tokens
and GitHub fine-grained tokens expire; put the expiry date in your calendar.

Releases are made by [release-please](https://github.com/googleapis/release-please)
from Conventional Commits on `main`; the tag it creates publishes the image and
the chart.
