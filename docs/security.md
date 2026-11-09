# Security

How Backflow is built to be run for several teams on one cluster, and what it can and cannot do. A vulnerability is reported through
[SECURITY.md](../SECURITY.md), which also says what counts as one; this page is the model that report is judged against. The
allow lists and the rest of the settings are in [Configuration](configuration.md). How Secrets are read, and which namespaces the
operator may read them in, is under [Permissions](#permissions).

## Architecture and security model

**What it is.** One Deployment (the operator, `/manager`) with three controllers and a few helpers, and three custom resources of the
group `backflow.io` (`v1`): `ScmConnection` (a Git host and the Secret with its token), `BackflowPolicy` (which Argo CD Applications to watch and how to
propose) and `DriftProposal` (one captured drift and its journey, made by the operator; its spec is an immutable observation). A fourth,
`ChangeRecord` of `internal.backflow.io`, is private to the optional actor webhook.

**What it does.** For an Application a policy selects, it reads the Application (`status.sync`) from the cluster and, through Argo CD's REST API, the
manifests Argo CD renders. A difference between the rendered (desired) state and the live one is a drift. It traces each changed field back to the
file that owns it in Git (plain manifests, Kustomize or Helm values, with several sources), writes the edit to a branch, and **verifies the mapping by
asking Argo CD to render the edited source again**: only an edit that reproduces the live state exactly becomes a merge request (or a direct
commit, if the policy says so). Anything else is `Unmapped` and left for a person. The result is a merge request; a person decides.

**What it never does.**

- **It never writes to the cluster.** Argo CD is the only thing that applies manifests. The operator's ClusterRole can read the cluster and write
  its own resources (`backflow.io`, `internal.backflow.io`), nothing else.
- **It never guesses a mapping** and never reaches a Git host the installer did not allow.
- It never lists or watches Secrets: it **gets** the few it needs **by name**, straight from the API server (so the only permission is `get`), and only
  in the namespace of the object that names them (and, for the Argo CD token of the installation, the one the installer named). `secretNamespaces`
  narrows even that.

**The trust boundaries.**

| Who | Can | Cannot |
|---|---|---|
| The installer (Helm values, flags) | set the Argo CD connection, the allow lists (`scm.allowedHosts`, `argocd.allowedServers`), who may use the default connection (`argocd.defaultAccess`), where Secrets are read | |
| A tenant (creates `ScmConnection` and `BackflowPolicy` in a namespace) | name a Git host and a token Secret of its namespace, select Applications | name a host or an Argo CD server the installer did not allow (the object is not `Ready`, no request is made, the Secret is not read); read a Secret of another namespace; see an Argo CD project `defaultAccess` does not give its namespace |
| Argo CD | apply manifests; decide what its token sees | be written to by Backflow |
| The Git host token | push a branch and open a merge request | do more than the token's scopes |
| The optional actor webhook | see updates of tracked resources (not Secrets, not dry runs, not system or GitOps users) and record who made them | deny anything (it is observe-only: failure policy `Ignore`, always answers allowed, a one second timeout) |

**Data it keeps.** Proposals hold the changed fields (desired and live values, which may be sensitive: Secrets are excluded by default), the
mapping and its status. With the actor webhook on, user names and groups go into `ChangeRecord`s for two hours (`actorWebhook.retention`) and, if
`recordUsername` is on, into the proposals; `usernameInMergeRequest` puts a name into the text of a merge request, which Git keeps for ever (off
by default). Finished proposals are deleted after 14 days by default. Tokens are never logged, put in a condition, an event, a metric or a commit.

**The pod.** Non-root, a read-only root file system, no capabilities, `seccompProfile: RuntimeDefault` (the Pod Security `restricted` profile); the
Git cache is an `emptyDir`. The metrics endpoint is served over TLS and protected by token review. A reported vulnerability goes through
[SECURITY.md](../SECURITY.md); what the version number promises is in [Compatibility and support](compatibility.md).

**Compatibility and support.** Semantic versioning from 1.0: `backflow.io/v1` gets no breaking change in 1.x, `v1beta1` is served but deprecated
until 2.0, and a deprecation is announced one minor release ahead. Backflow has a single maintainer: **only the latest minor release of 1.x gets fixes**
(no backports; upgrading within 1.x is safe by the rule above). A security report is acknowledged within five business days, fixed as soon as
reasonably possible, and disclosed in coordination within 90 days at most. The details, and the support matrix (Kubernetes, OpenShift, Argo CD, GitLab and GitHub,
with what CI tests), are in [Compatibility and support](compatibility.md) and [SECURITY.md](../SECURITY.md).

## Permissions

The operator never writes to the cluster. Everything it is allowed to do, and why:

| Resource | Verbs | Scope | Why |
|---|---|---|---|
| `applications.argoproj.io` | `get`, `list`, `watch` | cluster | find the Applications a policy selects and see when they drift or sync |
| `scmconnections`, `backflowpolicies`, `driftproposals` (`backflow.io`) | `get`, `list`, `watch`, `create`, `update`, `patch`, `delete` | cluster | its own resources; it creates and updates DriftProposals |
| their `/status` | `get`, `update`, `patch` | cluster | the controller that owns a resource writes its status |
| their `/finalizers` | `update` | cluster | cleanup of branches and merge requests when a proposal is deleted |
| `events` | `create`, `patch` | cluster | Events on policies and proposals |
| `secrets` | **`get` only** | every namespace, or the namespaces of `secretNamespaces` and of `argocd.tokenSecret` | the token of an ScmConnection, its CA bundle, the Argo CD token and CA bundle of a policy, the Argo CD token of `argocd.tokenSecret` |
| `leases`, `configmaps`, `events` | leader election | release namespace | only with `leaderElection` |
| `tokenreviews`, `subjectaccessreviews` | `create` | cluster | only with `metrics.enabled` (authentication and authorization of the metrics endpoint) |

Secrets are read **by name, straight from the API server**: the operator holds
no `list` or `watch` on them, keeps no informer for them, and so does not hold
every Secret of the cluster in memory. A Secret read stays in memory for
`secretCacheTTL` (30s) and is never logged.

By default the `get` on Secrets is a ClusterRole rule. To narrow it to the
namespaces where your ScmConnections and policies live, list them:

```bash
helm upgrade --install backflow oci://ghcr.io/sserkanml/charts/backflow -n backflow-system \
  --set 'secretNamespaces={team-a,team-b}'
```

The cluster-wide rule is then gone: the chart creates a Role and a RoleBinding
that grant `get` on Secrets in each listed namespace and in the release
namespace. An ScmConnection whose token Secret is in another namespace is not
`Ready`, with the reason `SecretForbidden` and a message that names the namespace
and the chart value. A BackflowPolicy whose Argo CD token is in another namespace
cannot reach Argo CD; its proposals are held and the message names the namespace.

**Token rotation.** Secrets are not watched, so a changed Secret is noticed at
the next read, not at once:

- An ScmConnection is verified again every `scmRecheckInterval` (default `5m`,
  the operator flag `--scm-recheck-interval`). A rotated token or CA bundle is
  therefore picked up within that interval, and `Ready` turns False if the old
  token stops working. Changing the ScmConnection itself verifies it at once.
- A proposal that is held because its credentials were refused retries with a
  growing delay (5 seconds to 10 minutes) and reads the Secret again each time.
- The Argo CD token is read when it is used, and kept for `secretCacheTTL` (30s,
  `--secret-cache-ttl`), and dropped as soon as Argo CD answers 401, so a rotated
  token is used within about 30 seconds.

## Multi-tenancy and access

Backflow reads Applications, and the live values and diffs of what they manage, from
Argo CD. Who may *see* an Application through a BackflowPolicy is decided in two ways,
one for each way a policy reaches Argo CD. Anyone who can create a BackflowPolicy in a
namespace is limited by them; the policy itself cannot widen them.

**A policy with its own `spec.argoCD.server`** (a token Secret in its namespace). It sees the
Applications that token can get from Argo CD, and nothing else: for each selected
Application the operator asks Argo CD, with that token, whether it may get it (the answer
is kept for a minute per token and Application). Argo CD answers 403 for an Application
that exists and one that does not, so a refused one is hidden either way. Which
Applications the token may get is the Argo CD RBAC of that account.

If Argo CD cannot be reached, the policy fails closed: an Application whose visibility was
confirmed earlier stays visible, however old the answer, and one that was never confirmed
is not shown (it is counted in `status.argoCD.hiddenApplications` with the reason `VisibilityUnknown`,
and the `Ready` message says the check is retried, every 30 seconds). Only a definite
401, 403 or 404 hides an Application or retires its proposals; an outage never does. The
confirmed answers live in the operator's memory, so after a restart during an outage
nothing is shown until Argo CD answers.

**A policy without `spec.argoCD.server`** uses the operator's default connection, whose account
can usually read every Application. Which Applications such a policy may match is
defined by the administrator who installs the operator, never by a policy:

```yaml
# values.yaml of the chart (the flag --argocd-default-access, once per rule)
argocd:
  server: https://argocd-server.argocd.svc
  tokenSecret: {namespace: argocd, name: backflow-local-user}
  defaultAccess:
    - policyNamespaces: [team-a]      # the namespaces of the policies; "*" is any
      projects: [team-a, shared]      # Argo CD projects (spec.project); "*" is any
    - policyNamespaces: [platform]
      projects: ["*"]
```

- A policy may match an Application only if its Argo CD project is allowed for the
  policy's namespace by some rule. Rules add up; a rule allows the pairs of its
  namespaces and its projects. An Application without `spec.project` is in `default`.
- **No rules (the default):** only the policies in the namespace the operator is
  installed in may use the default connection, for every project. This is the
  single-installer setup of [Install on OpenShift GitOps](install/openshift.md#install-on-openshift-gitops). With rules, only what they allow
  is allowed, the operator's own namespace included (add it to a rule if it needs one).
- A policy in a namespace no rule covers is not `Ready`: reason
  `ArgoCDDefaultNotAllowed`, and a message that names the chart value. It matches nothing,
  and the proposals it had are retired.
- **Single-tenant cluster** that wants every policy to see everything:
  `defaultAccess: [{policyNamespaces: ["*"], projects: ["*"]}]`.

**What a policy is not told.** An Application a policy may not see never appears by
name, repository, path or project in its status, in its proposals, in Events, in the
operator's log at the default level or in a metric label. The status holds a count and a
reason only:

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      reason: Resolved
      message: "Watching 1 application(s); Argo CD API: operator default (https://argocd-server.argocd.svc); 2 selected application(s) excluded: their Argo CD project is not allowed for this namespace (argocd.defaultAccess)"
  applicationCount: 1
  applications:
    - name: team-a-web
      managedResources: 3
      sources: [{position: 1, repoURL: "https://gitlab.com/team-a/web.git", type: Directory}]
  argoCD:
    source: OperatorDefault
    url: https://argocd-server.argocd.svc
    hiddenApplications:
      - reason: ProjectNotAllowed       # or NotVisibleToToken, VisibilityUnknown
        count: 2
```

The count does show that a name in `spec.applications.names` or a label selector matched
something the policy may not see; it is the one thing a policy can learn. A parent
Application (the app-of-apps behind a value set in an Application) that the policy may not
see is reported as `NotVisible` with the sentence "the parent Application is not visible
to this policy", and nothing else about it.

**Existing proposals.** When an Application stops being visible to its policy (a rule
removed, a project changed, a token that lost the permission), its open proposals end as
`Superseded` with the reason `AccessRevoked`, without a call to Argo CD. The rule is also
checked again before drift detection and before every proposal reads from Argo CD with the
default token, whatever the policy's status says.
