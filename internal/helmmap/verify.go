package helmmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/mapping"
)

// Defaults of Options.
const (
	DefaultMaxCandidates = 10
	DefaultBudget        = 2 * time.Minute
)

// Options bound the work of Verify. Every render counts against the budget.
type Options struct {
	// MaxCandidates is the most candidates one field may have; more, and
	// the field is not tried (TooManyCandidates). Zero means
	// DefaultMaxCandidates.
	MaxCandidates int
	// Budget is the time all renders of one Verify may take together. Zero
	// means DefaultBudget.
	Budget time.Duration
}

// ResourceKey identifies a rendered resource.
type ResourceKey struct {
	Group, Kind, Namespace, Name string
}

func (k ResourceKey) String() string {
	g := k.Group
	if g != "" {
		g += "/"
	}
	return fmt.Sprintf("%s%s %s/%s", g, k.Kind, k.Namespace, k.Name)
}

func keyOf(u *unstructured.Unstructured) ResourceKey {
	return ResourceKey{Group: u.GroupVersionKind().Group, Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName()}
}

// Field is one drifted field of the resource and the value files that could
// feed it.
type Field struct {
	// Index is the position of the change in the proposal; the edits made for
	// it carry it as ChangeIndex.
	Index int
	// Change is a Replace of a scalar: Desired is the value the render gives,
	// Live the value in the cluster.
	Change v1alpha1.FieldChange
	// Found is Source.Find for Desired.
	Found *Result
}

// Input is a proposal to verify: some fields of one resource.
type Input struct {
	App      App
	Resource ResourceKey
	Fields   []Field
	Source   *Source
	// Files are the unmodified files of the render (CollectFiles).
	Files map[string][]byte
}

// FieldFailure says why one field could not be mapped.
type FieldFailure struct {
	Pointer string
	Reason  string
	Detail  string
}

// Unmapped is the answer when a proposal cannot be traced back with
// certainty. Reason is machine-readable (the Reason* constants); Message is
// for people and names the fields.
type Unmapped struct {
	Reason  string
	Message string
	Fields  []FieldFailure
}

// Outcome is the result of Verify: edits that are verified, or Unmapped.
type Outcome struct {
	// Results holds one entry per edited value file, with the edited file, the
	// diff and the edits (ChangeIndex is the field's Index). Set when mapped.
	Results []mapping.Result
	// Unmapped is set when the proposal cannot be mapped.
	Unmapped *Unmapped
	// Renders is the number of renders done.
	Renders int
}

// Mapped reports whether the edits are verified.
func (o *Outcome) Mapped() bool { return o.Unmapped == nil }

// leaves is a rendered resource flattened to JSON pointers and canonical JSON
// scalars. Empty maps and lists are leaves of their own.
type leaves map[string]string

type snapshot map[ResourceKey]leaves

func flatten(prefix string, v any, out leaves) {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			out[prefix] = "{}"
			return
		}
		for k, c := range x {
			flatten(prefix+"/"+strings.NewReplacer("~", "~0", "/", "~1").Replace(k), c, out)
		}
	case []any:
		if len(x) == 0 {
			out[prefix] = "[]"
			return
		}
		for i, c := range x {
			flatten(fmt.Sprintf("%s/%d", prefix, i), c, out)
		}
	default:
		b, _ := json.Marshal(x)
		out[prefix] = string(b)
	}
}

func takeSnapshot(objs []*unstructured.Unstructured) (snapshot, error) {
	s := snapshot{}
	for _, o := range objs {
		k := keyOf(o)
		if _, dup := s[k]; dup {
			return nil, fmt.Errorf("%w: %s", errDuplicateResource, k)
		}
		l := leaves{}
		for f, v := range o.Object {
			flatten("/"+f, v, l)
		}
		s[k] = l
	}
	return s, nil
}

// class groups resources that could be the same one under a random name.
type class struct{ Group, Kind, Namespace string }

func classOf(k ResourceKey) class { return class{k.Group, k.Kind, k.Namespace} }

// nondeterminism is what differs between two renders of the same files.
type nondeterminism struct {
	// paths are the leaves of a resource that differ.
	paths map[ResourceKey]map[string]bool
	// classes are the resources that exist in one render only: random names.
	// Adding or removing a resource of such a class is not a change.
	classes map[class]bool
}

