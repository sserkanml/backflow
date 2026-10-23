package rendermap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// MaxConfirmations is the most renders spent, when an edit verifies, on keys
	// that were left out because too many keys fit the field and that some
	// template may read. Zero means DefaultMaxConfirmations.
	MaxConfirmations int
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
	// Found is the result of the source's Find for Desired.
	Found *Found
	// Fragments is the result of the source's FindFragments for Desired and Live, when both are
	// strings; nil otherwise. Its candidates are tried only when no edit of
	// the keys in Found, alone or together with the others, verifies.
	Fragments *Found
}

// Input is a proposal to verify: some fields of one resource.
type Input struct {
	App      App
	Resource ResourceKey
	Fields   []Field
	Provider Provider
	Writer   Writer
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
	Default Default
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

// hasUnder reports whether a leaf of the field is nondeterministic: the leaf
// itself, or for a structured field any leaf below it.
func (nd nondeterminism) hasUnder(res ResourceKey, pointer string, tree bool) bool {
	if !tree {
		return nd.paths[res][pointer]
	}
	for p := range nd.paths[res] {
		if under(pointer, p) {
			return true
		}
	}
	return false
}

// leavesText shows leaves for a message, bounded.
func leavesText(l leaves) string {
	if len(l) == 0 {
		return absentJSON
	}
	keys := slices.Sorted(maps.Keys(l))
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+l[k])
		if len(parts) == 3 {
			break
		}
	}
	text := strings.Join(parts, ", ")
	if len(keys) > 3 {
		text += fmt.Sprintf(" and %d more", len(keys)-3)
	}
	return text
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
	desired, live string // canonical JSON, "(absent)" for a side that has no value
	newValue      string // the live value as a template renders it (scalar fields)

	// structured is set for a change that is not the replacement of one scalar
	// by another: a map or list, or a field that appears or disappears. It is
	// judged on the leaves under its pointer: the render must have exactly
	// liveLeaves there after the edit, and desiredLeaves before it.
	structured    bool
	desiredV      any // decoded; nil when the side is absent
	liveV         any
	hasDesired    bool
	hasLive       bool
	desiredLeaves leaves
	liveLeaves    leaves
}

const absentJSON = "(absent)"

func isScalar(x any) bool {
	switch x.(type) {
	case string, float64, bool:
		return true
	}
	return false
}

func parseField(f Field) (parsedField, error) {
	p := parsedField{Field: f}
	switch f.Change.Op {
	case v1alpha1.OpReplace, v1alpha1.OpAdd, v1alpha1.OpRemove:
	default:
		return p, fmt.Errorf("%s of %s is not a change of a value", f.Change.Op, f.Change.Path)
	}
	if f.Change.Path == "" {
		return p, errors.New("a change of the whole resource is not a change of a field")
	}
	type side struct {
		raw     string
		present bool
		dst     *string
		val     *any
		has     *bool
	}
	for _, sd := range []side{
		{f.Change.Desired, f.Change.Op != v1alpha1.OpAdd, &p.desired, &p.desiredV, &p.hasDesired},
		{f.Change.Live, f.Change.Op != v1alpha1.OpRemove, &p.live, &p.liveV, &p.hasLive},
	} {
		if !sd.present {
			*sd.dst = absentJSON
			continue
		}
		var x any
		if err := json.Unmarshal([]byte(sd.raw), &x); err != nil {
			return p, fmt.Errorf("%s: %w", f.Change.Path, err)
		}
		if x == nil {
			return p, fmt.Errorf("%s is null: Helm reads null as the removal of a key", f.Change.Path)
		}
		b, _ := json.Marshal(x)
		*sd.dst, *sd.val, *sd.has = string(b), x, true
	}
	if f.Change.Op == v1alpha1.OpReplace && isScalar(p.desiredV) && isScalar(p.liveV) {
		p.newValue = RenderValue(p.liveV)
		return p, nil
	}
	p.structured = true
	p.desiredLeaves, p.liveLeaves = leaves{}, leaves{}
	if p.hasDesired {
		flatten(f.Change.Path, p.desiredV, p.desiredLeaves)
	}
	if p.hasLive {
		flatten(f.Change.Path, p.liveV, p.liveLeaves)
	}
	return p, nil
}

