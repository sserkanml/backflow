package managedfields

import (
	"testing"
	"time"
)

type obj = map[string]any

func mf(manager, operation, at string, fields obj) obj {
	return obj{"manager": manager, "operation": operation, "time": at, "fieldsType": "FieldsV1", "apiVersion": "v1", "fieldsV1": fields}
}

func live(spec obj, entries ...obj) map[string]any {
	items := make([]any, len(entries))
	for i, e := range entries {
		items[i] = e
	}
	return obj{"metadata": obj{"name": "x", "managedFields": items}, "spec": spec, "data": obj{"LOG_LEVEL": "debug", "KEEP": "1"}}
}

const (
	t1 = "2026-11-01T10:00:00Z"
	t2 = "2026-11-01T11:00:00Z"
	t3 = "2026-11-01T12:00:00Z"
)

func TestAttribute(t *testing.T) {
	deployment := obj{
		"replicas": 3,
		"template": obj{
			"metadata": obj{"annotations": obj{"kubectl.kubernetes.io/restartedAt": "now"}},
			"spec": obj{"containers": []any{
				obj{"name": "sidecar", "image": "s:1"},
				obj{"name": "web", "image": "nginx:1.28", "ports": []any{obj{"containerPort": 80, "protocol": "TCP"}}},
			}},
		},
	}
	argo := mf("argocd-controller", OperationApply, t3, obj{"f:spec": obj{"f:replicas": obj{}, "f:template": obj{"f:spec": obj{
		"f:containers": obj{`k:{"name":"web"}`: obj{".": obj{}, "f:image": obj{}}}}}}})
	tests := []struct {
		name     string
		live     map[string]any
		pointers []string
		want     string // manager of the latest owner, "" for none
		wantOp   string
		others   []string
		paths    []string
	}{
		{
			name:     "a map key",
			live:     live(nil, mf("kubectl-patch", OperationUpdate, t1, obj{"f:data": obj{"f:LOG_LEVEL": obj{}}}), mf("argocd-controller", OperationUpdate, t2, obj{"f:data": obj{".": obj{}, "f:KEEP": obj{}}})),
			pointers: []string{"/data/LOG_LEVEL"},
			want:     "kubectl-patch", wantOp: OperationUpdate, paths: []string{"/data/LOG_LEVEL"},
		},
		{
			name: "a field of a list element with a merge key",
			live: live(deployment, mf("kubectl-set", OperationUpdate, t1, obj{"f:spec": obj{"f:template": obj{"f:spec": obj{
				"f:containers": obj{`k:{"name":"web"}`: obj{"f:image": obj{}}}}}}})),
			pointers: []string{"/spec/template/spec/containers/1/image"},
			want:     "kubectl-set", wantOp: OperationUpdate, paths: []string{"/spec/template/spec/containers/1/image"},
		},
		{
			name: "a list that drifted as a whole is owned through its elements",
			live: live(deployment, mf("kubectl-edit", OperationUpdate, t1, obj{"f:spec": obj{"f:template": obj{"f:spec": obj{
				"f:containers": obj{`k:{"name":"web"}`: obj{"f:image": obj{}}}}}}})),
			pointers: []string{"/spec/template/spec/containers"},
			want:     "kubectl-edit", wantOp: OperationUpdate, paths: []string{"/spec/template/spec/containers"},
		},
		{
			name: "a key with several fields",
			live: live(deployment, mf("kubectl", OperationApply, t1, obj{"f:spec": obj{"f:template": obj{"f:spec": obj{
				"f:containers": obj{`k:{"name":"web"}`: obj{"f:ports": obj{`k:{"containerPort":80,"protocol":"TCP"}`: obj{".": obj{}, "f:containerPort": obj{}}}}}}}}})),
			pointers: []string{"/spec/template/spec/containers/1/ports/0/containerPort"},
			want:     "kubectl", wantOp: OperationApply, paths: []string{"/spec/template/spec/containers/1/ports/0/containerPort"},
		},
		{
			name: "a set list, and a name with a slash",
			live: func() map[string]any {
				l := live(deployment, mf("someone", OperationUpdate, t1, obj{
					"f:metadata": obj{"f:finalizers": obj{`v:"example.com/f"`: obj{}}},
					"f:spec":     obj{"f:template": obj{"f:metadata": obj{"f:annotations": obj{"f:kubectl.kubernetes.io/restartedAt": obj{}}}}},
				}))
				l["metadata"].(obj)["finalizers"] = []any{"other", "example.com/f"}
				return l
			}(),
			pointers: []string{"/metadata/finalizers/1", "/spec/template/metadata/annotations/kubectl.kubernetes.io~1restartedAt"},
			want:     "someone", wantOp: OperationUpdate,
			paths: []string{"/metadata/finalizers/1", "/spec/template/metadata/annotations/kubectl.kubernetes.io~1restartedAt"},
		},
		{
			name:     "an atomic list by index",
			live:     live(obj{"args": []any{"a", "b"}}, mf("kubectl-patch", OperationUpdate, t1, obj{"f:spec": obj{"f:args": obj{"i:1": obj{}}}})),
			pointers: []string{"/spec/args/1"},
			want:     "kubectl-patch", wantOp: OperationUpdate, paths: []string{"/spec/args/1"},
		},
		{
			name:     "a value that is owned as a unit owns what is below it",
			live:     live(obj{"selector": obj{"app": "x"}}, mf("kubectl-patch", OperationUpdate, t1, obj{"f:spec": obj{"f:selector": obj{}}})),
			pointers: []string{"/spec/selector/app"},
			want:     "kubectl-patch", wantOp: OperationUpdate, paths: []string{"/spec/selector/app"},
		},
		{
			name:     "a removed field has no owner",
			live:     live(nil, mf("kubectl-patch", OperationUpdate, t1, obj{"f:data": obj{"f:LOG_LEVEL": obj{}}})),
			pointers: []string{"/data/GONE"},
			want:     "",
		},
		{
			name: "several managers: the latest is the actor, the rest are listed",
			live: live(deployment,
				mf("kubectl", OperationUpdate, t1, obj{"f:spec": obj{"f:replicas": obj{}}}),
				mf("kubectl-set", OperationUpdate, t2, obj{"f:spec": obj{"f:template": obj{"f:spec": obj{"f:containers": obj{`k:{"name":"web"}`: obj{"f:image": obj{}}}}}}}),
				mf("kube-controller-manager", OperationUpdate, t1, obj{"f:status": obj{"f:replicas": obj{}}})),
			pointers: []string{"/spec/replicas", "/spec/template/spec/containers/1/image"},
			want:     "kubectl-set", wantOp: OperationUpdate, others: []string{"kubectl"},
			paths: []string{"/spec/template/spec/containers/1/image"},
		},
		{
			name:     "Argo CD is left out even when it wrote last",
			live:     live(deployment, mf("kubectl-set", OperationUpdate, t1, obj{"f:spec": obj{"f:template": obj{"f:spec": obj{"f:containers": obj{`k:{"name":"web"}`: obj{"f:image": obj{}}}}}}}), argo),
			pointers: []string{"/spec/template/spec/containers/1/image"},
			want:     "kubectl-set", wantOp: OperationUpdate, paths: []string{"/spec/template/spec/containers/1/image"},
		},
		{
			name:     "only Argo CD owns it: no actor",
			live:     live(deployment, argo),
			pointers: []string{"/spec/replicas"},
			want:     "",
		},
		{
			name: "the older application controller name is Argo CD's too",
			live: live(deployment, mf("argocd-application-controller", OperationUpdate, t2, obj{"f:spec": obj{"f:replicas": obj{}}}),
				mf("kubectl-scale", OperationUpdate, t1, obj{"f:spec": obj{"f:replicas": obj{}}})),
			pointers: []string{"/spec/replicas"},
			want:     "kubectl-scale", wantOp: OperationUpdate, paths: []string{"/spec/replicas"},
		},
		{
			name: "the same manager in two entries counts once, with the later time",
			live: live(deployment,
				mf("kubectl", OperationUpdate, t1, obj{"f:spec": obj{"f:replicas": obj{}}}),
				mf("kubectl", OperationUpdate, t2, obj{"f:data": obj{"f:LOG_LEVEL": obj{}}})),
			pointers: []string{"/spec/replicas", "/data/LOG_LEVEL"},
			want:     "kubectl", wantOp: OperationUpdate, paths: []string{"/spec/replicas", "/data/LOG_LEVEL"},
		},
		{
			name:     "an owner that is not in the object any more",
			live:     live(deployment, mf("kubectl-edit", OperationUpdate, t1, obj{"f:spec": obj{"f:template": obj{"f:spec": obj{"f:containers": obj{`k:{"name":"gone"}`: obj{"f:image": obj{}}}}}}})),
			pointers: []string{"/spec/template/spec/containers/1/image"},
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Attribute(tt.live, tt.pointers)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if got != nil {
					t.Fatalf("attribution = %+v, want none", got)
				}
				return
			}
			if got == nil {
				t.Fatal("no attribution")
			}
			if got.Latest.Manager != tt.want || got.Latest.Operation != tt.wantOp {
				t.Errorf("latest = %s (%s), want %s (%s)", got.Latest.Manager, got.Latest.Operation, tt.want, tt.wantOp)
			}
			if len(got.Others) != len(tt.others) {
				t.Fatalf("others = %+v, want %v", got.Others, tt.others)
			}
			for i, o := range got.Others {
				if o.Manager != tt.others[i] {
					t.Errorf("others[%d] = %s, want %s", i, o.Manager, tt.others[i])
				}
			}
			if !sameSet(got.Latest.Paths, tt.paths) {
				t.Errorf("paths = %v, want %v", got.Latest.Paths, tt.paths)
			}
		})
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			found = found || x == y
		}
		if !found {
			return false
		}
	}
	return true
}

