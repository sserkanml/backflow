# Actor webhook (actor phase 2)

Status: **implemented** (`internal/actorwebhook`, chart `actorWebhook.*`), off by default
(`actorWebhook.enabled: false`). The spike below was run first; where the implementation chose
differently from the spike's recommendation, the section "Decisions of the implementation" says so
and the earlier text is kept as the evidence.

`DriftProposal.spec.actor.manager/operation/time` (phase 1) come from `managedFields`, so
`username` and `groups` stay empty and `manager` is a string the client chooses. An
observe-only admission webhook sees the real identity (`AdmissionRequest.userInfo`). This note
records what a webhook receives, what it costs, and how to keep it from ever blocking or
slowing the cluster and from ever storing data values.

The spike ran on a local kind cluster (Kubernetes v1.35.0, Argo CD v3.5.3, kindnet), in two
throwaway namespaces and one throwaway `ValidatingWebhookConfiguration`, all deleted
afterwards. The receiver was a 100-line Python HTTPS server that answered `allowed: true` and
kept compact records (no values). Numbers are from one run on a laptop; they show orders of
magnitude, not benchmarks. Statements marked *(docs)* come from the Kubernetes or OpenShift
documentation or from memory of it and were **not** tested here.

## Decisions of the implementation

The design below was implemented with these decisions, which override the recommendation where they differ:

| Topic | Decision |
|---|---|
| Retention | **2 hours** (`actorWebhook.retention`), not 15 minutes. |
| Tolerance of the cross-check | **10 seconds** (`actorWebhook.tolerance`), not 30. |
| Storage | The spike's option D: a memory buffer per pod, flushed every 2 s (`flushInterval`) to **namespaced `ChangeRecord`s in the operator namespace**, key `(uid, user, manager, subresource)`, paths merged; the operator's RBAC for them is a **Role** in the release namespace (the cache of ChangeRecords is restricted to that namespace). Every replica writes; **the leader sweeps**. A cap on the number of records (`maxRecords`, 5000): the oldest are deleted (and the oldest in a full buffer dropped), counted in `backflow_actor_records_dropped_total{reason}`. |
| Names | `actorWebhook.recordUsername` (default `true`). `false` stores only `kind` (`person`, or `controller` for a service account): no username, no groups, and the record key holds no name. `storeGroups` of the spike is gone: groups are stored with the name. `usernameInMergeRequest` (default `false`) keeps the name out of merge request and commit text. |
| Matching | At proposal creation, for the latest owner of the drifted paths: record with the object's uid, **the same field manager** as the managedFields entry, `|record.lastSeen - entry.time| <= tolerance`, and a path equal to, above or below a path the entry owns; the closest in time wins. Anything else leaves the actor as phase 1 made it. Never delays or fails a proposal (one cached list, errors logged at V(1)). |
| `scale` | `kubectl scale` sends no `options.fieldManager`, and the managedFields entry the API server writes for it (manager from the user agent, `subresource: scale`) has **no `time`** (observed on kind 1.35). A record with an **empty** manager is matched against such an entry (`MatchSubresource`): same uid, same subresource, the entry's manager and operation are the actor's, the entry owns the path. With an entry that has a time, the time must be within the tolerance, as in `Match`. Without one the **history of the Application** bounds the records: after the last successful sync (the later of `status.operationState.finishedAt` of a Succeeded operation and the newest `status.history[].deployedAt`) and not after the proposal (`detectedAt` + tolerance); no known sync means `no_window` and no name; several users in the window mean `ambiguous` and no name. That sync is the Application's, not necessarily one that touched the resource: a window that starts too late only leaves a name out. A record that has a manager keeps the strict check. The Scale object carries the Deployment's uid and no annotations, so the `scale` condition cannot look at annotations: a scale of **any** Deployment, StatefulSet or ReplicaSet in a watched namespace reaches the webhook and is recorded (the handler does no I/O and cannot know what Argo CD tracks). Only the records of the uid of a drifted, tracked object are ever read; the others wait for the retention. Narrow it with `actorWebhook.excludeNamespaces` or the namespace selector. |
| Endpoint | The NetworkPolicy and the random path of the recommendation; `apiserverCIDRs` is required outside OpenShift when the policy is enabled. |
| TLS | `auto`: OpenShift service CA, else cert-manager if `cert-manager.io/v1` is there, else Helm-generated; or `serviceCA`, `certManager`, `helm`, `secret`. |
| API | `Actor.userAgent` is marked deprecated (never filled), not removed. `Actor.kind` is new. The merge request no longer prints the user agent. |
| Source of truth for the CEL | `internal/actorwebhook.Conditions` generates the chart's `_actor_conditions.tpl` (`hack/genactorconditions`, run by `make helm-sync`) and a chart test compares the rendered conditions to it; the unit tests evaluate them with cel-go against sample requests, including an absent `subResource`. |
| e2e | `ACTOR_WEBHOOK=true` adds section i to `hack/test-merge-request.sh`: the local operator serves the webhook, registered with `clientConfig.url` to the host. |

