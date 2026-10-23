package helmmap

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sserkanml/backflow/internal/rendermap"
)

// The bound on the number of candidates and the ranking by names leave keys
// untried. A mapping that was verified is only as good as the claim that no
// other key would have verified too. This file keeps that claim honest:
//
//  1. a key that no template reads is dropped from the untried: it cannot be the one;
//  2. the others that compete with the mapping (they would have been tried at
//     the stage the mapping was found, or before it) are rendered after all,
//     up to a bound of their own and within the render budget;
//  3. if a key verifies too, the proposal is ambiguous; if some could not be
//     rendered, it has too many candidates and says which were left out.
//
// Only when every key left out is proven not to work is the mapping kept.

// DefaultMaxConfirmations bounds the renders spent on keys that were left out
// when an edit verifies.
const DefaultMaxConfirmations = 30

// untriedKey is a candidate that was left out, with how well its key fit.
type untriedKey struct {
	cand  Candidate
	score int
	field int
	// rank is the stage the candidate would have been tried at, see stageRank.
	rank int
}

// stageRank orders the stages by how early they are tried: the edits for a field
// itself, parts of a value, overrides, parts of a value as overrides, the edits
// of an ancestor, and those as overrides.
func stageRank(fragments, overrides, ancestors bool) int {
	switch {
	case ancestors && overrides:
		return 5
	case ancestors:
		return 4
	case overrides && fragments:
		return 3
	case overrides:
		return 2
	case fragments:
		return 1
	}
	return 0
}

func asUntried(cands []Candidate) []untriedKey {
	out := make([]untriedKey, len(cands))
	for i, c := range cands {
		out[i] = untriedKey{cand: c}
	}
	return out
}

// noteUntried remembers the candidates left out for a field, tried at a stage
// of the given rank.
func (v *run) noteUntried(field, rank int, cut []untriedKey) {
	if len(cut) == 0 {
		return
	}
	if v.untried == nil {
		v.untried = map[int][]untriedKey{}
	}
	for _, u := range cut {
		u.field, u.rank = field, rank
		v.untried[field] = append(v.untried[field], u)
	}
}

// liveUntried are the keys left out for a field that compete with a mapping
// found at the stage of rank maxRank and cannot be proven unreadable: some
// template of the chart, of its dependencies or one of its value files mentions
// them. Best fitting first.
func (v *run) liveUntried(field, maxRank int) []untriedKey {
	list := v.untried[field]
	if len(list) == 0 {
		return nil
	}
	if v.refs == nil {
		v.refs = scanValueRefs(v.in.Files)
	}
	seen := map[string]bool{}
	var out []untriedKey
	for _, u := range list {
		k := fmt.Sprintf("%s %s %d", u.cand.File, u.cand.Path, u.rank)
		if u.rank > maxRank || seen[k] || !v.refs.mentions(u.cand.Path) {
			continue
		}
		seen[k] = true
		out = append(out, u)
	}
	slices.SortStableFunc(out, func(a, b untriedKey) int { return b.score - a.score })
	return out
}

func untriedNames(keys []untriedKey) string {
	names := make([]string, 0, 3)
	for _, u := range keys[:min(3, len(keys))] {
		names = append(names, u.cand.Path.String())
	}
	more := ""
	if len(keys) > 3 {
		more = fmt.Sprintf(" and %d more", len(keys)-3)
	}
	return strings.Join(names, ", ") + more
}

// describeEdits names the edits of a mapped outcome.
func describeEdits(out *Outcome) string {
	var parts []string
	for _, r := range out.Results {
		for _, e := range r.Edits {
			parts = append(parts, r.File+" "+rendermap.KeyPathFromPointer(e.Location).String())
		}
	}
	return strings.Join(parts, " + ")
}

