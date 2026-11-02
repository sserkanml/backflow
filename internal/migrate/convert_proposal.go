package migrate

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// The annotations that carried controller state in v1alpha1. In v1 they are status.signals.
const (
	annSupersededBy      = "backflow.io/superseded-by"
	annReverted          = "backflow.io/reverted"
	annLiveReverted      = "backflow.io/live-reverted"
	annAccessRevoked     = "backflow.io/access-revoked"
	annRejectionReported = "backflow.io/rejection-reported"
)

// AnnotationCreated holds the creationTimestamp of the v1alpha1 proposal: a proposal is created again, and the new
// object has a new one.
const AnnotationCreated = "migrate.backflow.io/created"

// maxChanges is how many changes a v1 proposal holds.
const maxChanges = 256

const phaseUnmapped = "Unmapped"

// FinishedPhase reports whether a phase is a final one that retention applies to.
func FinishedPhase(phase string) bool {
	switch phase {
	case "Merged", "Rejected", "Reverted", "Superseded", "Failed":
		return true
	}
	return false
}

// openPhase is a proposal that is still being worked on, whose spec and status must not lose information.
func openPhase(phase string) bool { return phase != phaseUnmapped && !FinishedPhase(phase) }

// maxLengths are the bounds v1 put on strings of the status, which v1alpha1 did not have. A value longer than
// its bound is cut, with a note, because the API server would refuse the whole status for it.
var maxLengths = map[string]int{
	"message": 2048, keySupersededBy: 253, "branch": 255, "commitSHA": 64, keyURL: 2048, "state": 64,
	"value": 16384, "file": 1024, "location": 1024, keyRepoURL: 2048, keyPath: 1024, "current": 16384, "new": 16384,
	"suggested": 16384, "option": 1024, "optionPath": 1024, keyRevision: 255, "diff": 32768, keyName: 512,
}

// clip cuts the strings of m that are longer than their bound.
func clip(m obj, path string, n *notes) obj {
	for k, v := range m {
		s, ok := v.(string)
		limit, bounded := maxLengths[k]
		if !ok || !bounded {
			continue
		}
		if r := []rune(s); len(r) > limit {
			m[k] = string(r[:limit])
			n.add("%s.%s: cut to %d characters (v1 has a bound)", path, k, limit)
		}
	}
	return m
}

// ConvertDriftProposal converts a v1alpha1 DriftProposal. The result is the object to create (metadata and spec) and
// the status to write to it after. policyUID, when not empty, is the uid of the policy that owns the proposal now.
//
// app is the Argo CD Application of the proposal (spec.application), or nil when it is not known. v1alpha1 stored the chart
// name of a chart from a Helm repository in source.path; v1 has a field for it, and the Application is what says which
// of the two the value is.
func ConvertDriftProposal(old obj, policyUID string, app obj) (*Result, error) {
	n := &notes{}
	spec, ok := asMap(old[keySpec])
	if !ok {
		return nil, bad("the object has no spec")
	}
	st, _ := asMap(old[keyStatus])
	phase := asString(st["phase"])
	meta, err := proposalMetadata(old, policyUID, n)
	if err != nil {
		return nil, err
	}
	out, kept, err := proposalSpec(spec, phase, app, n)
	if err != nil {
		return nil, err
	}
	res := &Result{Object: obj{keyAPIVersion: newAPIVersion, keyKind: "DriftProposal", keyMetadata: meta, keySpec: out}}
	res.Status = proposalStatus(old, spec, kept, n)
	res.Notes = n.sorted()
	return res, nil
}

// signalOf maps a lifecycle annotation to the field of status.signals that replaces it.
var signalOf = []struct{ annotation, field string }{
	{annSupersededBy, keySupersededBy}, {annReverted, "reverted"}, {annLiveReverted, "liveRevertedAtRevision"},
	{annAccessRevoked, "accessRevoked"}, {annRejectionReported, "rejectionReported"},
}