func TestTheLatestWinsByTime(t *testing.T) {
	l := live(obj{"replicas": 1},
		mf("zeta", OperationUpdate, t2, obj{"f:spec": obj{"f:replicas": obj{}}}),
		mf("alpha", OperationUpdate, t3, obj{"f:data": obj{"f:LOG_LEVEL": obj{}}}),
		mf("middle", OperationApply, t1, obj{"f:data": obj{"f:KEEP": obj{}}}))
	got, err := Attribute(l, []string{"/spec/replicas", "/data/LOG_LEVEL", "/data/KEEP"})
	if err != nil || got == nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if got.Latest.Manager != "alpha" || !got.Latest.Time.Equal(want) {
		t.Errorf("latest = %+v", got.Latest)
	}
	if len(got.Others) != 2 || got.Others[0].Manager != "zeta" || got.Others[1].Manager != "middle" {
		t.Errorf("others = %+v, want most recent first", got.Others)
	}
}

func TestGarbageIsAnErrorOrNothing(t *testing.T) {
	for name, l := range map[string]map[string]any{
		"no metadata":             {},
		"no managedFields":        {"metadata": obj{}},
		"null managedFields":      {"metadata": obj{"managedFields": nil}},
		"empty managedFields":     {"metadata": obj{"managedFields": []any{}}},
		"an entry without fields": {"metadata": obj{"managedFields": []any{obj{"manager": "kubectl", "operation": "Update"}}}},
		"another fields type":     {"metadata": obj{"managedFields": []any{obj{"manager": "x", "fieldsType": "Other", "fieldsV1": obj{"f:a": obj{}}}}}},
	} {
		got, err := Attribute(l, []string{"/spec/replicas"})
		if err != nil || got != nil {
			t.Errorf("%s: %+v, %v; want nothing and no error", name, got, err)
		}
	}
	for name, l := range map[string]map[string]any{
		"a string":                  {"metadata": obj{"managedFields": "nope"}},
		"an entry that is a string": {"metadata": obj{"managedFields": []any{"x"}}},
		"a bad time":                {"metadata": obj{"managedFields": []any{mf("kubectl", "Update", "yesterday", obj{"f:spec": obj{}})}}},
		"a bad field path":          {"metadata": obj{"managedFields": []any{mf("kubectl", "Update", t1, obj{"zz:spec": obj{}})}}},
		"fieldsV1 of a wrong type":  {"metadata": obj{"managedFields": []any{mf("kubectl", "Update", t1, nil)}, "x": 1}},
	} {
		got, err := Attribute(l, []string{"/spec/replicas"})
		if name == "fieldsV1 of a wrong type" {
			if err != nil || got != nil {
				t.Errorf("%s: %+v, %v", name, got, err)
			}
			continue
		}
		if err == nil || got != nil {
			t.Errorf("%s: %+v, %v; want an error", name, got, err)
		}
	}
}

