package rendermap

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
)

// Path affinity ranks value keys by how well their names fit what drifted: the
// key podAnnotations fits spec.template.metadata.annotations, extraEnvVars fits
// an env list, replica.replicaCount fits the replicas of the StatefulSet
// "redis-replicas". It never decides anything by itself: it only chooses which
// of many candidates are rendered, and every candidate is verified by a render.

// stopTokens are words of Kubernetes paths and of value keys that say nothing
// about what a key is for.
var stopTokens = map[string]bool{
	"spec": true, "template": true, "metadata": true, "container": true, "containers": true,
	"item": true, "items": true, "value": true, "values": true, "name": true, "kind": true,
	"data": true, "extra": true, "custom": true, "additional": true, "the": true, "of": true,
	"app": true, "apps": true, "kubernetes": true, "io": true,
}

// tokens splits identifiers into lower case words: camelCase, dots, dashes,
// slashes and digits separate. A plural s is dropped (replicas = replica).
func tokens(parts ...string) map[string]bool {
	out := map[string]bool{}
	for _, p := range parts {
		var word []rune
		flush := func() {
			if len(word) == 0 {
				return
			}
			w := strings.ToLower(string(word))
			word = word[:0]
			if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
				w = strings.TrimSuffix(w, "s")
			}
			if len(w) > 1 && !stopTokens[w] {
				out[w] = true
			}
		}
		runes := []rune(p)
		for i, r := range runes {
			switch {
			case unicode.IsLetter(r):
				if unicode.IsUpper(r) && i > 0 && unicode.IsLower(runes[i-1]) {
					flush()
				}
				word = append(word, r)
			default:
				flush()
			}
		}
		flush()
	}
	return out
}

// synonyms are words a chart uses for one another: the replicas of a workload
// are master.count in one chart and replicaCount in another.
var synonyms = map[string]string{"replica": "count", "count": "replica"}

// affinity counts the words two token sets share.
func affinity(a, b map[string]bool) int {
	n := 0
	for w := range a {
		if b[w] || (synonyms[w] != "" && b[synonyms[w]]) {
			n++
		}
	}
	return n
}

// fieldTokens are the words that name a scalar field: the last names of its
// pointer, and the name next to it in the render (the name of an env var), as
// the gate of the affinity, and the words of the resource's name to rank by.
func (v *run) fieldTokens(f parsedField, res leaves) (gate, resource map[string]bool) {
	gate = tokens(lastNames(f.Change.Path, 2)...)
	segs := pointerSegments(f.Change.Path)
	if len(segs) > 1 {
		parent := "/" + strings.Join(escapeSegments(segs[:len(segs)-1]), "/")
		if raw, ok := res[parent+"/name"]; ok {
			var name string
			if json.Unmarshal([]byte(raw), &name) == nil {
				maps.Copy(gate, tokens(name))
			}
		}
	}
	return gate, tokens(v.in.Resource.Name)
}

// rankCandidates narrows more candidates than limit down to the ones whose key
// fits the field best: the score of a key is ten for each word it shares with
// the field, one for each it shares with the resource's name. Only the best
// tier is kept, and only when it has a score and fits the bound, so the keys
// are never cut in the middle of a tie. cut are the candidates left out, with
// their scores: they are untried, and the caller must not forget it. ok is
// false when the names do not narrow them down; the field then has too many
// candidates, and nothing is cut.
func rankCandidates(cands []Candidate, gate, resource map[string]bool, limit int) (kept []Candidate, cut []untriedKey, ok bool) {
	if len(cands) <= limit {
		return cands, nil, true
	}
	score := func(c Candidate) int {
		kt := tokens(c.Path.String())
		return 10*affinity(gate, kt) + affinity(resource, kt)
	}
	top := 0
	for _, c := range cands {
		top = max(top, score(c))
	}
	if top == 0 {
		return cands, nil, false
	}
	for _, c := range cands {
		if score(c) == top {
			kept = append(kept, c)
		} else {
			cut = append(cut, untriedKey{cand: c, score: score(c)})
		}
	}
	if len(kept) > limit {
		return cands, nil, false
	}
	return kept, cut, true
}

// bound limits the scalar candidates of each field to opts.MaxCandidates by
// path affinity. It returns an Outcome when a field has more than that and the
// affinity does not narrow them down.
func (v *run) bound(fields []parsedField, res leaves, opts Options) ([]parsedField, *Outcome) {
	out := slices.Clone(fields)
	var bad []FieldFailure
	for i, f := range out {
		if f.structured || f.Found == nil {
			continue
		}
		gate, resource := v.fieldTokens(f, res)
		found := *f.Found
		var ok bool
		var cut []untriedKey
		if found.Candidates, cut, ok = rankCandidates(found.Candidates, gate, resource, opts.MaxCandidates); !ok {
			bad = append(bad, FieldFailure{Pointer: f.Change.Path, Reason: ReasonTooManyCandidates,
				Detail: fmt.Sprintf("%d value file keys hold %s, at most %d are tried and no name fits the field better than the others",
					len(found.Candidates), f.desired, opts.MaxCandidates)})
			continue
		}
		v.noteUntried(i, 0, cut)
		found.Overrides, cut, _ = rankCandidates(found.Overrides, gate, resource, opts.MaxCandidates)
		if v.overrides {
			v.noteUntried(i, stageRank(false, true, false), cut)
		}
		out[i].Found = &found
		if f.Fragments != nil {
			// A part of a value matches many keys (any key that holds "6", "3"
			// when a port changed); they are second choice, so when the names do
			// not narrow them down they are not tried, instead of failing the
			// field. They stay untried, which is checked at the end.
			frag := *f.Fragments
			if frag.Candidates, cut, ok = rankCandidates(frag.Candidates, gate, resource, opts.MaxCandidates); !ok {
				v.noteUntried(i, stageRank(true, false, false), asUntried(frag.Candidates))
				frag.Candidates = nil
			}
			v.noteUntried(i, stageRank(true, false, false), cut)
			if frag.Overrides, cut, ok = rankCandidates(frag.Overrides, gate, resource, opts.MaxCandidates); !ok {
				if v.overrides {
					v.noteUntried(i, stageRank(true, true, false), asUntried(frag.Overrides))
				}
				frag.Overrides = nil
			}
			if v.overrides {
				v.noteUntried(i, stageRank(true, true, false), cut)
			}
			out[i].Fragments = &frag
		}
	}
	if len(bad) > 0 {
		return nil, failure(bad)
	}
	return out, nil
}