**Hold before the actor is resolved.** The actor is set when the proposal is made and never again (`spec` is
immutable), and one record holds all changes of a user to an object. A proposal made before the newest change
is flushed could see only an earlier user's record and name them. With the webhook on, a drift therefore has to
be seen unchanged for `flushInterval + tolerance`, at most 5 s, in addition to `batchWindow` (the larger of the
two counts). A drift with other changes starts again. Without the webhook there is no hold.

Not decided here, left open: whether a record should survive an operator restart *before* its flush (it
does not), and the metric for fail-open counts (the apiserver has it; Backflow cannot scrape it).

## Recommendation in one page

| | |
|---|---|
| Webhook | one `ValidatingWebhookConfiguration`, `UPDATE` only, `failurePolicy: Ignore`, `sideEffects: None`, `timeoutSeconds: 1`, handler always answers `allowed: true` within a few ms |
| Narrowing | `rules` for namespaced `*` and `*/scale`, `namespaceSelector` (system namespaces out), and four `matchConditions` (no Secrets, no dry-run, tracked by Argo CD or `scale`, not system or GitOps users). Cut the calls of a busy namespace from 215 to 9 in the spike |
| What is read | the request's `userInfo`, `options.fieldManager`, `subResource`, `dryRun`, and a diff of `object` against `oldObject` reduced to **paths**. Values are parsed and dropped, never stored, never logged |
| Record | ~330 bytes: object uid and identity, resourceVersion, operation, manager, user, groups, changed paths, first and last seen, count |
| Storage (S5) | **pod memory buffer, flushed in batches to short-lived namespaced `ChangeRecord` objects in the operator namespace, TTL-cleaned by the leader.** Not memory alone (replicas, restarts), not one write per request (API load) |
| Endpoint (S7) | **NetworkPolicy to the kube-apiserver** (vanilla: its node addresses; OpenShift: the `policy-group.network.openshift.io/host-network` namespace label) **plus a cross-check of every record against the live object's `managedFields`.** The caller cannot be authenticated by the webhook; do not pretend it can |
| TLS (S8) | `actorWebhook.tls.mode`: `auto` (default) picks OpenShift service CA, else cert-manager, else a Helm-generated certificate; `serviceCA`, `certManager`, `helm`, `secret` force one |
| Privacy (S9) | user names only in `spec.actor` of a DriftProposal, never in metrics; `actorWebhook.usernameInMergeRequest: false` keeps them out of MR and commit text; records live 15 minutes |

## S1. What the apiserver sends

Webhook rules for `UPDATE` of every namespaced resource, `*/scale` and `*/status`. A `PATCH`,
an `apply`, an `edit` and a `scale` all arrive as `operation: UPDATE`.

Fields of the request that were present (`requestKeys`): `uid`, `kind`, `resource`, `name`,
`namespace`, `operation`, `userInfo`, `options`, `dryRun`, `object`, `oldObject`,
`requestKind`, `requestResource`, and, for a subresource, `subResource` and
`requestSubResource`. `subResource` is **absent, not empty**, for the main resource (see the CEL
trap in S2). `options` is an `UpdateOptions` object and holds `fieldManager`. The request has
**no `userAgent`**.

| Client | username | groups | `options.fieldManager` | notes |
|---|---|---|---|---|
| `kubectl patch` | `kubernetes-admin` | `kubeadm:cluster-admins`, `system:authenticated` | `kubectl-patch` | `extra`: `credential-id: X509SHA256=<hash>` |
| `kubectl edit` | same | same | `kubectl-edit` | |
| `kubectl label` | same | same | `kubectl-label` | |
| `kubectl scale` | same | same | none | `resource: deployments`, `subResource: scale`; `object` is the `Scale`, **without the Deployment's annotations or labels** |
| `kubectl apply` (client-side) | same | same | `kubectl-client-side-apply` | `last-applied-configuration` annotation in the diff |
| `kubectl apply --server-side` | same | same | `kubectl` (and `kubectl-last-applied` once, migrating) | **four `UPDATE`s with an empty diff** on first use: managedFields-only writes |
| `kubectl patch --dry-run=server` | same | same | `kubectl-patch` | `dryRun: true`, request still sent |
| `kubectl ... --as=alice --as-group=devs` | `alice` | `devs`, `system:authenticated` | `kubectl-patch` | the impersonated identity, no trace of the real caller in `userInfo` |
| bearer token of a ServiceAccount (plain `curl`) | `system:serviceaccount:<ns>:ci` | `system:serviceaccounts`, `system:serviceaccounts:<ns>`, `system:authenticated` | whatever the client chose (`my-ci-tool`) | `extra`: `credential-id: JTI=<id>` |
| Argo CD sync | `system:serviceaccount:argocd:argocd-application-controller` | `system:serviceaccounts`, `system:serviceaccounts:argocd`, `system:authenticated` | `argocd-controller` | `extra`: `authentication.kubernetes.io/pod-name`, `pod-uid`, `node-name`, `node-uid` (bound token) |
| kube-controller-manager controllers, kubelet | `system:serviceaccount:kube-system:deployment-controller`, `replicaset-controller`, `endpointslice-controller`, ...; `system:node:<node>` (group `system:nodes`) | | none | thousands of `*/status` writes; see S2 |

