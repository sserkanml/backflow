package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// Options are what `backflow migrate` was asked.
type Options struct {
	// BackupDir receives a file per object before anything is changed. Required unless DryRun, when objects are stored
	// as v1alpha1 (they are converted); objects stored as v1beta1 are only stored again, which changes nothing in them.
	BackupDir string
	// DryRun prints what would change and changes nothing.
	DryRun bool
	// StopOperator scales the operator's Deployment to zero (remembering the replicas in an annotation) when there is
	// something to migrate, and waits until it is gone. Without it the tool refuses to run while the operator runs.
	StopOperator bool
	// RestoreOperator only scales the operator back to the replicas StopOperator remembered, and does nothing else.
	RestoreOperator bool
	// SkipFinished drops the proposals that are finished (Merged, Rejected, Reverted, Superseded, Failed) instead of
	// migrating them: they are backed up and deleted. They are history; the merge requests are not touched.
	SkipFinished bool
	// DeleteTimeout is how long to wait for a deleted proposal to be gone; default two minutes.
	DeleteTimeout time.Duration
	// OperatorNamespace limits the search for a running operator to a namespace; empty looks everywhere.
	OperatorNamespace string
	// OperatorSelector is the label selector of the operator's Deployment; default app.kubernetes.io/name=backflow.
	OperatorSelector string
	// Out receives the report.
	Out io.Writer
	// Now defaults to time.Now.
	Now func() time.Time
}

// Report is what a run did.
type Report struct {
	// Converted, Skipped (already v1) and Failed count the objects of the three kinds. Planned is what a dry run
	// would convert.
	Converted, Planned, Skipped, Failed int
	// BackedUp and KeptBackups count the files written and the ones of an earlier run that were left.
	BackedUp, KeptBackups int
	// Restored counts the objects stored as v1beta1 that were stored again as v1 (a no-op update).
	Restored int
	// ChangeRecordsDropped counts the ChangeRecords deleted.
	ChangeRecordsDropped int
	// ProposalsDropped counts the finished proposals deleted by --skip-finished; ProposalsRestored the ones created again
	// from their backup because an earlier run was interrupted after the delete.
	ProposalsDropped, ProposalsRestored int
	// Proposals are the DriftProposals that are not migrated, as namespace/name. Each is also counted in Failed.
	Proposals []string
	// Finalized are the CRDs whose storedVersions was set to [v1].
	Finalized []string
}

// Complete reports whether nothing is left to do.
func (r *Report) Complete() bool { return r.Failed == 0 && len(r.Proposals) == 0 }

type crdInfo struct {
	name, kind string
	crd        *apiextensionsv1.CustomResourceDefinition
	// needs: objects of this CRD may still be stored as v1alpha1.
	needs bool
	// restore: objects of this CRD may still be stored as v1beta1, which has the schema of v1: they only have to be
	// stored again.
	restore bool
	// ready: the CRD serves and stores v1 (the objects can be written as v1).
	ready bool
	// touched: the tool changed the versions of the CRD, and has to settle them: drop v1alpha1 when every object is
	// migrated, take it out of service when not.
	touched bool
}

var kinds = []struct {
	kind ObjectKind
	crd  string
}{
	{KindScmConnection, "scmconnections.backflow.io"},
	{KindPolicy, "backflowpolicies.backflow.io"},
	{KindProposal, "driftproposals.backflow.io"},
}

const changeRecordCRD = "changerecords.backflow.io"

// RejectedDir, in the backup directory, holds the v1 objects the API server refused.
const RejectedDir = "rejected"

type runner struct {
	c   client.Client
	opt Options
	rep *Report
	// ready says, for a kind, whether the CRD serves v1 (after the tool prepared it, when it did).
	ready map[ObjectKind]bool
}

func (r *runner) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.opt.Out, format+"\n", args...)
}

