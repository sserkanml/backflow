// Package migrate rewrites the objects of backflow.io/v1alpha1 to backflow.io/v1beta1 (`backflow migrate`).
//
// The conversions work on the maps the API server returns, not on Go types: the v1alpha1 types are only a
// legacy of the API package, and a conversion that is a function of a map is easy to test with the YAML of a
// real object. Every function returns the converted object and one note per field that moved, was renamed or
// was dropped, which is what --dry-run prints.
package migrate

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Group and versions of the API that is migrated.
const (
	Group         = "backflow.io"
	OldVersion    = "v1alpha1"
	NewVersion    = "v1beta1"
	oldAPIVersion = Group + "/" + OldVersion
	newAPIVersion = Group + "/" + NewVersion
)

// AnnotationMigrated marks an object the tool has rewritten. It is how a second run knows what is done: an object
// that was stored as v1alpha1 and one that was rewritten look alike through the v1alpha1 endpoint.
const AnnotationMigrated = "migrate.backflow.io/migrated-from"

type obj = map[string]any

// notes collects what a conversion did.
type notes struct{ lines []string }

// sorted returns the notes in a fixed order: the maps they come from have none, and a dry run is read and compared.
func (n *notes) sorted() []string {
	out := slices.Clone(n.lines)
	slices.Sort(out)
	return out
}

func (n *notes) add(format string, args ...any) {
	n.lines = append(n.lines, fmt.Sprintf(format, args...))
}

// ConversionError is an object that cannot be converted, with the reason.
type ConversionError struct{ Reason string }

func (e *ConversionError) Error() string { return e.Reason }

func bad(format string, args ...any) error {
	return &ConversionError{Reason: fmt.Sprintf(format, args...)}
}

// Result is a converted object.
type Result struct {
	// Object is the object in v1beta1, with its metadata and without its status.
	Object obj
	// Status is the converted status, nil when the object had none that still applies.
	Status obj
	// Notes say what changed, one line per field.
	Notes []string
}

func asMap(v any) (obj, bool) { m, ok := v.(map[string]any); return m, ok }

func asString(v any) string { s, _ := v.(string); return s }

func asBool(v any) (bool, bool) { b, ok := v.(bool); return b, ok }

func asList(v any) ([]any, bool) { l, ok := v.([]any); return l, ok }

// dedupe keeps the first of equal strings, for the fields that became sets.
func dedupe(list []any) []any {
	var seen []string
	out := make([]any, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			if slices.Contains(seen, s) {
				continue
			}
			seen = append(seen, s)
		}
		out = append(out, v)
	}
	return out
}

// secretRef converts a corev1.SecretKeySelector to a SecretKeyReference: name and key, nothing else.
func secretRef(old any, path string, n *notes, required bool) (obj, error) {
	m, ok := asMap(old)
	if !ok || len(m) == 0 {
		if required {
			return nil, bad("%s is missing", path)
		}
		return nil, nil
	}
	name, key := asString(m["name"]), asString(m["key"])
	if name == "" || key == "" {
		return nil, bad("%s needs a name and a key (name %q, key %q)", path, name, key)
	}
	if opt, ok := asBool(m["optional"]); ok && opt {
		n.add("%s.optional was true and is dropped: the Secret and its key are required", path)
	}
	return obj{"name": name, "key": key}, nil
}

// metadataOf copies the metadata of an object and marks it migrated.
func metadataOf(old obj, n *notes) (obj, error) {
	meta, ok := asMap(old["metadata"])
	if !ok || asString(meta["name"]) == "" {
		return nil, bad("the object has no metadata.name")
	}
	out := maps.Clone(meta)
	ann := obj{}
	if existing, ok := asMap(meta["annotations"]); ok {
		maps.Copy(ann, existing)
	}
	ann[AnnotationMigrated] = OldVersion
	out["annotations"] = ann
	n.add("metadata.annotations[%s] = %s", AnnotationMigrated, OldVersion)
	return out, nil
}