Conclusions:

1. `username` and `groups` are exactly what a person needs, and they are the real, authenticated
   identity (including impersonation, which shows the impersonated user).
2. **There is no user agent.** The only `User-Agent` seen is the one of the apiserver's call
   (`kube-apiserver-admission`). `Actor.UserAgent` cannot be filled from a webhook. The field
   manager is the substitute, as in phase 1, and it is still client-chosen.
3. `fieldManager` is present for most writes, absent for subresource and controller writes.
4. `extra` holds a credential id (certificate hash or token JTI), and for a bound ServiceAccount
   token the pod name. Not needed; **do not store `extra`** (it identifies a credential).
5. `kubectl scale` is a `deployments/scale` request. It has no tracking annotation, so the
   handler must map `(namespace, name)` (and `uid` of the object, which the `Scale` carries) to
   the Deployment itself.
6. Server-side apply produces empty-diff updates. Records with no changed path are dropped.

## S2. Narrowing, and the number of calls

Four variants against the same scenario in one namespace: a user scales and rolls out an
8-replica Deployment, patches a tracked ConfigMap five times, updates a Secret twice, patches a
tracked Deployment and Argo CD syncs it. The webhook counts what reaches it.

| Variant | Narrowing | Calls |
|---|---|---|
| A | `UPDATE` on `*`, `*/scale`, `*/status` | **215** (70 `pods/status` by the node, 53 `replicasets/status`, 50 `deployments/status`, 16 `replicasets`, 8 `endpointslices`, ...) |
| B | A without `*/status` | 34 |
| C | B plus `matchConditions`: not Secrets, not dry-run, tracked (annotation `argocd.argoproj.io/tracking-id` or label `app.kubernetes.io/instance`) or `scale` | 14 |
| D | C plus not `system:serviceaccount:kube-system:*`, `system:node:*`, `system:kube-controller-manager`, `system:kube-scheduler`, Argo CD's application controller, Backflow's service account | **9** (the 5 ConfigMap patches and the 4 user writes on Deployments and `scale`) |

D is exactly what a person did. In C the 4 extra calls are ReplicaSets that **inherit the
Deployment's tracking annotation** from the Deployment controller; excluding the controllers'
service accounts removes them.

- **`rules`**: `operations: [UPDATE]`, `scope: Namespaced`, `resources: ["*", "*/scale"]`.
  Not `*/status` (status writes are controllers'). `CREATE` is not interesting (a new object is
  not drift); `DELETE` is out of scope for drift.
- **`namespaceSelector`**: cannot know which namespaces Argo CD manages. Default: exclude
  `kube-system`, `kube-public`, `kube-node-lease`, the operator's and Argo CD's namespaces and,
  on OpenShift, `openshift*` (`matchExpressions: kubernetes.io/metadata.name NotIn [...]`),
  configurable.
- **`objectSelector`**: matches **labels** only, so it works only with Argo CD's label tracking
  (`app.kubernetes.io/instance`, `application.resourceTrackingMethod: label`). Argo CD v3.5.3
  here tracks by **annotation**: no label on the objects. So `objectSelector` is not usable as the
  default; the annotation needs CEL.
- **`matchConditions`** (CEL), as tested:

  ```yaml
  matchConditions:
  - name: not-secrets
    expression: "request.resource.resource != 'secrets'"
  - name: not-dry-run
    expression: "!has(request.dryRun) || request.dryRun == false"
  - name: tracked-or-scale
    expression: >-
      (has(request.subResource) && request.subResource == 'scale') ||
      (!has(request.subResource) &&
       ((has(object.metadata.annotations) && 'argocd.argoproj.io/tracking-id' in object.metadata.annotations) ||
        (has(object.metadata.labels) && 'app.kubernetes.io/instance' in object.metadata.labels)))
  - name: not-system-or-gitops-users
    expression: >-
      !request.userInfo.username.startsWith('system:serviceaccount:kube-system:') &&
      !request.userInfo.username.startsWith('system:node:') &&
      !(request.userInfo.username in ['system:kube-controller-manager', 'system:kube-scheduler',
        'system:serviceaccount:argocd:argocd-application-controller',
        'system:serviceaccount:backflow-system:backflow-controller-manager'])
  ```

  **A trap that cost a run:** `request.subResource == ''` raises an evaluation error because the
  field is *absent*, not empty, for the main resource. With `failurePolicy: Ignore` an
  evaluation error means "no match", **silently**: variant C first showed 2 calls instead of 14,
  and only `apiserver_admission_match_condition_evaluation_errors_total` (50 of 54) gave it
  away. Use `has(request.subResource)`. The chart must be tested against a real apiserver (see
  the test plan), not only linted, and the operator should expose a way to see the metric.
