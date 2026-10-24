package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

func TestMappingFingerprintCoversKustomizeOptions(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"source": map[string]any{
		"path": "kust/overlays/dev", "kustomize": map[string]any{"namePrefix": "a-"},
	}}}}
	policy := &backflowv1alpha1.BackflowPolicy{}
	base := mappingFingerprint(app, policy)

	edit := policy.DeepCopy()
	edit.Spec.Kustomize = &backflowv1alpha1.KustomizeOptions{EditBases: true}
	if mappingFingerprint(app, edit) == base {
		t.Error("spec.kustomize.editBases must change the fingerprint")
	}
	patches := false
	noPatches := policy.DeepCopy()
	noPatches.Spec.Kustomize = &backflowv1alpha1.KustomizeOptions{AddPatches: &patches}
	if mappingFingerprint(app, noPatches) == base || mappingFingerprint(app, noPatches) == mappingFingerprint(app, edit) {
		t.Error("spec.kustomize.addPatches must change the fingerprint")
	}
	other := app.DeepCopy()
	_ = unstructured.SetNestedField(other.Object, "b-", "spec", "source", "kustomize", "namePrefix")
	if mappingFingerprint(other, policy) == base {
		t.Error("spec.source.kustomize options must change the fingerprint")
	}
}