func TestClassify(t *testing.T) {
	for manager, want := range map[string]Kind{
		"kubectl": Interactive, "kubectl-edit": Interactive, "kubectl-patch": Interactive, "kubectl-client-side-apply": Interactive,
		"kubectl-set": Interactive, "oc": Interactive, "oc-edit": Interactive, "Mozilla": Interactive, "Mozilla/5.0": Interactive,
		"k9s": Interactive, "helm": Interactive,
		// Known controllers, by name and by the way controllers name themselves.
		"kube-controller-manager": Controller, "kube-scheduler": Controller, "kubelet": Controller, "cloud-controller-manager": Controller,
		"cluster-autoscaler": Controller, "cert-manager-controller": Controller, "kustomize-controller": Controller,
		"helm-controller": Controller, "openshift-controller-manager": Controller, "cluster-version-operator": Controller,
		"Some-Operator": Controller,
		// Neither: a name nobody has verified is not called a controller (and not a person).
		"horizontal-pod-autoscaler": Unknown, "octopus": Unknown, "ocp-manager": Unknown, "my-custom-tool": Unknown,
		"controller": Unknown, "operator-sdk": Unknown, "": Unknown,
	} {
		if got := Classify(manager); got != want {
			t.Errorf("Classify(%q) = %s, want %s", manager, got, want)
		}
	}
	for manager, want := range map[string]bool{
		"argocd-controller": true, "argocd-application-controller": true, "argocd-server": true, "backflow": true,
		"kubectl": false, "argo-workflows": false,
	} {
		if got := IsIgnored(manager); got != want {
			t.Errorf("IsIgnored(%q) = %v, want %v", manager, got, want)
		}
	}
}
