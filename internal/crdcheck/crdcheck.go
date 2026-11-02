// Package crdcheck refuses to run the operator against CustomResourceDefinitions it cannot use:
// ones that do not have v1beta1, or whose objects are still stored as v1alpha1. Reading an object
// that is stored in an older version drops the fields the new schema does not have, so running on
// would lose data silently. `backflow migrate` rewrites the objects; this check only says so.
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
const Version = "v1beta1"

// Names are the CustomResourceDefinitions of the backflow.io group.
var Names = []string{
	"scmconnections.backflow.io",
	"backflowpolicies.backflow.io",
	"driftproposals.backflow.io",
}

// Check returns an error that says what to do when a Backflow CRD is not ready for this
// operator, nil when all are. A CRD that does not exist is not an error here: the controllers
// report that themselves.
func Check(ctx context.Context, c client.Reader) error {
	var problems []string
	for _, name := range Names {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &crd); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("cannot read the CustomResourceDefinition %s to check its versions: %w", name, err)
		}
		if p := problem(&crd); p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

func problem(crd *apiextensionsv1.CustomResourceDefinition) string {
	served := false
	for _, v := range crd.Spec.Versions {
		if v.Name == Version && v.Served && v.Storage {
			served = true
		}
	}
	if !served && slices.Contains(crd.Status.StoredVersions, "v1alpha1") {
		return fmt.Sprintf("%s is the CRD of an earlier release (objects are stored as v1alpha1): run `backflow migrate` with the "+
			"operator stopped before starting this version, and do not apply the new CRDs first (docs/upgrade-v1beta1.md)", crd.Name)
	}
	if !served {
		return fmt.Sprintf("%s does not serve and store %s: apply the CRDs of this release first "+
			"(helm show crds ... | kubectl apply --server-side --force-conflicts -f -)", crd.Name, Version)
	}
	if old := slices.DeleteFunc(slices.Clone(crd.Status.StoredVersions), func(v string) bool { return v == Version }); len(old) > 0 {
		return fmt.Sprintf("%s still has objects stored as %s: run `backflow migrate` (with the operator stopped) "+
			"before starting this version", crd.Name, strings.Join(old, ", "))
	}
	return ""
}