// proposalMetadata keeps the name, namespace and labels and the annotations that are not controller state. What the
// API server owns (uid, resourceVersion, creationTimestamp, managedFields) and the finalizers of the old instance do
// not go over.
func proposalMetadata(old obj, policyUID string, n *notes) (obj, error) {
	meta, ok := asMap(old[keyMetadata])
	if !ok || asString(meta[keyName]) == "" || asString(meta[keyNamespace]) == "" {
		return nil, bad("the object has no metadata.name or metadata.namespace")
	}
	out := obj{keyName: meta[keyName], keyNamespace: meta[keyNamespace]}
	if labels, ok := asMap(meta[keyLabels]); ok && len(labels) > 0 {
		out[keyLabels] = maps.Clone(labels)
	}
	ann := obj{}
	if existing, ok := asMap(meta["annotations"]); ok {
		for k, v := range existing {
			if !slices.ContainsFunc(signalOf, func(s struct{ annotation, field string }) bool { return s.annotation == k }) {
				ann[k] = v
			}
		}
	}
	ann[AnnotationMigrated] = OldVersion
	n.add("metadata.annotations[%s] = %s", AnnotationMigrated, OldVersion)
	if created := asString(meta["creationTimestamp"]); created != "" {
		ann[AnnotationCreated] = created
		n.add("metadata.annotations[%s] = %s (the proposal is created again; its creationTimestamp is new)", AnnotationCreated, created)
	}
	out["annotations"] = ann
	if fin, ok := asList(meta["finalizers"]); ok && len(fin) > 0 {
		n.add("metadata.finalizers %v are not kept: they belong to the old object", fin)
	}
	refs, _ := asList(meta["ownerReferences"])
	for _, r := range refs {
		ref, ok := asMap(r)
		if !ok {
			continue
		}
		ref = maps.Clone(ref)
		if strings.HasPrefix(asString(ref[keyAPIVersion]), Group+"/") {
			ref[keyAPIVersion] = newAPIVersion
		}
		if policyUID != "" && asString(ref[keyKind]) == "BackflowPolicy" {
			ref["uid"] = policyUID
		}
		out["ownerReferences"] = append(asListOr(out["ownerReferences"]), ref)
	}
	if _, ok := out["ownerReferences"]; ok {
		n.add("metadata.ownerReferences: apiVersion %s, uid of the policy as it is now", newAPIVersion)
	}
	return out, nil
}

func asListOr(v any) []any { l, _ := asList(v); return l }

var actorKinds = map[string]string{"person": kindPerson, "controller": kindController, kindPerson: kindPerson, kindController: kindController}

// proposalSpec converts the spec. It returns how many changes were kept.
func proposalSpec(spec obj, phase string, app obj, n *notes) (obj, int, error) {
	out := obj{}
	policy := asString(spec["policyName"])
	if policy == "" {
		return nil, 0, bad("spec.policyName is empty")
	}
	out["policyRef"] = obj{keyName: policy}
	n.add("spec.policyName -> spec.policyRef.name")
	for _, f := range []struct{ from, to string }{{"application", "applicationRef"}, {"resource", "resourceRef"}} {
		m, ok := asMap(spec[f.from])
		if !ok || len(m) == 0 {
			return nil, 0, bad("spec.%s is missing", f.from)
		}
		out[f.to] = maps.Clone(m)
		n.add("spec.%s -> spec.%s", f.from, f.to)
	}
	sources, err := proposalSources(spec, app, n)
	if err != nil {
		return nil, 0, err
	}
	out["sources"] = sources

	changes, ok := asList(spec["changes"])
	if !ok || len(changes) == 0 {
		return nil, 0, bad("spec.changes is empty")
	}
	kept, total := len(changes), len(changes)
	if kept > maxChanges {
		if openPhase(phase) {
			return nil, 0, bad("spec.changes has %d entries and the proposal is open (phase %q): v1 holds %d. "+
				"Close or reject its merge request and delete the proposal, or run again with --skip-finished once it is finished",
				kept, phase, maxChanges)
		}
		changes, kept = changes[:maxChanges], maxChanges
		out["truncated"] = true
		n.add("spec.changes: the first %d of %d are kept and spec.truncated is true (v1 holds %d)", maxChanges, total, maxChanges)
	}
	out["changes"] = changes

	if err := proposalActor(spec, out, n); err != nil {
		return nil, 0, err
	}
	detected := asString(spec["detectedAt"])
	if detected == "" {
		return nil, 0, bad("spec.detectedAt is empty")
	}
	out["detectedAt"] = detected
	return out, kept, nil
}

