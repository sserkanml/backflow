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
	// AllowCollateral lists the kinds of changes beyond the drifted fields an
	// edit may also make: CollateralLabels and CollateralAnnotations. Empty
	// means none. They are only used when no edit verifies without them.
	AllowCollateral []string
	// AllowPartial maps the fields that can be mapped when others cannot: a
	// field that cannot be traced back (an unsupported kind of change, or no
	// verified key) is left out, and Outcome.LeftOut says why. The edits of the
	// others are verified as before, and must not change any left-out field.
	// Without it, one field that cannot be mapped makes the proposal Unmapped.
	AllowPartial bool
	// AddOverrides lets an edit add a key to the Application's last value file
	// to override a default that only the chart defines (Result.Overrides). Such
	// edits are tried only when no edit of a key the value files already define
	// verifies, and are verified like any other edit.
	AddOverrides bool
}

// The kinds of collateral changes an edit may be allowed to make.
const (
	CollateralLabels      = "Labels"
	CollateralAnnotations = "Annotations"
)

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
	// Fragments is Source.FindFragments for Desired and Live, when both are
	// strings; nil otherwise. Its candidates are tried only when no edit of
	// the keys in Found, alone or together with the others, verifies.
	Fragments *Result
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
	// Current is the value of the field in the unmodified render. The field
	// did not drift, so this is also what the cluster is assumed to have; it
	// is not read from the cluster. After is the value once the edit is made.
	// Both are JSON, or "(absent)" for a field that does not exist on that
	// side.
	Current, After string
}

func (c Collateral) String() string {
	if c.Pointer == "" {
		return fmt.Sprintf("%s (%s before the edit, %s after)", c.Resource, c.Current, c.After)
	}
	return fmt.Sprintf("%s%s (current %s, after the edit %s)", c.Resource, c.Pointer, c.Current, c.After)
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
	// Collateral are the changes beyond the drifted fields that the edits also
	// make, all of them allowed by Options.AllowCollateral. Empty for an edit
	// that changes exactly the drifted fields.
	Collateral []Collateral
	// Overrides lists the edits of Results that add a key to override a chart
	// default, with the default each one overrides.
	Overrides []AppliedOverride
	// LeftOut are the fields the edits do not cover, with the reason for each.
	// Set only with Options.AllowPartial, when some fields were mapped.
	LeftOut []FieldFailure
	// Unmapped is set when the proposal cannot be mapped.
	Unmapped *Unmapped
	// Renders is the number of renders done.
	Renders int
}