- **Secrets are excluded twice**: by the CEL condition, and by never storing values. The
  `rules` cannot exclude a resource; listing every non-Secret resource is not maintainable.
  Because a broken CEL condition fails *to no call* with `Ignore`, a mistake there keeps the
  webhook silent rather than leaking, but the request body of a call that does arrive still holds
  the whole object, so the handler must treat `secrets` as "drop at once" as well (defence in
  depth).
- **Subresources and dry-run**: status writes are out by the `rules`; dry-run by CEL. Both would
  otherwise be most of the calls or records of changes that never happened.
- **Versions** *(docs)*: `matchConditions` is alpha in Kubernetes 1.27, **beta and on by default
  in 1.28**, GA in 1.30. OpenShift 4.15 is Kubernetes 1.28 (beta, on), OpenShift 4.17 is 1.30
  (GA). Below 1.28 the API field is dropped or rejected: the chart must not set it, and the
  handler filters by itself (all calls arrive; use only on small clusters, and warn). The chart
  decides from `.Capabilities.KubeVersion` (`>= 1.28`).

Cost of a call that does arrive: the body is the whole object (1.4 to 4.8 KB for a ConfigMap
in the spike; tens of KB for a Deployment or CRD). It is parsed in the handler and dropped.

## S3. Safety

Settings: `failurePolicy: Ignore`, `sideEffects: None`, `timeoutSeconds: 2` in the spike,
handler always `allowed: true`. 20 `kubectl patch` calls per case, latency of the API request as
`kubectl -v=6` reports it:

| Case | p50 | max |
|---|---|---|
| no webhook (baseline) | 2 ms | 4 ms |
| webhook up, fast handler | 4 ms | 9 ms |
| webhook up, handler takes 300 ms | 311 ms | 317 ms |
| handler takes 5 s (longer than the timeout) | **2009 ms** | 2011 ms |
| process hung: accepts the connection, never answers (`SIGSTOP`) | **2006 ms** | 2009 ms |
| no endpoints (pod gone or scaled to 0) | 3 ms | 4 ms |
| TLS verification fails (wrong CA) | 4 ms | 6 ms |

Every one of these calls returned `200 OK`: **nothing was ever denied.**
`apiserver_admission_webhook_fail_open_count{name="..."}` counted the calls that failed open,
which is the metric to alert on.

Reading:

- Added latency with a healthy webhook is ~2 ms. A webhook that is *up but slow or hung* costs
  **exactly `timeoutSeconds` on every matched request**: 2 s here. That is the real risk, and it
  applies to the tracked, non-system, non-dry-run writes only (S2: 9 of 215). Use
  **`timeoutSeconds: 1`**, the minimum useful value, and a handler that does no I/O: parse,
  append to an in-memory buffer, answer. The flush to the API server (S5) is asynchronous and must
  never be on the request path.
- A webhook that is down or unreachable costs nothing (connection refused, no endpoints, TLS
  failure all fail open in single-digit ms).
- `failurePolicy: Ignore` is not a safety net against a *slow* handler, only against a failing
  one. The only protection against slowness is the timeout and the narrow match. Do not run the
  handler on the same event loop as reconciles, and cap its work (bounded buffer, drop when full).
- Leader election: the webhook server must run in **every** replica (the Service reaches any
  ready pod). Readiness of the pod should include the webhook server being up, not the leader
  lease.
- The webhook configuration is cluster-scoped: if Backflow is uninstalled or the Service is
  deleted, the configuration remains and fails open. `helm uninstall` removes it.

## S4. What to record

Per matched request, the handler computes the changed **paths** of `object` against `oldObject`
(JSON pointers; lists compared by index while lengths match, else the list's own path;
`metadata.managedFields`, `resourceVersion`, `generation` ignored) and drops everything else.

```yaml
# one record (JSON, ~330 bytes; ~570 with long names and 12 paths)
uid: 59115bda-...            # of the object
group: apps
resource: deployments
namespace: team-a
name: web
resourceVersion: "123456"    # of the new object
operation: UPDATE
subResource: ""              # or "scale"
username: alice
groups: [devs, system:authenticated]
fieldManager: kubectl-edit
paths: [/spec/replicas, /spec/template/spec/containers/0/image]
firstSeen: 2026-11-01T22:00:00Z
lastSeen: 2026-11-01T22:00:03Z
count: 3
```

- **No values.** No `object`, no `oldObject`, no `extra`, no user agent, no request body, no
  request uid. A path can still leak a key *name* (`/data/DB_PASSWORD`); that is the same
  information the DriftProposal already carries in `spec.changes[].path`.
- **Dedupe and merge** by `(uid, username, fieldManager, subResource)`: paths are unioned (cap 64,
  then `"..."`), `lastSeen` and `resourceVersion` advance, `count` increments. One `kubectl edit`
  plus a quick follow-up patch by the same person is one record. Another user, or another manager,
  is another record, so two people changing the same object are not blurred together.
