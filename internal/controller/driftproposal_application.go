package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/appvalues"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/rendermap"
	"github.com/sserkanml/backflow/internal/scm"
)

// A drifted value that an option of the Application sets (helm.parameters,
// kustomize.images, ...) cannot be proposed to Git: Argo CD has no API that
// renders an Application with a changed spec, so a change of the option cannot
// be verified. What Backflow can prove is where the option is defined, when the
// Application is itself a resource of a parent Application whose source is in
// Git: it maps the child Application's field to the parent's file with the same
// engines it uses for any resource, verified by a render of the parent. The
// result is recorded in status.mapping.application. Nothing is written.

// parentLookupKey marks a context as the lookup of an option in a parent: the
// mapping then looks for the file that holds the value, never for a place to add
// a patch (see mapKustomize).
type parentLookupKey struct{}

func withParentLookup(ctx context.Context) context.Context {
	return context.WithValue(ctx, parentLookupKey{}, true)
}

func isParentLookup(ctx context.Context) bool { v, _ := ctx.Value(parentLookupKey{}).(bool); return v }

// probeValue is the value a location-only lookup edits the option to. It never
// leaves the verification: the diff is not recorded.
const probeValue = `"backflow-location-probe"`

// reasonApplicationValue is the reason of the Event that carries the diagnosis.
const reasonApplicationValue = "ApplicationValue"

// unmappedPaths are the JSON pointers of the fields of an Unmapped outcome.
func unmappedPaths(out *rendermap.Outcome) map[string]bool {
	paths := map[string]bool{}
	if out.Unmapped != nil {
		for _, f := range out.Unmapped.Fields {
			paths[f.Pointer] = true
		}
	}
	return paths
}

// applicationVerdict is the verdict of an Unmapped outcome. When options of the
// Application set the fields, it carries them, and the reason is SetByApplication
// if they set every field that could not be mapped.
func applicationVerdict(out *rendermap.Outcome, options []appvalues.Option) *verdictError {
	v := &verdictError{reason: out.Unmapped.Reason, err: errors.New(out.Unmapped.Message), options: options}
	covered := map[string]bool{}
	for _, o := range options {
		for _, f := range o.Fields {
			covered[f] = true
		}
	}
	all := len(out.Unmapped.Fields) > 0
	for p := range unmappedPaths(out) {
		all = all && covered[p]
	}
	if all && len(options) > 0 {
		v.reason = rendermap.ReasonSetByApplication
	}
	return v
}

// unmappedByApplication records an Unmapped proposal whose fields an option of the
// Application sets, with the diagnosis of where the option is defined.
func (r *DriftProposalReconciler) unmappedByApplication(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	app *unstructured.Unstructured, verdict *verdictError) (ctrl.Result, error) {
	val, err := r.describeApplicationValue(ctx, dp, app, verdict.options[0])
	if err != nil {
		if held, ok := errors.AsType[*heldError](err); ok {
			return r.retry(ctx, dp, held.reason, held.cause)
		}
		if ctx.Err() != nil {
			return ctrl.Result{}, ctx.Err()
		}
		return r.retry(ctx, dp, reasonRepositoryUnavailable, err)
	}
	sentence := applicationSentence(val, verdict.options)
	message := sentence
	if verdict.reason != rendermap.ReasonSetByApplication {
		message = verdict.err.Error() + "; " + sentence
	}
	if err := r.unmappedWith(ctx, dp, verdict.reason, message, val); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(dp, nil, corev1.EventTypeNormal, reasonApplicationValue, "Diagnosis", "%s", redactText(message))
	}
	return ctrl.Result{}, nil
}

