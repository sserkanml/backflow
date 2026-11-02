// Package crdcheck checks the CustomResourceDefinitions before and while the operator runs.
//
// It refuses to run against CRDs it cannot use: ones that do not serve and store v1, or whose objects are still stored as
// v1alpha1 (reading such an object drops the fields the new schema does not have, so running on would lose data silently;
// `backflow migrate` rewrites them). Objects stored as v1beta1 are not a reason to stop: v1 has the same schema and is served
// next to v1beta1 without conversion, so the operator reads and writes them; it only says that `backflow migrate` has not
// run yet (a warning in the log and the metric backflow_storage_migration_pending), because a CRD whose storedVersions
// still names v1beta1 cannot drop it, and v1beta1 cannot be removed in 2.0 until it is gone.
package crdcheck

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Version is the version the operator reads and writes.
const Version = "v1"

// Deprecated is the version that is served next to Version, with the same schema; objects may still be stored in it.
const Deprecated = "v1beta1"

// Names are the CustomResourceDefinitions of the backflow.io group.
var Names = []string{
	"scmconnections.backflow.io",
	"backflowpolicies.backflow.io",
	"driftproposals.backflow.io",
}

// Result is what a check found that does not stop the operator.
type Result struct {
	// Pending are the CRDs whose storedVersions still names v1beta1: backflow migrate has not run (or not finished).
	Pending []string
}

// Check returns an error that says what to do when a Backflow CRD is not ready for this
// operator, and the CRDs that still hold objects stored as v1beta1. A CRD that does not exist is not an error here: the
// controllers report that themselves.
func Check(ctx context.Context, c client.Reader) (Result, error) {
	var problems []string
	var res Result
	for _, name := range Names {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &crd); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return res, fmt.Errorf("cannot read the CustomResourceDefinition %s to check its versions: %w", name, err)
		}
		if p := problem(&crd); p != "" {
			problems = append(problems, p)
			continue
		}
		if slices.Contains(crd.Status.StoredVersions, Deprecated) {
			res.Pending = append(res.Pending, name)
		}
	}
	if len(problems) == 0 {
		return res, nil
	}
	return res, fmt.Errorf("%s", strings.Join(problems, "; "))
}

func problem(crd *apiextensionsv1.CustomResourceDefinition) string {
	served := false
	has := map[string]bool{}
	for _, v := range crd.Spec.Versions {
		has[v.Name] = true
		if v.Name == Version && v.Served && v.Storage {
			served = true
		}
	}
	switch {
	case !served && slices.Contains(crd.Status.StoredVersions, "v1alpha1"):
		return fmt.Sprintf("%s is the CRD of an earlier release (objects are stored as v1alpha1): run `backflow migrate` with the "+
			"operator stopped before starting this version, and do not apply the new CRDs first (docs/upgrade-1.0.md)", crd.Name)
	case !served && has[Deprecated]:
		return fmt.Sprintf("%s is the CRD of release 0.18 (it has v1beta1 but not v1): apply the CRDs of this release "+
			"(helm show crds ... | kubectl apply --server-side --force-conflicts -f -), or run `backflow migrate`, which does "+
			"(docs/upgrade-1.0.md)", crd.Name)
	case !served:
		return fmt.Sprintf("%s does not serve and store %s: apply the CRDs of this release first "+
			"(helm show crds ... | kubectl apply --server-side --force-conflicts -f -)", crd.Name, Version)
	}
	if old := slices.DeleteFunc(slices.Clone(crd.Status.StoredVersions), func(v string) bool {
		return v == Version || v == Deprecated
	}); len(old) > 0 {
		return fmt.Sprintf("%s still has objects stored as %s: run `backflow migrate` (with the operator stopped) "+
			"before starting this version", crd.Name, strings.Join(old, ", "))
	}
	return ""
}
