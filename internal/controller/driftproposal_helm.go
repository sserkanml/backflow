package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/argocd"
	"github.com/sserkanml/backflow/internal/helmmap"
	"github.com/sserkanml/backflow/internal/mapping"
)

// Reasons of the Mapped condition for Helm sources that come from the
// controller rather than from helmmap (whose reasons are used as they are).
const (
	reasonArgoCDUnauthorized      = "ArgoCDUnauthorized"
	reasonArgoCDApplicationAbsent = "ArgoCDApplicationNotFound"
	reasonArgoCDUnreachable       = "ArgoCDUnreachable"
	reasonArgoCDNotConfigured     = "ArgoCDNotConfigured"
	reasonArgoCDCredentials       = "ArgoCDCredentials"
)

// mappedChange is a verified mapping onto one or more files.
type mappedChange struct {
	strategy backflowv1alpha1.MappingStrategy
	results  []mapping.Result
	// collateral are the changes beyond the drifted fields that the policy
	// allowed (HelmValues only).
	collateral []helmmap.Collateral
}

// maxCollateralRecorded bounds the collateral changes kept in a status, and
// maxCollateralValue the length of a value in one.
const (
	maxCollateralRecorded = 64
	maxCollateralValue    = 512
)

// collateralStatus is the collateral as the status records it.
func (m *mappedChange) collateralStatus() []backflowv1alpha1.CollateralChange {
	if len(m.collateral) == 0 {
		return nil
	}
	short := func(v string) string {
		if r := []rune(v); len(r) > maxCollateralValue {
			return string(r[:maxCollateralValue]) + "..."
		}
		return v
	}
	out := make([]backflowv1alpha1.CollateralChange, 0, min(len(m.collateral), maxCollateralRecorded))
	for _, c := range m.collateral[:min(len(m.collateral), maxCollateralRecorded)] {
		out = append(out, backflowv1alpha1.CollateralChange{
			Group: c.Resource.Group, Kind: c.Resource.Kind, Namespace: c.Resource.Namespace, Name: c.Resource.Name,
			Path: c.Pointer, Current: short(c.Current), New: short(c.After),
		})
	}
	return out
}

// files maps each edited path to its new content.
func (m *mappedChange) files() map[string][]byte {
	out := make(map[string][]byte, len(m.results))
	for _, r := range m.results {
		out[r.File] = r.Edited
	}
	return out
}

// paths are the edited files, sorted.
func (m *mappedChange) paths() []string {
	out := make([]string, len(m.results))
	for i, r := range m.results {
		out[i] = r.File
	}
	slices.Sort(out)
	return out
}

// diff is the unified diff of all edited files.
func (m *mappedChange) diff() string {
	parts := make([]string, 0, len(m.results))
	for _, r := range m.results {
		parts = append(parts, r.Diff)
	}
	return strings.Join(parts, "")
}

// edits are the edits as the status records them. For Helm values the
// location is the dotted key path.
func (m *mappedChange) edits() []backflowv1alpha1.SourceEdit {
	var out []backflowv1alpha1.SourceEdit
	for _, r := range m.results {
		for _, e := range r.Edits {
			loc := e.Location
			if m.strategy == backflowv1alpha1.StrategyHelmValues {
				loc = helmmap.KeyPathFromPointer(e.Location).String()
			}
			out = append(out, backflowv1alpha1.SourceEdit{File: e.File, Location: loc, Value: e.Value, ChangeIndex: int32(e.ChangeIndex)}) //nolint:gosec // an index into spec.changes
		}
	}
	return out
}

// message is the status message of a mapped proposal.
func (m *mappedChange) message() string {
	files := strings.Join(m.paths(), ", ")
	if m.strategy == backflowv1alpha1.StrategyHelmValues {
		if len(m.collateral) > 0 {
			return fmt.Sprintf("Mapped to %s; the edit is verified by rendering through Argo CD. Besides the drifted fields it changes %d label or annotation field(s) the policy allows.",
				files, len(m.collateral))
		}
		return fmt.Sprintf("Mapped to %s; the edit is verified by rendering through Argo CD and changes exactly the drifted fields.", files)
	}
	return fmt.Sprintf("Mapped to %s; the edit reproduces the live state and is verified.", files)
}

// application reads the Argo CD Application of a proposal. A missing
// Application, or one whose CRD is not installed, is apierrors.IsNotFound.
func (r *DriftProposalReconciler) application(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (*unstructured.Unstructured, error) {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	key := types.NamespacedName{Namespace: dp.Spec.Application.Namespace, Name: dp.Spec.Application.Name}
	if err := r.Get(ctx, key, app); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, apierrors.NewNotFound(applicationGVK.GroupVersion().WithResource("applications").GroupResource(), key.Name)
		}
		return nil, err
	}
	return app, nil
}