// describeApplicationValue finds the parent of the Application and, when it can,
// the Git file and location of the option, verified by a render of the parent.
func (r *DriftProposalReconciler) describeApplicationValue(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	child *unstructured.Unstructured, opt appvalues.Option) (*backflowv1alpha1.ApplicationValue, error) {
	val := &backflowv1alpha1.ApplicationValue{
		Application: child.GetNamespace() + "/" + child.GetName(), Option: opt.Option, OptionPath: opt.Path,
		Current: opt.Current, Suggested: opt.Suggested,
	}
	state := func(s backflowv1alpha1.ApplicationParentState, name, format string, args ...any) {
		val.Parent = &backflowv1alpha1.ParentApplication{State: s, Name: name, Message: truncateText(fmt.Sprintf(format, args...), 1000)}
	}

	if set := appvalues.ApplicationSetOwner(child); set != "" {
		state(backflowv1alpha1.ParentGeneratedByApplicationSet, child.GetNamespace()+"/"+set,
			"the Application is generated by the ApplicationSet %s; change its template or its generators", set)
		return val, nil
	}
	parents, err := r.parentsOf(ctx, child)
	if err != nil {
		return nil, err
	}
	switch len(parents) {
	case 0:
		state(backflowv1alpha1.ParentNotManagedFromGit, "", "no Application manages the Application %s", val.Application)
		return val, nil
	case 1:
	default:
		names := make([]string, len(parents))
		for i, p := range parents {
			names[i] = p.GetNamespace() + "/" + p.GetName()
		}
		state(backflowv1alpha1.ParentAmbiguous, "", "more than one Application manages it: %s", strings.Join(names, ", "))
		return val, nil
	}
	parent := parents[0]
	parentName := parent.GetNamespace() + "/" + parent.GetName()

	if why := parentSourceProblem(parent); why != "" {
		state(backflowv1alpha1.ParentNotVerifiable, parentName, "%s", why)
		return val, nil
	}
	if sync, _, _ := unstructured.NestedString(parent.Object, "status", "sync", "status"); sync != "Synced" {
		state(backflowv1alpha1.ParentOutOfSync, parentName,
			"the parent Application %s is %s, so what Git says about the Application is not what runs", parentName, orUnknown(sync))
		return val, nil
	}

	located, err := r.locateInParent(ctx, dp, child, parent, opt)
	if err != nil {
		if verdict, ok := errors.AsType[*verdictError](err); ok {
			state(backflowv1alpha1.ParentNotVerifiable, parentName, "%s", verdict.err.Error())
			return val, nil
		}
		return nil, err
	}
	val.Parent = located
	val.Parent.Name = parentName
	return val, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "not synced"
	}
	return s
}

func truncateText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

// parentsOf returns the Applications that manage the Application: the one its
// tracking-id annotation names if it lists the child in status.resources (the
// fast path), otherwise every Application that lists it.
func (r *DriftProposalReconciler) parentsOf(ctx context.Context, child *unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	ref := appvalues.ResourceRef{Group: applicationGVK.Group, Kind: applicationGVK.Kind, Namespace: child.GetNamespace(), Name: child.GetName()}
	if id, ok := appvalues.ParseTrackingID(child.GetAnnotations()[appvalues.TrackingAnnotation]); ok && id.Group == ref.Group && id.Kind == ref.Kind {
		ns := id.AppNamespace
		if ns == "" {
			ns = child.GetNamespace()
		}
		parent := &unstructured.Unstructured{}
		parent.SetGroupVersionKind(applicationGVK)
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: id.AppName}, parent); err == nil && appvalues.Manages(parent, ref) {
			return []*unstructured.Unstructured{parent}, nil
		}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(applicationGVK.GroupVersion().WithKind(applicationGVK.Kind + "List"))
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for i := range list.Items {
		p := &list.Items[i]
		if p.GetNamespace() == child.GetNamespace() && p.GetName() == child.GetName() {
			continue
		}
		if appvalues.Manages(p, ref) {
			out = append(out, p)
		}
	}
	return out, nil
}