- **TTL.** A record is useful until the drift controller has paired it: batch window (30 s by
  default) plus Argo CD's refresh lag (up to ~3 min) plus the stability window. Default **15
  minutes** since `lastSeen`, `actorWebhook.retention` configurable (minimum 5 minutes). A record
  already attached to a proposal is not kept; the proposal's `spec.actor` is the long-lived copy.
- **Drop** records with no changed path (server-side apply migrations, `last-applied` only).

## S5. Where to keep records

The webhook Service reaches any ready pod, not only the leader; the drift controller runs in the
leader. Options:

| Option | Restart | HA (2+ replicas) | API load | RBAC and readers | Verdict |
|---|---|---|---|---|---|
| **A. Pod memory only** | lost | the leader sees only the records of its own pod: with N replicas, ~1/N of them | none | none | fine for one replica, **wrong for HA**: it silently attributes nothing |
| **B. Leader pulls from every pod** (headless Service, `GET /records`) | lost per pod | works while pods live | none | needs pod-to-pod authentication (TokenReview) and a second endpoint | more moving parts than C, still volatile |
| **C. One `ChangeRecord` object per record, written on each request** | kept | works | one write per matched request, **on the request path** (a slow API server slows the webhook, which slows the cluster) | RBAC on a new type | durable but violates S3 |
| **D. Memory buffer per pod, flushed in batches to `ChangeRecord` objects, TTL-cleaned by the leader** | at most one flush interval lost | works | bounded: at most one write per record per flush interval (2 s), after dedupe | RBAC on a new type | **recommended** |
| E. Annotation on the changed resource | kept | works | writes to the user's resource: changes it, triggers more events, may itself be drift | needs update on everything | no, and it breaks "Backflow never writes to the cluster" |
| F. `ConfigMap` / `Lease` per record | kept | works | as D | ConfigMap read by many | possible, but a typed CRD is clearer and has a schema and printer columns |

**Recommendation: D.**

- `ChangeRecord` (group `backflow.io`), **namespaced, in the operator's namespace**. Not
  cluster-scoped: it carries user names and the identity of resources in every namespace, so who
  may read it is a decision of the administrator, and a namespaced resource in the operator
  namespace is not matched by tenants' roles (and no aggregate `view/edit/admin` ClusterRole is
  created for it, unlike for DriftProposal). Operator RBAC: `create`, `update`, `patch`, `get`,
  `list`, `watch`, `delete` on `changerecords` in that namespace (a Role, not a ClusterRole).
- Deterministic name: `cr-<sha256(uid|username|fieldManager|subResource)[:16]>`, so a second
  flush of the same actor *updates* the object (`Update` with retry on conflict, merging paths).
  Two replicas flushing the same key conflict and retry; the merge is a union, so order does not
  matter.
- Flush every 2 s (configurable) and on shutdown; the buffer is bounded (e.g. 10,000 records;
  when full, drop the oldest and count `backflow_actor_records_dropped_total`).
- The leader deletes records whose `lastSeen` is older than the retention (a periodic sweep like
  `ScratchSweeper`). `ownerReferences` are not used (the object may be gone).
- The drift controller *reads* records with a cache restricted to that namespace (`List` by
  `uid` label `backflow.io/object-uid`, set on each record, hashed if needed).
- Load: with variant D narrowing, 9 matched requests produced at most 3 records in that scenario.
  A cluster with 100 tracked user writes per minute makes at most 100 small writes per minute,
  coalesced by the flush.
- Failure of the API server on flush: keep the buffer, retry with backoff, never block the
  handler.

## S6. Matching and forgery

A proposal has `spec.resource` (group, kind, namespace, name), the Application, the drifted
`changes[].path`, `detectedAt`, and the live object's uid and managedFields (already read in
phase 1).

**Matching.** For each drifted path, take the records with the same object `uid` whose `paths`
contain the path (or a parent or child of it), whose `lastSeen` is within
`[drift window start - skew, detectedAt]` (window start = the previous sync of the Application, or
`detectedAt - retention`), and pick the **latest** one. `spec.actor.username` and `groups` come
from it. If several users changed disjoint paths, `actor.others` lists them like phase 1.

**Cross-check with `managedFields`, so a forged review cannot attribute a change to someone
else.** The webhook cannot authenticate its caller (S7). The live object can: for the record's
`fieldManager` and the drifted paths, `managedFields` of the live object must contain an entry
with **the same manager**, whose `time` is within `N` seconds (default 30) of the record's
`lastSeen`, and whose `fieldsV1` owns the path. A record that fails the check is **ignored** (not
used for the username) and counted in `backflow_actor_records_rejected_total{reason}`. This binds
the username to a change the cluster itself recorded:

- a forged review for a change nobody made: no managedFields entry, ignored;
- a forged review that names another user for a real change: the manager and time match, so it
  *would* pass. This is the residual risk (see below): a forger who can reach the endpoint and
  knows a real change's manager and time can put a different username on it. The NetworkPolicy
  (S7) is what stops that; the cross-check stops everything else.
