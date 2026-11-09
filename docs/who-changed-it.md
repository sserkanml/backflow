# Who changed it

`kubectl get dp` has a column **Changed by**: the field manager that wrote the drifted
fields last. Backflow reads it from `metadata.managedFields` of the live object that Argo CD
returns for the drift (Argo CD v3.5 includes `managedFields` there), so it needs no extra
permission on the cluster and no webhook. `spec.managers` holds it, the latest writer first:

```yaml
spec:
  managers:
    - manager: kubectl-patch
      operation: Update
      time: "2026-11-01T13:26:26Z"
      fields: [/data/LOG_LEVEL]
```

The merge request says it (`Changed with: kubectl-patch (Update) at 2026-11-01 13:26:26 UTC`)
and so does the commit (a `Changed-with:` trailer).

| What changed the resource | Manager |
|---|---|
| `kubectl patch`, `edit`, `set image`, `label`, `annotate` | `kubectl-patch`, `kubectl-edit`, `kubectl-set`, `kubectl-label`, `kubectl-annotate` |
| `kubectl scale` | `kubectl` (Update, on the `scale` subresource) |
| `kubectl apply` | `kubectl-client-side-apply` (Update); with `--server-side` `kubectl` (Apply) |
| a controller such as the HPA | for example `kube-controller-manager` |
| `oc`, a browser console | `oc` and the like, or the browser (`Mozilla`) — expected, not verified |
| Argo CD's sync | `argocd-controller` — never named as the actor |

The `kubectl` rows were observed on a kind cluster (kubectl 1.32). `oc`, the console and the HPA were
not: the names in those two rows are what the clients are expected to send. A manager that is not
recognised is not called a person and not called a controller either (see below).

How it is decided. For every drifted field the entry of `managedFields` that owns it is found
(list elements are matched by their merge key). Argo CD's managers (`argocd-*`) and Backflow
are left out, and the most recent remaining entry is the actor. When several managers wrote
different drifted fields, the latest is the first of `spec.managers`, with its paths in `fields`, and the
others (at most four more) follow it. A manager is put in one of three classes, by its name only:

- **interactive**: a tool a person runs: `kubectl*`, `oc` and `oc-*`, browsers (`Mozilla`, `Chrome`, ...),
  `k9s`, `lens`, `helm`, `kustomize`, `curl`, ...
- **controller**: a known controller: `kube-controller-manager` (which is what the HPA scales
  through), `kube-scheduler`, `kubelet`, `kube-apiserver`, `cloud-controller-manager`,
  `cluster-autoscaler`, and names that end in `-controller`, `-operator` or `-controller-manager`
  (`cert-manager-controller`, Flux's `kustomize-controller`, `openshift-controller-manager`,
  `cluster-version-operator`). Only for these does the merge request carry a note that the change was
  not made by a person and that a controller that keeps doing it will undo the merge (an HPA changing
  `spec.replicas`, say).
- **unknown**: every other name. The merge request only says `Changed with: <manager>`, with no
  note either way, because a wrong "not a person" would mislead the reviewer.

The metric `backflow_drifts_detected_total` counts the class in `changed_by` (`interactive`, `controller`,
`unknown`), and `none` for a drift for which no manager could be named.

What it cannot tell:

