package kustomize

import (
	"fmt"
	"slices"
	"strings"
)

// Options are the fields of spec.source.kustomize of an Argo CD Application.
// Argo CD applies them on top of the repository's kustomization, so Git cannot
// change what they set.
type Options struct {
	NamePrefix, NameSuffix, Namespace string
	// Images are "name=newName:newTag" and "name:newTag" entries.
	Images            []string
	CommonLabels      map[string]string
	CommonAnnotations map[string]string
	// Replicas are name -> count.
	Replicas                map[string]string
	Components              []string
	IgnoreMissingComponents bool
	// Patches are the inline patches of the Application.
	Patches []AppPatch
	// Raw is spec.source.kustomize as the Application has it, for the position
	// of an option in its lists.
	Raw map[string]any
	// Other lists options that are set but not modelled (labelWithoutSelector,
	// forceCommonLabels, ...); they are applied by Argo CD too.
	Other []string
}

// AppPatch is an entry of spec.source.kustomize.patches.
type AppPatch struct {
	Path, Patch      string
	Kind, Name       string
	Group, Namespace string
}

// ParseOptions reads spec.source.kustomize from an Application's decoded
// source (nil when there are none).
func ParseOptions(k map[string]any) Options {
	o := Options{Raw: k}
	str := func(key string) string { s, _ := k[key].(string); return s }
	o.NamePrefix, o.NameSuffix, o.Namespace = str("namePrefix"), str("nameSuffix"), str(keyNamespace)
	if l, ok := k["images"].([]any); ok {
		for _, i := range l {
			if s, ok := i.(string); ok {
				o.Images = append(o.Images, s)
			}
		}
	}
	o.CommonLabels = stringMap(k["commonLabels"])
	o.CommonAnnotations = stringMap(k["commonAnnotations"])
	o.Replicas = map[string]string{}
	if l, ok := k[keyReplicas].([]any); ok {
		for _, r := range l {
			m, _ := r.(map[string]any)
			if name, _ := m[keyName].(string); name != "" {
				o.Replicas[name] = fmt.Sprint(m["count"])
			}
		}
	}
	if l, ok := k["components"].([]any); ok {
		for _, c := range l {
			if s, ok := c.(string); ok {
				o.Components = append(o.Components, s)
			}
		}
	}
	o.IgnoreMissingComponents, _ = k["ignoreMissingComponents"].(bool)
	if l, ok := k[keyPatches].([]any); ok {
		for _, p := range l {
			m, _ := p.(map[string]any)
			ap := AppPatch{}
			ap.Path, _ = m["path"].(string)
			ap.Patch, _ = m["patch"].(string)
			if t, ok := m["target"].(map[string]any); ok {
				ap.Kind, _ = t["kind"].(string)
				ap.Name, _ = t[keyName].(string)
				ap.Group, _ = t["group"].(string)
				ap.Namespace, _ = t[keyNamespace].(string)
			}
			o.Patches = append(o.Patches, ap)
		}
	}
	modelled := map[string]bool{"namePrefix": true, "nameSuffix": true, keyNamespace: true, "images": true, "commonLabels": true,
		"commonAnnotations": true, keyReplicas: true, "components": true, "ignoreMissingComponents": true, keyPatches: true, "version": true}
	for key, v := range k {
		if !modelled[key] && !isZero(v) {
			o.Other = append(o.Other, key)
		}
	}
	slices.Sort(o.Other)
	return o
}

func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func stringMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := map[string]string{}
	for k, x := range m {
		out[k] = fmt.Sprint(x)
	}
	return out
}

// PatchFiles are the files the Application's patches name.
func (o Options) PatchFiles() []string {
	var out []string
	for _, p := range o.Patches {
		if p.Path != "" {
			out = append(out, p.Path)
		}
	}
	return out
}

// imageName returns the name part of an image reference or an images entry
// ("registry/app:1.2" -> "registry/app"); digests and tags are cut off.
func imageName(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// SetsImage returns the entry of spec.source.kustomize.images that sets the
// image, or "". An entry "name=new:tag" matches the image by the name it
// replaces or by the name it gives.
func (o Options) SetsImage(image string) string {
	name := imageName(image)
	for _, e := range o.Images {
		before, after, replaces := strings.Cut(e, "=")
		if imageName(before) == name || (replaces && imageName(after) == name) {
			return e
		}
	}
	return ""
}
