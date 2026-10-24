package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/helmmap"
)

// Why a hold of an Application is reported as it is: the files of a repository
// that Git is ahead with, or a change of the Application itself.
const (
	holdGitAhead           = "GitAhead"
	holdApplicationChanged = "ApplicationChanged"
)

// chartChange says how a chart that Argo CD pulls from a Helm repository or a
// registry changed since the last sync, or returns "": its version in the
// source, or the version a range resolved to. A new chart version changes what
// is rendered without a commit, so it is a change of the Application, not Git
// being ahead.
func chartChange(app *unstructured.Unstructured) string {
	sources := specSources(app)
	if len(sources) == 0 {
		return ""
	}
	compared := statusStrings(app, fieldStatus, "sync", "revisions")
	synced := statusStrings(app, fieldStatus, fieldOperationState, fieldSyncResult, "revisions")
	last, _ := sourcesOf(app, []string{fieldStatus, fieldOperationState, fieldSyncResult, fieldSpecSource},
		[]string{fieldStatus, fieldOperationState, fieldSyncResult, fieldSpecSources})
	for i, src := range sources {
		chart := strField(src, "chart")
		if chart == "" {
			continue
		}
		switch {
		case at(synced, i) != "" && at(compared, i) != "" && at(synced, i) != at(compared, i):
			return fmt.Sprintf("the chart %s of source %d moved from %s to %s since the last sync",
				chart, i+1, at(synced, i), at(compared, i))
		case i < len(last) && last[i] != nil && (strField(last[i], "targetRevision") != strField(src, "targetRevision") ||
			strField(last[i], "chart") != chart || strField(last[i], "repoURL") != strField(src, "repoURL")):
			return fmt.Sprintf("the chart %s of source %d changed in the Application since the last sync (%s, was %s)",
				chart, i+1, strField(src, "targetRevision"), strField(last[i], "targetRevision"))
		}
	}
	return ""
}

// multiSourceFingerprint identifies what a look at a multi-source Application
// depends on besides the revisions: its sources, as written.
func multiSourceFingerprint(app *unstructured.Unstructured) string {
	b, _ := json.Marshal(specSources(app))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// refFiles tells which files of a ref source the render reads: the value files
// and file parameters of the Helm source that name it ($ref/path, globs
// included).
type refFiles struct {
	patterns []string
	exact    map[string]bool
}

// Matches reports whether a changed path is a file the render reads, or could
// start to read (a new file under a glob).
func (r *refFiles) Matches(p string) bool {
	if r.exact[p] {
		return true
	}
	for _, pat := range r.patterns {
		if ok, _ := doublestar.Match(pat, p); ok {
			return true
		}
	}
	return false
}

// refFilesAt reads the files a ref source contributes at one revision. A value
// file that does not exist yet still counts: it may appear. A symbolic link
// gives no answer.
func refFilesAt(fsys fs.FS, files []refFile) (*refFiles, string) {
	out := &refFiles{exact: map[string]bool{}}
	for _, f := range files {
		p := path.Clean(strings.TrimPrefix(f.Path, "/"))
		if strings.ContainsAny(p, "*?[") {
			out.patterns = append(out.patterns, p)
			matches, err := doublestar.Glob(fsys, p, doublestar.WithFilesOnly())
			if err != nil {
				return nil, fmt.Sprintf("the value file pattern %s is invalid: %v", f.Raw, err)
			}
			for _, m := range matches {
				out.exact[m] = true
			}
			continue
		}
		out.exact[p] = true
		if info, err := lstatFS(fsys, p); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Sprintf("the value file %s is a symbolic link", f.Raw)
		}
	}
	return out, ""
}

// lstatFS describes a path without following a symbolic link where the file
// system can tell; a view of a Git commit reports a link with the symlink mode.
func lstatFS(fsys fs.FS, name string) (fs.FileInfo, error) {
	if _, ok := fsys.(fs.ReadLinkFS); ok {
		return fs.Lstat(fsys, name)
	}
	return fs.Stat(fsys, name)
}

// lookAtMultiSource counts, per Git source whose revision differs from the last
// sync, the changed files its render depends on: the value files a ref source
// provides, and for a chart in Git the chart's files (helmmap.RelevantFiles,
// without the value files that come from a ref source). A chart that comes from
// a Helm repository has no files to count; its version is a change of the
// Application (chartChange). Each repository is read with the ScmConnection
// matched to its own host.
func (r *DriftReconciler) lookAtMultiSource(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy,
	summary backflowv1alpha1.ApplicationSummary, app *unstructured.Unstructured, ms *multiSource) (aheadCheck, error) {
	sources := specSources(app)
	compared := statusStrings(app, fieldStatus, "sync", "revisions")
	synced := statusStrings(app, fieldStatus, fieldOperationState, fieldSyncResult, "revisions")
	if len(compared) != len(sources) || len(synced) != len(sources) {
		return aheadCheck{unchecked: "Argo CD reports no revision for every source"}, nil
	}
	helm, _ := sources[ms.Render]["helm"].(map[string]any)
	files := valueFileRefs(helm)

	total := 0
	for i, src := range sources {
		if compared[i] == synced[i] || strField(src, "chart") != "" {
			continue
		}
		repoURL := strField(src, "repoURL")
		if !isFullSHA(compared[i]) || !isFullSHA(synced[i]) {
			return aheadCheck{unchecked: fmt.Sprintf("source %d is not at commits (%s, %s)", i+1, shortRevision(synced[i]), shortRevision(compared[i]))}, nil
		}
		var auth *gitrepo.Auth
		if access, err := scmAccessForRepo(ctx, r.Client, policy.Namespace, policy, summary.Name, repoURL); err == nil && access != nil {
			auth = access.auth
		}
		var hit aheadCheck
		var err error
		if name := strField(src, "ref"); name != "" {
			var mine []refFile
			for _, f := range files {
				if f.Ref == name {
					mine = append(mine, f)
				}
			}
			hit, err = r.countRelevant(ctx, repoURL, synced[i], compared[i], auth, func(fsys fs.FS, _ string) (relevantFiles, string, error) {
				rel, unchecked := refFilesAt(fsys, mine)
				if unchecked != "" {
					return nil, unchecked, nil
				}
				return rel, "", nil
			})
		} else {
			hit, err = r.countRelevant(ctx, repoURL, synced[i], compared[i], auth, func(fsys fs.FS, sha string) (relevantFiles, string, error) {
				spec := helmSpecFrom(app, helm, repoURL, strField(src, "path"), sha)
				spec.ValueFiles = ownFiles(spec.ValueFiles)
				spec.FileParameters = slices.DeleteFunc(spec.FileParameters, func(f helmmap.FileParameter) bool {
					return splitRefPath(f.Path).Ref != ""
				})
				rel, err := helmmap.RelevantFiles(fsys, spec)
				if err != nil {
					if helmmap.ReasonOf(err) != "" || errors.Is(err, helmmap.ErrNotAChart) {
						return nil, fmt.Sprintf("%v (at %s)", err, shortRevision(sha)), nil
					}
					return nil, "", err
				}
				return rel, "", nil
			})
		}
		if err != nil {
			return aheadCheck{}, err
		}
		if hit.unchecked != "" {
			return aheadCheck{unchecked: fmt.Sprintf("source %d: %s", i+1, hit.unchecked)}, nil
		}
		total += hit.changed
	}
	return aheadCheck{changed: total}, nil
}

// ownFiles keeps the value files that are not named through a ref source.
func ownFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if splitRefPath(f).Ref == "" {
			out = append(out, f)
		}
	}
	return out
}