- records for an object `uid` that does not exist, or whose `resourceVersion` is older than the
  live object's `creationTimestamp`, are discarded.

When the manager is `argocd-controller` or an excluded service account, no record is used (phase 1
already leaves Argo CD's managers out).

## S7. Who may call the webhook

Tested: from an ordinary pod, `curl -k -X POST` of a forged `AdmissionReview` with
`username: "mallory-is-innocent"`, `groups: [system:masters]` was **accepted and recorded**. The
spoofed `User-Agent` (`curl/8.10.1` instead of `kube-apiserver-admission`) is visible but proves
nothing. **Nothing in the request authenticates the apiserver** by default.

Options for authenticating the caller:

1. *Client certificate or bearer token from the apiserver.* The apiserver can authenticate to
   webhooks with a kubeconfig named in its `--admission-control-config-file`
   (`AdmissionConfiguration`, `WebhookAdmission` plugin, `kubeConfigFile`) *(docs)*. That is a
   flag of the control plane: not available from a Helm chart, not on managed clusters, not
   something to require. The `ValidatingWebhookConfiguration` itself has no field for headers or
   credentials.
2. *A shared secret in the URL path or query* (`clientConfig.service.path`, or `url`): a long random
   path is guessable only by whoever can read the webhook configuration, which has the same readers
   as the webhook definition (cluster-admin and the chart installer). It raises the bar against a
   pod that scans the port; it is not authentication and it is visible in `kubectl get
   validatingwebhookconfiguration -o yaml`. Cheap, and worth doing as a second layer.
3. *Network:* only the apiserver can reach the port. Tested:

   ```yaml
   kind: NetworkPolicy
   spec:
     podSelector: {matchLabels: {app: actor-webhook}}
     policyTypes: [Ingress]
     ingress:
     - from: [{ipBlock: {cidr: 172.18.0.2/32}}]   # the control-plane node, kind
       ports: [{port: 8443, protocol: TCP}]
   ```

   With it, the forged POST from a pod timed out (`http=000` after 4 s) while the real apiserver
   calls kept arriving. The apiserver there runs on the host network, and its calls reach the pod
   with the node's address as the source (the `ipBlock` matched).

- **Vanilla Kubernetes:** the apiserver's address depends on the distribution: a control-plane node
  address (kubeadm, kind), several for HA, a managed control plane outside the cluster for EKS/GKE/AKS.
  The chart cannot know them: `actorWebhook.networkPolicy.apiserverCIDRs: []` must be given by the
  installer; empty means *no policy is created* and the README says why that is weaker. (A CNI that
  does not enforce NetworkPolicy gives no protection at all.)
- **OpenShift** *(docs, not tested here)*: the apiserver runs on the host network. Traffic from the
  host network is matched by the namespace label `policy-group.network.openshift.io/host-network`,
  so the policy is

  ```yaml
  ingress:
  - from: [{namespaceSelector: {matchLabels: {policy-group.network.openshift.io/host-network: ""}}}]
    ports: [{port: 8443, protocol: TCP}]
  ```

  The chart detects OpenShift by API group (`route.openshift.io/v1` in `.Capabilities.APIVersions`)
  and creates this policy by default there. It admits all host-network traffic (kubelets, the
  apiserver), not only the apiserver: acceptable, since those are cluster-admin-equivalent
  anyway, and it keeps tenants' pods out.

**Recommendation for S7:** the layers, in order of value: (1) NetworkPolicy to the apiserver
(automatic on OpenShift, `apiserverCIDRs` elsewhere); (2) the managedFields cross-check (S6),
which makes every record verifiable against the cluster; (3) an unguessable path in
`clientConfig.service.path`; (4) `Ignore` + narrow match so a flood of forged requests cannot
slow the apiserver, and a per-source rate limit and bounded buffer in the handler, so a flood
cannot grow memory. State plainly in the README that the webhook cannot verify its caller
unless the cluster is configured for it.

## S8. TLS

The apiserver must trust the webhook's serving certificate, through `caBundle`.

| Mode | How | Rotation | Fit |
|---|---|---|---|
| **OpenShift service CA** | annotation `service.beta.openshift.io/serving-cert-secret-name: <secret>` on the Service; annotation `service.beta.openshift.io/inject-cabundle: "true"` on the `ValidatingWebhookConfiguration` *(docs)* | by the service CA operator; the pod must reload the file | the OpenShift default: no extra component |
| **cert-manager** | `Certificate` for the Service names, `cert-manager.io/inject-ca-from: <ns>/<certificate>` on the webhook configuration | by cert-manager; the pod must reload | needs cert-manager; common on vanilla |
| **Helm-generated** | `genCA` and `genSignedCert` in a template, `lookup` of the existing Secret so an upgrade keeps it; `caBundle` rendered from it | none: valid for 5 years (configurable); renewed by `helm upgrade --set actorWebhook.tls.renew=true` | works anywhere, no dependency; weakest (a CA key in a Secret) |
| controller-runtime's own rotation | the manager generating certs at start | it has none built in for webhooks; `certwatcher` only re-reads files | not a source of certificates |
| **BYO Secret** | `actorWebhook.tls.secretName` + `caBundle` | yours | air-gapped or policy-driven clusters |

The webhook server **reads the certificate through controller-runtime's `certwatcher`** (files
mounted from the Secret), so a rotated certificate needs no restart in any mode. The metrics server
already does this.