func findNondeterminism(a, b snapshot) nondeterminism {
	nd := nondeterminism{paths: map[ResourceKey]map[string]bool{}, classes: map[class]bool{}}
	for k, la := range a {
		lb, ok := b[k]
		if !ok {
			nd.classes[classOf(k)] = true
			continue
		}
		for p := range unionKeys(la, lb) {
			va, oka := la[p]
			vb, okb := lb[p]
			if oka != okb || va != vb {
				if nd.paths[k] == nil {
					nd.paths[k] = map[string]bool{}
				}
				nd.paths[k][p] = true
			}
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			nd.classes[classOf(k)] = true
		}
	}
	return nd
}

func unionKeys(a, b leaves) map[string]bool {
	u := make(map[string]bool, len(a)+len(b))
	for k := range a {
		u[k] = true
	}
	for k := range b {
		u[k] = true
	}
	return u
}

// change is one difference between two renders. Pointer is empty when a whole
// resource appeared or disappeared.
type change struct {
	Key     ResourceKey
	Pointer string
}

func (c change) String() string {
	if c.Pointer == "" {
		return c.Key.String()
	}
	return c.Key.String() + c.Pointer
}

// diff lists what differs between base and other across all resources,
// leaving out what is nondeterministic. The order is stable.
func diff(base, other snapshot, nd nondeterminism) []change {
	var out []change
	keys := slices.SortedFunc(maps.Keys(unionSnap(base, other)), func(a, b ResourceKey) int {
		return strings.Compare(a.String(), b.String())
	})
	for _, k := range keys {
		lb, inBase := base[k]
		lo, inOther := other[k]
		if !inBase || !inOther {
			if !nd.classes[classOf(k)] {
				out = append(out, change{Key: k})
			}
			continue
		}
		for _, p := range slices.Sorted(maps.Keys(unionKeys(lb, lo))) {
			vb, okb := lb[p]
			vo, oko := lo[p]
			if okb == oko && vb == vo {
				continue
			}
			if nd.paths[k][p] {
				continue
			}
			out = append(out, change{Key: k, Pointer: p})
		}
	}
	return out
}

func unionSnap(a, b snapshot) map[ResourceKey]bool {
	u := make(map[ResourceKey]bool, len(a)+len(b))
	for k := range a {
		u[k] = true
	}
	for k := range b {
		u[k] = true
	}
	return u
}

// parsedField is a field with its values decoded.
type parsedField struct {
	Field
	desired, live string // canonical JSON
	newValue      string // the live value as a template renders it
}

func parseField(f Field) (parsedField, error) {
	p := parsedField{Field: f}
	if f.Change.Op != v1alpha1.OpReplace {
		return p, fmt.Errorf("%s of %s is not a replaced value", f.Change.Op, f.Change.Path)
	}
	for _, v := range []struct {
		raw string
		dst *string
	}{{f.Change.Desired, &p.desired}, {f.Change.Live, &p.live}} {
		var x any
		if err := json.Unmarshal([]byte(v.raw), &x); err != nil {
			return p, fmt.Errorf("%s: %w", f.Change.Path, err)
		}
		switch x.(type) {
		case string, float64, bool:
		default:
			return p, fmt.Errorf("%s is not a string, number or bool", f.Change.Path)
		}
		b, _ := json.Marshal(x)
		*v.dst = string(b)
		if v.dst == &p.live {
			p.newValue = RenderValue(x)
		}
	}
	return p, nil
}

// run holds the state of one Verify.
type run struct {
	ctx     context.Context //nolint:containedctx // scoped to one call
	r       Renderer
	in      Input
	renders int
	// budget is set once the time budget ran out.
	budget bool
}

var (
	errBudget            = errors.New("render budget exceeded")
	errDuplicateResource = errors.New("the render has two resources with the same identity")
)

func (v *run) render(files map[string][]byte) (snapshot, error) {
	if v.ctx.Err() != nil {
		return nil, v.budgetOr(v.ctx.Err())
	}
	v.renders++
	objs, err := v.r.Render(v.ctx, v.in.App, files)
	if err != nil {
		return nil, v.budgetOr(err)
	}
	return takeSnapshot(objs)
}

// budgetOr turns a failure caused by the deadline into errBudget. The parent
// context is checked by the caller of Verify, before and after.
func (v *run) budgetOr(err error) error {
	if errors.Is(v.ctx.Err(), context.DeadlineExceeded) {
		v.budget = true
		return errBudget
	}
	return err
}

