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
	// BackupDir receives a file per object before anything is changed. Required unless DryRun.
	BackupDir string
	// DryRun prints what would change and changes nothing.
	DryRun bool
	// ServeV1alpha1 lets the tool serve v1alpha1 of a CRD while it reads and rewrites the objects, and put it back after.
	// Without it the CRDs must serve v1alpha1 already.
	ServeV1alpha1 bool
	// OperatorNamespace limits the search for a running operator to a namespace; empty looks everywhere.
	OperatorNamespace string
	// Out receives the report.
	Out io.Writer
	// Now defaults to time.Now.
	Now func() time.Time
}

// Report is what a run did.
type Report struct {
	// Converted, Skipped (already v1beta1) and Failed count the objects of ScmConnection and BackflowPolicy. Planned is
	// what a dry run would convert.
	Converted, Planned, Skipped, Failed int
	// BackedUp and KeptBackups count the files written and the ones of an earlier run that were left.
	BackedUp, KeptBackups int
	// ChangeRecordsDropped counts the ChangeRecords deleted.
	ChangeRecordsDropped int
	// Proposals are the v1alpha1 DriftProposals that remain, as namespace/name.
	Proposals []string
	// Finalized are the CRDs whose storedVersions was set to [v1beta1].
	Finalized []string
}

// Complete reports whether nothing is left to do.
func (r *Report) Complete() bool { return r.Failed == 0 && len(r.Proposals) == 0 }

