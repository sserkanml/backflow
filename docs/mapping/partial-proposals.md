# Partial proposals

For the other sources see [What maps](overview.md).

By default a proposal is all or nothing: when one drifted field cannot be traced
back to Git, the whole proposal is `Unmapped`, even though the other fields
could be proposed. A policy can opt in to proposing what can be proven:

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  mapping:
    allowPartial: true        # default false
```

With it, a proposal with at least one mappable field is mapped for those fields
only (Directory and Helm sources alike). Every field that is left out is listed
in `status.mapping.unmapped` (`path`, `reason`, `message`), the `Mapped`
condition is `True` with reason `PartiallyMapped`, and its message names both
sets. `spec.changes` is unchanged and still lists every change. A proposal where
nothing can be mapped is `Unmapped` as before.

Nothing is guessed: the edits of the mapped fields are verified together, they
must change exactly those fields to their live values, and every left-out field
must render exactly as it does in Git. An edit that would also change a left-out
field is not accepted, and the field it was for is left out as well. If the
verification of the combined edit fails, the whole proposal is `Unmapped`.

A partial merge request (or direct commit) is marked "(partial)" in its title
and commit subject, and its body has a **Not included in this merge request**
table: path, live value, Git value and the reason, followed by a warning (see
below). The changes table and the diff list the mapped fields only. The same
rule applies when the proposal is mapped again at a moved head of the target
branch, and `DirectCommit` mode honours it too.

> [!WARNING]
> **The fields left out are lost at the next Argo CD sync.** A sync applies the
> whole manifest, so it resets every field of the resource to its Git value,
> including the live changes that could not be mapped. With automated sync that
> happens right after the merge request is merged; with manual sync, when
> someone syncs. Either way, to keep one of those changes, port it to Git by
> hand, in the merge request or before it is merged. The merge request body and
> the commit message say so, in the same words for both sync modes.

Lifecycle: once the merge request is merged, the proposal is `Merged` and
stays so. The left-out fields do not become a new proposal: with manual sync,
Argo CD compares against the merge commit but has not synced it, so Backflow
pauses detection for the Application (Git is ahead, a `GitAhead` Event says so)
and makes no proposal; when the Application is synced, the left-out fields are
reset and there is nothing left to propose. With automated sync the sync comes
first. A new proposal for those fields appears only if somebody changes them in
the cluster again after the sync.