// Verify decides whether the drifted fields of a resource can be traced back
// to edits in value files, by rendering. It never guesses: an edit is
// accepted only when a render with that edit alone differs from the render
// without it in the drifted field and nowhere else, and the new value is the
// live value. It returns an error for failures a retry can fix (Argo CD
// unreachable, say); everything else about the proposal is an Unmapped
// Outcome.
func Verify(ctx context.Context, r Renderer, in Input, opts Options) (*Outcome, error) {
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = DefaultMaxCandidates
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	bctx, cancel := context.WithTimeout(ctx, opts.Budget)
	defer cancel()
	v := &run{ctx: bctx, r: r, in: in}

	out, err := v.verify(opts)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, errBudget) {
			return &Outcome{Renders: v.renders, Unmapped: &Unmapped{
				Reason:  ReasonRenderBudgetExceeded,
				Message: fmt.Sprintf("the renders needed to verify the edits did not finish within %s", opts.Budget),
			}}, nil
		}
		return nil, err
	}
	out.Renders = v.renders
	return out, nil
}

func unmapped(reason, format string, args ...any) *Outcome {
	return &Outcome{Unmapped: &Unmapped{Reason: reason, Message: fmt.Sprintf(format, args...)}}
}

func (v *run) verify(opts Options) (*Outcome, error) {
	in := v.in
	if len(in.Fields) == 0 {
		return nil, errors.New("no drifted fields")
	}
	// A Secret's data is masked by Argo CD, and a Secret value must not be
	// proposed to Git.
	if in.Resource.Group == "" && in.Resource.Kind == "Secret" {
		return unmapped(ReasonSecretNotVerifiable, "%s is a Secret: Argo CD masks its data, and Secret values are never proposed to Git", in.Resource), nil
	}
	fields, fail := preflight(in.Fields, opts)
	if fail != nil {
		return fail, nil
	}

	// The unmodified files, twice: what differs between the two is
	// nondeterministic.
	base, nd, out, err := v.baseline(fields)
	if out != nil || err != nil {
		return out, err
	}

	accepted, rejected, err := v.tryCandidates(base, nd, fields)
	if err != nil {
		return v.renderErr(err)
	}
	var bad []FieldFailure
	for i, f := range fields {
		switch len(accepted[i]) {
		case 1:
		case 0:
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonNoVerifiedCandidate, Detail: noCandidateDetail(f, rejected[i])})
		default:
			names := make([]string, len(accepted[i]))
			for j, c := range accepted[i] {
				names[j] = c.File + " " + c.Path.String()
			}
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonAmbiguousCandidates,
				Detail: "each of these gives the live value and changes nothing else: " + strings.Join(names, "; ")})
		}
	}
	if len(bad) > 0 {
		return failure(bad), nil
	}

	chosen := make([]Candidate, len(fields))
	for i := range fields {
		chosen[i] = accepted[i][0]
	}
	return v.combine(base, nd, fields, chosen)
}

// preflight parses the fields and applies the limits that need no render.
func preflight(in []Field, opts Options) ([]parsedField, *Outcome) {
	fields := make([]parsedField, len(in))
	var bad []FieldFailure
	for i, f := range in {
		p, err := parseField(f)
		if err != nil {
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonUnsupportedChange, Detail: err.Error()})
		}
		fields[i] = p
	}
	if len(bad) > 0 {
		return nil, failure(bad)
	}
	for _, f := range fields {
		if n := len(f.Found.Candidates); n > opts.MaxCandidates {
			return nil, failure([]FieldFailure{{Pointer: f.Change.Path, Reason: ReasonTooManyCandidates,
				Detail: fmt.Sprintf("%d value file keys hold %s, at most %d are tried", n, f.desired, opts.MaxCandidates)}})
		}
	}
	return fields, nil
}

// baseline renders the unmodified files twice and checks that the drifted
// fields are what the render gives. It returns an Outcome when they are not.
func (v *run) baseline(fields []parsedField) (snapshot, nondeterminism, *Outcome, error) {
	in := v.in
	base, err := v.render(in.Files)
	if err != nil {
		out, err := v.baseFailure(err)
		return nil, nondeterminism{}, out, err
	}
	again, err := v.render(in.Files)
	if err != nil {
		out, err := v.baseFailure(err)
		return nil, nondeterminism{}, out, err
	}
	nd := findNondeterminism(base, again)

	res, ok := base[in.Resource]
	if !ok {
		return nil, nd, unmapped(ReasonRenderMismatch, "the render has no %s", in.Resource), nil
	}
	var bad []FieldFailure
	for _, f := range fields {
		if nd.paths[in.Resource][f.Change.Path] {
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonNondeterministicField,
				Detail: "the rendered value differs between two renders of the same files"})
		} else if got, has := res[f.Change.Path]; !has || got != f.desired {
			detail := fmt.Sprintf("the render gives %s, the proposal expects %s", got, f.desired)
			if !has {
				detail = fmt.Sprintf("the render has no such field, the proposal expects %s", f.desired)
			}
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonRenderMismatch, Detail: detail})
		}
	}
	if len(bad) > 0 {
		return nil, nd, failure(bad), nil
	}
	return base, nd, nil, nil
}