// fullHelmSpec is the Helm source of the Application at a revision, with the
// inline settings (values, valuesObject, parameters) that decide which keys a
// value file cannot change.
func fullHelmSpec(app *unstructured.Unstructured, repoURL, path, sha string) helmmap.Spec {
	spec := helmSpecAt(app, repoURL, path, sha)
	helm, _, _ := unstructured.NestedMap(app.Object, "spec", "source", "helm")
	spec.Values, _, _ = unstructured.NestedString(helm, "values")
	if obj, ok, _ := unstructured.NestedMap(helm, "valuesObject"); ok && len(obj) > 0 {
		if b, err := json.Marshal(obj); err == nil {
			spec.ValuesObject = b
		}
	}
	if items, _, _ := unstructured.NestedSlice(helm, "parameters"); len(items) > 0 {
		for _, it := range items {
			m, _ := it.(map[string]any)
			name, _ := m["name"].(string)
			value, _ := m["value"].(string)
			force, _ := m["forceString"].(bool)
			spec.Parameters = append(spec.Parameters, helmmap.Parameter{Name: name, Value: value, ForceString: force})
		}
	}
	return spec
}

// helmAppOf describes the Application to the renderer.
func helmAppOf(app *unstructured.Unstructured) helmmap.App {
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(app.Object, path...)
		return v
	}
	out := helmmap.App{
		Name: app.GetName(), Namespace: app.GetNamespace(), Project: str("spec", "project"),
		DestNamespace: str("spec", "destination", "namespace"),
		ReleaseName:   str("spec", "source", "helm", "releaseName"),
	}
	if out.ReleaseName == "" {
		out.ReleaseName = app.GetName()
	}
	if res, known := appResources(app); known {
		for _, a := range res {
			out.Resources = append(out.Resources, helmmap.ListedResource{Group: a.Group, Kind: a.Kind, Namespace: a.Namespace, Name: a.Name})
		}
	}
	return out
}

// renderer returns the proposal's policy's renderer: it talks to Argo CD with
// the policy's URL, token and TLS settings, the same as the drift controller.
func (r *DriftProposalReconciler) renderer(ctx context.Context, policy *backflowv1alpha1.BackflowPolicy) (helmmap.Renderer, error) {
	if policy.Spec.ArgoCD == nil {
		return nil, &heldError{reason: reasonArgoCDNotConfigured, cause: fmt.Errorf("BackflowPolicy %q has no spec.argoCD", policy.Name)}
	}
	cfg, err := argoCDConfig(ctx, r.Client, policy)
	if err != nil {
		return nil, &heldError{reason: reasonArgoCDCredentials, cause: err}
	}
	build := r.NewRenderer
	if build == nil {
		build = func(c argocd.Config) (helmmap.Renderer, error) {
			cl, err := argocd.New(c)
			if err != nil {
				return nil, err
			}
			return &helmmap.ArgoRenderer{Client: cl}, nil
		}
	}
	return r.renderers.get(policy.Namespace+"/"+policy.Name, cfg, build)
}

// policyOf reads the proposal's policy.
func (r *DriftProposalReconciler) policyOf(ctx context.Context, dp *backflowv1alpha1.DriftProposal) (*backflowv1alpha1.BackflowPolicy, error) {
	var policy backflowv1alpha1.BackflowPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: dp.Namespace, Name: dp.Spec.PolicyName}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &heldError{reason: reasonPolicyNotFound, cause: fmt.Errorf("BackflowPolicy %q not found", dp.Spec.PolicyName)}
		}
		return nil, err
	}
	return &policy, nil
}

// allowedCollateral is what the policy allows an edit to change beyond the
// drifted fields (spec.helm.allowCollateralChanges).
func allowedCollateral(policy *backflowv1alpha1.BackflowPolicy) []string {
	if policy.Spec.Helm == nil {
		return nil
	}
	out := make([]string, len(policy.Spec.Helm.AllowCollateralChanges))
	for i, k := range policy.Spec.Helm.AllowCollateralChanges {
		out[i] = string(k)
	}
	return out
}

// argoFailure turns an error of the render call into a held proposal: Argo CD
// cannot be reached, or does not accept the token, right now. It is not a
// verdict about the source, so it is retried.
func argoFailure(err error) error {
	switch {
	case errors.Is(err, argocd.ErrUnauthorized):
		return &heldError{reason: reasonArgoCDUnauthorized, cause: err}
	case errors.Is(err, argocd.ErrNotFound):
		return &heldError{reason: reasonArgoCDApplicationAbsent, cause: err}
	}
	return &heldError{reason: reasonArgoCDUnreachable, cause: err}
}

