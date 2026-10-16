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
	// NearMiss is the best edit that reproduces the drifted field but also
	// changes other fields. Set only with NoVerifiedCandidate.
	NearMiss *NearMiss
}

// Collateral is a field an edit changes although it did not drift.
type Collateral struct {
	Resource ResourceKey
	// Pointer is the JSON pointer of the field; empty when a whole resource
	// appears or disappears.
	Pointer string
	// Live is the value now: not drifted, so the value of the unmodified
	// render. After is the value once the edit is made. Both are JSON, or
	// "(absent)" for a field that does not exist on that side.
	Live, After string
}

func (c Collateral) String() string {
	if c.Pointer == "" {
		return fmt.Sprintf("%s (%s before the edit, %s after)", c.Resource, c.Live, c.After)
	}
	return fmt.Sprintf("%s%s (live %s, after the edit %s)", c.Resource, c.Pointer, c.Live, c.After)
}

// NearMiss is an edit that came close: it reproduces drifted fields, but
// changes other fields too, so the merge request would move more than the
// cluster did. The proposal stays unmapped; this is for the person who reads
// why.
type NearMiss struct {
	File string
	Path KeyPath
	// Value is the value the key would be set to, as rendered text.
	Value string
	// Covers are the pointers of the drifted fields the edit reproduces.
	Covers []string
	// Collateral lists the other changes, at most MaxCollateralShown.
	Collateral []Collateral
	// CollateralTotal is how many other changes there are.
	CollateralTotal int
}

// MaxCollateralShown bounds NearMiss.Collateral.
const MaxCollateralShown = 3

