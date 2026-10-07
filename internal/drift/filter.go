package drift

import (
	"strings"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

// Allowed reports whether a resource of the given group and kind is captured
// by a policy: it must match Include (when set) and must not match Exclude.
// Secrets are excluded unless an Include entry names the Secret kind literally.
func Allowed(group, kind string, include, exclude []v1alpha1.KindSelector) bool {
	if len(include) > 0 && !matchesAny(include, group, kind) {
		return false
	}
	if matchesAny(exclude, group, kind) {
		return false
	}
	if group == "" && kind == "Secret" {
		for _, s := range include {
			if s.Kind == "Secret" && matchesSelector(s, group, kind) {
				return true
			}
		}
		return false
	}
	return true
}

// IgnorePointers returns the JSON pointers of all IgnoreRules that apply to
// the given resource.
func IgnorePointers(rules []v1alpha1.IgnoreRule, group, kind, name string) []string {
	var out []string
	for _, r := range rules {
		if !matchesSelector(r.KindSelector, group, kind) {
			continue
		}
		if r.Name != "" && !wildcardMatch(r.Name, name) {
			continue
		}
		out = append(out, r.JSONPointers...)
	}
	return out
}

func matchesAny(sels []v1alpha1.KindSelector, group, kind string) bool {
	for _, s := range sels {
		if matchesSelector(s, group, kind) {
			return true
		}
	}
	return false
}

func matchesSelector(s v1alpha1.KindSelector, group, kind string) bool {
	return wildcardMatch(s.Group, group) && wildcardMatch(s.Kind, kind)
}

// wildcardMatch matches s against pattern where '*' matches any run of
// characters. An empty pattern only matches an empty string.
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