// proposalSources makes the list of sources from source and sources: the first source was stored twice.
func proposalSources(spec, app obj, n *notes) ([]any, error) {
	if list, ok := asList(spec["sources"]); ok && len(list) > 0 {
		out := make([]any, 0, len(list))
		for i, s := range list {
			m, ok := asMap(s)
			if !ok {
				return nil, bad("spec.sources[%d] is not an object", i)
			}
			out = append(out, maps.Clone(m))
		}
		n.add("spec.source is dropped: it repeats spec.sources[0]")
		return out, nil
	}
	src, ok := asMap(spec["source"])
	if !ok || asString(src[keyRepoURL]) == "" {
		return nil, bad("spec.source is missing")
	}
	first := obj{keyPosition: int64(1), "role": "Render"}
	for _, k := range []string{keyRepoURL, keyRevision, keyTargetRevision, keyPath, "type"} {
		if v, ok := src[k]; ok && v != "" {
			first[k] = v
		}
	}
	n.add("spec.source -> spec.sources[0] (position 1, role Render)")
	if asString(src["type"]) == sourceHelm && asString(src[keyPath]) != "" {
		chartFromApplication(first, app, n)
	}
	return []any{first}, nil
}

// chartFromApplication decides whether the path of a Helm source is a chart name. v1alpha1 stored the chart of a chart from
// a Helm repository in path, and a path in a Git repository there too; the Application says which: its source at that
// position, with the same repository, has a chart or it has not. Without the Application, or when it has moved on, the
// value stays where it was, and the note says so.
func chartFromApplication(source obj, app obj, n *notes) {
	path := asString(source[keyPath])
	if app == nil {
		n.add("spec.sources[0].path %q may be a chart name (v1alpha1 stored it there); the Application was not found, so it stays in path", path)
		return
	}
	meta, _ := asMap(app[keyMetadata])
	where := asString(meta[keyNamespace]) + "/" + asString(meta[keyName])
	entry := applicationSource(app, int(source[keyPosition].(int64)))
	if entry == nil || strings.TrimSuffix(asString(entry[keyRepoURL]), "/") != strings.TrimSuffix(asString(source[keyRepoURL]), "/") {
		n.add("spec.sources[0].path %q stays in path: source %d of the Application %s is another repository", path, 1, where)
		return
	}
	chart := asString(entry["chart"])
	switch {
	case chart == "":
		n.add("spec.sources[0].path %q is a path in a Git repository (the Application %s has no chart there)", path, where)
	case chart != path:
		n.add("spec.sources[0].path %q stays in path: the Application %s has the chart %q now", path, where, chart)
	default:
		source["chart"] = chart
		delete(source, keyPath)
		n.add("spec.sources[0].path -> chart %q (the Application %s has that chart at spec.sources[0])", chart, where)
	}
}

// applicationSource is the source of an Argo CD Application at a position, from 1: spec.sources, or spec.source.
func applicationSource(app obj, position int) obj {
	spec, _ := asMap(app[keySpec])
	if list, ok := asList(spec["sources"]); ok && len(list) > 0 {
		if position < 1 || position > len(list) {
			return nil
		}
		m, _ := asMap(list[position-1])
		return m
	}
	if position == 1 {
		m, _ := asMap(spec["source"])
		return m
	}
	return nil
}

// proposalActor splits actor into actor (who) and managers (which tool wrote the fields).
func proposalActor(spec, out obj, n *notes) error {
	a, ok := asMap(spec["actor"])
	if !ok || len(a) == 0 {
		return nil
	}
	who := obj{}
	for _, k := range []string{"username", "groups"} {
		if v, ok := a[k]; ok && v != "" {
			who[k] = v
		}
	}
	if kind := asString(a[keyKind]); kind != "" {
		converted, known := actorKinds[kind]
		if !known {
			return bad("spec.actor.kind %q is not person or controller", kind)
		}
		who[keyKind] = converted
		n.add("spec.actor.kind: %s -> %s", kind, converted)
	}
	if len(who) > 0 {
		out["actor"] = who
	}
	var managers []any
	if asString(a["manager"]) != "" {
		managers = append(managers, managerWrite(a))
	}
	others, _ := asList(a["others"])
	for _, o := range others {
		if m, ok := asMap(o); ok && asString(m["manager"]) != "" {
			managers = append(managers, managerWrite(m))
		}
	}
	if len(managers) > 0 {
		out["managers"] = managers
		n.add("spec.actor.{manager,operation,time,fields,others} -> spec.managers (%d)", len(managers))
	}
	if asString(a["userAgent"]) != "" {
		n.add("spec.actor.userAgent is dropped: it was never filled")
	}
	return nil
}

func managerWrite(m obj) obj {
	out := obj{"manager": m["manager"]}
	for _, k := range []string{"operation", "time", "fields"} {
		if v, ok := m[k]; ok && v != "" {
			out[k] = v
		}
	}
	return out
}