const (
	providerGitLab = "GitLab"
	providerGitHub = "GitHub"
)

var providers = map[string]string{"gitlab": providerGitLab, "github": providerGitHub, providerGitLab: providerGitLab, providerGitHub: providerGitHub}

// ConvertScmConnection converts a v1alpha1 ScmConnection.
func ConvertScmConnection(old obj) (*Result, error) {
	n := &notes{}
	meta, err := metadataOf(old, n)
	if err != nil {
		return nil, err
	}
	spec, ok := asMap(old["spec"])
	if !ok {
		return nil, bad("the object has no spec")
	}
	out := obj{}
	provider := asString(spec["provider"])
	if provider == "" {
		return nil, bad("spec.provider is empty")
	}
	converted, known := providers[provider]
	if !known {
		return nil, bad("spec.provider %q is not gitlab or github", provider)
	}
	if converted != provider {
		n.add("spec.provider: %s -> %s", provider, converted)
	}
	out["provider"] = converted
	url := asString(spec["url"])
	if url == "" {
		return nil, bad("spec.url is empty")
	}
	out["url"] = url
	if hosts, ok := asList(spec["hosts"]); ok && len(hosts) > 0 {
		deduped := dedupe(hosts)
		if len(deduped) != len(hosts) {
			n.add("spec.hosts: %d duplicate(s) removed (it is a set now)", len(hosts)-len(deduped))
		}
		out["hosts"] = deduped
	}
	if out["tokenSecretRef"], err = secretRef(spec["tokenSecretRef"], "spec.tokenSecretRef", n, true); err != nil {
		return nil, err
	}
	ca, err := secretRef(spec["caSecretRef"], "spec.caSecretRef", n, false)
	if err != nil {
		return nil, err
	}
	if ca != nil {
		out["caSecretRef"] = ca
	}
	if committer, ok := asMap(spec["committer"]); ok && len(committer) > 0 {
		out["committer"] = committer
	}

	res := &Result{Object: obj{"apiVersion": newAPIVersion, "kind": "ScmConnection", "metadata": meta, "spec": out}}
	res.Status = scmStatus(old["status"], n)
	res.Notes = n.sorted()
	return res, nil
}

// scmStatus keeps the status as it is: the conditions, who the token authenticated as and when it was checked all
// still apply. status.hosts is new and the operator fills it in.
func scmStatus(old any, n *notes) obj {
	st, ok := asMap(old)
	if !ok || len(st) == 0 {
		return nil
	}
	out := obj{}
	for _, k := range []string{"observedGeneration", "conditions", "authenticatedAs", "lastCheckTime"} {
		if v, ok := st[k]; ok {
			out[k] = v
		}
	}
	n.add("status: kept (conditions, authenticatedAs, lastCheckTime); status.hosts is filled in by the operator")
	return out
}

