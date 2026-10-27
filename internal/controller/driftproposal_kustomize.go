package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/kustomize"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// kustomizeOptionsOf reads spec.source.kustomize of the Application: the
// options Argo CD applies on top of the repository's kustomization.
func kustomizeOptionsOf(app *unstructured.Unstructured) kustomize.Options {
	k, _, _ := unstructured.NestedMap(app.Object, "spec", "source", "kustomize")
	return kustomize.ParseOptions(k)
}

// kustomizeSourceProblem returns a *verdictError for an Application whose
// Kustomize source cannot be mapped at all.
func kustomizeSourceProblem(app *unstructured.Unstructured) error {
	if sources, ok, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); ok && len(sources) > 0 {
		return &verdictError{reason: reasonUnsupportedSourceType,
			err: errors.New("the Application has several sources; only single-source Kustomize Applications are mapped")}
	}
	return nil
}

// mapKustomize traces the proposal back to the files of a Kustomize source at
// the revision of tree: one extra render to learn where each resource comes
// from, then candidates in tiers (settings, existing patches, resource files,
// shared bases, a new patch), each accepted only when a render of the edited
// files through Argo CD reproduces exactly the drifted fields. Errors are as
// for mapHelm.
func (r *DriftProposalReconciler) mapKustomize(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	app *unstructured.Unstructured, tree fs.FS) (*mappedChange, error) {
	if err := kustomizeSourceProblem(app); err != nil {
		return nil, err
	}
	src, err := kustomize.NewSource(tree, dp.Spec.Source.Path, kustomizeOptionsOf(app))
	if err != nil {
		if reason := kustomize.ReasonOf(err); reason != "" {
			return nil, &verdictError{reason: reason, err: err}
		}
		return nil, err
	}
	files, err := kustomize.CollectFiles(tree, src)
	if err != nil {
		return nil, err
	}
	policy, err := r.policyOf(ctx, dp)
	if err != nil {
		return nil, err
	}
	rend, err := r.renderer(ctx, policy)
	if err != nil {
		return nil, err
	}
	rapp := helmAppOf(app)
	located, err := kustomize.Locate(ctx, rend, src, files, rapp)
	if err != nil {
		return nil, renderFailure(ctx, err)
	}
	res := dp.Spec.Resource
	key := rendermap.ResourceKey{Group: res.Group, Kind: res.Kind, Namespace: res.Namespace, Name: res.Name}
	if located.Objects[key] == nil {
		return nil, &verdictError{reason: rendermap.ReasonRenderMismatch,
			err: fmt.Errorf("%s is not in the render of the source at this revision", resourceLabel(res))}
	}
	others, err := r.otherUsers(ctx, dp, app, tree, src)
	if err != nil {
		return nil, err
	}
	pol := kustomize.Policy{
		EditBases: policy.Spec.KustomizeEditsBases(), AddPatches: policy.Spec.KustomizeAddsPatches(),
		Application: app.GetName(), OtherUsers: others,
	}
	if isParentLookup(ctx) {
		// Where the value is defined, not where a patch could be added.
		pol.EditBases, pol.AddPatches, pol.OtherUsers = true, false, nil
	}
	fields, err := kustomize.FieldsFor(src, files, located.Origins[key], located.Objects[key], pol, dp.Spec.Changes)
	if err != nil {
		return nil, err
	}
	out, err := kustomize.Verify(ctx, rend, kustomize.Input{
		App: rapp, Resource: key, Changes: dp.Spec.Changes, Source: src, Files: files, Located: located, Fields: fields,
		Policy: pol,
	}, rendermap.Options{AllowCollateral: allowedCollateral(policy), AllowPartial: policy.Spec.PartialAllowed()})
	if err != nil {
		return nil, renderFailure(ctx, err)
	}
	if !out.Mapped() {
		return nil, applicationVerdict(out, kustomize.ApplicationOptions(fields, unmappedPaths(out)))
	}
	desc := kustomize.Describe(out)
	change := &mappedChange{strategy: desc.Strategy, results: out.Results, collateral: out.Collateral, renders: out.Renders, kustomize: &desc}
	for _, a := range out.Applied {
		if a.Candidate.NewFile && !slices.Contains(change.created, a.Candidate.File) {
			change.created = append(change.created, a.Candidate.File)
		}
	}
	slices.Sort(change.created)
	for _, l := range out.LeftOut {
		change.leftOut = append(change.leftOut, leftOutField{path: l.Pointer, reason: l.Reason, message: l.Detail})
	}
	return change, nil
}