// Run migrates. It returns an error for a problem that stops the run (a running operator, CRDs that are not ready,
// the cluster); objects that cannot be converted are counted in the report and the run goes on with the others.
func Run(ctx context.Context, c client.Client, opt Options) (*Report, error) {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	r := &runner{c: c, opt: opt, rep: &Report{}, ready: map[ObjectKind]bool{}}
	if opt.DryRun {
		r.printf("DRY RUN: nothing is changed and no file is written")
	}
	if opt.RestoreOperator {
		return r.rep, r.restoreOperator(ctx)
	}

	infos, err := r.inspectCRDs(ctx)
	if err != nil {
		return r.rep, err
	}
	if !anyWork(infos) {
		// Nothing is stored as v1alpha1 or v1beta1: there is nothing to race, and the operator may be the new one, running.
		r.printf("nothing to migrate")
		return r.rep, nil
	}
	if err := r.operatorMayRun(ctx, infos); err != nil {
		return r.rep, err
	}
	defer r.settle(context.WithoutCancel(ctx), infos)
	// The objects are read and backed up before the CRDs get v1: the CRDs of the last release serve v1alpha1 already.
	if err := r.makeReadable(ctx, infos); err != nil {
		return r.rep, err
	}

	// Everything is read first, so that the backup is complete before anything is changed.
	found := map[ObjectKind][]obj{}
	for _, info := range infos {
		if !info.needs {
			continue
		}
		items, err := r.list(ctx, info.kind)
		if err != nil {
			return r.rep, err
		}
		for _, o := range items {
			if migrated(o) {
				r.rep.Skipped++
				continue
			}
			found[ObjectKind(info.kind)] = append(found[ObjectKind(info.kind)], o)
		}
	}
	if err := r.backup(found); err != nil {
		return r.rep, err
	}
	if err := r.prepare(ctx, infos); err != nil {
		return r.rep, err
	}

	failed := map[ObjectKind]bool{}
	for _, info := range infos {
		if info.restore && !r.restoreStored(ctx, info) {
			failed[ObjectKind(info.kind)] = true
		}
	}
	for _, k := range []ObjectKind{KindScmConnection, KindPolicy} {
		for _, o := range found[k] {
			if !r.convert(ctx, k, o) {
				failed[k] = true
			}
		}
	}
	if !r.migrateProposals(ctx, needsMigration(infos, KindProposal), found[KindProposal]) {
		failed[KindProposal] = true
	}
	if anyNeeds(infos) {
		r.dropChangeRecords(ctx)
	}
	err = r.finalize(ctx, infos, failed)
	if err == nil {
		err = r.ensureInternalCRD(ctx, infos)
	}
	return r.rep, err
}

// operatorMayRun checks that the operator does not race the migration. Objects of another schema are converted, and a
// proposal is deleted and created again: then the operator must be stopped (and a backup is written). Objects stored as
// v1beta1 are only stored again without a change, which is safe with the operator running.
func (r *runner) operatorMayRun(ctx context.Context, infos []*crdInfo) error {
	if !anyNeeds(infos) {
		r.printf("only objects stored as %s: they are stored again as %s without a change, which is safe with the operator running",
			BetaVersion, NewVersion)
		return nil
	}
	opt := r.opt
	if !opt.DryRun && opt.BackupDir == "" {
		return errors.New("--backup-dir is required: a backup of every object is written before anything is changed")
	}
	if opt.StopOperator && !opt.DryRun {
		if err := r.stopOperator(ctx); err != nil {
			return err
		}
	}
	return checkOperatorStopped(ctx, r.c, opt.OperatorNamespace, opt.OperatorSelector, opt.Now())
}

// needsMigration reports whether objects of the kind may still be stored as v1alpha1.
func needsMigration(infos []*crdInfo, kind ObjectKind) bool {
	return slices.ContainsFunc(infos, func(i *crdInfo) bool { return i.needs && i.kind == string(kind) })
}

// anyWork reports whether objects may be stored as v1alpha1 or v1beta1.
func anyWork(infos []*crdInfo) bool {
	return slices.ContainsFunc(infos, func(i *crdInfo) bool { return i.needs || i.restore })
}

