package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/kustomize"
)

// kustomizeSpecOf describes the Kustomize source of the Application as Argo CD
// sees it. Only what names files is taken: the path, the components and the
// patch files of spec.source.kustomize.
func kustomizeSpecOf(app *unstructured.Unstructured, path string) kustomize.Spec {
	k, _, _ := unstructured.NestedMap(app.Object, "spec", "source", "kustomize")
	spec := kustomize.Spec{Path: path}
	spec.Components, _, _ = unstructured.NestedStringSlice(k, "components")
	spec.IgnoreMissingComponents, _, _ = unstructured.NestedBool(k, "ignoreMissingComponents")
	patches, _, _ := unstructured.NestedSlice(k, "patches")
	for _, it := range patches {
		if m, ok := it.(map[string]any); ok {
			if p, _ := m["path"].(string); p != "" {
				spec.PatchFiles = append(spec.PatchFiles, p)
			}
		}
	}
	return spec
}

// kustomizeFingerprint identifies what decides the files of a Kustomize source
// besides the repository and the two revisions, so a changed Application is
// looked at again.
func kustomizeFingerprint(app *unstructured.Unstructured, summary backflowv1alpha1.ApplicationSummary) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%#v", kustomizeSpecOf(app, summary.Path))))
	return hex.EncodeToString(sum[:8])
}

// lookAtKustomize counts the changed files that the build of a Kustomize source
// reads, at either revision: its kustomization files and everything they
// reference, including bases outside the Application's directory
// (kustomize.RelevantFiles). When the set is not closed (a remote base,
// helmCharts, a reference that cannot be followed) no answer is given and the
// Application stays paused as for any Git-ahead state.
func (r *DriftReconciler) lookAtKustomize(ctx context.Context, summary backflowv1alpha1.ApplicationSummary,
	app *unstructured.Unstructured, st aheadState, auth *gitrepo.Auth) (aheadCheck, error) {
	return r.countRelevant(ctx, summary.RepoURL, st.synced, st.compared, auth, func(fsys fs.FS, sha string) (relevantFiles, string, error) {
		rel, err := kustomize.RelevantFiles(fsys, kustomizeSpecOf(app, summary.Path))
		if err != nil {
			return nil, "", err
		}
		if !rel.Closed() {
			return nil, fmt.Sprintf("the files of the kustomization are not closed: %s (at %s)",
				strings.Join(rel.Reasons(), "; "), shortRevision(sha)), nil
		}
		return rel, "", nil
	})
}
