package controller

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

// specSources returns spec.sources of an Application: nil for an Application
// with a single source.
func specSources(app *unstructured.Unstructured) []map[string]any {
	items, _, _ := unstructured.NestedSlice(app.Object, "spec", "sources")
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		out = append(out, m)
	}
	return out
}

// statusStrings reads a list of strings of the Application's status, such as
// status.sync.revisions: one entry per source, in order.
func statusStrings(app *unstructured.Unstructured, path ...string) []string {
	out, _, _ := unstructured.NestedStringSlice(app.Object, path...)
	return out
}

func at(list []string, i int) string {
	if i < len(list) {
		return list[i]
	}
	return ""
}

func strField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// summarizeSources describes every source of a multi-source Application, with
// what Argo CD resolved for each: status.sync.revisions, the revisions of the
// last sync and status.sourceTypes are positional. It returns nil for an
// Application with one source.
func summarizeSources(app *unstructured.Unstructured) []backflowv1alpha1.SourceSummary {
	sources := specSources(app)
	if len(sources) == 0 {
		return nil
	}
	compared := statusStrings(app, fieldStatus, "sync", "revisions")
	synced := statusStrings(app, fieldStatus, fieldOperationState, fieldSyncResult, "revisions")
	types := statusStrings(app, fieldStatus, "sourceTypes")
	out := make([]backflowv1alpha1.SourceSummary, len(sources))
	for i, src := range sources {
		s := backflowv1alpha1.SourceSummary{
			Position: int32(i + 1), RepoURL: strField(src, "repoURL"), Path: strField(src, "path"),
			Chart: strField(src, "chart"), TargetRevision: strField(src, "targetRevision"), Ref: strField(src, "ref"),
			Revision: at(compared, i), SyncedRevision: at(synced, i), SourceType: at(types, i),
		}
		if s.SourceType == "" && s.Ref == "" && len(types) == 0 {
			// Not reconciled yet: say what the spec says.
			switch {
			case s.Chart != "" || src["helm"] != nil:
				s.SourceType = "Helm"
			case src["kustomize"] != nil:
				s.SourceType = "Kustomize"
			default:
				s.SourceType = "Directory"
			}
		}
		out[i] = s
	}
	return out
}

// isGitSource reports whether a source of the summary is a Git repository: it
// has no chart, which Argo CD pulls from a Helm repository or a registry.
func isGitSource(s backflowv1alpha1.SourceSummary) bool { return s.Chart == "" }

// multiSource describes the one shape of multi-source Application that is
// mapped: a single Helm source, which renders, plus Git ref sources whose files
// its value files name as $ref/path.
type multiSource struct {
	// Render is the index (0-based) of the Helm source, Refs maps the name of
	// each ref source to its index.
	Render int
	Refs   map[string]int
}

// helmMultiSource recognises the shape. why is empty for a match; otherwise it
// says in a sentence what is outside the shape, for an Event or a reason.
func helmMultiSource(app *unstructured.Unstructured) (*multiSource, string) {
	sources := specSources(app)
	if len(sources) == 0 {
		return nil, "the Application has one source"
	}
	ms := &multiSource{Render: -1, Refs: map[string]int{}}
	types := statusStrings(app, fieldStatus, "sourceTypes")
	for i, src := range sources {
		if ref := strField(src, "ref"); ref != "" {
			if strField(src, "chart") != "" || strField(src, "path") != "" && strField(src, "path") != "." {
				return nil, fmt.Sprintf("source %d is a ref source that also renders", i+1)
			}
			ms.Refs[ref] = i
			continue
		}
		if ms.Render >= 0 {
			return nil, fmt.Sprintf("sources %d and %d both render", ms.Render+1, i+1)
		}
		ms.Render = i
		kind := at(types, i)
		switch {
		case kind != "" && kind != "Helm":
			return nil, fmt.Sprintf("source %d is a %s source", i+1, kind)
		case strField(src, "chart") == "" && src["helm"] == nil && kind != "Helm":
			return nil, fmt.Sprintf("source %d is not a Helm source", i+1)
		case src["plugin"] != nil:
			return nil, fmt.Sprintf("source %d uses a plugin", i+1)
		}
	}
	if ms.Render < 0 {
		return nil, "no source renders"
	}
	if len(ms.Refs) == 0 {
		return nil, "the Application has several sources but no ref source"
	}
	return ms, ""
}

// unsupportedMultiSource is what is said about a multi-source Application that
// is outside the mapped shape.
func unsupportedMultiSource(why string) string {
	return fmt.Sprintf("it has several sources, and only a Helm source with Git ref sources ($values) is supported: %s", why)
}

// valueFileRefs returns, for the Helm source, each value file path named
// $ref/path as (ref name, path inside that repository, true), in order; plain
// value files (relative to the chart) give an empty ref name.
func valueFileRefs(helm map[string]any) (refs []refFile) {
	files, _, _ := unstructured.NestedStringSlice(helm, "valueFiles")
	params, _, _ := unstructured.NestedSlice(helm, "fileParameters")
	for _, p := range params {
		if m, ok := p.(map[string]any); ok {
			if path := strField(m, "path"); path != "" {
				files = append(files, path)
			}
		}
	}
	for _, f := range files {
		refs = append(refs, splitRefPath(f))
	}
	return refs
}

// refFile is a file named by a value file or file parameter of the Helm source.
type refFile struct {
	Raw string
	// Ref is the name of the ref source, empty for a file of the chart.
	Ref string
	// Path is the path inside the ref's repository, or the path as written.
	Path string
}

// splitRefPath splits "$values/env/dev.yaml" into the ref "values" and the path
// "env/dev.yaml", also for the "${values}" spelling.
func splitRefPath(raw string) refFile {
	f := refFile{Raw: raw, Path: raw}
	rest, ok := strings.CutPrefix(raw, "$")
	if !ok {
		return f
	}
	name, path, found := strings.Cut(rest, "/")
	name = strings.TrimSuffix(strings.TrimPrefix(name, "{"), "}")
	if !found || name == "" {
		return f
	}
	f.Ref, f.Path = name, strings.TrimPrefix(path, "/")
	return f
}