// AppliedOverride is an edit that adds a key to a value file to override a
// default of the chart.
type AppliedOverride struct {
	// File and Path are where the key was added; Value is the JSON written.
	File  string
	Path  KeyPath
	Value string
	// Default is the chart default it overrides.
	Default ChartDefault
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

	// The edits tried so far, and the key of each. A later stage adds to them.
	tries []*tryout
	index map[editKey]*tryout

	// leftOut are the pointers of the fields a partial mapping leaves out: no
	// edit may change them, whatever the collateral rule says.
	leftOut []string

	// overrides is set when edits that add a key are allowed.
	overrides bool
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

	out, err := v.verifyPartially(opts)
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

// verifyPartially verifies the fields of the input. With Options.AllowPartial a
// field that cannot be mapped is left out and the others are verified again
// without it, until the rest verifies or no field is left. A failure that is
// not about one field (the edits interact, the render budget ran out) is not
// worked around: the proposal is Unmapped as a whole.
func (v *run) verifyPartially(opts Options) (*Outcome, error) {
	in := v.in
	if len(in.Fields) == 0 {
		return nil, errors.New("no drifted fields")
	}
	// A Secret's data is masked by Argo CD, and a Secret value must not be
	// proposed to Git.
	if in.Resource.Group == "" && in.Resource.Kind == "Secret" {
		return unmapped(ReasonSecretNotVerifiable, "%s is a Secret: Argo CD masks its data, and Secret values are never proposed to Git", in.Resource), nil
	}
	fields := in.Fields
	var left []FieldFailure
	if opts.AllowPartial {
		fields = fields[:0:0]
		for _, f := range in.Fields {
			if _, err := parseField(f); err != nil {
				left = append(left, FieldFailure{Pointer: f.Change.Path, Reason: ReasonUnsupportedChange, Detail: err.Error()})
				continue
			}
			fields = append(fields, f)
		}
		if len(fields) == 0 {
			return failure(left), nil
		}
	}
	for {
		attempt := &run{ctx: v.ctx, r: v.r, in: in, leftOut: leftOutPointers(left), overrides: opts.AddOverrides}
		out, err := attempt.verify(fields, opts)
		v.renders += attempt.renders
		v.budget = v.budget || attempt.budget
		if err != nil {
			return nil, err
		}
		if out.Mapped() {
			out.LeftOut = left
			return out, nil
		}
		if !opts.AllowPartial || len(out.Unmapped.Fields) == 0 {
			return out, nil
		}
		bad, rest := splitByFailure(fields, out.Unmapped.Fields)
		if len(bad) == 0 {
			return out, nil
		}
		left = append(left, bad...)
		if len(rest) == 0 {
			return failure(left), nil
		}
		fields = rest
	}
}

// splitByFailure separates the fields that have a failure of their own from
// the others. A failure whose pointer is not exactly that of one field (several
// fields named together) is not about one field and splits nothing.
func splitByFailure(fields []Field, failures []FieldFailure) (bad []FieldFailure, rest []Field) {
	byPointer := map[string]FieldFailure{}
	for _, ff := range failures {
		byPointer[ff.Pointer] = ff
	}
	for _, f := range fields {
		if ff, ok := byPointer[f.Change.Path]; ok {
			bad = append(bad, ff)
			continue
		}
		rest = append(rest, f)
	}
	return bad, rest
}

// leftOutPointers are the JSON pointers of the left-out fields.
func leftOutPointers(left []FieldFailure) []string {
	out := make([]string, len(left))
	for i, ff := range left {
		out[i] = ff.Pointer
	}
	return out
}

// verify verifies some fields of the input, all of which must be mapped.
func (v *run) verify(in []Field, opts Options) (*Outcome, error) {
	fields, fail := preflight(in, opts)
	if fail != nil {
		return fail, nil
	}

	// The unmodified files, twice: what differs between the two is
	// nondeterministic.
	base, nd, out, err := v.baseline(fields)
	if out != nil || err != nil {
		return out, err
	}

	return v.stages(base, nd, fields, opts)
}

// A stage is one way of looking for an edit: with the keys that hold the whole
// value or with those that hold a part of it, and with the changes beyond the
// drifted fields that are allowed. The strictest comes first, and the next is
// only tried when the one before verifies no edit set.
type stage struct {
	fragments bool
	// overrides: the candidates are the keys added to the last value file to
	// override a chart default, not the keys the value files define.
	overrides bool
	rule      collateralRule
}

func (v *run) stages(base snapshot, nd nondeterminism, fields []parsedField, opts Options) (*Outcome, error) {
	hasFragments := false
	for _, f := range fields {
		hasFragments = hasFragments || (f.Fragments != nil && len(f.Fragments.Candidates) > 0)
	}
	stages := []stage{{}}
	if hasFragments {
		stages = append(stages, stage{fragments: true})
	}
	rule := newCollateralRule(opts.AllowCollateral, v.in.Resource, fields, v.leftOut)
	if rule.any() {
		// Only when nothing verified without them. The edits have been rendered
		// already; what changes is which of their other changes are accepted.
		stages = append(stages, stage{fragments: hasFragments, rule: rule})
	}
	if v.overrides {
		// The last resort: keys that only the chart defines are set from the
		// Application's last value file. The edits of existing lines come first,
		// whole values before parts of them, and so do the stricter rules.
		if hasOverrides(fields, false) {
			stages = append(stages, stage{overrides: true})
		}
		if hasOverrides(fields, true) {
			stages = append(stages, stage{overrides: true, fragments: true})
		}
		if rule.any() && (hasOverrides(fields, false) || hasOverrides(fields, true)) {
			stages = append(stages, stage{overrides: true, fragments: hasOverrides(fields, true), rule: rule})
		}
	}

	var last *Outcome
	for _, st := range stages {
		if st.fragments {
			if bad := tooManyWithFragments(fields, opts); bad != nil {
				return failure(bad), nil
			}
		}
		if st.overrides {
			if bad := tooManyOverrides(fields, opts); bad != nil {
				return failure(bad), nil
			}
			// A later stage with the same candidates adds nothing new; the first
			// of them already tried the whole values, the next the fragments.
			if err := v.addTries(base, nd, fields, false, true); err != nil {
				return v.renderErr(err)
			}
			if st.fragments {
				if err := v.addTries(base, nd, fields, true, true); err != nil {
					return v.renderErr(err)
				}
			}
		} else if err := v.addTries(base, nd, fields, st.fragments, false); err != nil {
			return v.renderErr(err)
		}
		out, escalate, err := v.evaluate(base, nd, fields, st.rule)
		if err != nil || !escalate {
			return out, err
		}
		last = out
	}
	return last, nil
}

// tooManyWithFragments applies the bound on candidates to whole-value and
// fragment candidates together, before any fragment is rendered.
func tooManyWithFragments(fields []parsedField, opts Options) []FieldFailure {
	var bad []FieldFailure
	for _, f := range fields {
		if f.Fragments == nil {
			continue
		}
		if n := len(f.Found.Candidates) + len(f.Fragments.Candidates); n > opts.MaxCandidates {
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonTooManyCandidates,
				Detail: fmt.Sprintf("%d value file keys hold %s or a part of it, at most %d are tried", n, f.desired, opts.MaxCandidates)})
		}
	}
	return bad
}