type crdInfo struct {
	name, kind string
	crd        *apiextensionsv1.CustomResourceDefinition
	// needs: objects of this CRD may still be stored as v1alpha1.
	needs bool
	// served: the tool put v1alpha1 in service and has to take it out.
	served bool
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

// RejectedDir, in the backup directory, holds the v1beta1 objects the API server refused.
const RejectedDir = "rejected"

type runner struct {
	c   client.Client
	opt Options
	rep *Report
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
	if !opt.DryRun && opt.BackupDir == "" {
		return nil, errors.New("--backup-dir is required: a backup of every object is written before anything is changed")
	}
	r := &runner{c: c, opt: opt, rep: &Report{}}
	if opt.DryRun {
		r.printf("DRY RUN: nothing is changed and no file is written")
	}
	if err := CheckOperatorStopped(ctx, c, opt.OperatorNamespace, opt.Now()); err != nil {
		return nil, err
	}

	infos, err := r.inspectCRDs(ctx)
	if err != nil {
		return r.rep, err
	}
	defer r.unserve(context.WithoutCancel(ctx), infos)
	if err := r.serve(ctx, infos); err != nil {
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

	failed := map[ObjectKind]bool{}
	for _, k := range []ObjectKind{KindScmConnection, KindPolicy} {
		for _, o := range found[k] {
			if !r.convert(ctx, k, o) {
				failed[k] = true
			}
		}
	}
	r.dropChangeRecords(ctx)
	r.listProposals(found[KindProposal])
	return r.rep, r.finalize(ctx, infos, failed)
}

// migrated reports whether an object is already in v1beta1: the tool marked it, or it was made through v1beta1.
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
		if asString(m["apiVersion"]) != newAPIVersion {
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
		if !info.needs {
			r.printf("%s: no object is stored as %s (storedVersions %v), nothing to do", k.kind, OldVersion, crd.Status.StoredVersions)
			infos = append(infos, info)
			continue
		}
		if !servesAndStores(crd, NewVersion) {
			return nil, fmt.Errorf("the CRD %s does not serve and store %s: apply the CRDs of this release first "+
				"(helm show crds ... | kubectl apply --server-side --force-conflicts -f -)", k.crd, NewVersion)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func servesAndStores(crd *apiextensionsv1.CustomResourceDefinition, version string) bool {
	for _, v := range crd.Spec.Versions {
		if v.Name == version {
			return v.Served && v.Storage
		}
	}
	return false
}

func versionServed(crd *apiextensionsv1.CustomResourceDefinition, version string) (index int, served bool) {
	for i, v := range crd.Spec.Versions {
		if v.Name == version {
			return i, v.Served
		}
	}
	return -1, false
}

// serve puts v1alpha1 in service for the CRDs that need it, when the tool may.
func (r *runner) serve(ctx context.Context, infos []*crdInfo) error {
	for _, info := range infos {
		if !info.needs {
			continue
		}
		i, served := versionServed(info.crd, OldVersion)
		if served {
			continue
		}
		switch {
		case i < 0:
			return fmt.Errorf("the CRD %s has no %s: the objects stored in it cannot be read, so they cannot be migrated", info.name, OldVersion)
		case r.opt.DryRun:
			return fmt.Errorf("the CRD %s does not serve %s, so its objects cannot be listed: serve it "+
				"(kubectl patch crd %s --type=json -p '[{\"op\":\"replace\",\"path\":\"/spec/versions/%d/served\",\"value\":true}]') "+
				"or run without --dry-run and with --serve-v1alpha1", info.name, OldVersion, info.name, i)
		case !r.opt.ServeV1alpha1:
			return fmt.Errorf("the CRD %s does not serve %s: serve it "+
				"(kubectl patch crd %s --type=json -p '[{\"op\":\"replace\",\"path\":\"/spec/versions/%d/served\",\"value\":true}]') "+
				"and put it back afterwards, or run again with --serve-v1alpha1, which does both", info.name, OldVersion, info.name, i)
		}
		info.crd.Spec.Versions[i].Served = true
		if err := r.c.Update(ctx, info.crd); err != nil {
			return fmt.Errorf("cannot serve %s in %s: %w", OldVersion, info.name, err)
		}
		info.served = true
		r.printf("%s: %s is served while the objects are migrated, and is put back after", info.kind, OldVersion)
	}
	return nil
}

// unserve takes v1alpha1 out of service again where the tool put it in.
func (r *runner) unserve(ctx context.Context, infos []*crdInfo) {
	for _, info := range infos {
		if !info.served {
			continue
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := r.c.Get(ctx, types.NamespacedName{Name: info.name}, crd); err != nil {
			r.printf("WARNING: cannot read %s to stop serving %s: %v", info.name, OldVersion, err)
			continue
		}
		if i, _ := versionServed(crd, OldVersion); i >= 0 {
			crd.Spec.Versions[i].Served = false
		}
		if err := r.c.Update(ctx, crd); err != nil {
			r.printf("WARNING: %s is still served in %s; put it back with kubectl patch crd %s: %v", OldVersion, info.name, info.name, err)
			continue
		}
		r.printf("%s: %s is not served any more", info.kind, OldVersion)
	}
}

func (r *runner) gvk(version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Group, Version: version, Kind: kind}
}

// list lists the objects of a kind through the v1alpha1 endpoint, waiting for the endpoint to be there after a CRD
// was changed.
func (r *runner) list(ctx context.Context, kind string) ([]obj, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(r.gvk(OldVersion, kind+"List"))
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

// keepRejected writes the v1beta1 object that the API server refused, so that a person can correct it and apply it with
// kubectl replace. The object itself must not be edited through v1alpha1: once the CRD stores v1beta1, a write through
// v1alpha1 is pruned to the v1beta1 schema and drops the fields v1beta1 does not have.
func (r *runner) keepRejected(kind ObjectKind, meta, object obj) {
	path := Backup{Dir: filepath.Join(r.opt.BackupDir, RejectedDir)}.Path(kind, asString(meta["namespace"]), asString(meta["name"]))
	path = strings.TrimSuffix(path, ".yaml") + "." + NewVersion + ".yaml"
	data, err := yaml.Marshal(object)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = os.WriteFile(path, data, 0o600)
	}
	if err != nil {
		r.printf("    the %s object could not be written for you to correct: %v", NewVersion, err)
		return
	}
	r.printf("    correct %s and apply it with: kubectl replace -f %s", path, path)
	r.printf("    do NOT edit the object itself: a write through %s is pruned to the %s schema and loses its %s fields", OldVersion, NewVersion, OldVersion)
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

// listProposals names the DriftProposals that are still v1alpha1. They are not migrated by this version.
func (r *runner) listProposals(items []obj) {
	for _, o := range items {
		r.rep.Proposals = append(r.rep.Proposals, nameOf(o))
	}
	if len(items) == 0 {
		return
	}
	r.printf("DriftProposal: %d object(s) are still %s; they are backed up and not changed (the migration of proposals is not part of this version):", len(items), OldVersion)
	for _, o := range items {
		st, _ := asMap(o["status"])
		r.printf("    %s (phase %s)", nameOf(o), asString(st["phase"]))
	}
}

// finalize sets storedVersions of a CRD to [v1beta1] when every object of it is rewritten.
func (r *runner) finalize(ctx context.Context, infos []*crdInfo, failed map[ObjectKind]bool) error {
	var errs []error
	for _, info := range infos {
		if !info.needs {
			continue
		}
		kind := ObjectKind(info.kind)
		switch {
		case failed[kind]:
			r.printf("%s: storedVersions of %s is not changed: some objects could not be converted", kind, info.name)
			continue
		case kind == KindProposal && len(r.rep.Proposals) > 0:
			r.printf("%s: storedVersions of %s is not changed: %d object(s) are still %s", kind, info.name, len(r.rep.Proposals), OldVersion)
			continue
		}
		if r.opt.DryRun {
			r.printf("%s: storedVersions of %s would be set to [%s]", kind, info.name, NewVersion)
			continue
		}
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
