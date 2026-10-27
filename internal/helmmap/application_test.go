package helmmap

import (
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/appvalues"
)

func TestApplicationOptions(t *testing.T) {
	repo := fstest.MapFS{
		"c/Chart.yaml":      {Data: []byte("apiVersion: v2\nname: c\nversion: 0.1.0\n")},
		"c/values.yaml":     {Data: []byte("config:\n  feature: alpha\nimage:\n  tag: 1.27\nreplicaCount: 1\nui:\n  color: red\n")},
		"c/values-dev.yaml": {Data: []byte("config:\n  feature: beta\n")},
	}
	spec := Spec{
		Path: "c", ValueFiles: []string{"values-dev.yaml"},
		Parameters:   []Parameter{{Name: "config.feature", Value: "beta"}, {Name: "image.tag", Value: "1.27"}},
		ValuesObject: []byte(`{"replicaCount":3,"ui":{"color":"red"}}`),
	}
	helm := map[string]any{
		"parameters": []any{
			map[string]any{"name": "config.feature", "value": "beta"},
			map[string]any{"name": "image.tag", "value": "1.27"},
		},
		"valuesObject": map[string]any{"replicaCount": float64(3), "ui": map[string]any{"color": "red"}},
	}
	src, err := NewSource(repo, spec)
	if err != nil {
		t.Fatal(err)
	}
	files, err := CollectFiles(repo, src)
	if err != nil {
		t.Fatal(err)
	}
	changes := []v1alpha1.FieldChange{
		{Path: "/data/FEATURE", Op: v1alpha1.OpReplace, Desired: `"beta"`, Live: `"gamma"`},
		{Path: "/spec/image", Op: v1alpha1.OpReplace, Desired: `"repo/app:1.27"`, Live: `"repo/app:1.28"`},
		{Path: "/spec/replicas", Op: v1alpha1.OpReplace, Desired: `"3"`, Live: `"5"`},
		{Path: "/data/COLOR", Op: v1alpha1.OpReplace, Desired: `"red"`, Live: `"blue"`},
		{Path: "/data/OTHER", Op: v1alpha1.OpReplace, Desired: `"zzz"`, Live: `"yyy"`},
	}
	fields, err := FieldsFor(src, files, changes)
	if err != nil {
		t.Fatal(err)
	}
	unmapped := map[string]bool{"/data/FEATURE": true, "/spec/image": true, "/spec/replicas": true, "/data/COLOR": true, "/data/OTHER": true}
	got := ApplicationOptions(appvalues.SingleSource, fields, unmapped, helm)
	want := []appvalues.Option{
		{Option: "spec.source.helm.parameters[name=config.feature]", Path: "/spec/source/helm/parameters/0/value", Current: `"beta"`, Suggested: `"gamma"`, Fields: []string{"/data/FEATURE"}},
		{Option: "spec.source.helm.parameters[name=image.tag]", Path: "/spec/source/helm/parameters/1/value", Current: `"1.27"`, Suggested: `"1.28"`, Fields: []string{"/spec/image"}},
		{Option: "spec.source.helm.valuesObject.replicaCount", Path: "/spec/source/helm/valuesObject/replicaCount", Current: "3", Suggested: "5", Fields: []string{"/spec/replicas"}},
		{Option: "spec.source.helm.valuesObject.ui.color", Path: "/spec/source/helm/valuesObject/ui/color", Current: `"red"`, Suggested: `"blue"`, Fields: []string{"/data/COLOR"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%+v\nwant:\n%+v", got, want)
	}
	if only := ApplicationOptions(appvalues.SingleSource, fields, map[string]bool{"/data/OTHER": true}, helm); len(only) != 0 {
		t.Errorf("a field no option sets: %+v", only)
	}
	if none := ApplicationOptions(appvalues.SingleSource, fields, map[string]bool{}, helm); len(none) != 0 {
		t.Errorf("no unmapped field: %+v", none)
	}
}
