package crdcheck

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func crd(name string, versions []apiextensionsv1.CustomResourceDefinitionVersion, stored ...string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Versions: versions},
		Status:     apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: stored},
	}
}

func TestCheck(t *testing.T) {
	both := []apiextensionsv1.CustomResourceDefinitionVersion{
		{Name: "v1alpha1", Served: false, Storage: false}, {Name: "v1beta1", Served: true, Storage: true}}
	onlyOld := []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true}}
	only := both[1:]
	tests := []struct {
		name string
		crds []*apiextensionsv1.CustomResourceDefinition
		want string // part of the error; empty means no error
	}{
		{"fresh install", []*apiextensionsv1.CustomResourceDefinition{crd(Names[0], only, "v1beta1"), crd(Names[1], only, "v1beta1"), crd(Names[2], only, "v1beta1")}, ""},
		{"migrated, v1alpha1 still in the CRD but no longer stored", []*apiextensionsv1.CustomResourceDefinition{crd(Names[2], both, "v1beta1")}, ""},
		{"no CRD at all", nil, ""},
		{"objects still stored as v1alpha1", []*apiextensionsv1.CustomResourceDefinition{crd(Names[2], both, "v1alpha1", "v1beta1")}, "backflow migrate"},
		{"only v1alpha1 stored", []*apiextensionsv1.CustomResourceDefinition{crd(Names[0], both, "v1alpha1")}, "v1alpha1"},
		{"old CRDs from the previous release", []*apiextensionsv1.CustomResourceDefinition{crd(Names[1], onlyOld, "v1alpha1")}, "run `backflow migrate`"},
		{"a CRD with no v1beta1 and nothing stored as v1alpha1", []*apiextensionsv1.CustomResourceDefinition{crd(Names[1], onlyOld)}, "apply the CRDs of this release"},
		{"v1beta1 is not the storage version", []*apiextensionsv1.CustomResourceDefinition{crd(Names[1],
			[]apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1beta1", Served: true}}, "v1beta1")}, "does not serve and store"},
		{"one bad among good ones", []*apiextensionsv1.CustomResourceDefinition{crd(Names[0], only, "v1beta1"), crd(Names[2], both, "v1alpha1")}, driftproposals},
	}
	scheme := runtime.NewScheme()
	_ = apiextensionsv1.AddToScheme(scheme)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme)
			for _, c := range tt.crds {
				b = b.WithObjects(c).WithStatusSubresource(c)
			}
			err := Check(context.Background(), b.Build())
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("error = %v, want one that says %q", err, tt.want)
			}
		})
	}
}

const driftproposals = "driftproposals.backflow.io"
