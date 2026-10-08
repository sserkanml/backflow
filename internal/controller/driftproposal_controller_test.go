/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

var _ = Describe("DriftProposal Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		driftproposal := &backflowv1alpha1.DriftProposal{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind DriftProposal")
			err := k8sClient.Get(ctx, typeNamespacedName, driftproposal)
			if err != nil && errors.IsNotFound(err) {
				resource := &backflowv1alpha1.DriftProposal{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: backflowv1alpha1.DriftProposalSpec{
						PolicyName:  "demo",
						Application: backflowv1alpha1.ObjectRef{Name: "demo-app", Namespace: "argocd"},
						Resource: backflowv1alpha1.ResourceRef{
							Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "demo-config",
						},
						Source: backflowv1alpha1.SourceRef{
							RepoURL: "https://example.com/repo.git", Revision: "abc123",
							Path: "apps/demo", Type: backflowv1alpha1.SourceDirectory,
						},
						Changes: []backflowv1alpha1.FieldChange{{
							Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`,
						}},
						DetectedAt: metav1.Now(),
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &backflowv1alpha1.DriftProposal{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance DriftProposal")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("rejects spec updates but accepts status updates", func() {
			resource := &backflowv1alpha1.DriftProposal{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			resource.Spec.Changes[0].Live = `"trace"`
			err := k8sClient.Update(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsInvalid(err)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("spec is immutable"))

			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Status.Phase = backflowv1alpha1.PhaseDetected
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &DriftProposalReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
