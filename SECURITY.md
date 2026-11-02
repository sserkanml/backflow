# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Report it privately through
[GitHub's private vulnerability reporting](https://github.com/sserkanml/backflow/security/advisories/new) of this repository
(Security → Report a vulnerability). Say which version you ran, what you did, what happened and what you expected; a manifest or a
log (without tokens) helps more than a description.

Backflow has a single maintainer. A report is **acknowledged within five business days**. The fix comes **as soon as reasonably
possible**; there is no fixed deadline, and you are told where it stands when you ask. It is released as a patch release of the latest
minor release, with an advisory that credits you if you want it.

Disclosure is coordinated: please say nothing in public until a fix is released. If there is none **90 days** after your report, you are free to
publish what you found; we ask you to tell us first, and we will say if we need a little more time for a fix that is nearly done.

## Supported versions

**Only the latest minor release of 1.x gets fixes.** There are no backports to an earlier minor release: to get a fix, upgrade to the latest
minor release (a minor release of 1.x does not break `v1`; see [docs/compatibility.md](docs/compatibility.md)). Versions before 1.0 are not
supported.

## What counts

The operator holds credentials (the token of a Git host, the token of Argo CD) that tenants of a cluster name in their own objects, and
it makes requests with them. These count as vulnerabilities:

- a way to make the operator send a request, or a token, to an address that the installer did not allow (the allow lists
  `scm.allowedHosts` and `argocd.allowedServers` exist for this);
- a way to make the operator read a Secret outside the namespace the object that names it is in (a policy or a connection may name Secrets of its
  own namespace only), or outside `secretNamespaces`;
- a way to make the operator write to the cluster (it never does: Argo CD applies; Backflow writes to Git);
- a way to see an Argo CD project through the operator's own connection that `argocd.defaultAccess` does not allow;
- a token in a log, an event, a condition, a metric, a Git commit or a merge request text;
- an injection into a Git command, a file path (a mapping that writes outside the repository), a branch name or a merge request;
- a crash that a tenant can cause with an object and that stops the detection for the other tenants.

These do not: a user with cluster-admin who changes the operator's flags or RBAC; a proposal that the operator makes for a change a person made
(that is what it is for); the behaviour of Argo CD, GitLab or GitHub themselves.

## How the operator is built for it

See "Architecture and security model" in the [README](README.md#architecture-and-security-model).
