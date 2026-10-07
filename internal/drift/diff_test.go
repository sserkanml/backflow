package drift

import (
	"reflect"
	"testing"

	"github.com/sserkanml/backflow/api/v1alpha1"
)

type obj = map[string]interface{}
type arr = []interface{}

func TestDiff(t *testing.T) {
	tests := []struct {
		name    string
		desired obj
		live    obj
		extra   []string
		want    []v1alpha1.FieldChange
	}{
		{
			name:    "changed configmap value",
			desired: obj{"data": obj{"LOG_LEVEL": "info"}},
			live:    obj{"data": obj{"LOG_LEVEL": "debug"}},
			want:    []v1alpha1.FieldChange{{Path: "/data/LOG_LEVEL", Op: v1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`}},
		},
		{
			name:    "added and removed keys",
			desired: obj{"data": obj{"a": "1", "gone": "x"}},
			live:    obj{"data": obj{"a": "1", "new": "y"}},
			want: []v1alpha1.FieldChange{
				{Path: "/data/gone", Op: v1alpha1.OpRemove, Desired: `"x"`},
				{Path: "/data/new", Op: v1alpha1.OpAdd, Live: `"y"`},
			},
		},
		{
			name:    "replica change",
			desired: obj{"spec": obj{"replicas": float64(1)}},
			live:    obj{"spec": obj{"replicas": float64(3)}},
			want:    []v1alpha1.FieldChange{{Path: "/spec/replicas", Op: v1alpha1.OpReplace, Desired: "1", Live: "3"}},
		},
		{
			name: "container image change inside list",
			desired: obj{"spec": obj{"containers": arr{
				obj{"name": "app", "image": "nginx:1.0"}, obj{"name": "side", "image": "busybox"},
			}}},
			live: obj{"spec": obj{"containers": arr{
				obj{"name": "app", "image": "nginx:2.0"}, obj{"name": "side", "image": "busybox"},
			}}},
			want: []v1alpha1.FieldChange{{Path: "/spec/containers/0/image", Op: v1alpha1.OpReplace, Desired: `"nginx:1.0"`, Live: `"nginx:2.0"`}},
		},
		{
			name:    "list length change replaces whole array",
			desired: obj{"spec": obj{"args": arr{"a", "b"}}},
			live:    obj{"spec": obj{"args": arr{"a", "b", "c"}}},
			want:    []v1alpha1.FieldChange{{Path: "/spec/args", Op: v1alpha1.OpReplace, Desired: `["a","b"]`, Live: `["a","b","c"]`}},
		},
		{
			name:    "escaped keys",
			desired: obj{"metadata": obj{"labels": obj{"app.kubernetes.io/name": "a", "x~y": "1"}}},
			live:    obj{"metadata": obj{"labels": obj{"app.kubernetes.io/name": "b", "x~y": "2"}}},
			want: []v1alpha1.FieldChange{
				{Path: "/metadata/labels/app.kubernetes.io~1name", Op: v1alpha1.OpReplace, Desired: `"a"`, Live: `"b"`},
				{Path: "/metadata/labels/x~0y", Op: v1alpha1.OpReplace, Desired: `"1"`, Live: `"2"`},
			},
		},
		{
			name:    "builtin ignores",
			desired: obj{"metadata": obj{"name": "x", "annotations": obj{"keep": "1"}}},
			live: obj{
				"status": obj{"ready": true},
				"metadata": obj{
					"name": "x", "uid": "u", "resourceVersion": "5", "generation": float64(2),
					"creationTimestamp": "now", "managedFields": arr{obj{"a": "b"}},
					"annotations": obj{
						"keep": "1",
						"kubectl.kubernetes.io/last-applied-configuration": "{}",
						"argocd.argoproj.io/tracking-id":                   "app:x",
					},
				},
			},
			want: nil,
		},
		{
			name:    "other annotations still diff",
			desired: obj{"metadata": obj{"annotations": obj{"a": "1"}}},
			live:    obj{"metadata": obj{"annotations": obj{"a": "2"}}},
			want:    []v1alpha1.FieldChange{{Path: "/metadata/annotations/a", Op: v1alpha1.OpReplace, Desired: `"1"`, Live: `"2"`}},
		},
		{
			name:    "extra ignore covers subtree",
			desired: obj{"spec": obj{"replicas": float64(1), "template": obj{"x": "1"}}},
			live:    obj{"spec": obj{"replicas": float64(3), "template": obj{"x": "2"}}},
			extra:   []string{"/spec/replicas", "/spec/template"},
			want:    nil,
		},
		{
			name:    "extra ignore on list element",
			desired: obj{"l": arr{"a", "b"}},
			live:    obj{"l": arr{"a", "c"}},
			extra:   []string{"/l/1"},
			want:    nil,
		},
		{
			name:    "type change replaces",
			desired: obj{"spec": obj{"x": obj{"a": "1"}}},
			live:    obj{"spec": obj{"x": "str"}},
			want:    []v1alpha1.FieldChange{{Path: "/spec/x", Op: v1alpha1.OpReplace, Desired: `{"a":"1"}`, Live: `"str"`}},
		},
		{
			name:    "added whole subtree",
			desired: obj{"spec": obj{}},
			live:    obj{"spec": obj{"nodeSelector": obj{"a": "b"}}},
			want:    []v1alpha1.FieldChange{{Path: "/spec/nodeSelector", Op: v1alpha1.OpAdd, Live: `{"a":"b"}`}},
		},
		{
			name:    "no diff",
			desired: obj{"spec": obj{"replicas": float64(1), "l": arr{"a"}}},
			live:    obj{"spec": obj{"replicas": float64(1), "l": arr{"a"}}},
			want:    nil,
		},
		{
			name:    "deterministic order",
			desired: obj{"b": "1", "a": "1", "c": "1"},
			live:    obj{"b": "2", "a": "2", "c": "2"},
			want: []v1alpha1.FieldChange{
				{Path: "/a", Op: v1alpha1.OpReplace, Desired: `"1"`, Live: `"2"`},
				{Path: "/b", Op: v1alpha1.OpReplace, Desired: `"1"`, Live: `"2"`},
				{Path: "/c", Op: v1alpha1.OpReplace, Desired: `"1"`, Live: `"2"`},
			},
		},
		{name: "nil inputs", desired: nil, live: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Diff(tt.desired, tt.live, tt.extra...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
