package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/helmmap"
)

// helmSpecAt describes the Helm source of the Application as Argo CD sees it
// at one revision. Only what decides which files are read is taken: the chart
// path, the value files, the file parameters, and the variables the paths may
// use, of which ARGOCD_APP_REVISION differs per revision.
func helmSpecAt(app *unstructured.Unstructured, repoURL, path, sha string) helmmap.Spec {
	helm, _, _ := unstructured.NestedMap(app.Object, "spec", "source", "helm")
	return helmSpecFrom(app, helm, repoURL, path, sha)
}

// helmSpecFrom is helmSpecAt for the helm settings of any source.
func helmSpecFrom(app *unstructured.Unstructured, helm map[string]any, repoURL, path, sha string) helmmap.Spec {
	spec := helmmap.Spec{Path: path}
	spec.ValueFiles, _, _ = unstructured.NestedStringSlice(helm, "valueFiles")
	spec.IgnoreMissingValueFiles, _, _ = unstructured.NestedBool(helm, "ignoreMissingValueFiles")
	if items, _, _ := unstructured.NestedSlice(helm, "fileParameters"); len(items) > 0 {
		for _, it := range items {
			m, _ := it.(map[string]any)
			name, _ := m["name"].(string)
			p, _ := m["path"].(string)
			spec.FileParameters = append(spec.FileParameters, helmmap.FileParameter{Name: name, Path: p})
		}
	}
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(app.Object, path...)
		return v
	}
	short := func(n int) string {
		if len(sha) > n {
			return sha[:n]
		}
		return sha
	}
	spec.Env = map[string]string{
		"ARGOCD_APP_NAME":                   app.GetName(),
		"ARGOCD_APP_NAMESPACE":              str("spec", "destination", "namespace"),
		"ARGOCD_APP_PROJECT_NAME":           str("spec", "project"),
		"ARGOCD_APP_REVISION":               sha,
		"ARGOCD_APP_REVISION_SHORT":         short(7),
		"ARGOCD_APP_REVISION_SHORT_8":       short(8),
		"ARGOCD_APP_SOURCE_REPO_URL":        repoURL,
		"ARGOCD_APP_SOURCE_PATH":            path,
		"ARGOCD_APP_SOURCE_TARGET_REVISION": str("spec", "source", "targetRevision"),
	}
	return spec
}

// helmFingerprint identifies what a look at a Helm source depends on besides
// the repository and the two revisions, so a changed Application is looked at
// again.
func helmFingerprint(app *unstructured.Unstructured, summary backflowv1alpha1.ApplicationSummary) string {
	spec := helmSpecAt(app, summary.RepoURL, summary.Path, "")
	delete(spec.Env, "ARGOCD_APP_REVISION")
	delete(spec.Env, "ARGOCD_APP_REVISION_SHORT")
	delete(spec.Env, "ARGOCD_APP_REVISION_SHORT_8")
	sum := sha256.Sum256([]byte(fmt.Sprintf("%#v", spec)))
	return hex.EncodeToString(sum[:8])
}

// lookAtHelm counts the changed files that the render of a Helm source depends
// on, at either revision (a file the Application stopped using still counts, as
// does one it started to use). Which files those are comes from
// helmmap.RelevantFiles, evaluated on the tree of each revision, so globs and
// file:// dependencies are expanded where they stand at that revision.
//
// A symbolic link among them, a chart that is not one at either revision, a
// missing value file or a dependency outside the repository gives no answer
// (unchecked), and the Application stays paused as for any Git-ahead state; a
// tree that cannot be read is an error, tried again later.
func (r *DriftReconciler) lookAtHelm(ctx context.Context, summary backflowv1alpha1.ApplicationSummary,
	app *unstructured.Unstructured, st aheadState, auth *gitrepo.Auth) (aheadCheck, error) {
	return r.countRelevant(ctx, summary.RepoURL, st.synced, st.compared, auth, func(fsys fs.FS, sha string) (relevantFiles, string, error) {
		rel, err := helmmap.RelevantFiles(fsys, helmSpecAt(app, summary.RepoURL, summary.Path, sha))
		if err != nil {
			if helmmap.ReasonOf(err) != "" || errors.Is(err, helmmap.ErrNotAChart) {
				return nil, fmt.Sprintf("%v (at %s)", err, shortRevision(sha)), nil
			}
			return nil, "", err
		}
		return rel, "", nil
	})
}

// relevantFiles tells whether a changed path is one a render depends on.
type relevantFiles interface {
	Matches(path string) bool
}

// countRelevant counts the changed files that a render depends on, at either
// revision (a file the Application stopped using still counts, as does one it
// started to use). at works out the files for the tree of one revision; it
// returns a reason when they cannot be told, which gives no answer for good
// (unchecked); a tree that cannot be read is an error, tried again later.
func (r *DriftReconciler) countRelevant(ctx context.Context, repoURL, from, to string,
	auth *gitrepo.Auth, at func(fsys fs.FS, sha string) (rel relevantFiles, unchecked string, err error)) (aheadCheck, error) {
	opener, ok := r.Git.(TreeOpener)
	if !ok {
		return aheadCheck{unchecked: "the repository's files cannot be read"}, nil
	}
	paths, err := r.Git.ChangedPaths(ctx, repoURL, from, to, auth)
	if err != nil {
		return aheadCheck{}, err
	}

	var relevant []relevantFiles
	for _, sha := range []string{from, to} {
		fsys, release, err := opener.OpenTree(ctx, repoURL, sha, auth)
		if err != nil {
			return aheadCheck{}, err
		}
		rel, unchecked, err := at(fsys, sha)
		release()
		if err != nil {
			return aheadCheck{}, err
		}
		if unchecked != "" {
			return aheadCheck{unchecked: unchecked}, nil
		}
		relevant = append(relevant, rel)
	}
	n := 0
	for _, p := range paths {
		if slices.ContainsFunc(relevant, func(rel relevantFiles) bool { return rel.Matches(p) }) {
			n++
		}
	}
	return aheadCheck{changed: n}, nil
}
