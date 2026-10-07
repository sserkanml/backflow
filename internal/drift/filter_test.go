package drift

import (
	"reflect"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

func ks(group, kind string) v1alpha1.KindSelector {
	return v1alpha1.KindSelector{Group: group, Kind: kind}
}

func TestAllowed(t *testing.T) {
	tests := []struct {
		name             string
		group, kind      string
		include, exclude []v1alpha1.KindSelector
		want             bool
	}{
		{"no filters allow", "apps", "Deployment", nil, nil, true},
		{"core configmap", "", "ConfigMap", nil, nil, true},
		{"include match", "apps", "Deployment", []v1alpha1.KindSelector{ks("apps", "Deployment")}, nil, true},
		{"include miss", "", "ConfigMap", []v1alpha1.KindSelector{ks("apps", "Deployment")}, nil, false},
		{"include any group wildcard", "apps", "Deployment", []v1alpha1.KindSelector{ks("*", "Deployment")}, nil, true},
		{"empty group is core only", "apps", "Deployment", []v1alpha1.KindSelector{ks("", "Deployment")}, nil, false},
		{"kind prefix wildcard", "apps", "StatefulSet", []v1alpha1.KindSelector{ks("apps", "State*")}, nil, true},
		{"kind infix wildcard", "x.io", "FooBarBaz", []v1alpha1.KindSelector{ks("*", "Foo*Baz")}, nil, true},
		{"exclude wins", "apps", "Deployment", nil, []v1alpha1.KindSelector{ks("apps", "*")}, false},
		{"exclude after include", "apps", "Deployment", []v1alpha1.KindSelector{ks("*", "*")}, []v1alpha1.KindSelector{ks("apps", "Deployment")}, false},
		{"secret excluded by default", "", "Secret", nil, nil, false},
		{"secret excluded by wildcard include", "", "Secret", []v1alpha1.KindSelector{ks("*", "*")}, nil, false},
		{"secret explicitly included", "", "Secret", []v1alpha1.KindSelector{ks("", "Secret")}, nil, true},
		{"secret explicit but excluded", "", "Secret", []v1alpha1.KindSelector{ks("", "Secret")}, []v1alpha1.KindSelector{ks("", "Secret")}, false},
		{"non-core Secret kind unaffected", "x.io", "Secret", nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Allowed(tt.group, tt.kind, tt.include, tt.exclude); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIgnorePointers(t *testing.T) {
	rules := []v1alpha1.IgnoreRule{
		{KindSelector: ks("apps", "Deployment"), JSONPointers: []string{"/spec/replicas"}},
		{KindSelector: ks("apps", "Deployment"), Name: "other", JSONPointers: []string{"/spec/paused"}},
		{KindSelector: ks("", "ConfigMap"), Name: "demo-*", JSONPointers: []string{"/data/x"}},
	}
	if got := IgnorePointers(rules, "apps", "Deployment", "demo"); !reflect.DeepEqual(got, []string{"/spec/replicas"}) {
		t.Errorf("deployment: %v", got)
	}
	if got := IgnorePointers(rules, "", "ConfigMap", "demo-config"); !reflect.DeepEqual(got, []string{"/data/x"}) {
		t.Errorf("configmap: %v", got)
	}
	if got := IgnorePointers(rules, "", "Service", "demo"); got != nil {
		t.Errorf("service: %v", got)
	}
}