// ConvertBackflowPolicy converts a v1alpha1 BackflowPolicy.
func ConvertBackflowPolicy(old obj) (*Result, error) {
	n := &notes{}
	meta, err := metadataOf(old, n)
	if err != nil {
		return nil, err
	}
	spec, ok := asMap(old["spec"])
	if !ok {
		return nil, bad("the object has no spec")
	}
	out := obj{}

	argo := obj{}
	if ns := asString(spec["argoCDNamespace"]); ns != "" {
		argo["namespace"] = ns
		n.add("spec.argoCDNamespace -> spec.argoCD.namespace")
	}
	if own, ok := asMap(spec["argoCD"]); ok && len(own) > 0 {
		server := obj{}
		if url := asString(own["url"]); url != "" {
			server["url"] = url
		}
		if v, ok := asBool(own["insecureSkipTLSVerify"]); ok {
			server["insecureSkipTLSVerify"] = v
		}
		if server["tokenSecretRef"], err = secretRef(own["tokenSecretRef"], "spec.argoCD.tokenSecretRef", n, true); err != nil {
			return nil, err
		}
		ca, err := secretRef(own["caSecretRef"], "spec.argoCD.caSecretRef", n, false)
		if err != nil {
			return nil, err
		}
		if ca != nil {
			server["caSecretRef"] = ca
		}
		argo["server"] = server
		n.add("spec.argoCD.{url, tokenSecretRef, caSecretRef, insecureSkipTLSVerify} -> spec.argoCD.server.{...}")
	}
	if len(argo) > 0 {
		out["argoCD"] = argo
	}

	apps, ok := asMap(spec["applications"])
	if !ok {
		return nil, bad("spec.applications is missing")
	}
	apps = copyMap(apps)
	if names, ok := asList(apps["names"]); ok {
		deduped := dedupe(names)
		if len(deduped) != len(names) {
			n.add("spec.applications.names: %d duplicate(s) removed (it is a set now)", len(names)-len(deduped))
		}
		apps["names"] = deduped
	}
	out["applications"] = apps

	for _, k := range []string{"mode", "include", "exclude", "ignoreFields", "mapping", "batchWindow"} {
		if v, ok := spec[k]; ok {
			out[k] = v
		}
	}
	if helm, ok := asMap(spec["helm"]); ok {
		out["helm"] = renameKeys(helm, map[string]string{
			"fallbackToApplicationOverride": "allowApplicationOverride",
			"addOverrides":                  "allowValueOverrides",
		}, "spec.helm", n)
	}
	if kust, ok := asMap(spec["kustomize"]); ok {
		out["kustomize"] = renameKeys(kust, map[string]string{
			"editBases":  "allowBaseEdits",
			"addPatches": "allowNewPatches",
		}, "spec.kustomize", n)
	}
	if mr, ok := asMap(spec["mergeRequest"]); ok && len(mr) > 0 {
		out["git"] = gitOf(mr, n)
	}
	// spec.retention is not set: the API default (14 days for finished proposals) applies.

	res := &Result{Object: obj{"apiVersion": newAPIVersion, "kind": "BackflowPolicy", "metadata": meta, "spec": out}}
	res.Status = policyStatus(old["status"], n)
	res.Notes = n.sorted()
	return res, nil
}

func copyMap(m obj) obj {
	return maps.Clone(m)
}

// renameKeys copies a map renaming keys, with a note for each rename.
func renameKeys(m obj, renames map[string]string, path string, n *notes) obj {
	out := obj{}
	for k, v := range m {
		if to, ok := renames[k]; ok {
			n.add("%s.%s -> %s.%s", path, k, path, to)
			k = to
		}
		out[k] = v
	}
	return out
}

// gitOf splits spec.mergeRequest: the branches are settings of Git, the rest of the merge request.
func gitOf(mr obj, n *notes) obj {
	git, req := obj{}, obj{}
	for k, v := range mr {
		switch k {
		case "branchPrefix", "targetBranch":
			git[k] = v
			n.add("spec.mergeRequest.%s -> spec.git.%s", k, k)
		case "labels", "reviewers":
			if list, ok := asList(v); ok {
				v = dedupe(list)
			}
			req[k] = v
			n.add("spec.mergeRequest.%s -> spec.git.mergeRequest.%s", k, k)
		case "assignActor":
			// The old field could not be set to false from Go (omitempty with a default of true); what the API server
			// returns is what is in effect, and it is a pointer now, so that false stays false.
			if b, ok := asBool(v); ok {
				req[k] = b
				n.add("spec.mergeRequest.assignActor (%t) -> spec.git.mergeRequest.assignActor", b)
			}
		}
	}
	if len(req) > 0 {
		git["mergeRequest"] = req
	}
	return git
}