// renderFailure turns an error of a render into a verdict (the source does not
// render) or a held proposal (Argo CD cannot be used right now).
func renderFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, rendermap.ErrRenderFailed) {
		return &verdictError{reason: rendermap.ReasonRenderFailed, err: err}
	}
	if reason := rendermap.ReasonOf(err); reason != "" {
		return &verdictError{reason: reason, err: err}
	}
	return argoFailure(err)
}

// otherUsers maps each file of the closed set of src that is outside the
// Application's path to the other Kustomize Applications, of any
// BackflowPolicy, that build it. Applications that no policy tracks cannot be
// known; the text of a merge request says so.
func (r *DriftProposalReconciler) otherUsers(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	app *unstructured.Unstructured, tree fs.FS, src *kustomize.Source) (map[string][]string, error) {
	var policies backflowv1alpha1.BackflowPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil, err
	}
	mine := src.Relevance().Files()
	users := map[string][]string{}
	seen := map[types.NamespacedName]bool{{Namespace: app.GetNamespace(), Name: app.GetName()}: true}
	repo := normalRepoURL(dp.Spec.Source.RepoURL)
	for i := range policies.Items {
		p := &policies.Items[i]
		for _, s := range p.Status.Applications {
			id := types.NamespacedName{Namespace: p.Spec.ArgoCDNamespace, Name: s.Name}
			if seen[id] || s.SourceType != string(backflowv1alpha1.SourceKustomize) || normalRepoURL(s.RepoURL) != repo {
				continue
			}
			seen[id] = true
			other := &unstructured.Unstructured{}
			other.SetGroupVersionKind(applicationGVK)
			if err := r.Get(ctx, id, other); err != nil {
				continue // gone, or not readable: it cannot be named
			}
			rel, err := kustomize.RelevantFiles(tree, kustomizeSpecOf(other, s.Path))
			if err != nil {
				continue
			}
			for _, f := range mine {
				if !src.InPath(f) && rel.Matches(f) {
					users[f] = append(users[f], id.String())
				}
			}
		}
	}
	for f := range users {
		slices.Sort(users[f])
	}
	return users, nil
}

func normalRepoURL(u string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git"))
}

// kustomizeNotes is what the commit message and the merge request say about a
// Kustomize edit: what kind of edit each one is, that it was verified, and
// the warnings of files outside the Application's path. Empty for other
// strategies.
func kustomizeNotes(dp *backflowv1alpha1.DriftProposal, m *mappedChange) string {
	if m == nil || m.kustomize == nil {
		return ""
	}
	changes := mappedChanges(dp, m.leftOut)
	paths := make([]string, len(changes))
	for i, ch := range changes {
		paths[i] = singleLine(ch.Path)
	}
	var b strings.Builder
	b.WriteString(singleLinesKeepBreaks(m.kustomize.Text()))
	fmt.Fprintf(&b, "\n\nVerified by rendering through Argo CD: this edit changes exactly the drifted fields (%s) and nothing else.",
		strings.Join(paths, ", "))
	return b.String()
}

// singleLinesKeepBreaks makes every line single-line text and keeps the breaks
// between them.
func singleLinesKeepBreaks(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = singleLine(l)
	}
	return strings.Join(lines, "\n")
}

func kustomizeNoteList(dp *backflowv1alpha1.DriftProposal, m *mappedChange) []string {
	if n := kustomizeNotes(dp, m); n != "" {
		return []string{n}
	}
	return nil
}
