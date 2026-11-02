package crdcheck

import (
	"context"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type versions = []apiextensionsv1.CustomResourceDefinitionVersion

func crd(name string, vs versions, stored ...string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Versions: vs},
		Status:     apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: stored},
	}
}

func TestCheck(t *testing.T) {
	v1 := versions{{Name: "v1", Served: true, Storage: true}, {Name: "v1beta1", Served: true}}
	v1WithAlpha := append(slices.Clone(v1), apiextensionsv1.CustomResourceDefinitionVersion{Name: "v1alpha1"})
	release017 := versions{{Name: "v1alpha1", Served: true, Storage: true}}
	release018 := versions{{Name: "v1beta1", Served: true, Storage: true}}
	all := func(stored ...string) []*apiextensionsv1.CustomResourceDefinition {
		return []*apiextensionsv1.CustomResourceDefinition{crd(Names[0], v1, stored...), crd(Names[1], v1, stored...), crd(Names[2], v1, stored...)}
	}
	tests := []struct {
		name    string
		crds    []*apiextensionsv1.CustomResourceDefinition
		want    string   // part of the error; empty means no error
		pending []string // the CRDs that still hold v1beta1
	}{
		{"fresh install", all("v1"), "", nil},
		{"migrated, v1alpha1 still in the CRD but no longer stored", []*apiextensionsv1.CustomResourceDefinition{crd(Names[2], v1WithAlpha, "v1")}, "", nil},
		{"no CRD at all", nil, "", nil},
		{"0.18 objects not migrated yet: runs, and says so", all("v1beta1", "v1"), "", sorted(Names)},
		{"0.18 objects of one kind not migrated yet", []*apiextensionsv1.CustomResourceDefinition{crd(Names[0], v1, "v1"), crd(Names[1], v1, "v1beta1", "v1")}, "", Names[1:2]},
		{"objects still stored as v1alpha1", []*apiextensionsv1.CustomResourceDefinition{crd(Names[2], v1WithAlpha, "v1alpha1", "v1")}, "backflow migrate", nil},
		{"only v1alpha1 stored", []*apiextensionsv1.CustomResourceDefinition{crd(Names[0], v1WithAlpha, "v1alpha1")}, "v1alpha1", nil},
		{"the CRDs of 0.17", []*apiextensionsv1.CustomResourceDefinition{crd(Names[1], release017, "v1alpha1")}, "run `backflow migrate`", nil},
		{"the CRDs of 0.18: they have no v1", []*apiextensionsv1.CustomResourceDefinition{crd(Names[1], release018, "v1beta1")}, "CRD of release 0.18", nil},
		{"a CRD with no v1 and nothing known", []*apiextensionsv1.CustomResourceDefinition{crd(Names[1], versions{{Name: "v2", Served: true, Storage: true}})}, "apply the CRDs of this release", nil},
		{"v1 is not the storage version", []*apiextensionsv1.CustomResourceDefinition{crd(Names[1], versions{{Name: "v1", Served: true}}, "v1")}, "does not serve and store", nil},
		{"one bad among good ones", []*apiextensionsv1.CustomResourceDefinition{crd(Names[0], v1, "v1"), crd(Names[2], v1WithAlpha, "v1alpha1")}, "driftproposals", nil},
	}
	scheme := runtime.NewScheme()
	_ = apiextensionsv1.AddToScheme(scheme)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme)
			for _, c := range tt.crds {
				b = b.WithObjects(c).WithStatusSubresource(c)
			}
			res, err := Check(context.Background(), b.Build())
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("error = %v, want one that says %q", err, tt.want)
			}
			slices.Sort(res.Pending)
			if !slices.Equal(res.Pending, tt.pending) {
				t.Errorf("pending = %v, want %v", res.Pending, tt.pending)
			}
		})
	}
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