- **No username, without the actor webhook.** `managedFields` do not hold one, so `spec.actor`
  is not set, "Changed by: unknown" stays in the merge request and nobody is assigned
  (`git.mergeRequest.assignActor`). A username needs the identity of the request, which the optional
  [actor webhook](#the-actor-webhook-who-it-really-was) records.
- **A removed field has no owner.** When the change is a field that is gone from the live object,
  there is no entry to read, and the proposal has no actor.
- **It is a hint, not proof.** The name is set by the client (`--field-manager`, or the user agent): it can
  be anything, and anyone who can write to the object can pick any name, or the name of a controller. Read
  "Changed with" as where to look first. Who it really was needs the identity of the request, which the
  [actor webhook](#the-actor-webhook-who-it-really-was) records.
- **It is set when the proposal is created** and never changes (spec is immutable): a later change
  of the same field by someone else shows in a new proposal.
- **Nothing blocks on it.** An object without `managedFields`, or with ones that cannot be read, gives a
  proposal without an actor (logged at V(1)), counted as `changed_by="none"`.

## The actor webhook: who it really was

Off by default. With `actorWebhook.enabled=true` the chart installs an **observe-only admission
webhook** and the operator names the user of a change in `spec.actor` (`username`, `groups`, `kind`).
The apiserver sends it the request, including the identity it authenticated; the webhook
**always answers "allowed"**, keeps the paths of the fields that changed (never a value), and the
operator pairs that with the drift when it makes the proposal.

```bash
helm upgrade --install backflow oci://ghcr.io/sserkanml/charts/backflow -n backflow-system \
  --set actorWebhook.enabled=true \
  --set 'actorWebhook.networkPolicy.apiserverCIDRs={10.0.0.4/32}'   # not on OpenShift
```

```yaml
spec:
  actor:
    username: alice            # from the webhook, only when a record is confirmed (below)
    groups: [devs, system:authenticated]
    kind: person               # or controller (a service account)
    manager: kubectl-patch     # from managedFields, as before
    operation: Update
    time: "2026-11-01T13:26:26Z"
```

**It cannot block or slow the cluster.** The webhook is `UPDATE` only, `failurePolicy: Ignore`,
`sideEffects: None`, `timeoutSeconds: 1`, and its handler does no I/O: it parses the review, keeps the
changed paths in memory and answers. What reaches it is narrowed by the apiserver first
(`matchConditions`, Kubernetes 1.28 or later; the chart refuses older ones unless
`actorWebhook.allowUnfiltered=true`): only updates of resources Argo CD tracks (the
`argocd.argoproj.io/tracking-id` annotation or the `app.kubernetes.io/instance` label) and `scale`;
never Secrets, dry runs, status writes, the controllers of `kube-system`, nodes, the controller manager,
the Argo CD application controller (`actorWebhook.excludeUsers`) or Backflow itself. In a busy namespace
that was 9 calls out of 215 in the spike. A webhook that is down or unreachable costs nothing; one that is
*up but hung* costs the timeout (1 s) on each matched request, which is why the timeout is short and the
match narrow. The numbers and the evidence are in
[docs/design/actor-webhook.md](design/actor-webhook.md).

**Where the records are kept.** Each replica serves the webhook (the Service reaches any ready pod) and
flushes what it saw every 2 seconds to `ChangeRecord` objects in the operator's namespace: one per
(object uid, user, field manager, subresource), the changed paths merged. They hold user names, so they are
for administrators, not for tenants (no tenant role is created for them; the operator has a Role in its own
namespace). The leader deletes them after `actorWebhook.retention` (2 hours) and the oldest beyond
`actorWebhook.maxRecords` (5000), counted in `backflow_actor_records_dropped_total`.

**A record is believed only if the cluster agrees.** When a proposal is made, the operator takes the
records of the object (by uid) and keeps one only if the live `managedFields` agree: the same field
manager, an entry written within `actorWebhook.tolerance` (10 s) of the record, and an entry that owns
the drifted path. A review nobody made, or one that names someone else, fails that and sets no name
(`backflow_actor_matches_total{result="manager_mismatch"|"time_mismatch"|"path_mismatch"}`). Without a
matching record the actor is what managedFields say, as without the webhook. The webhook never delays or
fails a proposal.

**Who can call it.** The webhook cannot tell who calls it (the apiserver does not authenticate to
admission webhooks unless the control plane is configured for it, which a chart cannot do). Three layers
instead: a NetworkPolicy that admits the API server only (on OpenShift the `host-network` policy group,
elsewhere `actorWebhook.networkPolicy.apiserverCIDRs`, which is required; a CNI that does not enforce
NetworkPolicy gives no protection); a random URL path, kept across upgrades; and the cross-check above,
which makes a forged record useless unless the forger also knows a real change's manager and time.
**Without the network policy any pod that can reach the port can post a forged review**, and could put a
different user name on a real change.

**TLS** (`actorWebhook.tls.mode`): `auto` uses the OpenShift service CA on OpenShift, else cert-manager if
its API is installed, else a certificate made by Helm (five years, kept across upgrades,
`actorWebhook.tls.renew=true` to replace it); or name `serviceCA`, `certManager`, `helm`, or `secret` (your
own `tls.secretName` and `tls.caBundle`). The webhook reads the certificate from files and picks up a
renewed one without a restart.

**Privacy.** User names are personal data.

- `actorWebhook.recordUsername=false` stores only `kind` (`person`, or `controller` for a service account):
  no name, no groups, in the records or in the proposal.
- `actorWebhook.usernameInMergeRequest` (default `false`) keeps the name out of the text of the merge
  request and the commit trailer, because Git keeps it for ever. The proposal still has it. A policy that
  sets `git.mergeRequest.assignActor` assigns the merge request to that user regardless, since that is what
  the option asks for.
- Names are never labels of a metric and are not logged at the default level. The records live two hours.

**What to watch.** The webhook fails open, so a broken one is silent. Alert on the apiserver's own metrics:

- `apiserver_admission_webhook_fail_open_count{name="<release>-backflow-controller-manager-actor-webhook"}`
  rising: the apiserver could not reach the webhook or it was too slow, and changes were not recorded.
- `apiserver_admission_match_condition_evaluation_errors_total` above zero for it: **a match condition
  failed to evaluate, which under `failurePolicy: Ignore` means the webhook is not called at all**. The
  chart's conditions are tested against sample requests (a field that is absent is not empty:
  `request.subResource == ''` is an error, `!has(request.subResource)` is not), but a custom
  `actorWebhook.excludeUsers` or namespace selector is yours to check.
- Backflow's own: `backflow_actor_webhook_calls_total` (`recorded`, `ignored`, `invalid`, `busy`),
  `backflow_actor_records_written_total`, `backflow_actor_records_dropped_total{reason}`,
  `backflow_actor_matches_total{result}`. No calls recorded while people patch tracked resources means the
  webhook is not reached.

**OpenShift.** On OpenShift 4.15 and later (Kubernetes 1.28, `matchConditions` in beta and on) the chart
uses the service CA (`service.beta.openshift.io/serving-cert-secret-name` on the Service and
`inject-cabundle` on the webhook configuration) and admits the API server through the
`policy-group.network.openshift.io/host-network` label; `matchConditions` is GA from 4.17. Add the
OpenShift GitOps application controller to `actorWebhook.excludeUsers`
(`system:serviceaccount:openshift-gitops:openshift-gitops-argocd-application-controller`). These OpenShift
details follow the documentation and were not run on a cluster; the kind spike and the chart tests were.

**What stays a limitation.**

- An admission request carries **no user agent** (the API server's own is all there is), so
  there is no user agent in the API (a field that was never filled was removed in v1beta1); the field manager is the hint for the tool.
- `kubectl scale` and `oc scale` go through the `scale` subresource. The request carries **no field
  manager**, and the managedFields entry the API server writes for it (manager from the user agent,
  `subresource: scale`) has **no time**, so a scale cannot be checked like other changes. A record without a
  manager is accepted only against such an entry: it has the record's subresource, belongs to the manager shown
  as the actor, owns the drifted path, and has the same object uid. When the entry has a time, the record must
  be within `actorWebhook.tolerance` of it, as usual. When it has none, **the history of the Application
  bounds the record instead**: only a record made after the last successful sync of the Application
  (`status.operationState.finishedAt` of a `Succeeded` operation, or the newest `status.history[].deployedAt`,
  whichever is later), which put the resource back to Git's state, and not after the proposal is made (plus the
  tolerance), is used. So a scale by Alice at 14:00 that Argo CD synced back at 14:05 cannot be attributed to a
  drift that appears at 15:00 when Bob's scale was not recorded. If no successful sync is known, nobody is named
  (`backflow_actor_matches_total{result="no_window"}`). If records of different users are in the window the
  result is `ambiguous` and nobody is named. The sync is the Application's last one, not necessarily one that
  touched the resource, so the window can start later than necessary; the error is always a name left out,
  never a wrong one. A record that has a manager keeps the strict check. This remains the weakest check of the
  feature: anyone who can post to the webhook port could name a user for a scale inside the window, which the
  NetworkPolicy is there to prevent. The Scale object has no annotations either, so a scale of any Deployment,
  StatefulSet or ReplicaSet in a watched namespace is recorded, tracked by Argo CD or not; only the records of a
  drifted, tracked object are ever read, and the others expire with the retention.
- A change made as a shared account (a CI token) is that account; impersonation shows the impersonated user.
- Changes made while the webhook, or the operator, is down are not named (fail open is the point).
- The records are in a pod's memory for up to the flush interval, so a crash can lose the last seconds.
- Kubernetes before 1.28 has no `matchConditions`; with `allowUnfiltered` every update of every namespaced
  resource reaches the webhook.