// hasOverrides reports whether any field has an override candidate: for the
// whole value, or for a part of it.
func hasOverrides(fields []parsedField, fragments bool) bool {
	for _, f := range fields {
		found := f.Found
		if fragments {
			found = f.Fragments
		}
		if found != nil && len(found.Overrides) > 0 {
			return true
		}
	}
	return false
}

// tooManyOverrides applies the bound on candidates to the override candidates
// of a field, before any is rendered.
func tooManyOverrides(fields []parsedField, opts Options) []FieldFailure {
	var bad []FieldFailure
	for _, f := range fields {
		n := 0
		for _, found := range []*Result{f.Found, f.Fragments} {
			if found != nil {
				n += len(found.Overrides)
			}
		}
		if n > opts.MaxCandidates {
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonTooManyCandidates,
				Detail: fmt.Sprintf("%d chart defaults hold %s or a part of it, at most %d are tried", n, f.desired, opts.MaxCandidates)})
		}
	}
	return bad
}

// evaluate decides on the edits tried so far. escalate is set when no edit set
// verifies, which is when a more permissive stage is worth a try.
func (v *run) evaluate(base snapshot, nd nondeterminism, fields []parsedField, rule collateralRule) (out *Outcome, escalate bool, err error) {
	var accepted []*tryout
	for _, t := range v.tries {
		if t.accepted(rule) {
			accepted = append(accepted, t)
		}
	}
	accepted = dropShadowedOverrides(accepted)
	if bad := uncovered(fields, v.tries, accepted, rule, v.overrides); len(bad) > 0 {
		return failure(bad), true, nil
	}

	sets, tooMany := coverSets(len(fields), accepted)
	if tooMany {
		return unmapped(ReasonAmbiguousCandidates, "more than %d different sets of edits reproduce the drift", maxEditSets), false, nil
	}
	type verifiedSet struct {
		set        []*tryout
		final      map[string][]byte
		applied    []appliedEdit
		collateral []Collateral
	}
	var verified []verifiedSet
	var interacted []string
	for _, set := range sets {
		files, applied, err := applySet(v.in.Files, fields, set)
		if err != nil {
			interacted = append(interacted, fmt.Sprintf("%s: %v", describeSet(set), err))
			continue
		}
		var collateral []Collateral
		if len(set) == 1 {
			collateral = set[0].collateral // all of it is allowed: the edit was accepted
		}
		if len(set) > 1 {
			snap, err := v.render(files)
			switch {
			case errors.Is(err, ErrRenderFailed):
				interacted = append(interacted, describeSet(set)+": the chart does not render with all the edits together")
				continue
			case err != nil:
				o, e := v.renderErr(err)
				return o, false, e
			}
			why, allowed := judgeChanges(base, snap, diff(base, snap, nd), v.in.Resource, fields, rule)
			if why != "" {
				interacted = append(interacted, describeSet(set)+": "+why)
				continue
			}
			collateral = allowed
		}
		verified = append(verified, verifiedSet{set, files, applied, collateral})
	}
	switch {
	case len(verified) == 0:
		return unmapped(ReasonEditsInteract, "the edits verified one by one do not work together: %s", strings.Join(interacted, "; ")), true, nil
	case len(verified) > 1:
		names := make([]string, len(verified))
		for i, vs := range verified {
			names[i] = describeSet(vs.set)
		}
		return failure([]FieldFailure{{Pointer: pointers(fields), Reason: ReasonAmbiguousCandidates,
			Detail: "each of these reproduces the drift and changes nothing else: " + strings.Join(names, "; ")}}), false, nil
	}
	return &Outcome{
		Results:    buildResults(v.in.Files, verified[0].final, verified[0].applied),
		Collateral: verified[0].collateral,
		Overrides:  overridesOf(verified[0].applied),
	}, false, nil
}