// leavesUnder returns the leaves of a resource at the pointer or below it.
func leavesUnder(l leaves, pointer string) leaves {
	out := leaves{}
	for k, v := range l {
		if k == pointer || strings.HasPrefix(k, pointer+"/") {
			out[k] = v
		}
	}
	return out
}

func sameLeaves(a, b leaves) bool { return maps.Equal(a, b) }

// under reports whether pointer is the path or a path below it.
func under(path, pointer string) bool { return pointer == path || strings.HasPrefix(pointer, path+"/") }

// reached reports whether the render has exactly the live state at the field.
func (f parsedField) reached(l leaves) bool {
	return sameLeaves(leavesUnder(l, f.Change.Path), f.liveLeaves)
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

	// untried are the candidates left out for each field by the bound on their
	// number, and refs what the templates read (built when needed).
	untried map[int][]untriedKey
	refs    References
	// mappedRank is the rank of the stage that found the mapping (see stageRank),
	// mappedRule the collateral rule it was found under.
	mappedRank int
	mappedRule collateralRule

	// structured are the candidates for maps and lists, made after the
	// baseline (they compare the value files with the render).
	structured *structuredSet

	// shared is what the attempts of one Verify keep for each other (see
	// verifyPartially); nil for a run on its own.
	shared *renderCache
}

// renderCache holds the renders of one Verify. An attempt after a field was
// left out renders the same files again: the unmodified ones (twice, to find
// what changes on every render) and the edits that were tried. Each render can
// be seconds of Argo CD's time, so the attempts share them.
type renderCache struct {
	snaps map[string]snapshot
	// base and again are the two renders of the unmodified files.
	base, again snapshot
	haveBase    bool
}

