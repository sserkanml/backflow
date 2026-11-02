# Compatibility and support

## Versioning

Backflow follows [semantic versioning](https://semver.org) from 1.0.0. What the version number promises covers the **public surface**:

- the API `backflow.io/v1`: the kinds `ScmConnection`, `BackflowPolicy` and `DriftProposal`, their fields, their validation and the meaning of their
  conditions, reasons and phases;
- the flags and the `BACKFLOW_*` variables of the operator, and the values of the Helm chart;
- the metrics (names, labels and meaning) and the alert rules of the chart;
- the commands of `manager migrate`.

Not part of it: `internal.backflow.io` (the `ChangeRecord`, which is private to the operator), the Go packages (`api/v1` is the types of the API,
not a library with a promise of its own; `internal/...` is internal), the text of log lines and condition messages (the reasons are the
contract), the Git branch names and the text of the merge requests, and the images' contents other than `/manager`.

### Rules

1. **`v1` gets no breaking change in 1.x.** A field is added, never removed, renamed or given another meaning; a validation does not get stricter
   for objects that exist (the CRD relies on ratcheting, Kubernetes 1.30); a value that the operator accepts in 1.0 is accepted in every 1.x. A
   new phase, condition reason or metric label value may appear (consumers must tolerate values they do not know; the API says so where it
   matters).
2. **`v1beta1` is served, deprecated, until 2.0.** Its schema is the one of `v1`, with conversion `None`, so an object reads and writes the same
   through either; the API server sends every client that uses it a deprecation warning. It is removed in 2.0, not before. Move your manifests and
   tools to `v1` (`manager migrate convert-files`) and run `manager migrate`, which stores the objects as `v1`; the metric
   `backflow_storage_migration_pending` says whether any CRD still holds `v1beta1` objects.
3. **Deprecations are announced one minor release ahead.** Something that will go in the next major version is marked deprecated (in the API
   with `deprecated: true` and a warning, for a flag, a value or a metric in the release notes and the documentation, and in the log where
   the operator can see it used) in a minor release **at least one minor release** before the major release that removes it. A minor release
   never removes what was not announced.
4. **Security fixes are the exception**: a fix may change a behaviour when keeping it would put a user at risk. It is in the release notes and
   in an advisory (see [SECURITY.md](../SECURITY.md)).
5. **Patch releases** (1.x.y) fix bugs, and add no feature and no migration step.
6. **Upgrades** go one minor release at a time or straight to the latest of 1.x; `manager migrate` is the only step between majors and between
   the API versions. From 0.17 and 0.18 see [upgrade-1.0.md](upgrade-1.0.md).

### Releases that are supported

Backflow has a single maintainer, so **only the latest minor release of 1.x gets fixes**, security fixes included. There are no backports to an
earlier minor release: a fix is a patch release of the latest minor release, and to get it you upgrade to it. That upgrade is safe by rule 1
above (no breaking change in 1.x). A report of a security problem is acknowledged within five business days, fixed as soon as reasonably
possible, and disclosed in coordination with the reporter within 90 days at most (see [SECURITY.md](../SECURITY.md)).

## Support matrix

| | Supported | Tested in CI |
|---|---|---|
| Kubernetes | **1.30** or later (the CRDs use CEL validation and rely on ratcheting) | the API tests and the end-to-end tests run on kind with **1.35** |
| OpenShift | **4.17** or later (Kubernetes 1.30) | not run on OpenShift in CI; the OpenShift settings (service CA, trusted CA bundle, OperatorHub install) are covered by chart tests and an OLM test on kind |
| Argo CD | **3.x**: the Application resource and the REST API (manifests of an Application). 2.x has the same API and is not tested | **3.5.3** (pinned in `hack/dev-up.sh`) |
| OpenShift GitOps | **1.18** or later, whose `ArgoCD` resource has `spec.localUsers` (the read-only account the operator uses, see the README) | not run in CI |
| GitLab | gitlab.com (SaaS) and self-managed (API v4) | the merge-request tests run against gitlab.com; self-managed is the same API and is not run in CI |
| GitHub | github.com (SaaS) and GitHub Enterprise Server (REST API) | the merge-request tests run against github.com; Enterprise Server is not run in CI |
| Helm | 3.8 or later (OCI charts) | Helm 4.2 |
| OLM | the releases that read file-based catalogs; install mode AllNamespaces | v0.28.0 on kind |
| Architectures | linux/amd64 and linux/arm64 (the images) | amd64 in CI |

"Supported" is what the project will take a bug report for. "Tested in CI" is narrower and says where a claim has been checked; a
claim that is not tested is marked so, here and not elsewhere.

### What a Git host needs

A token that can read the repository, push a branch and open a merge request (GitLab: `api` and `write_repository`; GitHub: contents and pull requests).
Backflow never writes to the cluster, and never needs a token with more.
