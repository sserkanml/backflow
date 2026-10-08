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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

var _ = Describe("BackflowPolicy Controller", func() {
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
		backflowpolicy := &backflowv1alpha1.BackflowPolicy{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind BackflowPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, backflowpolicy)
			if err != nil && errors.IsNotFound(err) {
				resource := &backflowv1alpha1.BackflowPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: backflowv1alpha1.BackflowPolicySpec{
						ArgoCDNamespace: "argocd",
						Applications:    backflowv1alpha1.ApplicationSelector{Names: []string{"demo-app"}},
						Mode:            backflowv1alpha1.ModeReportOnly,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &backflowv1alpha1.BackflowPolicy{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance BackflowPolicy")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &BackflowPolicyReconciler{
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

func TestPolicyOlder(t *testing.T) {
	at := func(sec int64, ns, name string) *backflowv1alpha1.BackflowPolicy {
		return &backflowv1alpha1.BackflowPolicy{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(time.Unix(sec, 0)),
		}}
	}
	tests := []struct {
		name string
		a, b *backflowv1alpha1.BackflowPolicy
		want bool
	}{
		{"earlier timestamp wins over name", at(1, "x", "z"), at(2, "x", "a"), true},
		{"later timestamp loses", at(2, "x", "a"), at(1, "x", "z"), false},
		{"same timestamp, name decides", at(1, "x", "a"), at(1, "x", "b"), true},
		{"same timestamp and name, namespace decides", at(1, "a", "p"), at(1, "b", "p"), true},
		{"identical is not older", at(1, "a", "p"), at(1, "a", "p"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policyOlder(tt.a, tt.b); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

var _ = Describe("BackflowPolicy application conflicts", func() {
	const (
		argoNS      = "default"
		otherPolicy = "bfp-conflict"
	)
	var (
		ctx      = context.Background()
		created  []*backflowv1alpha1.BackflowPolicy
		apps     []*unstructured.Unstructured
		reconcil = func(p *backflowv1alpha1.BackflowPolicy) *backflowv1alpha1.BackflowPolicy {
			r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			got := &backflowv1alpha1.BackflowPolicy{}
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(p), got)).To(Succeed())
			return got
		}
		newPolicy = func(ns, name string, appNames ...string) *backflowv1alpha1.BackflowPolicy {
			p := &backflowv1alpha1.BackflowPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
				Spec: backflowv1alpha1.BackflowPolicySpec{
					ArgoCDNamespace: argoNS,
					Applications:    backflowv1alpha1.ApplicationSelector{Names: appNames},
					Mode:            backflowv1alpha1.ModeReportOnly,
				},
			}
			ExpectWithOffset(1, k8sClient.Create(ctx, p)).To(Succeed())
			created = append(created, p)
			return p
		}
		ready = func(p *backflowv1alpha1.BackflowPolicy) *metav1.Condition {
			return meta.FindStatusCondition(p.Status.Conditions, "Ready")
		}
	)

	BeforeEach(func() {
		created, apps = nil, nil
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: otherPolicy}})
		if err != nil {
			Expect(errors.IsAlreadyExists(err)).To(BeTrue())
		}
		for _, n := range []string{"conflict-app-1", "conflict-app-2", "conflict-app-3"} {
			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(applicationGVK)
			app.SetName(n)
			app.SetNamespace(argoNS)
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			apps = append(apps, app)
		}
	})

	AfterEach(func() {
		for _, p := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, p))).To(Succeed())
		}
		for _, a := range apps {
			Expect(k8sClient.Delete(ctx, a)).To(Succeed())
		}
	})

	It("blocks every policy except the oldest one that selects the same Application", func() {
		oldest := newPolicy("default", "conflict-a", "conflict-app-1", "conflict-app-2")
		// Younger, in another namespace, overlaps on conflict-app-2 only.
		second := newPolicy(otherPolicy, "conflict-b", "conflict-app-2", "conflict-app-3")
		// Overlaps only with the blocked second policy, so it must not be blocked.
		third := newPolicy("default", "conflict-c", "conflict-app-3")

		Expect(ready(reconcil(oldest)).Status).To(Equal(metav1.ConditionTrue))

		got := ready(reconcil(second))
		Expect(got.Status).To(Equal(metav1.ConditionFalse))
		Expect(got.Reason).To(Equal("ApplicationConflict"))
		Expect(got.Message).To(ContainSubstring("conflict-app-2"))
		Expect(got.Message).To(ContainSubstring("default/conflict-a"))

		Expect(ready(reconcil(third)).Status).To(Equal(metav1.ConditionTrue),
			"a blocked policy must not block a younger one")
	})

	It("unblocks a policy when the older one is deleted or stops selecting the Application", func() {
		oldest := newPolicy("default", "conflict-a", "conflict-app-1")
		second := newPolicy("default", "conflict-b", "conflict-app-1")
		Expect(ready(reconcil(second)).Reason).To(Equal("ApplicationConflict"))

		By("narrowing the older policy")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oldest), oldest)).To(Succeed())
		oldest.Spec.Applications.Names = []string{"conflict-app-2"}
		Expect(k8sClient.Update(ctx, oldest)).To(Succeed())
		Expect(ready(reconcil(second)).Status).To(Equal(metav1.ConditionTrue))

		By("selecting it again, then deleting the older policy")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oldest), oldest)).To(Succeed())
		oldest.Spec.Applications.Names = []string{"conflict-app-1"}
		Expect(k8sClient.Update(ctx, oldest)).To(Succeed())
		Expect(ready(reconcil(second)).Reason).To(Equal("ApplicationConflict"))
		Expect(k8sClient.Delete(ctx, oldest)).To(Succeed())
		Expect(ready(reconcil(second)).Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not treat policies of different Argo CD namespaces as conflicting", func() {
		first := newPolicy("default", "conflict-a", "conflict-app-1")
		second := newPolicy("default", "conflict-b", "conflict-app-1")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(second), second)).To(Succeed())
		second.Spec.ArgoCDNamespace = "elsewhere"
		Expect(k8sClient.Update(ctx, second)).To(Succeed())

		Expect(ready(reconcil(first)).Status).To(Equal(metav1.ConditionTrue))
		Expect(ready(reconcil(second)).Reason).To(Equal("NoApplicationsMatched"))
	})
})