// scalarOf decodes a JSON value that is a string, number or bool. Anything
// else (an object, null, invalid JSON) is not one.
func scalarOf(raw string) (any, bool) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, false
	}
	switch v.(type) {
	case string, float64, bool:
		return v, true
	}
	return nil, false
}

// helmSourceProblem returns a *verdictError for a Helm Application whose
// source cannot be mapped at all: several sources, or a chart that comes from a
// Helm repository or an OCI registry instead of the Git repository.
func helmSourceProblem(app *unstructured.Unstructured) error {
	if sources, ok, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); ok && len(sources) > 0 {
		return &verdictError{reason: reasonUnsupportedSourceType,
			err: errors.New("the Application has several sources; only single-source Helm Applications are mapped")}
	}
	if chart, _, _ := unstructured.NestedString(app.Object, "spec", "source", "chart"); chart != "" {
		return &verdictError{reason: reasonUnsupportedSourceType,
			err: fmt.Errorf("the chart %q comes from a Helm repository or registry, not from the Git repository; only charts in Git are mapped", chart)}
	}
	return nil
}

// mapHelm traces the proposal back to the value files of a Helm source at the
// revision of tree. Candidates are the keys that hold the old values; an edit
// is only accepted when rendering through Argo CD shows it changes exactly the
// drifted fields (see helmmap.Verify). Errors:
//   - *verdictError: the change cannot be traced back with certainty;
//   - *heldError: Argo CD or the policy cannot be used right now;
//   - other errors: the repository could not be read.
func (r *DriftProposalReconciler) mapHelm(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	app *unstructured.Unstructured, tree fs.FS, sha string) (*mappedChange, error) {
	if err := helmSourceProblem(app); err != nil {
		return nil, err
	}
	spec := fullHelmSpec(app, dp.Spec.Source.RepoURL, dp.Spec.Source.Path, sha)
	src, err := helmmap.NewSource(tree, spec)
	if err != nil {
		if reason := helmmap.ReasonOf(err); reason != "" {
			return nil, &verdictError{reason: reason, err: err}
		}
		return nil, err
	}
	files, err := helmmap.CollectFiles(tree, src)
	if err != nil {
		if reason := helmmap.ReasonOf(err); reason != "" {
			return nil, &verdictError{reason: reason, err: err}
		}
		return nil, err
	}

	fields := make([]helmmap.Field, len(dp.Spec.Changes))
	for i, ch := range dp.Spec.Changes {
		found := &helmmap.Result{}
		if old, ok := scalarOf(ch.Desired); ok && ch.Op == backflowv1alpha1.OpReplace {
			if found, err = src.Find(files, old); err != nil {
				return nil, &verdictError{reason: helmmap.ReasonInvalidInlineValues, err: err}
			}
		}
		field := helmmap.Field{Index: i, Change: ch, Found: found}
		// A string that changed may be a part of it that a key holds: the tag of an
		// image, say. Those keys are tried only when no key holds the whole value.
		if oldS, ok := scalarOf(ch.Desired); ok && ch.Op == backflowv1alpha1.OpReplace {
			newV, newOK := scalarOf(ch.Live)
			o, oIsString := oldS.(string)
			n, nIsString := newV.(string)
			if newOK && oIsString && nIsString {
				if field.Fragments, err = src.FindFragments(files, o, n); err != nil {
					return nil, &verdictError{reason: helmmap.ReasonInvalidInlineValues, err: err}
				}
			}
		}
		fields[i] = field
	}

	policy, err := r.policyOf(ctx, dp)
	if err != nil {
		return nil, err
	}
	rend, err := r.renderer(ctx, policy)
	if err != nil {
		return nil, err
	}
	res := dp.Spec.Resource
	out, err := helmmap.Verify(ctx, rend, helmmap.Input{
		App:      helmAppOf(app),
		Resource: helmmap.ResourceKey{Group: res.Group, Kind: res.Kind, Namespace: res.Namespace, Name: res.Name},
		Fields:   fields, Source: src, Files: files,
	}, helmmap.Options{AllowCollateral: allowedCollateral(policy)})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, argoFailure(err)
	}
	if !out.Mapped() {
		return nil, &verdictError{reason: out.Unmapped.Reason, err: errors.New(out.Unmapped.Message)}
	}
	return &mappedChange{strategy: backflowv1alpha1.StrategyHelmValues, results: out.Results, collateral: out.Collateral}, nil
}