// checkUntried is the last word on a result when keys were left out (see above).
func (v *run) checkUntried(out *Outcome, base snapshot, nd nondeterminism, fields []parsedField, opts Options) *Outcome {
	if len(v.untried) == 0 {
		return out
	}
	if !out.Mapped() {
		return v.noteUntriedOnFailure(out, fields, opts)
	}
	var live []untriedKey
	var perField = map[int][]untriedKey{}
	for i := range fields {
		l := v.liveUntried(i, v.mappedRank)
		perField[i] = l
		live = append(live, l...)
	}
	if len(live) == 0 {
		return out
	}
	tooMany := func(why string) *Outcome {
		var bad []FieldFailure
		for i, f := range fields {
			if len(perField[i]) == 0 {
				continue
			}
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonTooManyCandidates,
				Detail: fmt.Sprintf("an edit verifies, but %d more value keys fit the field and were not tried, at most %d are (the best: %s): %s",
					len(perField[i]), opts.MaxCandidates, untriedNames(perField[i]), why)})
		}
		return failure(bad)
	}
	limit := opts.MaxConfirmations
	if limit <= 0 {
		limit = DefaultMaxConfirmations
	}
	if len(live) > limit {
		return tooMany(fmt.Sprintf("more than the %d that are rendered to check, and Backflow does not pick between keys", limit))
	}
	// A rival competes under the rule the mapping was found with: a key that
	// would only be accepted with the changes the policy allows beside an edit
	// comes in a later stage, after strict edits.
	rivals, err := v.confirm(base, nd, fields, live, v.mappedRule)
	if err != nil {
		if errors.Is(err, errBudget) || errors.Is(err, rendermap.ErrRenderFailed) || ctxDone(v) {
			return tooMany("they could not be rendered to check within the render budget")
		}
		return tooMany("they could not be rendered to check: " + err.Error())
	}
	if len(rivals) == 0 {
		return out
	}
	names := make([]string, len(rivals))
	for i, t := range rivals {
		names[i] = t.key()
	}
	return failure([]FieldFailure{{Pointer: pointers(fields), Reason: ReasonAmbiguousCandidates,
		Detail: "each of these reproduces the drift and changes nothing else: " + describeEdits(out) + "; " + strings.Join(names, "; ")}})
}

func ctxDone(v *run) bool { return v.ctx.Err() != nil }

// confirm renders the keys that were left out, and returns those that verify.
func (v *run) confirm(base snapshot, nd nondeterminism, fields []parsedField, keys []untriedKey, rule collateralRule) ([]*tryout, error) {
	if v.index == nil {
		v.index = map[editKey]*tryout{}
	}
	var added []*tryout
	for _, u := range keys {
		value := v.valueOf(fields[u.field], u.cand)
		k := editKey{u.cand.File, u.cand.Path.String(), value}
		if _, tried := v.index[k]; tried {
			continue
		}
		t := &tryout{cand: u.cand, value: value, fragment: u.cand.Fragment, origin: []int{u.field}}
		v.index[k] = t
		v.tries = append(v.tries, t)
		added = append(added, t)
	}
	if err := v.tryAll(base, nd, fields, added); err != nil {
		return nil, err
	}
	var rivals []*tryout
	for _, t := range added {
		if t.accepted(rule) {
			rivals = append(rivals, t)
		}
	}
	return rivals, nil
}

// valueOf is the value an edit of the candidate writes for the field.
func (v *run) valueOf(f parsedField, c Candidate) string {
	switch {
	case c.Structured != nil:
		return c.Structured.text()
	case c.Fragment:
		return c.NewValue
	}
	return f.newValue
}

// noteUntriedOnFailure: no tried key explains a field, and some that were left
// out might. The field cannot claim that none exists.
func (v *run) noteUntriedOnFailure(out *Outcome, fields []parsedField, opts Options) *Outcome {
	if out.Unmapped == nil || len(out.Unmapped.Fields) == 0 {
		return out
	}
	byPointer := map[string]int{}
	for i, f := range fields {
		byPointer[f.Change.Path] = i
	}
	changed := false
	for k, ff := range out.Unmapped.Fields {
		i, ok := byPointer[ff.Pointer]
		if !ok {
			continue
		}
		live := v.liveUntried(i, 1<<30)
		if len(live) == 0 {
			continue
		}
		out.Unmapped.Fields[k].Detail += fmt.Sprintf("; %d more value keys fit the field and were not tried, at most %d are (the best: %s)",
			len(live), opts.MaxCandidates, untriedNames(live))
		if ff.Reason == ReasonNoVerifiedCandidate {
			out.Unmapped.Fields[k].Reason = ReasonTooManyCandidates
		}
		changed = true
	}
	if changed {
		return failure(out.Unmapped.Fields)
	}
	return out
}