**Default `actorWebhook.tls.mode: auto`:** OpenShift (`route.openshift.io/v1`) uses the service CA;
else cert-manager if the `cert-manager.io/v1` API is present; else Helm-generated. Explicit values
`serviceCA`, `certManager`, `helm`, `secret` override. The mode in use is shown in the NOTES.

## S9. Privacy

User names (and group names) are personal data.

- **Where they appear:** `ChangeRecord` (operator namespace, readable by the operator and
  administrators only, TTL 15 min); `spec.actor.username/groups` of a DriftProposal (readable by
  whoever can read DriftProposals in that namespace); the merge request description and the commit
  trailer.
- **Where they must not:** metrics (labels stay `changed_by` class only: `interactive`, `controller`,
  `unknown`, `none`), operator logs at the default level, Events of the policy, Argo CD, ScmConnection
  status. `backflow_actor_*` counters carry no user label.
- **Chart switches:**
  - `actorWebhook.usernameInMergeRequest: true|false` (default **false**): the MR and the commit say
    `Changed with: kubectl-patch (Update) at ...` as today and, only when true, `by alice`. The
    proposal's `spec.actor.username` is still filled (the cluster owner can see it).
  - `actorWebhook.storeGroups: true|false` (default **false**): groups can be large and identify a
    team more than a person; `username` is enough to find who to ask.
  - `actorWebhook.retention` for `ChangeRecord`s (S4).
- Git is forever: a username written into a merge request or commit cannot be taken back. That is why
  the default keeps it out and why it is a switch, not a flag of the DriftProposal.
- `extra`, `uid` of the user, credential ids and the apiserver's user agent are never read into a
  record.

## What stays a limitation

- **No user agent.** `Actor.UserAgent` stays empty; it should be removed from the API in a later
  version or documented as unused. The manager name remains the client-chosen hint for the tool.
- **Impersonation** shows the impersonated user, not who impersonated.
- **A user through a shared account** (a CI token, a shared kubeconfig) is that account. The webhook
  names the credential, not the human.
- **Argo CD-managed changes are not attributed** (excluded on purpose): the origin of a sync is Git.
- **Changes while the webhook or the operator is down are not attributed** (fail-open is the point). The
  proposal falls back to phase 1's managedFields data.
- **Scale and other subresources** are matched to the parent by `(namespace, name, uid)`, so a
  Deployment tracked by a *label* convention not covered by the CEL, or a custom resource without the
  tracking annotation or the instance label, is not seen. `actorWebhook.extraMatchCondition` lets the
  administrator add a CEL expression.
- **Kubernetes below 1.28** has no `matchConditions`: every `UPDATE` of the namespaced resources
  reaches the webhook. The chart refuses to enable it unless `actorWebhook.allowUnfiltered=true`.
- **Forgery by anyone who can reach the port** is limited by the cross-check, not eliminated (S6).
- A **broken CEL expression silently disables** the webhook (Ignore). The operator watches
  `apiserver_admission_match_condition_evaluation_errors_total` only if it can scrape the
  apiserver, which it usually cannot; the e2e test (below) is the guard.
- The records are **eventually consistent**: up to one flush interval of records can be lost on a
  pod crash.

## Design

Values (all under `actorWebhook`, **off by default**):

```yaml
actorWebhook:
  enabled: false
  port: 9443
  timeoutSeconds: 1                  # 1 or 2
  namespaceSelector: {}              # default excludes system namespaces and the operator's
  excludeUsers: []                   # extra usernames to exclude, besides the built-in ones
  extraMatchCondition: ""            # an extra CEL expression, ANDed
  allowUnfiltered: false             # allow on Kubernetes < 1.28
  retention: 15m
  flushInterval: 2s
  maxBufferedRecords: 10000
  storeGroups: false
  usernameInMergeRequest: false
  tls:
    mode: auto                       # auto | serviceCA | certManager | helm | secret
    secretName: ""
    caBundle: ""
    renew: false
  networkPolicy:
    enabled: true
    apiserverCIDRs: []               # vanilla: the control-plane addresses; OpenShift: not needed
  path: ""                           # empty: a random path generated once and kept (lookup)
```

Flow:

1. The apiserver calls `POST /<path>` on the Service; the handler (in every replica) decodes the
   `AdmissionReview`, drops it if the resource is a Secret or the request has no tracked
   identity, computes changed paths, merges the record into the in-memory buffer, and answers
   `allowed: true`. It does no I/O and never returns an error status.
