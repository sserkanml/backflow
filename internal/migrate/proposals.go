package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultDeleteTimeout is how long the tool waits for a deleted proposal to be gone.
const defaultDeleteTimeout = 2 * time.Minute

// proposalItem is a proposal the tool has to look at: one that is still v1alpha1 (live), or one with a backup whose
// migration was not completed.
type proposalItem struct {
	namespace, name string
	live            obj // the v1alpha1 object in the cluster; nil if there is none
}

func (p *proposalItem) String() string { return "DriftProposal " + p.namespace + "/" + p.name }

// migrateProposals converts the DriftProposals. The order for each one is: backup (done before, for all objects) ->
// delete -> create -> status -> a file that says it is complete. A proposal cannot be updated: its spec is immutable and
// the old object has the old field names. The file is what a second run uses to tell an interrupted proposal from a
// finished one, so a crash at any point is recoverable from the backup. It reports whether every proposal is done.
func (r *runner) migrateProposals(ctx context.Context, needs bool, found []obj) bool {
	if !needs {
		// storedVersions has no v1alpha1: an earlier run finished the proposals, and what is left in a backup directory is
		// not an interrupted migration.
		return true
	}
	items := map[string]*proposalItem{}
	for _, o := range found {
		meta, _ := asMap(o["metadata"])
		it := &proposalItem{namespace: asString(meta["namespace"]), name: asString(meta["name"]), live: o}
		items[nameOf(o)] = it
	}
	if r.opt.BackupDir != "" {
		pending, err := Backup{Dir: r.opt.BackupDir}.Pending(KindProposal)
		if err != nil {
			r.printf("DriftProposal: cannot read the backup directory: %v", err)
			return false
		}
		for _, p := range pending {
			key := p[0] + "/" + p[1]
			if items[key] == nil {
				items[key] = &proposalItem{namespace: p[0], name: p[1]}
			}
		}
	}
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	ok := true
	for _, k := range keys {
		if !r.proposal(ctx, items[k]) {
			ok = false
			r.rep.Failed++
			r.rep.Proposals = append(r.rep.Proposals, k)
		}
	}
	return ok
}

func (r *runner) deleteTimeout() time.Duration {
	if r.opt.DeleteTimeout > 0 {
		return r.opt.DeleteTimeout
	}
	return defaultDeleteTimeout
}

// proposal migrates one proposal. It reports whether it is done.
func (r *runner) proposal(ctx context.Context, it *proposalItem) bool {
	b := Backup{Dir: r.opt.BackupDir}
	snapshot, recreated := it.live, false
	if snapshot == nil {
		// Nothing v1alpha1 is listed: either the proposal was created again by an earlier run, or it is gone.
		cur, err := r.get(ctx, OldVersion, "DriftProposal", it.namespace, it.name)
		if err != nil {
			r.printf("%s: FAILED, cannot read it: %v", it, err)
			return false
		}
		switch {
		case cur != nil && migrated(cur):
			recreated = true
		case cur != nil:
			it.live, snapshot = cur, cur
		}
	}
	if snapshot == nil {
		o, err := b.Load(KindProposal, it.namespace, it.name)
		if err != nil {
			r.printf("%s: FAILED, there is no object and no readable backup to recreate it from: %v", it, err)
			return false
		}
		snapshot = o
	}
	st, _ := asMap(snapshot["status"])
	phase := asString(st["phase"])

	if r.opt.SkipFinished && FinishedPhase(phase) && !recreated {
		return r.dropFinished(ctx, it, phase)
	}
	spec, _ := asMap(snapshot["spec"])
	policy, err := r.policyUID(ctx, it.namespace, asString(spec["policyName"]))
	if err != nil {
		r.printf("%s: FAILED, not changed: %v", it, err)
		return false
	}
	res, err := ConvertDriftProposal(snapshot, policy)
	if err != nil {
		r.printf("%s: FAILED, not converted (and not deleted): %v", it, err)
		return false
	}
	if !recreated {
		// The old object is deleted next. Before that the API server is asked whether it would take the new one.
		if err := r.validate(ctx, res.Object); err != nil {
			r.printf("%s: FAILED, the API server would refuse the %s object, so the proposal is not deleted: %v", it, NewVersion, err)
			r.keepRejectedProposal(res)
			return false
		}
	}
	if r.opt.DryRun {
		r.rep.Planned++
		how := "deleted and created again"
		switch {
		case recreated:
			how = "given its status again"
		case it.live == nil:
			how = "created again from its backup"
		}
		r.printf("%s (phase %s): would be %s as %s:", it, phase, how, NewVersion)
		for _, n := range res.Notes {
			r.printf("    %s", n)
		}
		return true
	}

	if !recreated {
		if it.live != nil {
			if err := r.deleteOld(ctx, it); err != nil {
				r.printf("%s: FAILED, %v", it, err)
				return false
			}
		} else {
			r.rep.ProposalsRestored++
			r.printf("%s: gone, created again from its backup", it)
		}
		if err := r.create(ctx, res.Object); err != nil {
			r.printf("%s: FAILED, deleted and not created (run again: it is created from its backup): %v", it, err)
			r.keepRejectedProposal(res)
			return false
		}
	}
	if err := r.restoreStatus(ctx, it, res); err != nil {
		r.printf("%s: FAILED, created without its status (run again to give it): %v", it, err)
		r.keepRejectedProposal(res)
		return false
	}
	if err := b.MarkDone(KindProposal, it.namespace, it.name); err != nil {
		r.printf("%s: migrated, but the file that says so could not be written (a second run does it again, harmlessly): %v", it, err)
	}
	r.rep.Converted++
	r.printf("%s (phase %s): migrated to %s", it, phase, NewVersion)
	return true
}