// parentSourceProblem says why the parent's Git file cannot be found, or "".
func parentSourceProblem(parent *unstructured.Unstructured) string {
	if sources, ok, _ := unstructured.NestedSlice(parent.Object, "spec", "sources"); ok && len(sources) > 0 {
		return "the parent Application has several sources; only a parent with one Git source is looked at"
	}
	repo, _, _ := unstructured.NestedString(parent.Object, "spec", "source", "repoURL")
	if chart, _, _ := unstructured.NestedString(parent.Object, "spec", "source", "chart"); chart != "" || repo == "" {
		return "the parent Application does not have a Git source (its chart comes from a Helm repository or a registry)"
	}
	if hasCredentials(repo) {
		return "the repository URL of the parent Application contains credentials, which Backflow does not use"
	}
	switch t, _, _ := unstructured.NestedString(parent.Object, "status", "sourceType"); backflowv1alpha1.SourceType(t) {
	case backflowv1alpha1.SourceDirectory, backflowv1alpha1.SourceHelm, backflowv1alpha1.SourceKustomize:
		return ""
	default:
		return fmt.Sprintf("the parent Application has a %q source, which is not mapped", t)
	}
}

// locateInParent maps the option of the child, as a field of the child
// Application resource, to the parent's Git file, with the engine of the parent's
// source type and a render of the parent. With a suggested value the edit sets it,
// and the diff is kept; without, the option is edited to a probe value only to
// find the file, and nothing of the edit is kept.
func (r *DriftProposalReconciler) locateInParent(ctx context.Context, dp *backflowv1alpha1.DriftProposal,
	child, parent *unstructured.Unstructured, opt appvalues.Option) (*backflowv1alpha1.ParentApplication, error) {
	current, ok := jsonAt(child.Object, opt.Path)
	if !ok {
		return nil, &verdictError{reason: rendermap.ReasonRenderMismatch, err: fmt.Errorf("the Application has no %s", opt.Path)}
	}
	live, probe := opt.Suggested, false
	if live == "" {
		if !strings.HasPrefix(current, `"`) {
			return nil, &verdictError{reason: rendermap.ReasonRenderMismatch, err: errors.New("the option holds no string whose place could be looked up without a value")}
		}
		live, probe = probeValue, true
	}
	repo, _, _ := unstructured.NestedString(parent.Object, "spec", "source", "repoURL")
	path, _, _ := unstructured.NestedString(parent.Object, "spec", "source", "path")
	target, _, _ := unstructured.NestedString(parent.Object, "spec", "source", "targetRevision")
	revision, _, _ := unstructured.NestedString(parent.Object, "status", "sync", "revision")
	sourceType, _, _ := unstructured.NestedString(parent.Object, "status", "sourceType")
	if revision == "" {
		return nil, &verdictError{reason: rendermap.ReasonRenderMismatch, err: errors.New("the parent Application has no synced revision yet")}
	}

	synth := dp.DeepCopy()
	synth.Spec.Application = backflowv1alpha1.ObjectRef{Name: parent.GetName(), Namespace: parent.GetNamespace()}
	synth.Spec.Resource = backflowv1alpha1.ResourceRef{
		Group: applicationGVK.Group, Version: applicationGVK.Version, Kind: applicationGVK.Kind,
		Namespace: child.GetNamespace(), Name: child.GetName(),
	}
	synth.Spec.Sources = nil
	synth.Spec.Source = backflowv1alpha1.SourceRef{
		RepoURL: repo, Revision: revision, TargetRevision: target, Path: path, Type: backflowv1alpha1.SourceType(sourceType),
	}
	synth.Spec.Changes = []backflowv1alpha1.FieldChange{{Path: opt.Path, Op: backflowv1alpha1.OpReplace, Desired: current, Live: live}}

	auth, err := r.authForParent(ctx, dp, repo)
	if err != nil {
		return nil, err
	}
	tree, err := r.Repos.Open(ctx, repo, revision, auth)
	if err != nil {
		if errors.Is(err, gitrepo.ErrRevisionNotFound) {
			return nil, &verdictError{reason: reasonRevisionNotFound, err: err}
		}
		return nil, err
	}
	defer func() { _ = tree.Close() }()

	change, err := r.mapTree(withParentLookup(ctx), synth, parent, tree, revision)
	if err != nil {
		if _, ok := errors.AsType[*heldError](err); ok {
			return nil, err
		}
		if verdict, ok := errors.AsType[*verdictError](err); ok {
			return nil, &verdictError{reason: verdict.reason, err: fmt.Errorf("a render of the parent Application with the edit does not verify it: %w", verdict.err)}
		}
		return nil, err
	}
	if backflowv1alpha1.SourceType(sourceType) == backflowv1alpha1.SourceDirectory && !removesALine(change.diff()) {
		// A Directory edit is not rendered: a replacement of a field the file does
		// not have is an addition, and then the file does not define the option.
		return nil, &verdictError{reason: rendermap.ReasonNoVerifiedCandidate, err: errors.New("the parent's file does not hold the current value of the option")}
	}
	edits := change.edits()
	if len(edits) == 0 {
		return nil, &verdictError{reason: rendermap.ReasonNoVerifiedCandidate, err: errors.New("the parent's render shows no edit for the option")}
	}
	logf.FromContext(ctx).V(1).Info("Located the Application option in its parent", "proposal", dp.Name, "parent", parent.GetName(), "file", edits[0].File)
	located := &backflowv1alpha1.ParentApplication{
		State: backflowv1alpha1.ParentLocated, RepoURL: scm.RedactURL(repo), Revision: revision,
		File: edits[0].File, Location: edits[0].Location,
	}
	if !probe {
		located.Diff = truncateDiff(change.diff())
	}
	return located, nil
}

