package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// serverFields are the parts of the metadata of a backed-up object that the API server owns and sets again.
var serverFields = []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink", "deletionTimestamp", "deletionGracePeriodSeconds"}

// RestoreReport counts what a restore did.
type RestoreReport struct {
	Created, Existing, Failed int
}

// Restore creates the objects of a backup again, as backflow.io/v1alpha1, for a rollback to the last release: the CRDs of that
// release must be installed (v1alpha1 served and stored) and the operator stopped. An object that exists is left alone. The
// owner reference of a proposal gets the uid its policy has now.
func Restore(ctx context.Context, c client.Client, dir string, out io.Writer) (*RestoreReport, error) {
	rep := &RestoreReport{}
	printf := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	if dir == "" {
		return rep, errors.New("--backup-dir is required")
	}
	if err := CheckOperatorStopped(ctx, c, "", time.Now()); err != nil {
		return rep, err
	}
	r := &runner{c: c, opt: Options{}, rep: &Report{}}
	for _, kind := range []ObjectKind{KindScmConnection, KindPolicy, KindProposal} {
		crdName := ""
		for _, k := range kinds {
			if k.kind == kind {
				crdName = k.crd
			}
		}
		if err := checkLastRelease(ctx, c, crdName); err != nil {
			return rep, err
		}
		entries, err := os.ReadDir(filepath.Join(dir, string(kind)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return rep, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
				names = append(names, e.Name())
			}
		}
		slices.Sort(names)
		for _, name := range names {
			data, err := os.ReadFile(filepath.Join(dir, string(kind), name))
			if err != nil {
				return rep, err
			}
			var o obj
			if err := yaml.Unmarshal(data, &o); err != nil {
				rep.Failed++
				printf("%s %s: cannot read the backup: %v", kind, name, err)
				continue
			}
			r.restoreOne(ctx, kind, o, rep, printf)
		}
	}
	return rep, nil
}

func checkLastRelease(ctx context.Context, c client.Reader, crdName string) error {
	crd, err := getCRD(ctx, c, crdName)
	if err != nil {
		return fmt.Errorf("the CRD %s must be installed, as the release you roll back to has it (helm show crds ... | kubectl apply -f -): %w", crdName, err)
	}
	if !servesAndStores(crd, OldVersion) {
		return fmt.Errorf("the CRD %s does not serve and store %s: install the CRDs of the release you roll back to", crdName, OldVersion)
	}
	return nil
}

func (r *runner) restoreOne(ctx context.Context, kind ObjectKind, o obj, rep *RestoreReport, printf func(string, ...any)) {
	meta, _ := asMap(o[keyMetadata])
	meta = maps.Clone(meta)
	for _, f := range serverFields {
		delete(meta, f)
	}
	name := asString(meta["namespace"]) + "/" + asString(meta[keyName])
	if refs, ok := asList(meta["ownerReferences"]); ok {
		fixed := make([]any, 0, len(refs))
		for _, ref := range refs {
			m, _ := asMap(ref)
			m = maps.Clone(m)
			if asString(m[keyKind]) == string(KindPolicy) {
				if p, err := r.get(ctx, OldVersion, string(KindPolicy), asString(meta["namespace"]), asString(m[keyName])); err == nil && p != nil {
					m["uid"] = metaString(p, "uid")
				}
			}
			fixed = append(fixed, m)
		}
		meta["ownerReferences"] = fixed
	}
	status := o[keyStatus]
	object := obj{keyAPIVersion: oldAPIVersion, keyKind: string(kind), keyMetadata: meta, keySpec: o[keySpec]}
	u := &unstructured.Unstructured{Object: object}
	if err := r.c.Create(ctx, u); err != nil {
		if apierrors.IsAlreadyExists(err) {
			rep.Existing++
			printf("%s %s: exists, left as it is", kind, name)
			return
		}
		rep.Failed++
		printf("%s %s: FAILED: %v", kind, name, err)
		return
	}
	if status != nil {
		u.Object[keyStatus] = status
		if err := r.c.Status().Update(ctx, u); err != nil {
			rep.Failed++
			printf("%s %s: created, but its status was refused: %v", kind, name, err)
			return
		}
	}
	rep.Created++
	printf("%s %s: restored", kind, name)
}

// restoreUsage is the help of `migrate restore`.
const restoreUsage = `usage: backflow migrate restore --backup-dir DIR

Creates the objects of a backup again, as backflow.io/v1alpha1, for a rollback to the release before v1. The CRDs of that
release must be installed (helm show crds ... --version <old> | kubectl apply -f -; delete the v1 CRDs first) and the
operator of the release you roll back from must be stopped. An object that exists is left alone.

flags:
`

func getCRD(ctx context.Context, c client.Reader, name string) (*apiextensionsv1.CustomResourceDefinition, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	return crd, c.Get(ctx, types.NamespacedName{Name: name}, crd)
}