// dropFinished backs up (already done) and deletes a finished proposal, for --skip-finished.
func (r *runner) dropFinished(ctx context.Context, it *proposalItem, phase string) bool {
	if r.opt.DryRun {
		r.rep.Planned++
		r.printf("%s (phase %s): would be deleted, not migrated (--skip-finished); its backup is kept", it, phase)
		return true
	}
	if it.live != nil {
		if err := r.deleteOld(ctx, it); err != nil {
			r.printf("%s: FAILED, %v", it, err)
			return false
		}
	}
	if err := (Backup{Dir: r.opt.BackupDir}).MarkDone(KindProposal, it.namespace, it.name); err != nil {
		r.printf("%s: deleted, but the file that says so could not be written: %v", it, err)
	}
	r.rep.ProposalsDropped++
	r.printf("%s (phase %s): deleted, not migrated (--skip-finished); its backup is kept", it, phase)
	return true
}

// keepRejectedProposal writes the v1beta1 form of a proposal, status included, that the API server refused.
func (r *runner) keepRejectedProposal(res *Result) {
	object := copyJSON(res.Object)
	if res.Status != nil {
		object["status"] = copyJSON(res.Status)
	}
	if path, ok := r.writeRejected(KindProposal, asString(metaOf(object)["namespace"]), asString(metaOf(object)["name"]), object); ok {
		r.printf("    what was tried is in %s", path)
	}
	r.printf("    v1beta1 does not take this proposal as it is. A finished one can be dropped (--skip-finished, or kubectl delete it;")
	r.printf("    its backup stays); an open one is better closed in Git first. The backup is the v1alpha1 object.")
}

func metaOf(o obj) obj { m, _ := asMap(o["metadata"]); return m }

// copyJSON copies an object through JSON, which is what the API client does with it anyway.
func copyJSON(o obj) obj {
	data, _ := json.Marshal(o)
	out := obj{}
	_ = json.Unmarshal(data, &out)
	return out
}

// get reads an object of the group through the endpoint of a version. It returns nil if there is none.
func (r *runner) get(ctx context.Context, version, kind, namespace, name string) (obj, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.gvk(version, kind))
	if err := r.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return u.Object, nil
}

// policyUID is the uid of the policy that owns a proposal. The policy is converted in place, so it is the same as before;
// a proposal whose policy is gone is not touched (the garbage collector removes it, and the tool must not make a copy).
func (r *runner) policyUID(ctx context.Context, namespace, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("spec.policyName is empty")
	}
	p, err := r.get(ctx, NewVersion, string(KindPolicy), namespace, name)
	if err != nil {
		return "", fmt.Errorf("cannot read its policy %s/%s: %w", namespace, name, err)
	}
	if p == nil {
		return "", fmt.Errorf("its policy %s/%s does not exist", namespace, name)
	}
	return metaString(p, "uid"), nil
}

// validate asks the API server, without storing anything, whether it would accept the object.
func (r *runner) validate(ctx context.Context, object obj) error {
	probe := copyJSON(object)
	meta := metaOf(probe)
	delete(meta, "name")
	meta["generateName"] = "migrate-check-"
	return r.c.Create(ctx, &unstructured.Unstructured{Object: probe}, client.DryRunAll)
}

func (r *runner) create(ctx context.Context, object obj) error {
	return r.c.Create(ctx, &unstructured.Unstructured{Object: copyJSON(object)})
}

// deleteOld deletes the v1alpha1 proposal and waits until it is gone: a finalizer would keep it, and a create with the
// same name would fail until then.
func (r *runner) deleteOld(ctx context.Context, it *proposalItem) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.gvk(OldVersion, "DriftProposal"))
	u.SetNamespace(it.namespace)
	u.SetName(it.name)
	uid := types.UID(metaString(it.live, "uid"))
	if err := r.c.Delete(ctx, u, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("cannot delete it: %w", err)
	}
	deadline := time.Now().Add(r.deleteTimeout())
	for {
		cur, err := r.get(ctx, OldVersion, "DriftProposal", it.namespace, it.name)
		if err != nil {
			return fmt.Errorf("cannot see whether it is gone: %w", err)
		}
		if cur == nil {
			return nil
		}
		if time.Now().After(deadline) {
			meta, _ := asMap(cur["metadata"])
			return fmt.Errorf("deleted, but still there after %s (finalizers %v): remove them or wait for the object to go, "+
				"then run again; it is deleted again and created from the backup", r.deleteTimeout(), meta["finalizers"])
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// restoreStatus writes the status of a created proposal through the status subresource. It overwrites what is there, so
// doing it twice is harmless.
func (r *runner) restoreStatus(ctx context.Context, it *proposalItem, res *Result) error {
	if res.Status == nil {
		return nil
	}
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(r.gvk(NewVersion, "DriftProposal"))
	if err := r.c.Get(ctx, types.NamespacedName{Namespace: it.namespace, Name: it.name}, cur); err != nil {
		return err
	}
	cur.Object["status"] = copyJSON(res.Status)
	return r.c.Status().Update(ctx, cur)
}