// policyStatus converts what still applies of the status of a policy. The operator rebuilds all of it at its
// next pass, so a value that has no place any more is dropped, not an error.
func policyStatus(old any, n *notes) obj {
	st, ok := asMap(old)
	if !ok || len(st) == 0 {
		return nil
	}
	out := obj{}
	for _, k := range []string{"observedGeneration", "conditions", "openProposals"} {
		if v, ok := st[k]; ok {
			out[k] = v
		}
	}
	if apps, ok := asList(st["applications"]); ok {
		converted := make([]any, 0, len(apps))
		for _, a := range apps {
			if m, ok := asMap(a); ok {
				converted = append(converted, applicationSummary(m))
			}
		}
		out["applicationCount"] = int64(len(converted))
		if len(converted) > maxStatusApplications {
			converted = converted[:maxStatusApplications]
			out["applicationsTruncated"] = true
		}
		out["applications"] = converted
	}
	if a, ok := asMap(st["argoCD"]); ok && len(a) > 0 {
		argo := obj{"source": a["source"], "url": a["url"]}
		if count, ok := number(st["excludedApplications"]); ok && count > 0 {
			if reason := asString(st["excludedReason"]); reason != "" {
				argo["hiddenApplications"] = []any{obj{"reason": reason, "count": count}}
			}
		}
		out["argoCD"] = argo
	}
	n.add("status: kept (conditions, applications with their sources, argoCD); matchedApplications is dropped, " +
		"excludedApplications/excludedReason became argoCD.hiddenApplications")
	return out
}

// maxStatusApplications is how many Applications status.applications lists.
const maxStatusApplications = 100

func number(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

var sourceTypes = []string{"Directory", "Kustomize", "Helm", "Plugin"}

// applicationSummary converts an entry of status.applications: the fields of the first source move into
// sources[0] (an Application with one source has one entry), scmConnection is a name, reason became untrackedReason.
func applicationSummary(old obj) obj {
	out := obj{"name": old["name"]}
	if v, ok := number(old["managedResources"]); ok {
		out["managedResources"] = v
	}
	if r := asString(old["reason"]); r != "" {
		out["untrackedReason"] = r
	}
	var sources []any
	if list, ok := asList(old["sources"]); ok && len(list) > 0 {
		for _, s := range list {
			if m, ok := asMap(s); ok {
				sources = append(sources, sourceSummary(m))
			}
		}
	} else {
		first := obj{"position": int64(1)}
		for _, k := range []string{"repoURL", "path", "targetRevision", "syncedRevision", "sourceType", "scmConnection"} {
			if v, ok := old[k]; ok {
				first[k] = v
			}
		}
		// A single-source Application reports no other revision than the one it synced.
		if v, ok := old["syncedRevision"]; ok {
			first["revision"] = v
		}
		sources = []any{sourceSummary(first)}
	}
	if len(sources) > 0 {
		out["sources"] = sources
	}
	return out
}

func sourceSummary(old obj) obj {
	out := obj{}
	for _, k := range []string{"position", "repoURL", "path", "chart", "targetRevision", "revision", "syncedRevision", "ref"} {
		if v, ok := old[k]; ok && v != "" {
			out[k] = v
		}
	}
	if t := asString(old["sourceType"]); slices.Contains(sourceTypes, t) {
		out["type"] = t
	}
	if c := asString(old["scmConnection"]); c != "" {
		out["scmConnectionName"] = c
	}
	return out
}

// ObjectKind is the kind a conversion function is for.
type ObjectKind string

// The kinds of backflow.io that hold state.
const (
	KindScmConnection ObjectKind = "ScmConnection"
	KindPolicy        ObjectKind = "BackflowPolicy"
	KindProposal      ObjectKind = "DriftProposal"
	KindChangeRecord  ObjectKind = "ChangeRecord"
)

// Convert converts an object of the kind, or says that the kind is not converted (a ChangeRecord is dropped).
func Convert(kind ObjectKind, old obj) (*Result, error) {
	switch kind {
	case KindScmConnection:
		return ConvertScmConnection(old)
	case KindPolicy:
		return ConvertBackflowPolicy(old)
	case KindProposal:
		return ConvertDriftProposal(old, "")
	}
	return nil, errors.New("the " + strings.ToLower(string(kind)) + " is not converted by this version of the tool")
}