func filesKey(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, n := range names {
		_, _ = fmt.Fprintf(h, "%d:%s%d:", len(n), n, len(files[n]))
		_, _ = h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

var (
	errBudget            = errors.New("render budget exceeded")
	errDuplicateResource = errors.New("the render has two resources with the same identity")
)

func (v *run) render(files map[string][]byte) (snapshot, error) {
	var key string
	if v.shared != nil {
		key = filesKey(files)
		if snap, ok := v.shared.snaps[key]; ok {
			return snap, nil
		}
	}
	snap, err := v.renderFresh(files)
	if err == nil && v.shared != nil {
		v.shared.snaps[key] = snap
	}
	return snap, err
}

// renderFresh renders without looking at the cache.
func (v *run) renderFresh(files map[string][]byte) (snapshot, error) {
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
	shared := &renderCache{snaps: map[string]snapshot{}}
	for {
		attempt := &run{ctx: v.ctx, r: v.r, in: in, leftOut: leftOutPointers(left), overrides: opts.AddOverrides, shared: shared}
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
	fields, fail := preflight(in)
	if fail != nil {
		return fail, nil
	}

	// The unmodified files, twice: what differs between the two is
	// nondeterministic.
	base, nd, out, err := v.baseline(fields)
	if out != nil || err != nil {
		return out, err
	}
	// Too many keys hold the value: the keys whose names fit the field best are
	// tried, if that narrows them down (see rankCandidates).
	fields, out = v.bound(fields, base[v.in.Resource], opts)
	if out != nil {
		return out, nil
	}

	out, err = v.stages(base, nd, fields, opts)
	if err != nil || out == nil {
		return out, err
	}
	return v.checkUntried(out, base, nd, fields, opts), nil
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
	// own: the edits for structured fields themselves go with the first stage;
	// ancestors: the edits of an ancestor of a field (the resources of a
	// container for a drifted limit) come after every edit of a key that holds
	// the field's own value.
	own, ancestors bool
	rule           collateralRule
}

func (v *run) stages(base snapshot, nd nondeterminism, fields []parsedField, opts Options) (*Outcome, error) {
	v.structured = v.structuredCandidates(base, fields, opts)
	for i, cut := range v.structured.untried {
		for _, u := range cut {
			if v.untried == nil {
				v.untried = map[int][]untriedKey{}
			}
			u.field = i
			v.untried[i] = append(v.untried[i], u)
		}
	}
	if len(v.structured.failures) > 0 {
		return failure(v.structured.failures), nil
	}
	rule := newCollateralRule(opts.AllowCollateral, v.in.Resource, fields, v.leftOut)
	var last *Outcome
	for _, st := range v.planStages(fields, rule) {
		if st.fragments {
			if bad := tooManyWithFragments(fields, opts); bad != nil {
				return failure(bad), nil
			}
		}
		if st.overrides && !st.ancestors {
			if bad := tooManyOverrides(fields, opts); bad != nil {
				return failure(bad), nil
			}
		}
		if err := v.addStage(base, nd, fields, st); err != nil {
			return v.renderErr(err)
		}
		out, escalate, err := v.evaluate(base, nd, fields, st.rule)
		if err != nil || !escalate {
			v.mappedRank = stageRank(st.fragments, st.overrides, st.ancestors)
			v.mappedRule = st.rule
			return out, err
		}
		last = out
	}
	return last, nil
}

// planStages lists the stages in the order they are tried: the strictest first,
// and the next only when the one before verifies no edit set.
func (v *run) planStages(fields []parsedField, rule collateralRule) []stage {
	hasFragments := false
	for _, f := range fields {
		hasFragments = hasFragments || (f.Fragments != nil && len(f.Fragments.Candidates) > 0)
	}
	stages := []stage{{own: true}}
	if hasFragments {
		stages = append(stages, stage{fragments: true})
	}
	// withRule adds a stage and, when the policy allows collateral changes, the
	// same stage under that rule. The edits have been rendered already; what
	// changes is which of their other changes are accepted.
	withRule := func(st stage) {
		stages = append(stages, st)
		if rule.any() {
			st.rule = rule
			stages = append(stages, st)
		}
	}
	if rule.any() {
		stages = append(stages, stage{fragments: hasFragments, rule: rule})
	}
	if v.overrides {
		// Keys that only the chart defines are set from the Application's last
		// value file. The edits of existing lines come first, whole values
		// before parts of them, and so do the stricter rules.
		if hasOverrides(fields, false) || v.structured.any(0, true) {
			withRule(stage{overrides: true})
		}
		if hasOverrides(fields, true) {
			withRule(stage{overrides: true, fragments: true})
		}
	}
	// The edits of an ancestor of a field (the resources of a container for a
	// drifted limit) come last: they say more than the field did.
	if v.structured.any(1, false) {
		withRule(stage{ancestors: true})
	}
	if v.overrides && v.structured.any(1, true) {
		withRule(stage{ancestors: true, overrides: true})
	}
	return stages
}

// addStage renders the candidates a stage adds to the ones tried before.
func (v *run) addStage(base snapshot, nd nondeterminism, fields []parsedField, st stage) error {
	switch {
	case st.overrides && st.ancestors:
		return v.addStructured(base, nd, fields, 1, true)
	case st.overrides:
		// A later stage with the same candidates adds nothing new; the first of
		// them already tried the whole values, the next the fragments.
		if err := v.addTries(base, nd, fields, st.fragments, true); err != nil {
			return err
		}
		if !st.fragments {
			return v.addStructured(base, nd, fields, 0, true)
		}
	case st.ancestors:
		return v.addStructured(base, nd, fields, 1, false)
	default:
		if err := v.addTries(base, nd, fields, st.fragments, false); err != nil {
			return err
		}
		if st.own {
			return v.addStructured(base, nd, fields, 0, false)
		}
	}
	return nil
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
		for _, found := range []*Found{f.Found, f.Fragments} {
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
		files, applied, err := applySet(v.in.Writer, v.in.Files, fields, set)
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
		// Edits that give the same files are one way of mapping, however they
		// were found.
		if slices.ContainsFunc(verified, func(o verifiedSet) bool {
			return maps.EqualFunc(o.final, files, func(a, b []byte) bool { return bytes.Equal(a, b) })
		}) {
			continue
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
func preflight(in []Field) ([]parsedField, *Outcome) {
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
	return fields, nil
}

// baseline renders the unmodified files twice and checks that the drifted
// fields are what the render gives. It returns an Outcome when they are not.
func (v *run) baseline(fields []parsedField) (snapshot, nondeterminism, *Outcome, error) {
	in := v.in
	var base, again snapshot
	if v.shared != nil && v.shared.haveBase {
		base, again = v.shared.base, v.shared.again
	} else {
		// Two renders of the same files, never taken from the cache: what
		// differs between them is nondeterministic.
		var err error
		if base, err = v.renderFresh(in.Files); err != nil {
			out, err := v.baseFailure(err)
			return nil, nondeterminism{}, out, err
		}
		if again, err = v.renderFresh(in.Files); err != nil {
			out, err := v.baseFailure(err)
			return nil, nondeterminism{}, out, err
		}
		if v.shared != nil {
			v.shared.base, v.shared.again, v.shared.haveBase = base, again, true
		}
	}
	nd := findNondeterminism(base, again)

	res, ok := base[in.Resource]
	if !ok {
		return nil, nd, unmapped(ReasonRenderMismatch, "the render has no %s", in.Resource), nil
	}
	var bad []FieldFailure
	for _, f := range fields {
		if nd.hasUnder(in.Resource, f.Change.Path, f.structured) {
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonNondeterministicField,
				Detail: "the rendered value differs between two renders of the same files"})
		} else if f.structured {
			if got := leavesUnder(res, f.Change.Path); !sameLeaves(got, f.desiredLeaves) {
				bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonRenderMismatch,
					Detail: fmt.Sprintf("the render gives %s, the proposal expects %s", leavesText(got), leavesText(f.desiredLeaves))})
			}
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
	driftedTrees        []string        // the pointers of its structured fields: everything below is drifted too
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
		if f.structured {
			r.driftedTrees = append(r.driftedTrees, f.Change.Path)
		}
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
	if c.Resource == r.resource {
		for _, p := range r.driftedTrees {
			if under(p, c.Pointer) {
				return false
			}
		}
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

// shown is the value of the edit for a message: the new value of a key, or
// what a structured edit does, bounded.
func (t *tryout) shown() string {
	if e := t.cand.Structured; e != nil {
		if e.Kind == EditRemoveKey {
			return "(removed)"
		}
		text, _ := JSONText(e.Value)
		return textOf(text)
	}
	return t.value
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
	return v.tryAll(base, nd, fields, added)
}

// addStructured adds the structured candidates of a level, and tries them.
func (v *run) addStructured(base snapshot, nd nondeterminism, fields []parsedField, level int, overrides bool) error {
	if v.index == nil {
		v.index = map[editKey]*tryout{}
	}
	list := v.structured.edits[level]
	if overrides {
		list = v.structured.overrides[level]
	}
	var added []*tryout
	for _, sc := range list {
		k := editKey{sc.cand.File, sc.cand.Path.String(), sc.value}
		if t, ok := v.index[k]; ok {
			for _, o := range sc.origin {
				if !slices.Contains(t.origin, o) {
					t.origin = append(t.origin, o)
				}
			}
			continue
		}
		t := &tryout{cand: sc.cand, value: sc.value, origin: slices.Clone(sc.origin)}
		v.index[k] = t
		v.tries = append(v.tries, t)
		added = append(added, t)
	}
	return v.tryAll(base, nd, fields, added)
}

// tryAll renders each new edit on its own and records what it changed.
func (v *run) tryAll(base snapshot, nd nondeterminism, fields []parsedField, added []*tryout) error {
	byPointer := map[string]int{}
	for i, f := range fields {
		if !f.structured {
			byPointer[f.Change.Path] = i
		}
	}
	for _, t := range added {
		files, why := tryEdit(v.in.Writer, v.in.Files, t.cand, t.value)
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
		var reached []string // the structured fields the edit brought to their live state
		for i, f := range fields {
			if f.structured && f.reached(snap[v.in.Resource]) && !f.reached(base[v.in.Resource]) {
				t.covers = append(t.covers, i)
				reached = append(reached, f.Change.Path)
			}
		}
		for _, ch := range diff(base, snap, nd) {
			if i, ok := byPointer[ch.Pointer]; ok && ch.Key == v.in.Resource && snap[ch.Key][ch.Pointer] == fields[i].live {
				t.covers = append(t.covers, i)
				continue
			}
			if ch.Key == v.in.Resource && slices.ContainsFunc(reached, func(p string) bool { return under(p, ch.Pointer) }) {
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
			// A key reproduces the field and also changes other fields, which the
			// policy does not allow: that is what to tell, however the other keys
			// that were tried failed.
			ff.Reason = ReasonValueAffectsOtherFields
			rest := near.disallowed(rule)
			nm := &NearMiss{File: near.cand.File, Path: near.cand.Path, Value: near.shown(), CollateralTotal: len(rest)}
			for _, c := range near.covers {
				nm.Covers = append(nm.Covers, fields[c].Change.Path)
			}
			nm.Collateral = rest[:min(len(rest), MaxCollateralShown)]
			ff.NearMiss = nm
			ff.Detail = nm.String()
			if len(others) > 0 {
				ff.Detail += fmt.Sprintf(" (%d other candidate(s) were rejected for other reasons)", len(others))
			}
		} else {
			ff.Detail = noCandidateDetail(f, others, overrides)
		}
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
func tryEdit(w Writer, base map[string][]byte, c Candidate, value string) (files map[string][]byte, why string) {
	content, ok := base[c.File]
	if !ok {
		return nil, "the file was not provided"
	}
	written, err := w.Write(c, content, value)
	if err != nil {
		return nil, err.Error()
	}
	files = maps.Clone(base)
	files[c.File] = written.Edited
	return files, ""
}

// judgeChanges decides whether a render with some edits differs from the base
// by exactly the drifted fields at their live values, plus changes the rule
// allows. It returns why not, or the allowed changes beyond the drifted fields.
func judgeChanges(base, edited snapshot, changes []change, res ResourceKey, fields []parsedField, rule collateralRule) (string, []Collateral) {
	want := map[string]parsedField{}
	var trees []parsedField
	for _, f := range fields {
		if f.structured {
			trees = append(trees, f)
		} else {
			want[f.Change.Path] = f
		}
	}
	var extra []string
	var allowed []Collateral
	seen := map[string]bool{}
	for _, f := range trees {
		if f.reached(edited[res]) {
			seen[f.Change.Path] = true
		}
	}
	for _, ch := range changes {
		if ch.Key == res {
			if f, ok := want[ch.Pointer]; ok && edited[res][ch.Pointer] == f.live {
				seen[ch.Pointer] = true
				continue
			}
			if slices.ContainsFunc(trees, func(f parsedField) bool { return seen[f.Change.Path] && under(f.Change.Path, ch.Pointer) }) {
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
	for _, f := range trees {
		if !seen[f.Change.Path] {
			return fmt.Sprintf("it does not change %s", f.Change.Path), nil
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
func applySet(wr Writer, base map[string][]byte, fields []parsedField, set []*tryout) (map[string][]byte, []appliedEdit, error) {
	files := maps.Clone(base)
	var applied []appliedEdit
	for _, t := range set {
		w, err := wr.Write(t.cand, files[t.cand.File], t.value)
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
			File: e.cand.File, Location: e.cand.Path.Pointer(), Value: e.w.Edits[0].Value, ChangeIndex: e.changeIndex,
		})
	}
	out := make([]mapping.Result, 0, len(order))
	for _, f := range order {
		out = append(out, *byFile[f])
	}
	return out
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
	for _, found := range []*Found{f.Found, f.Fragments} {
		if found == nil || found.Diagnostics == nil {
			continue
		}
		for _, msg := range found.Diagnostics.Explain(found, overrides) {
			say(msg)
		}
	}
	for _, found := range []*Found{f.Found, f.Fragments} {
		if found == nil || found.Diagnostics == nil {
			continue
		}
		for _, msg := range found.Diagnostics.ExplainEmpty(found) {
			say(msg)
		}
	}
	parts = append(parts, rejected...)
	if len(parts) == 0 {
		return "no value file holds " + f.desired + "; the field may be derived from a value by the chart"
	}
	return strings.Join(parts, "; ")
}