// proposalStatus converts the status and the annotations that were state. It is nil when there is nothing to write.
func proposalStatus(old, spec obj, kept int, n *notes) obj {
	st, _ := asMap(old[keyStatus])
	meta, _ := asMap(old[keyMetadata])
	out := obj{}
	phase := asString(st["phase"])
	if phase != "" {
		out["phase"] = phase
	}
	if conds := proposalConditions(st["conditions"], n); len(conds) > 0 {
		out["conditions"] = conds
	}
	if m := proposalMapping(st["mapping"], spec, kept, n); m != nil {
		out["mapping"] = m
	}
	if g := proposalGit(st, n); g != nil {
		out["git"] = g
	}
	for _, k := range []string{keySupersededBy, "message"} {
		if s := asString(st[k]); s != "" {
			out[k] = clip(obj{k: s}, keyStatus, n)[k]
		}
	}
	if FinishedPhase(phase) {
		out["finishedAt"] = finishedAt(meta, st).UTC().Format(time.RFC3339)
		n.add("status.finishedAt = %s (the last change of a condition, else creationTimestamp): retention counts from it",
			out["finishedAt"])
	}
	if sig := proposalSignals(meta, n); sig != nil {
		out["signals"] = sig
	}
	if len(out) == 0 {
		return nil
	}
	n.add("status: phase, conditions, mapping, supersededBy and message kept; git and signals built from the fields that moved")
	return out
}

// finishedAt is when a finished proposal finished: the latest change of one of its conditions, else its creation. The
// retention of finished proposals counts from it, so it must not be the time the proposal is created again.
func finishedAt(meta, st obj) time.Time {
	var latest time.Time
	conds, _ := asList(st["conditions"])
	for _, c := range conds {
		m, _ := asMap(c)
		if at, err := time.Parse(time.RFC3339, asString(m["lastTransitionTime"])); err == nil && at.After(latest) {
			latest = at
		}
	}
	if latest.IsZero() {
		latest, _ = time.Parse(time.RFC3339, asString(meta["creationTimestamp"]))
	}
	return latest
}

// proposalConditions keeps the conditions. v1 keys the list by type; a type that is listed twice keeps its last entry.
func proposalConditions(v any, n *notes) []any {
	list, _ := asList(v)
	var out []any
	for _, c := range list {
		m, ok := asMap(c)
		if !ok {
			continue
		}
		i := slices.IndexFunc(out, func(e any) bool { em, _ := asMap(e); return em["type"] == m["type"] })
		if i >= 0 {
			n.add("status.conditions: %v is listed twice, the last one is kept (the list is keyed by type now)", m["type"])
			out[i] = m
			continue
		}
		out = append(out, m)
	}
	return out
}

func proposalGit(st obj, n *notes) obj {
	g := obj{}
	branch := asString(st["branch"])
	mr, hasMR := asMap(st["mergeRequest"])
	if branch == "" && hasMR {
		branch = asString(mr["branch"])
	}
	if branch != "" {
		g["branch"] = branch
	}
	if hasMR && len(mr) > 0 {
		out := obj{}
		for _, k := range []string{keyURL, "number", "state"} {
			if v, ok := mr[k]; ok {
				out[k] = v
			}
		}
		g["mergeRequest"] = out
		n.add("status.mergeRequest -> status.git.mergeRequest (its branch is status.git.branch)")
	}
	if sha := asString(st["commitSHA"]); sha != "" {
		g["commitSHA"] = sha
	}
	push := obj{}
	if v, ok := number(st["pushRejections"]); ok && v != 0 {
		push["rejections"] = v
	}
	if at := asString(st["lastPushRejectedAt"]); at != "" {
		push["lastRejectedAt"] = at
	}
	if len(push) > 0 {
		g["push"] = push
		n.add("status.pushRejections, lastPushRejectedAt -> status.git.push.{rejections,lastRejectedAt}")
	}
	if len(g) == 0 {
		return nil
	}
	return clipGit(g, n)
}

func clipGit(g obj, n *notes) obj {
	clip(g, "status.git", n)
	if mr, ok := asMap(g["mergeRequest"]); ok {
		clip(mr, "status.git.mergeRequest", n)
	}
	return g
}