2. A background flusher writes merged records to `ChangeRecord` objects every `flushInterval`.
3. The leader's sweeper deletes expired records.
4. When the drift controller creates a proposal (`actorOf` in `drift_actor.go`), it first takes the
   phase 1 attribution from managedFields, then, if records exist, **verifies** them (S6) and
   fills `username` and, if enabled, `groups`, keeping `manager`, `operation` and `time` from
   managedFields. With the webhook off or no verified record, nothing changes from phase 1.
5. The merge request text uses the username only when `usernameInMergeRequest` is true.

Hard rules (must be tests):

- the handler returns `allowed: true` for every input, including garbage, oversized bodies, and
  unknown kinds, and answers within a fixed budget;
- no value of `object`/`oldObject` reaches a log line, a record, a metric or an Event;
- no code path performs an API call on the request path;
- the webhook configuration has `failurePolicy: Ignore`, `sideEffects: None`,
  `timeoutSeconds <= 2`, never `Fail`;
- a Secret is never recorded.

## Implementation and test plan

**Phase A: record path** (`internal/actorwebhook`)
1. `Diff(object, oldObject) []string`: unit tests (maps, list equal length, list length change,
   ignored metadata, null vs absent, large nested objects, no values in the output).
2. `Record`, `Buffer` with merge/dedupe/cap/drop-oldest and `Flush`: table-driven unit tests,
   including two flushers racing on the same key.
3. `Handler`: unit tests with crafted `AdmissionReview` bodies (all request shapes of S1: patch,
   scale, apply, dry-run, Secret, garbage JSON, 10 MB body): always `allowed: true`; records only
   what should be recorded; a Secret and a dry-run leave no trace.
4. `ChangeRecord` CRD + controller-gen markers; chart RBAC (Role in the operator namespace).

**Phase B: use**
5. `Verify(record, liveObject)` against managedFields (S6): unit tests for each rejection reason
   (no entry, other manager, time outside `N`, path not owned, uid mismatch).
6. `actorOf` takes the verified record; envtest with a **fake AdmissionReview** posted to the handler,
   a real `ChangeRecord` in envtest, a proposal created by the drift controller, assert the username
   is set only when the cross-check passes, and not set for a forged one.
7. Sweeper: unit and envtest (expiry).
8. MR/commit text: `usernameInMergeRequest` on and off (the existing text tests).

**Phase C: chart and cluster**
9. Chart: templates for the webhook configuration (CEL by Kubernetes version), Service, TLS modes,
   NetworkPolicy (vanilla and OpenShift), RBAC; `helm template` golden tests in `test/chart` (like
   the RBAC ones, semantic comparison), including `enabled: false` rendering nothing.
10. **e2e on kind** (a new `hack/test-actor.sh`, token-less like `test-application-values.sh`):
    install with the webhook on, make changes with `kubectl patch/edit/scale/apply`, an
    impersonated user, and a ServiceAccount token; assert the call counts are the narrowed ones (the
    spike's variant D), the usernames end up in `spec.actor`, a forged POST from a pod is blocked by
    the NetworkPolicy and, with the policy removed, rejected by the cross-check; scale the webhook to
    0 and hang it (`SIGSTOP`) and assert `kubectl patch` latency stays below `timeoutSeconds + 1 s`
    and nothing is denied. It must also assert
    `apiserver_admission_match_condition_evaluation_errors_total` stays 0, which catches the CEL trap.
11. OpenShift: the service-CA and host-network label variants on a real cluster before the release
    notes claim them.

## Open questions

1. **Retention vs. Argo CD refresh lag.** 15 minutes is a guess; slow Argo CD refresh or long
   `batchWindow` settings may need more. Should it derive from the largest `batchWindow` of the
   policies?
2. **Who may read `ChangeRecord`s?** The proposal is a namespaced resource a tenant can read; the
   record is in the operator namespace. Is exposing the username in the tenant's own namespace (via
   `spec.actor`) acceptable to the people the names belong to? This is a data-protection decision,
   not only a technical one.
3. **Cross-check strictness.** `N = 30 s` of skew between the webhook's `lastSeen` and a
   managedFields `time`: is it enough for slow clients and a busy apiserver? Needs measurement
   under load.
4. **Excluding all of `kube-system:*` service accounts** also drops a real human-run job in
   `kube-system`. Acceptable?
5. **Label tracking.** Argo CD installations with `resourceTrackingMethod: label` allow an
   `objectSelector` instead of CEL, which also works below Kubernetes 1.28. Worth supporting as a
   second mode?
6. **Argo CD's own managed changes** are excluded, but Argo CD *reverting* drift (self-heal) is not
   "someone changed it". Should a record for such a revert be kept to explain a `Reverted`
   proposal?
7. **API removal:** `Actor.UserAgent` is unused and cannot be filled. Remove it before 1.0?
8. **Cluster-wide webhook configuration and multi-tenancy:** with
   `argocd.defaultAccess`, a tenant's proposal should show only usernames of changes in what it may
   see. The pairing is by object uid, so it inherits the Application boundary, but it needs a test.