// tryCandidates renders every candidate of every field on its own and sorts
// them into accepted and rejected (with the reason).
func (v *run) tryCandidates(base snapshot, nd nondeterminism, fields []parsedField) (accepted [][]Candidate, rejected [][]string, err error) {
	accepted = make([][]Candidate, len(fields))
	rejected = make([][]string, len(fields))
	for i, f := range fields {
		for _, c := range f.Found.Candidates {
			files, why := tryEdit(v.in.Files, c, f.newValue)
			if why == "" {
				snap, err := v.render(files)
				switch {
				case errors.Is(err, ErrRenderFailed):
					why = "the chart does not render with it"
				case err != nil:
					return nil, nil, err
				default:
					why = judge(base, snap, nd, v.in.Resource, f)
				}
			}
			if why != "" {
				rejected[i] = append(rejected[i], fmt.Sprintf("%s %s: %s", c.File, c.Path, why))
				continue
			}
			accepted[i] = append(accepted[i], c)
		}
	}
	return accepted, rejected, nil
}

// combine puts the chosen edits together. Each was verified on its own; with
// several fields they are rendered once more, all together.
func (v *run) combine(base snapshot, nd nondeterminism, fields []parsedField, chosen []Candidate) (*Outcome, error) {
	if len(fields) > 1 {
		if why := conflicts(fields, chosen); why != "" {
			return unmapped(ReasonEditsInteract, "%s", why), nil
		}
	}
	files, edits, err := applyAll(v.in.Files, fields, chosen)
	if err != nil {
		return unmapped(ReasonEditsInteract, "the edits cannot be applied together: %v", err), nil
	}
	if len(fields) > 1 {
		snap, err := v.render(files)
		switch {
		case errors.Is(err, ErrRenderFailed):
			return unmapped(ReasonEditsInteract, "the chart does not render with all the edits together"), nil
		case err != nil:
			return v.renderErr(err)
		}
		if why := judgeChanges(diff(base, snap, nd), snap, v.in.Resource, fields); why != "" {
			return unmapped(ReasonEditsInteract, "the edits verified one by one do not work together: %s", why), nil
		}
	}
	return &Outcome{Results: buildResults(v.in.Files, files, edits)}, nil
}

// baseFailure turns an error of a render of the unmodified files into an
// Outcome when it is about the chart, and into an error when a retry may help.
func (v *run) baseFailure(err error) (*Outcome, error) {
	switch {
	case errors.Is(err, ErrRenderFailed):
		return unmapped(ReasonRenderFailed, "the chart does not render: %v", err), nil
	case errors.Is(err, ErrFilesTooLarge):
		return unmapped(ReasonFilesTooLarge, "%v", err), nil
	case errors.Is(err, errDuplicateResource):
		return unmapped(ReasonRenderMismatch, "%v", err), nil
	}
	return nil, err
}

func (v *run) renderErr(err error) (*Outcome, error) {
	if errors.Is(err, ErrFilesTooLarge) {
		return unmapped(ReasonFilesTooLarge, "%v", err), nil
	}
	return nil, err
}

func failure(fields []FieldFailure) *Outcome {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%s: %s (%s)", f.Pointer, f.Reason, f.Detail))
	}
	reason := fields[0].Reason
	msg := strings.Join(parts, "; ")
	if len(fields) > 1 {
		msg = fmt.Sprintf("%d fields cannot be mapped: %s", len(fields), msg)
	}
	return &Outcome{Unmapped: &Unmapped{Reason: reason, Message: msg, Fields: fields}}
}

// tryEdit applies one candidate's edit to a copy of the files.
func tryEdit(base map[string][]byte, c Candidate, value string) (files map[string][]byte, why string) {
	content, ok := base[c.File]
	if !ok {
		return nil, "the file was not provided"
	}
	w, err := SetValue(c.File, content, c.Path, value)
	if err != nil {
		return nil, err.Error()
	}
	files = maps.Clone(base)
	files[c.File] = w.Edited
	return files, ""
}