// dropShadowedOverrides removes the accepted overrides whose fields an accepted
// edit of an existing line reproduces as well: an override is a last resort and
// must not compete with the edit of a key the value files define.
func dropShadowedOverrides(accepted []*tryout) []*tryout {
	covered := map[int]bool{}
	for _, t := range accepted {
		if !t.cand.Override {
			for _, i := range t.covers {
				covered[i] = true
			}
		}
	}
	out := accepted[:0:0]
	for _, t := range accepted {
		if t.cand.Override {
			shadowed := true
			for _, i := range t.covers {
				shadowed = shadowed && covered[i]
			}
			if shadowed {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func overridesOf(edits []appliedEdit) []AppliedOverride {
	var out []AppliedOverride
	for _, e := range edits {
		if e.cand.Override && e.cand.Default != nil {
			out = append(out, AppliedOverride{File: e.cand.File, Path: e.cand.Path, Value: e.w.Edits[0].Value, Default: *e.cand.Default})
		}
	}
	return out
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
	// fragment is set when the key holds only a part of the drifted value.
	fragment bool
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

// accepted: the edit reproduces drifted fields and changes nothing else that
// the rule does not allow.
func (t *tryout) accepted(rule collateralRule) bool {
	return t.why == "" && len(t.covers) > 0 && len(t.disallowed(rule)) == 0
}

// disallowed are the changes of the edit that the rule does not allow.
func (t *tryout) disallowed(rule collateralRule) []Collateral {
	var out []Collateral
	for _, c := range t.collateral {
		if !rule.allows(c) {
			out = append(out, c)
		}
	}
	return out
}

// collateralRule says which changes beyond the drifted fields an accepted edit
// may make. The zero value allows none.
type collateralRule struct {
	labels, annotations bool
	resource            ResourceKey     // the drifted resource
	drifted             map[string]bool // its drifted fields, by pointer
	leftOut             []string        // its fields a partial mapping leaves out
}

func newCollateralRule(kinds []string, res ResourceKey, fields []parsedField, leftOut []string) collateralRule {
	r := collateralRule{resource: res, drifted: map[string]bool{}, leftOut: leftOut}
	for _, k := range kinds {
		switch k {
		case CollateralLabels:
			r.labels = true
		case CollateralAnnotations:
			r.annotations = true
		}
	}
	for _, f := range fields {
		r.drifted[f.Change.Path] = true
	}
	return r
}

func (r collateralRule) any() bool { return r.labels || r.annotations }

// allows says whether a change beyond the drifted fields is one the policy
// allowed: a label or an annotation of a resource or of its pod template. Never
// a selector, whatever the policy says, and never a field that is itself
// drifted but did not end at its live value.
func (r collateralRule) allows(c Collateral) bool {
	if c.Pointer == "" || (c.Resource == r.resource && r.drifted[c.Pointer]) {
		return false
	}
	// A field a partial mapping leaves out stays as it is, or the merge request
	// would change what it says it does not.
	if c.Resource == r.resource {
		for _, p := range r.leftOut {
			if c.Pointer == p || strings.HasPrefix(c.Pointer, p+"/") || strings.HasPrefix(p, c.Pointer+"/") {
				return false
			}
		}
	}
	segs := strings.Split(strings.TrimPrefix(c.Pointer, "/"), "/")
	for _, seg := range segs[:len(segs)-1] {
		if seg == "selector" || seg == "matchLabels" || seg == "matchExpressions" {
			return false
		}
	}
	under := func(prefixes ...string) bool {
		for _, prefix := range prefixes {
			if c.Pointer == prefix || strings.HasPrefix(c.Pointer, prefix+"/") {
				return true
			}
		}
		return false
	}
	// The resource itself, the pod template of a workload, and the pod template
	// of the jobs of a CronJob.
	switch {
	case r.labels && under("/metadata/labels", "/spec/template/metadata/labels", "/spec/jobTemplate/spec/template/metadata/labels"):
		return true
	case r.annotations && under("/metadata/annotations", "/spec/template/metadata/annotations", "/spec/jobTemplate/spec/template/metadata/annotations"):
		return true
	}
	return false
}

func (t *tryout) key() string {
	if t.cand.Override {
		return t.cand.File + " " + t.cand.Path.String() + " (a new key)"
	}
	return t.cand.File + " " + t.cand.Path.String()
}

// conflicts reports two edits that set the same key to different values.
func conflicts(a, b *tryout) bool {
	return a.cand.File == b.cand.File && a.cand.Path.Equal(b.cand.Path) && a.value != b.value
}

// addTries renders the candidate edits of every field that were not tried
// yet, each on its own. An edit proposed for several fields is tried once.
// With fragments the candidates are those of FindFragments, otherwise those of
// Find; with overrides the keys to add instead of the keys to edit.
func (v *run) addTries(base snapshot, nd nondeterminism, fields []parsedField, fragments, overrides bool) error {
	if v.index == nil {
		v.index = map[editKey]*tryout{}
	}
	var added []*tryout
	for i, f := range fields {
		found := f.Found
		if fragments {
			found = f.Fragments
		}
		if found == nil {
			continue
		}
		cands := found.Candidates
		if overrides {
			cands = found.Overrides
		}
		for _, c := range cands {
			value := f.newValue
			if c.Fragment {
				value = c.NewValue
			}
			k := editKey{c.File, c.Path.String(), value}
			if t, ok := v.index[k]; ok {
				if !slices.Contains(t.origin, i) {
					t.origin = append(t.origin, i)
				}
				continue
			}
			t := &tryout{cand: c, value: value, fragment: c.Fragment, origin: []int{i}}
			v.index[k] = t
			v.tries = append(v.tries, t)
			added = append(added, t)
		}
	}
	byPointer := map[string]int{}
	for i, f := range fields {
		byPointer[f.Change.Path] = i
	}
	for _, t := range added {
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
			return err
		}
		for _, ch := range diff(base, snap, nd) {
			if i, ok := byPointer[ch.Pointer]; ok && ch.Key == v.in.Resource && snap[ch.Key][ch.Pointer] == fields[i].live {
				t.covers = append(t.covers, i)
				continue
			}
			t.collateral = append(t.collateral, collateralOf(base, snap, ch))
		}
	}
	return nil
}

func collateralOf(base, edited snapshot, ch change) Collateral {
	const absent = "(absent)"
	if ch.Pointer == "" {
		c := Collateral{Resource: ch.Key, Current: absent, After: absent}
		if _, ok := base[ch.Key]; ok {
			c.Current = "(resource exists)"
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
	return Collateral{Resource: ch.Key, Pointer: ch.Pointer, Current: val(base), After: val(edited)}
}

// uncovered returns a failure for each drifted field that no accepted edit
// reproduces, with the reasons and the nearest miss.
func uncovered(fields []parsedField, tries, accepted []*tryout, rule collateralRule, overrides bool) []FieldFailure {
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
			case slices.Contains(t.covers, i) && len(t.disallowed(rule)) > 0:
				if near == nil || len(t.disallowed(rule)) < len(near.disallowed(rule)) ||
					(len(t.disallowed(rule)) == len(near.disallowed(rule)) && len(t.covers) > len(near.covers)) {
					near = t
				}
			case len(t.covers) == 0:
				others = append(others, t.key()+": it does not reproduce the drift")
			}
		}
		ff := FieldFailure{Pointer: f.Change.Path, Reason: ReasonNoVerifiedCandidate}
		if near != nil {
			rest := near.disallowed(rule)
			nm := &NearMiss{File: near.cand.File, Path: near.cand.Path, Value: near.value, CollateralTotal: len(rest)}
			for _, c := range near.covers {
				nm.Covers = append(nm.Covers, fields[c].Change.Path)
			}
			nm.Collateral = rest[:min(len(rest), MaxCollateralShown)]
			ff.NearMiss = nm
			others = append(others, nm.String())
		}
		ff.Detail = noCandidateDetail(f, others, overrides)
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

// writeCandidate sets the key of a candidate in the content of its file, or
// adds it when the candidate overrides a chart default.
func writeCandidate(c Candidate, content []byte, value string) (*Written, error) {
	if c.Override {
		tag := ""
		if c.Default != nil {
			tag = c.Default.Tag
		}
		return AddValue(c.File, content, c.Path, value, tag)
	}
	return SetValue(c.File, content, c.Path, value)
}

// tryEdit applies one candidate's edit to a copy of the files.
func tryEdit(base map[string][]byte, c Candidate, value string) (files map[string][]byte, why string) {
	content, ok := base[c.File]
	if !ok {
		return nil, "the file was not provided"
	}
	w, err := writeCandidate(c, content, value)
	if err != nil {
		return nil, err.Error()
	}
	files = maps.Clone(base)
	files[c.File] = w.Edited
	return files, ""
}

// judgeChanges decides whether a render with some edits differs from the base
// by exactly the drifted fields at their live values, plus changes the rule
// allows. It returns why not, or the allowed changes beyond the drifted fields.
func judgeChanges(base, edited snapshot, changes []change, res ResourceKey, fields []parsedField, rule collateralRule) (string, []Collateral) {
	want := map[string]parsedField{}
	for _, f := range fields {
		want[f.Change.Path] = f
	}
	var extra []string
	var allowed []Collateral
	seen := map[string]bool{}
	for _, ch := range changes {
		if ch.Key == res {
			if f, ok := want[ch.Pointer]; ok && edited[res][ch.Pointer] == f.live {
				seen[ch.Pointer] = true
				continue
			}
		}
		if c := collateralOf(base, edited, ch); rule.allows(c) {
			allowed = append(allowed, c)
			continue
		}
		extra = append(extra, ch.String())
	}
	if len(extra) > 0 {
		shown := extra
		if len(shown) > 3 {
			shown = append(slices.Clone(shown[:3]), fmt.Sprintf("and %d more", len(extra)-3))
		}
		return "it also changes " + strings.Join(shown, ", "), nil
	}
	for p := range want {
		if !seen[p] {
			return fmt.Sprintf("it does not change %s", p), nil
		}
	}
	return "", allowed
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
		w, err := writeCandidate(t.cand, files[t.cand.File], t.value)
		if err != nil {
			return nil, nil, err
		}
		files[t.cand.File] = w.Edited
		// The change that proposed the edit is the one whose value the key held.
		applied = append(applied, appliedEdit{cand: t.cand, w: w, changeIndex: fields[t.origin[0]].Index})
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

func hasOverrideFor(r *Result, p KeyPath) bool {
	for _, c := range r.Overrides {
		if c.Path.Equal(p) {
			return true
		}
	}
	return false
}

// noCandidateDetail explains why a field has no verified candidate.
func noCandidateDetail(f parsedField, rejected []string, overrides bool) string {
	var parts []string
	seen := map[string]bool{}
	say := func(msg string) {
		if !seen[msg] {
			seen[msg] = true
			parts = append(parts, msg)
		}
	}
	// What the whole value and a part of it were found in, said once.
	for _, found := range []*Result{f.Found, f.Fragments} {
		if found == nil {
			continue
		}
		for _, p := range found.ChartDefaults {
			switch {
			case !overrides:
				say(fmt.Sprintf("the value comes from the chart's own defaults (%s); chart defaults are not edited", p))
			case hasOverrideFor(found, p):
				say(fmt.Sprintf("the value comes from the chart's own defaults (%s)", p))
			default:
				reason := "it cannot be overridden"
				for _, n := range found.NoOverride {
					if n.Path.Equal(p) {
						reason = n.Reason
					}
				}
				say(fmt.Sprintf("the value comes from the chart's own defaults (%s); %s", p, reason))
			}
		}
		for _, c := range found.Controlled {
			say(fmt.Sprintf("the value is set by %s at %s in the Application, not by a value file", c.Layer, c.Path))
		}
		if len(found.RemoteValueFiles) > 0 {
			say("it may come from a remote value file: " + strings.Join(found.RemoteValueFiles, ", "))
		}
	}
	parts = append(parts, rejected...)
	if len(parts) == 0 {
		return "no value file holds " + f.desired + "; the field may be derived from a value by the chart"
	}
	return strings.Join(parts, "; ")
}