func (n *NearMiss) String() string {
	parts := make([]string, len(n.Collateral))
	for i, c := range n.Collateral {
		parts[i] = c.String()
	}
	more := ""
	if n.CollateralTotal > len(n.Collateral) {
		more = fmt.Sprintf(", and %d more", n.CollateralTotal-len(n.Collateral))
	}
	return fmt.Sprintf("setting %s in %s to %s would reproduce %s but also changes %s%s",
		n.Path, n.File, n.Value, strings.Join(n.Covers, ", "), strings.Join(parts, ", "), more)
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

	tries, err := v.tryEdits(base, nd, fields)
	if err != nil {
		return v.renderErr(err)
	}
	var accepted []*tryout
	for _, t := range tries {
		if t.accepted() {
			accepted = append(accepted, t)
		}
	}
	if bad := uncovered(fields, tries, accepted); len(bad) > 0 {
		return failure(bad), nil
	}

	sets, tooMany := coverSets(len(fields), accepted)
	if tooMany {
		return unmapped(ReasonAmbiguousCandidates, "more than %d different sets of edits reproduce the drift", maxEditSets), nil
	}
	type verifiedSet struct {
		set     []*tryout
		final   map[string][]byte
		applied []appliedEdit
	}
	var verified []verifiedSet
	var interacted []string
	for _, set := range sets {
		files, applied, err := applySet(v.in.Files, fields, set)
		if err != nil {
			interacted = append(interacted, fmt.Sprintf("%s: %v", describeSet(set), err))
			continue
		}
		if len(set) > 1 {
			snap, err := v.render(files)
			switch {
			case errors.Is(err, ErrRenderFailed):
				interacted = append(interacted, describeSet(set)+": the chart does not render with all the edits together")
				continue
			case err != nil:
				return v.renderErr(err)
			}
			if why := judgeChanges(diff(base, snap, nd), snap, v.in.Resource, fields); why != "" {
				interacted = append(interacted, describeSet(set)+": "+why)
				continue
			}
		}
		verified = append(verified, verifiedSet{set, files, applied})
	}
	switch {
	case len(verified) == 0:
		return unmapped(ReasonEditsInteract, "the edits verified one by one do not work together: %s", strings.Join(interacted, "; ")), nil
	case len(verified) > 1:
		names := make([]string, len(verified))
		for i, vs := range verified {
			names[i] = describeSet(vs.set)
		}
		return failure([]FieldFailure{{Pointer: pointers(fields), Reason: ReasonAmbiguousCandidates,
			Detail: "each of these reproduces the drift and changes nothing else: " + strings.Join(names, "; ")}}), nil
	}
	return &Outcome{Results: buildResults(v.in.Files, verified[0].final, verified[0].applied)}, nil
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

// editKey is one key of a value file set to one value.
type editKey struct{ File, Path, Value string }

// tryout is one edit and what a render with only that edit showed.
type tryout struct {
	cand  Candidate
	value string // the live value as a template renders it
	// origin lists the fields whose candidates proposed this edit.
	origin []int
	// why is set when the edit could not be tried: it cannot be written, or
	// the chart does not render with it.
	why string
	// covers are the drifted fields the edit changed to their live value.
	covers []int
	// collateral are all other changes the edit made.
	collateral []Collateral
}

// accepted: the edit reproduces drifted fields and changes nothing else.
func (t *tryout) accepted() bool {
	return t.why == "" && len(t.covers) > 0 && len(t.collateral) == 0
}

func (t *tryout) key() string { return t.cand.File + " " + t.cand.Path.String() }

// conflicts reports two edits that set the same key to different values.
func conflicts(a, b *tryout) bool {
	return a.cand.File == b.cand.File && a.cand.Path.Equal(b.cand.Path) && a.value != b.value
}

// tryEdits renders every candidate edit of every field on its own. An edit
// proposed for several fields is tried once.
func (v *run) tryEdits(base snapshot, nd nondeterminism, fields []parsedField) ([]*tryout, error) {
	var tries []*tryout
	index := map[editKey]*tryout{}
	for i, f := range fields {
		for _, c := range f.Found.Candidates {
			k := editKey{c.File, c.Path.String(), f.newValue}
			if t, ok := index[k]; ok {
				t.origin = append(t.origin, i)
				continue
			}
			t := &tryout{cand: c, value: f.newValue, origin: []int{i}}
			index[k] = t
			tries = append(tries, t)
		}
	}
	byPointer := map[string]int{}
	for i, f := range fields {
		byPointer[f.Change.Path] = i
	}
	for _, t := range tries {
		files, why := tryEdit(v.in.Files, t.cand, t.value)
		if why != "" {
			t.why = why
			continue
		}
		snap, err := v.render(files)
		switch {
		case errors.Is(err, ErrRenderFailed):
			t.why = "the chart does not render with it"
			continue
		case err != nil:
			return nil, err
		}
		for _, ch := range diff(base, snap, nd) {
			if i, ok := byPointer[ch.Pointer]; ok && ch.Key == v.in.Resource && snap[ch.Key][ch.Pointer] == fields[i].live {
				t.covers = append(t.covers, i)
				continue
			}
			t.collateral = append(t.collateral, collateralOf(base, snap, ch))
		}
	}
	return tries, nil
}

func collateralOf(base, edited snapshot, ch change) Collateral {
	const absent = "(absent)"
	if ch.Pointer == "" {
		c := Collateral{Resource: ch.Key, Live: absent, After: absent}
		if _, ok := base[ch.Key]; ok {
			c.Live = "(resource exists)"
		}
		if _, ok := edited[ch.Key]; ok {
			c.After = "(resource exists)"
		}
		return c
	}
	val := func(s snapshot) string {
		if v, ok := s[ch.Key][ch.Pointer]; ok {
			return v
		}
		return absent
	}
	return Collateral{Resource: ch.Key, Pointer: ch.Pointer, Live: val(base), After: val(edited)}
}

// uncovered returns a failure for each drifted field that no accepted edit
// reproduces, with the reasons and the nearest miss.
func uncovered(fields []parsedField, tries, accepted []*tryout) []FieldFailure {
	covered := make([]bool, len(fields))
	for _, t := range accepted {
		for _, i := range t.covers {
			covered[i] = true
		}
	}
	var bad []FieldFailure
	for i, f := range fields {
		if covered[i] {
			continue
		}
		var near *tryout
		var others []string
		for _, t := range tries {
			if !slices.Contains(t.origin, i) && !slices.Contains(t.covers, i) {
				continue
			}
			switch {
			case t.why != "":
				others = append(others, t.key()+": "+t.why)
			case slices.Contains(t.covers, i) && len(t.collateral) > 0:
				if near == nil || len(t.collateral) < len(near.collateral) ||
					(len(t.collateral) == len(near.collateral) && len(t.covers) > len(near.covers)) {
					near = t
				}
			case len(t.covers) == 0:
				others = append(others, t.key()+": it does not reproduce the drift")
			}
		}
		ff := FieldFailure{Pointer: f.Change.Path, Reason: ReasonNoVerifiedCandidate}
		if near != nil {
			nm := &NearMiss{File: near.cand.File, Path: near.cand.Path, Value: near.value, CollateralTotal: len(near.collateral)}
			for _, c := range near.covers {
				nm.Covers = append(nm.Covers, fields[c].Change.Path)
			}
			nm.Collateral = near.collateral[:min(len(near.collateral), MaxCollateralShown)]
			ff.NearMiss = nm
			others = append(others, nm.String())
		}
		ff.Detail = noCandidateDetail(f, others)
		bad = append(bad, ff)
	}
	return bad
}

// maxEditSets bounds the sets of edits that are tried together.
const maxEditSets = 32

// coverSets returns the minimal sets of accepted edits that together
// reproduce all n drifted fields, without two edits of the same key to
// different values. tooMany is set when there are more than maxEditSets.
func coverSets(n int, accepted []*tryout) (sets [][]*tryout, tooMany bool) {
	seen := map[string]bool{}
	var all [][]int
	var walk func(chosen []int, covered []bool)
	walk = func(chosen []int, covered []bool) {
		if len(all) > 4*maxEditSets {
			return
		}
		first := slices.Index(covered, false)
		if first < 0 {
			sorted := slices.Sorted(slices.Values(chosen))
			if k := fmt.Sprint(sorted); !seen[k] {
				seen[k] = true
				all = append(all, sorted)
			}
			return
		}
		for j, t := range accepted {
			if !slices.Contains(t.covers, first) || slices.Contains(chosen, j) {
				continue
			}
			clash := false
			for _, c := range chosen {
				clash = clash || conflicts(accepted[c], t)
			}
			if clash {
				continue
			}
			next := slices.Clone(covered)
			for _, c := range t.covers {
				next[c] = true
			}
			walk(append(slices.Clone(chosen), j), next)
		}
	}
	walk(nil, make([]bool, n))

	// Keep the minimal ones: no other set is a proper subset.
	for _, a := range all {
		minimal := true
		for _, b := range all {
			if len(b) < len(a) && isSubset(b, a) {
				minimal = false
				break
			}
		}
		if minimal {
			set := make([]*tryout, len(a))
			for i, j := range a {
				set[i] = accepted[j]
			}
			sets = append(sets, set)
		}
	}
	return sets, len(sets) > maxEditSets
}

func isSubset(sub, of []int) bool {
	for _, x := range sub {
		if !slices.Contains(of, x) {
			return false
		}
	}
	return true
}

func describeSet(set []*tryout) string {
	names := make([]string, len(set))
	for i, t := range set {
		names[i] = t.key()
	}
	return strings.Join(names, " + ")
}

func pointers(fields []parsedField) string {
	p := make([]string, len(fields))
	for i, f := range fields {
		p[i] = f.Change.Path
	}
	return strings.Join(p, ", ")
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

// appliedEdit is a written edit, kept for the result.
type appliedEdit struct {
	cand        Candidate
	w           *Written
	changeIndex int
}

// applySet applies the edits of a set one after the other to a copy of the
// files.
func applySet(base map[string][]byte, fields []parsedField, set []*tryout) (map[string][]byte, []appliedEdit, error) {
	files := maps.Clone(base)
	var applied []appliedEdit
	for _, t := range set {
		w, err := SetValue(t.cand.File, files[t.cand.File], t.cand.Path, t.value)
		if err != nil {
			return nil, nil, err
		}
		files[t.cand.File] = w.Edited
		applied = append(applied, appliedEdit{cand: t.cand, w: w, changeIndex: fields[t.covers[0]].Index})
	}
	return files, applied, nil
}

func buildResults(base, final map[string][]byte, edits []appliedEdit) []mapping.Result {
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
			File: e.cand.File, Location: e.cand.Path.pointer(), Value: e.w.Edits[0].Value, ChangeIndex: e.changeIndex,
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