// authForParent returns the credentials for the parent's repository: the
// ScmConnection of the proposal's namespace that matches its host, or nil.
func (r *DriftProposalReconciler) authForParent(ctx context.Context, dp *backflowv1alpha1.DriftProposal, repo string) (*gitrepo.Auth, error) {
	var conns backflowv1alpha1.ScmConnectionList
	if err := r.List(ctx, &conns, client.InNamespace(dp.Namespace)); err != nil {
		return nil, err
	}
	access, err := scmAccessByName(ctx, r.Client, dp.Namespace, connectionFor(repo, conns.Items))
	if err != nil {
		return nil, &heldError{reason: reasonRepositoryAuthFailed, cause: err}
	}
	if access == nil {
		return nil, nil
	}
	return access.auth, nil
}

// jsonAt returns the JSON of the value at a JSON pointer of an object.
func jsonAt(obj map[string]any, pointer string) (string, bool) {
	var cur any = obj
	if pointer == "" {
		return "", false
	}
	for seg := range strings.SplitSeq(pointer[1:], "/") {
		seg = strings.NewReplacer("~1", "/", "~0", "~").Replace(seg)
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[seg]
			if !ok {
				return "", false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return "", false
			}
			cur = c[i]
		default:
			return "", false
		}
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// applicationSentence is the one sentence of the diagnosis: which option of which
// Application sets the value, and where it is defined.
func applicationSentence(val *backflowv1alpha1.ApplicationValue, options []appvalues.Option) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Set by %s of the Application %s", val.Option, val.Application)
	if len(options) > 1 {
		more := make([]string, 0, len(options)-1)
		for _, o := range options[1:] {
			more = append(more, o.Option)
		}
		fmt.Fprintf(&b, " (and by %s)", strings.Join(more, ", "))
	}
	p := val.Parent
	switch {
	case p == nil:
	case p.State == backflowv1alpha1.ParentLocated:
		fmt.Fprintf(&b, ", which is defined in %s of %s (Application %s, at %s)", p.File, p.RepoURL, p.Name, p.Location)
		if val.Suggested != "" {
			fmt.Fprintf(&b, "; set it to %s there", val.Suggested)
		}
	case p.State == backflowv1alpha1.ParentNotManagedFromGit:
		b.WriteString(", which is not managed from Git")
	default:
		fmt.Fprintf(&b, ", %s", p.Message)
	}
	b.WriteString("; no merge request is made, because no Argo CD API renders an Application with a changed option, so the new value cannot be verified")
	return b.String()
}

// removesALine reports whether a unified diff removes a line of the file.
func removesALine(diff string) bool {
	for line := range strings.SplitSeq(diff, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			return true
		}
	}
	return false
}