// migrated reports whether an object is already in the schema of v1: the tool marked it, or it was made through v1beta1 or v1.
// Through the v1alpha1 endpoint the two look alike, so the apiVersion of the field managers is what tells.
func migrated(o obj) bool {
	meta, _ := asMap(o["metadata"])
	if ann, ok := asMap(meta["annotations"]); ok {
		if _, ok := ann[AnnotationMigrated]; ok {
			return true
		}
	}
	entries, _ := asList(meta["managedFields"])
	if len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		m, _ := asMap(e)
		if v := asString(m["apiVersion"]); v != newAPIVersion && v != betaAPIVersion {
			return false
		}
	}
	return true
}

func (r *runner) inspectCRDs(ctx context.Context) ([]*crdInfo, error) {
	var infos []*crdInfo
	for _, k := range kinds {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := r.c.Get(ctx, types.NamespacedName{Name: k.crd}, crd); err != nil {
			if apierrors.IsNotFound(err) {
				r.printf("%s: the CRD is not installed, nothing to do", k.kind)
				continue
			}
			return nil, fmt.Errorf("cannot read the CRD %s: %w", k.crd, err)
		}
		info := &crdInfo{name: k.crd, kind: string(k.kind), crd: crd, needs: slices.Contains(crd.Status.StoredVersions, OldVersion)}
		info.restore = slices.Contains(crd.Status.StoredVersions, BetaVersion)
		info.ready = servesAndStores(crd, NewVersion)
		r.ready[k.kind] = info.ready || (!info.needs && !info.restore)
		if !info.needs && !info.restore {
			r.printf("%s: no object is stored as %s or %s (storedVersions %v), nothing to do", k.kind, OldVersion, BetaVersion,
				crd.Status.StoredVersions)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func anyNeeds(infos []*crdInfo) bool {
	return slices.ContainsFunc(infos, func(i *crdInfo) bool { return i.needs })
}

func servesAndStores(crd *apiextensionsv1.CustomResourceDefinition, version string) bool {
	for _, v := range crd.Spec.Versions {
		if v.Name == version {
			return v.Served && v.Storage
		}
	}
	return false
}

// versionServed finds the v1alpha1 version of a CRD: its index (-1 if there is none) and whether it is served.
func versionServed(crd *apiextensionsv1.CustomResourceDefinition) (index int, served bool) {
	for i, v := range crd.Spec.Versions {
		if v.Name == OldVersion {
			return i, v.Served
		}
	}
	return -1, false
}

// makeReadable makes sure the objects stored as v1alpha1 can be listed through v1alpha1. The CRDs of the last release serve
// it; a CRD that an earlier run left with v1alpha1 out of service gets it back. A dry run changes nothing.
func (r *runner) makeReadable(ctx context.Context, infos []*crdInfo) error {
	for _, info := range infos {
		if !info.needs {
			continue
		}
		i, served := versionServed(info.crd)
		switch {
		case i < 0:
			return fmt.Errorf("the CRD %s has no %s: the objects stored in it cannot be read, so they cannot be migrated", info.name, OldVersion)
		case served:
			continue
		case r.opt.DryRun:
			return fmt.Errorf("the CRD %s does not serve %s, so its objects cannot be listed: a real run serves it for its length", info.name, OldVersion)
		}
		info.crd.Spec.Versions[i].Served = true
		if err := r.c.Update(ctx, info.crd); err != nil {
			return fmt.Errorf("cannot serve %s in %s: %w", OldVersion, info.name, err)
		}
		info.touched = true
		r.printf("%s: %s is served while the objects are migrated", info.kind, OldVersion)
		if err := r.waitServed(ctx, info.kind, OldVersion); err != nil {
			return err
		}
	}
	return nil
}

// prepare makes the CRDs of the kinds that have v1alpha1 objects able to hold both versions: v1 is served and stored
// (so what the tool writes is stored as v1) and v1alpha1 is served (so what is stored as v1alpha1 can be read). The
// CRDs of the last release have v1alpha1 only: the tool adds v1, from the CRD of this release, to what is installed,
// and keeps the v1alpha1 version as it is. It runs after the backup. A dry run only says what it would do.
func (r *runner) prepare(ctx context.Context, infos []*crdInfo) error {
	for _, info := range infos {
		if !info.needs && !info.restore {
			continue
		}
		if r.opt.DryRun {
			switch {
			case info.ready:
			case info.needs:
				r.printf("%s: the CRD would get %s (served and stored) next to %s and %s", info.kind, NewVersion, OldVersion, BetaVersion)
			default:
				r.printf("%s: the CRD would get %s (served and stored) next to %s", info.kind, NewVersion, BetaVersion)
			}
			continue
		}
		if !info.ready {
			if err := r.addNewVersion(ctx, info); err != nil {
				return err
			}
		}
		info.touched = true
		r.ready[ObjectKind(info.kind)] = true
		if err := r.waitServed(ctx, info.kind, NewVersion); err != nil {
			return err
		}
	}
	return nil
}

// addNewVersion replaces the CRD with the one of this release (v1 served and stored, v1beta1 served) plus, when objects are
// stored as v1alpha1, the v1alpha1 version that is installed, served and not stored (the only way to read them).
func (r *runner) addNewVersion(ctx context.Context, info *crdInfo) error {
	target, err := releaseCRD(info.name)
	if err != nil {
		return err
	}
	target.Spec.Versions = slices.DeleteFunc(target.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool {
		return v.Name == OldVersion
	})
	if info.needs {
		for i := range info.crd.Spec.Versions {
			if info.crd.Spec.Versions[i].Name == OldVersion {
				legacy := info.crd.Spec.Versions[i].DeepCopy()
				legacy.Served, legacy.Storage = true, false
				target.Spec.Versions = append(target.Spec.Versions, *legacy)
			}
		}
	}
	target.ObjectMeta = info.crd.ObjectMeta
	if err := r.c.Update(ctx, target); err != nil {
		return fmt.Errorf("cannot add %s to the CRD %s: %w", NewVersion, info.name, err)
	}
	info.crd = target
	info.touched = true
	r.printf("%s: the CRD now serves and stores %s, and serves %s while the objects are migrated", info.kind, NewVersion,
		strings.Join(servedVersionNames(target), ", "))
	return nil
}

func servedVersionNames(crd *apiextensionsv1.CustomResourceDefinition) []string {
	var out []string
	for _, v := range crd.Spec.Versions {
		if v.Served && !v.Storage {
			out = append(out, v.Name)
		}
	}
	return out
}

// restoreStored stores the objects of a kind that are stored as v1beta1 again, as v1: an update that changes nothing. The
// schemas are the same and the CRD has no conversion, so the object that is read is the object that is written, and the API
// server writes it in the storage version. A merge patch of {} does it without a resourceVersion to conflict on, which is
// why it is safe with the operator running: whatever the operator writes meanwhile is stored as v1 anyway. It reports
// whether every object was done.
func (r *runner) restoreStored(ctx context.Context, info *crdInfo) bool {
	version := NewVersion
	if !r.ready[ObjectKind(info.kind)] {
		version = BetaVersion // a dry run: the CRD has no v1 yet, and the objects are the same through v1beta1
	}
	items, err := r.listVersion(ctx, version, info.kind)
	if err != nil {
		r.printf("%s: FAILED, cannot list the objects: %v", info.kind, err)
		return false
	}
	if r.opt.DryRun {
		r.rep.Planned += len(items)
		r.printf("%s: %d object(s) would be stored again as %s (no change in them)", info.kind, len(items), NewVersion)
		return true
	}
	ok := true
	for _, o := range items {
		u := &unstructured.Unstructured{Object: obj{keyAPIVersion: newAPIVersion, keyKind: info.kind,
			keyMetadata: obj{keyName: metaString(o, keyName), keyNamespace: metaString(o, keyNamespace)}}}
		if err := r.c.Patch(ctx, u, client.RawPatch(types.MergePatchType, []byte("{}"))); client.IgnoreNotFound(err) != nil {
			r.printf("%s %s: FAILED, not stored again: %v", info.kind, nameOf(o), err)
			r.rep.Failed++
			ok = false
			continue
		}
		r.rep.Restored++
	}
	r.printf("%s: %d object(s) stored again as %s", info.kind, len(items), NewVersion)
	return ok
}

// waitServed waits for the endpoint of a version to answer after a CRD was changed.
func (r *runner) waitServed(ctx context.Context, kind, version string) error {
	_, err := r.listVersion(ctx, version, kind)
	return err
}

// settle puts the CRDs in their final state: a CRD with no object left as v1alpha1 loses the v1alpha1 version, and a CRD
// that still has some takes it out of service, so that nothing writes through it (a write through v1alpha1, once v1 is
// stored, is pruned to the v1 schema and loses fields).
func (r *runner) settle(ctx context.Context, infos []*crdInfo) {
	if r.opt.DryRun {
		return
	}
	for _, info := range infos {
		if !info.touched {
			continue
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := r.c.Get(ctx, types.NamespacedName{Name: info.name}, crd); err != nil {
			r.printf("WARNING: cannot read %s to settle its versions: %v", info.name, err)
			continue
		}
		done := len(crd.Status.StoredVersions) == 1 && crd.Status.StoredVersions[0] == NewVersion
		changed := false
		if i, served := versionServed(crd); i >= 0 {
			if done {
				crd.Spec.Versions = slices.Delete(crd.Spec.Versions, i, i+1)
				changed = true
			} else if served {
				crd.Spec.Versions[i].Served = false
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := r.c.Update(ctx, crd); err != nil {
			r.printf("WARNING: %s still serves %s; put it back with kubectl patch crd %s: %v", info.name, OldVersion, info.name, err)
			continue
		}
		if done {
			r.printf("%s: %s is removed from the CRD", info.kind, OldVersion)
		} else {
			r.printf("%s: %s is not served any more", info.kind, OldVersion)
		}
	}
}

// ensureInternalCRD creates the CRD of the ChangeRecords of this release (internal.backflow.io) when it is not there: the
// operator needs it, and the CRDs of the last release do not have it.
func (r *runner) ensureInternalCRD(ctx context.Context, infos []*crdInfo) error {
	if !anyNeeds(infos) {
		return nil
	}
	const name = "changerecords.internal.backflow.io"
	err := r.c.Get(ctx, types.NamespacedName{Name: name}, &apiextensionsv1.CustomResourceDefinition{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("cannot read the CRD %s: %w", name, err)
	}
	if r.opt.DryRun {
		r.printf("the CRD %s would be created", name)
		return nil
	}
	crd, err := releaseCRD(name)
	if err != nil {
		return err
	}
	crd.ResourceVersion = ""
	if err := r.c.Create(ctx, crd); err != nil {
		return fmt.Errorf("cannot create the CRD %s: %w", name, err)
	}
	r.printf("the CRD %s was created", name)
	return nil
}

func (r *runner) gvk(version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: version, Kind: kind}
}

// list lists the objects of a kind through the v1alpha1 endpoint, waiting for the endpoint to be there after a CRD
// was changed.
func (r *runner) list(ctx context.Context, kind string) ([]obj, error) {
	return r.listVersion(ctx, OldVersion, kind)
}

// listVersion lists the objects of a kind through the endpoint of a version.
func (r *runner) listVersion(ctx context.Context, version, kind string) ([]obj, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(r.gvk(version, kind+"List"))
		err := r.c.List(ctx, list)
		if err == nil {
			out := make([]obj, 0, len(list.Items))
			for i := range list.Items {
				out = append(out, list.Items[i].Object)
			}
			return out, nil
		}
		if (apierrors.IsNotFound(err) || strings.Contains(err.Error(), "no matches for kind")) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		return nil, fmt.Errorf("cannot list %s: %w", kind, err)
	}
}

func nameOf(o obj) string {
	meta, _ := asMap(o["metadata"])
	if ns := asString(meta["namespace"]); ns != "" {
		return ns + "/" + asString(meta["name"])
	}
	return asString(meta["name"])
}

// backup writes a file for every object that is still v1alpha1, before anything is changed.
func (r *runner) backup(found map[ObjectKind][]obj) error {
	b := Backup{Dir: r.opt.BackupDir}
	for _, k := range []ObjectKind{KindScmConnection, KindPolicy, KindProposal} {
		for _, o := range found[k] {
			if r.opt.DryRun {
				r.printf("%s %s: would be backed up to %s", k, nameOf(o), b.Path(k, metaString(o, "namespace"), metaString(o, "name")))
				continue
			}
			path, result, err := b.Save(k, o)
			if err != nil {
				return fmt.Errorf("the backup of %s %s failed, nothing was changed: %w", k, nameOf(o), err)
			}
			if result == BackupKept {
				r.rep.KeptBackups++
				r.printf("%s %s: the backup of an earlier run is kept (%s)", k, nameOf(o), path)
			} else {
				r.rep.BackedUp++
			}
		}
	}
	if !r.opt.DryRun {
		r.printf("backup: %d file(s) written, %d of an earlier run kept, in %s", r.rep.BackedUp, r.rep.KeptBackups, r.opt.BackupDir)
	}
	return nil
}

func metaString(o obj, key string) string {
	meta, _ := asMap(o["metadata"])
	return asString(meta[key])
}

// convert rewrites one object in place. It reports whether the object is done.
func (r *runner) convert(ctx context.Context, kind ObjectKind, old obj) bool {
	name := nameOf(old)
	res, err := Convert(kind, old)
	if err != nil {
		r.rep.Failed++
		r.printf("%s %s: FAILED, not converted: %v", kind, name, err)
		return false
	}
	if r.opt.DryRun {
		r.rep.Planned++
		r.printf("%s %s: would be converted to %s:", kind, name, NewVersion)
		for _, n := range res.Notes {
			r.printf("    %s", n)
		}
		return true
	}
	meta, _ := asMap(res.Object["metadata"])
	delete(meta, "managedFields")
	u := &unstructured.Unstructured{Object: res.Object}
	if err := r.c.Update(ctx, u); err != nil {
		r.rep.Failed++
		r.printf("%s %s: FAILED, the API server refused the %s object (it is unchanged): %v", kind, name, NewVersion, err)
		r.keepRejected(kind, meta, res.Object)
		return false
	}
	r.rep.Converted++
	if res.Status != nil {
		u.Object["status"] = res.Status
		if err := r.c.Status().Update(ctx, u); err != nil {
			r.printf("%s %s: converted; its status could not be kept (the operator fills it in): %v", kind, name, err)
			return true
		}
	}
	r.printf("%s %s: converted to %s (%d change(s))", kind, name, NewVersion, len(res.Notes))
	return true
}

// keepRejected writes the v1 object that the API server refused, so that a person can correct it and apply it with
// kubectl replace. The object itself must not be edited through v1alpha1: once the CRD stores v1, a write through
// v1alpha1 is pruned to the v1 schema and drops the fields v1 does not have.
func (r *runner) keepRejected(kind ObjectKind, meta, object obj) {
	path, ok := r.writeRejected(kind, asString(meta["namespace"]), asString(meta["name"]), object)
	if !ok {
		return
	}
	r.printf("    correct %s and apply it with: kubectl replace -f %s", path, path)
	r.printf("    do NOT edit the object itself: a write through %s is pruned to the %s schema and loses its %s fields", OldVersion, NewVersion, OldVersion)
}

// writeRejected writes a v1 object the API server refused, to <backup-dir>/rejected/<Kind>/<ns>.<name>.v1.yaml.
func (r *runner) writeRejected(kind ObjectKind, namespace, name string, object obj) (string, bool) {
	if r.opt.DryRun {
		return "", false
	}
	path := Backup{Dir: filepath.Join(r.opt.BackupDir, RejectedDir)}.Path(kind, namespace, name)
	path = strings.TrimSuffix(path, ".yaml") + "." + NewVersion + ".yaml"
	data, err := yaml.Marshal(object)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = os.WriteFile(path, data, 0o600)
	}
	if err != nil {
		r.printf("    the %s object could not be written for you to look at: %v", NewVersion, err)
		return "", false
	}
	return path, true
}

// dropChangeRecords deletes the ChangeRecords of the old CRD. They are an in-memory buffer written down, kept for two
// hours; the operator writes new ones in internal.backflow.io.
func (r *runner) dropChangeRecords(ctx context.Context) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.c.Get(ctx, types.NamespacedName{Name: changeRecordCRD}, crd); err != nil {
		if !apierrors.IsNotFound(err) {
			r.printf("ChangeRecord: cannot read the CRD %s: %v", changeRecordCRD, err)
		}
		return
	}
	items, err := r.list(ctx, string(KindChangeRecord))
	if err != nil {
		r.printf("ChangeRecord: %v", err)
		return
	}
	if r.opt.DryRun {
		r.printf("ChangeRecord: %d object(s) would be deleted, not migrated (they hold user names for two hours at most, "+
			"and are not backed up); delete the CRD %s afterwards", len(items), changeRecordCRD)
		return
	}
	for _, o := range items {
		u := &unstructured.Unstructured{Object: o}
		u.SetGroupVersionKind(r.gvk(OldVersion, string(KindChangeRecord)))
		if err := r.c.Delete(ctx, u); client.IgnoreNotFound(err) != nil {
			r.printf("ChangeRecord %s: not deleted: %v", nameOf(o), err)
			continue
		}
		r.rep.ChangeRecordsDropped++
	}
	r.printf("ChangeRecord: %d object(s) deleted, not migrated (they hold user names for two hours at most, and are not backed up); "+
		"delete the CRD %s afterwards: kubectl delete crd %s", r.rep.ChangeRecordsDropped, changeRecordCRD, changeRecordCRD)
}

// finalize sets storedVersions of a CRD to [v1] when every object of it is rewritten.
func (r *runner) finalize(ctx context.Context, infos []*crdInfo, failed map[ObjectKind]bool) error {
	var errs []error
	for _, info := range infos {
		if !info.needs && !info.restore {
			continue
		}
		kind := ObjectKind(info.kind)
		switch {
		case failed[kind]:
			r.printf("%s: storedVersions of %s is not changed: some objects could not be migrated", kind, info.name)
			continue
		}
		if r.opt.DryRun {
			r.printf("%s: storedVersions of %s would be set to [%s]", kind, info.name, NewVersion)
			continue
		}
		if info.needs {
			// Nothing may have been left or made in v1alpha1 in the meantime.
			left, err := r.list(ctx, info.kind)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if n := countUnmigrated(left); n > 0 {
				r.printf("%s: storedVersions of %s is not changed: %d object(s) are not converted yet", kind, info.name, n)
				continue
			}
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := r.c.Get(ctx, types.NamespacedName{Name: info.name}, crd); err != nil {
			errs = append(errs, err)
			continue
		}
		crd.Status.StoredVersions = []string{NewVersion}
		if err := r.c.Status().Update(ctx, crd); err != nil {
			errs = append(errs, fmt.Errorf("cannot set storedVersions of %s: %w", info.name, err))
			continue
		}
		r.rep.Finalized = append(r.rep.Finalized, info.name)
		r.printf("%s: storedVersions of %s is [%s]", kind, info.name, NewVersion)
	}
	return errors.Join(errs...)
}

func countUnmigrated(items []obj) int {
	n := 0
	for _, o := range items {
		if !migrated(o) {
			n++
		}
	}
	return n
}