// proposalSignals turns the five annotations into status.signals.
func proposalSignals(meta obj, n *notes) obj {
	ann, _ := asMap(meta["annotations"])
	out := obj{}
	for _, s := range signalOf {
		v := asString(ann[s.annotation])
		if v == "" {
			continue
		}
		switch s.field {
		case "accessRevoked", "rejectionReported":
			out[s.field] = true
		default:
			out[s.field] = v
		}
		n.add("metadata.annotations[%s] -> status.signals.%s", s.annotation, s.field)
	}
	if len(out) == 0 {
		return nil
	}
	for k, v := range out {
		if s, ok := v.(string); ok {
			limit := map[string]int{keySupersededBy: 253, "reverted": 1024, "liveRevertedAtRevision": 255}[k]
			if r := []rune(s); limit > 0 && len(r) > limit {
				out[k] = string(r[:limit])
				n.add("status.signals.%s: cut to %d characters", k, limit)
			}
		}
	}
	return out
}

// proposalMapping converts status.mapping: the strategy Unmapped and the reason are gone (the Mapped condition says
// them), repoURL moved to every edit, application became applicationOption.
func proposalMapping(v any, spec obj, kept int, n *notes) obj {
	old, ok := asMap(v)
	if !ok || len(old) == 0 {
		return nil
	}
	out := obj{}
	switch strategy := asString(old["strategy"]); strategy {
	case "":
	case "Unmapped":
		n.add("status.mapping.strategy: Unmapped is dropped (a proposal that is not mapped has no strategy; the Mapped condition says why)")
	default:
		out["strategy"] = strategy
	}
	if asString(old["reason"]) != "" {
		n.add("status.mapping.reason is dropped: the Mapped condition has it")
	}
	edits := mappingEdits(old, kept, n)
	if len(edits) > 0 {
		out["edits"] = edits
	}
	if verified, ok := asBool(old["verified"]); ok && verified {
		out["verified"] = true
	}
	if d := asString(old["diff"]); d != "" {
		out["diff"] = clip(obj{"diff": d}, "status.mapping", n)["diff"]
	}
	for _, k := range []string{"collateral", "unmapped"} {
		list, _ := asList(old[k])
		if len(list) == 0 {
			continue
		}
		items := make([]any, 0, len(list))
		for _, e := range list {
			if m, ok := asMap(e); ok {
				items = append(items, clip(maps.Clone(m), "status.mapping."+k, n))
			}
		}
		out[k] = items
	}
	if app, ok := asMap(old["application"]); ok && len(app) > 0 {
		out["applicationOption"] = applicationOption(app, spec, n)
		n.add("status.mapping.application -> status.mapping.applicationOption (application \"namespace/name\" -> applicationRef)")
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mappingEdits(old obj, kept int, n *notes) []any {
	repo := asString(old[keyRepoURL])
	list, _ := asList(old["edits"])
	var edits []any
	for _, e := range list {
		m, ok := asMap(e)
		if !ok {
			continue
		}
		if idx, ok := number(m["changeIndex"]); ok && int(idx) >= kept {
			n.add("status.mapping.edits: the edit of %v is dropped, its change is beyond the %d kept", m["file"], kept)
			continue
		}
		m = maps.Clone(m)
		if _, has := m[keyRepoURL]; !has && repo != "" {
			m[keyRepoURL] = repo
		}
		edits = append(edits, clip(m, "status.mapping.edits", n))
	}
	if repo != "" {
		n.add("status.mapping.repoURL -> status.mapping.edits[].repoURL")
		if len(edits) == 0 {
			n.add("status.mapping.repoURL had no edit to move to and is dropped")
		}
	}
	return edits
}

// applicationOption converts ApplicationValue: its "namespace/name" is an object reference now.
func applicationOption(app, spec obj, n *notes) obj {
	out := maps.Clone(app)
	ref := asString(app["application"])
	delete(out, "application")
	namespace, name, found := strings.Cut(ref, "/")
	if !found {
		// No namespace was stored; the Application of the proposal is the best that is known, and it says so.
		appRef, _ := asMap(spec["application"])
		namespace, name = asString(appRef[keyNamespace]), ref
		n.add("status.mapping.application %q has no namespace: the namespace of spec.application (%s) is used", ref, namespace)
	}
	out["applicationRef"] = obj{keyName: name, keyNamespace: namespace}
	clip(out, "status.mapping.applicationOption", n)
	if parent, ok := asMap(out["parent"]); ok {
		out["parent"] = clip(maps.Clone(parent), "status.mapping.applicationOption.parent", n)
	}
	return out
}