// judge decides whether a render with one candidate's edit is exactly the
// base plus the drifted field at its live value. It returns the reason when
// not.
func judge(base, edited snapshot, nd nondeterminism, res ResourceKey, f parsedField) string {
	return judgeChanges(diff(base, edited, nd), edited, res, []parsedField{f})
}

func judgeChanges(changes []change, edited snapshot, res ResourceKey, fields []parsedField) string {
	want := map[string]parsedField{}
	for _, f := range fields {
		want[f.Change.Path] = f
	}
	var extra []string
	seen := map[string]bool{}
	for _, c := range changes {
		if c.Key == res {
			if _, ok := want[c.Pointer]; ok {
				seen[c.Pointer] = true
				continue
			}
		}
		extra = append(extra, c.String())
	}
	if len(extra) > 0 {
		shown := extra
		if len(shown) > 3 {
			shown = append(slices.Clone(shown[:3]), fmt.Sprintf("and %d more", len(extra)-3))
		}
		return "it also changes " + strings.Join(shown, ", ")
	}
	for p, f := range want {
		if !seen[p] {
			return fmt.Sprintf("it does not change %s", p)
		}
		if got := edited[res][p]; got != f.live {
			return fmt.Sprintf("it makes %s %s, not the live %s", p, got, f.live)
		}
	}
	return ""
}

// conflicts reports edits that cannot both be right: the same key set to two
// different values.
func conflicts(fields []parsedField, chosen []Candidate) string {
	for i := range chosen {
		for j := i + 1; j < len(chosen); j++ {
			if chosen[i].File == chosen[j].File && chosen[i].Path.Equal(chosen[j].Path) && fields[i].newValue != fields[j].newValue {
				return fmt.Sprintf("%s and %s are fed by the same key (%s in %s) but are live with different values",
					fields[i].Change.Path, fields[j].Change.Path, chosen[i].Path, chosen[i].File)
			}
		}
	}
	return ""
}

// edit is one applied change, kept for the result.
type edit struct {
	field parsedField
	cand  Candidate
	w     *Written
}

// applyAll applies the chosen edits one after the other. Two fields that
// chose the same key are one edit.
func applyAll(base map[string][]byte, fields []parsedField, chosen []Candidate) (map[string][]byte, []edit, error) {
	files := maps.Clone(base)
	var edits []edit
	for i, c := range chosen {
		done := false
		for _, e := range edits {
			if e.cand.File == c.File && e.cand.Path.Equal(c.Path) {
				edits = append(edits, edit{field: fields[i], cand: c, w: e.w})
				done = true
				break
			}
		}
		if done {
			continue
		}
		w, err := SetValue(c.File, files[c.File], c.Path, fields[i].newValue)
		if err != nil {
			return nil, nil, err
		}
		files[c.File] = w.Edited
		edits = append(edits, edit{field: fields[i], cand: c, w: w})
	}
	return files, edits, nil
}

func buildResults(base, final map[string][]byte, edits []edit) []mapping.Result {
	byFile := map[string]*mapping.Result{}
	var order []string
	for _, e := range edits {
		r := byFile[e.cand.File]
		if r == nil {
			r = &mapping.Result{File: e.cand.File, Edited: final[e.cand.File],
				Diff: mapping.UnifiedDiff(e.cand.File, base[e.cand.File], final[e.cand.File])}
			byFile[e.cand.File] = r
			order = append(order, e.cand.File)
		}
		r.Edits = append(r.Edits, mapping.Edit{
			File: e.cand.File, Location: e.cand.Path.pointer(), Value: e.w.Edits[0].Value, ChangeIndex: e.field.Index,
		})
	}
	out := make([]mapping.Result, 0, len(order))
	for _, f := range order {
		out = append(out, *byFile[f])
	}
	return out
}

// noCandidateDetail explains why a field has no verified candidate.
func noCandidateDetail(f parsedField, rejected []string) string {
	var parts []string
	for _, p := range f.Found.ChartDefaults {
		parts = append(parts, fmt.Sprintf("the value comes from the chart's own defaults (%s); chart defaults are not edited", p))
	}
	for _, c := range f.Found.Controlled {
		parts = append(parts, fmt.Sprintf("the value is set by %s at %s in the Application, not by a value file", c.Layer, c.Path))
	}
	if len(f.Found.RemoteValueFiles) > 0 {
		parts = append(parts, "it may come from a remote value file: "+strings.Join(f.Found.RemoteValueFiles, ", "))
	}
	parts = append(parts, rejected...)
	if len(parts) == 0 {
		return "no value file holds " + f.desired + "; the field may be derived from a value by the chart"
	}
	return strings.Join(parts, "; ")
}
