package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1 "github.com/sserkanml/backflow/api/v1"
)

func TestParseAccessRule(t *testing.T) {
	tests := []struct {
		in      string
		want    AccessRule
		wantErr bool
	}{
		{in: "team-a:team-a", want: AccessRule{PolicyNamespaces: []string{"team-a"}, Projects: []string{"team-a"}}},
		{in: "team-a, team-b : shared,team-a", want: AccessRule{PolicyNamespaces: []string{"team-a", "team-b"}, Projects: []string{"shared", "team-a"}}},
		{in: "*:*", want: AccessRule{PolicyNamespaces: []string{"*"}, Projects: []string{"*"}}},
		{in: "team-a", wantErr: true},
		{in: ":team-a", wantErr: true},
		{in: "team-a:", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseAccessRule(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("%q: error %v, want error %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got.String() != tt.want.String() {
			t.Errorf("%q: got %s, want %s", tt.in, got, tt.want)
		}
	}
	var rules AccessRules
	if err := rules.Set("a:b"); err != nil || rules.Set("c:d") != nil || rules.String() != "a:b c:d" {
		t.Errorf("a repeatable flag keeps every rule: %q, %v", rules.String(), err)
	}
}

func TestDefaultAccessRules(t *testing.T) {
	rules := func(s ...string) AccessRules {
		var r AccessRules
		for _, v := range s {
			if err := r.Set(v); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	tests := []struct {
		name        string
		d           *ArgoCDDefaults
		ns, project string
		wantNS      bool
		wantProject bool
	}{
		{name: "no rules: the operator's namespace, every project", d: &ArgoCDDefaults{OperatorNamespace: "backflow-system"},
			ns: "backflow-system", project: "anything", wantNS: true, wantProject: true},
		{name: "no rules: any other namespace is refused", d: &ArgoCDDefaults{OperatorNamespace: "backflow-system"},
			ns: "team-a", project: "team-a"},
		{name: "no rules and an unknown operator namespace refuses everyone", d: &ArgoCDDefaults{},
			ns: "", project: "default"},
		{name: "exact namespace and project", d: &ArgoCDDefaults{Access: rules("team-a:team-a")},
			ns: "team-a", project: "team-a", wantNS: true, wantProject: true},
		{name: "exact: another project of the namespace is refused", d: &ArgoCDDefaults{Access: rules("team-a:team-a")},
			ns: "team-a", project: "team-b", wantNS: true},
		{name: "exact: another namespace is refused", d: &ArgoCDDefaults{Access: rules("team-a:team-a")},
			ns: "team-b", project: "team-a"},
		{name: "a rule list replaces the operator namespace's own access", d: &ArgoCDDefaults{OperatorNamespace: "backflow-system", Access: rules("team-a:team-a")},
			ns: "backflow-system", project: "team-a"},
		{name: "wildcard projects", d: &ArgoCDDefaults{Access: rules("team-a:*")},
			ns: "team-a", project: "whatever", wantNS: true, wantProject: true},
		{name: "wildcard namespaces", d: &ArgoCDDefaults{Access: rules("*:shared")},
			ns: "any", project: "shared", wantNS: true, wantProject: true},
		{name: "wildcard namespaces, other project", d: &ArgoCDDefaults{Access: rules("*:shared")},
			ns: "any", project: "private", wantNS: true},
		{name: "single-tenant setup", d: &ArgoCDDefaults{Access: rules("*:*")},
			ns: "any", project: "any", wantNS: true, wantProject: true},
		{name: "several rules add up", d: &ArgoCDDefaults{Access: rules("team-a:one", "team-a:two", "team-b:three")},
			ns: "team-a", project: "two", wantNS: true, wantProject: true},
		{name: "rules do not mix namespaces and projects", d: &ArgoCDDefaults{Access: rules("team-a:one", "team-b:two")},
			ns: "team-a", project: "two", wantNS: true},
		{name: "an Application without a project is in default", d: &ArgoCDDefaults{Access: rules("team-a:default")},
			ns: "team-a", project: "", wantNS: true, wantProject: true},
		{name: "no defaults at all: nothing is checked", d: nil, ns: "x", project: "y", wantNS: true, wantProject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.NamespaceAllowed(tt.ns); got != tt.wantNS {
				t.Errorf("NamespaceAllowed(%q) = %v, want %v", tt.ns, got, tt.wantNS)
			}
			if got := tt.d.ProjectAllowed(tt.ns, tt.project); got != tt.wantProject {
				t.Errorf("ProjectAllowed(%q, %q) = %v, want %v", tt.ns, tt.project, got, tt.wantProject)
			}
		})
	}
}

var _ = Describe("The default Argo CD connection's access rules", func() {
	const (
		argoNS      = "default"
		visibleApp  = "access-visible-app"
		hiddenApp   = "access-hidden-app-zq81"
		hiddenProj  = "secret-project-zq81"
		allowedProj = "team-a-project"
	)
	var (
		ctx      = context.Background()
		created  []client.Object
		recorder *events.FakeRecorder
		create   = func(obj client.Object) {
			ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
			created = append(created, obj)
		}
		newApp = func(name, project string) *unstructured.Unstructured {
			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(applicationGVK)
			app.SetName(name)
			app.SetNamespace(argoNS)
			Expect(unstructured.SetNestedField(app.Object, project, "spec", "project")).To(Succeed())
			return app
		}
		newPolicy = func(ns, name string, appNames ...string) *backflowv1.BackflowPolicy {
			p := &backflowv1.BackflowPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
				Spec: backflowv1.BackflowPolicySpec{
					ArgoCD: backflowv1.ArgoCDSpec{Namespace: argoNS}, Applications: backflowv1.ApplicationSelector{Names: appNames},
					Mode: backflowv1.ModeReportOnly,
				},
			}
			create(p)
			return p
		}
		reconcilePolicy = func(d *ArgoCDDefaults, p *backflowv1.BackflowPolicy) *backflowv1.BackflowPolicy {
			r := &BackflowPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: d, Recorder: recorder}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			got := &backflowv1.BackflowPolicy{}
			ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(p), got)).To(Succeed())
			return got
		}
		rule = func(s string) *ArgoCDDefaults {
			d := &ArgoCDDefaults{Server: "https://argocd.example", TokenSecretNamespace: "argocd", TokenSecretName: "t", OperatorNamespace: "backflow-system"}
			if s != "" {
				ExpectWithOffset(1, d.Access.Set(s)).To(Succeed())
			}
			return d
		}
	)

	BeforeEach(func() {
		created = nil
		recorder = events.NewFakeRecorder(50)
		if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}); err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
		create(newApp(visibleApp, allowedProj))
		create(newApp(hiddenApp, hiddenProj))
	})
	AfterEach(func() {
		for _, o := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, o))).To(Succeed())
		}
	})

	// shows is everything a user of the policy can read about it.
	shows := func(p *backflowv1.BackflowPolicy) string {
		status, err := json.Marshal(p.Status)
		Expect(err).NotTo(HaveOccurred())
		var evs []string
		for len(recorder.Events) > 0 {
			evs = append(evs, <-recorder.Events)
		}
		return string(status) + strings.Join(evs, "\n")
	}

	It("refuses a policy in a namespace without a rule, and names the chart value", func() {
		p := reconcilePolicy(rule("team-b:*"), newPolicy("default", "access-norule", visibleApp, hiddenApp))
		ready := meta.FindStatusCondition(p.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("ArgoCDDefaultNotAllowed"))
		Expect(ready.Message).To(And(ContainSubstring("argocd.defaultAccess"), ContainSubstring(`"default"`)))
		Expect(p.Status.Applications).To(BeEmpty())
		Expect(appNames(p.Status)).To(BeEmpty())
		Expect(shows(p)).NotTo(Or(ContainSubstring(visibleApp), ContainSubstring(hiddenApp), ContainSubstring(hiddenProj)))
	})

	It("lets only the operator's namespace use the default when there are no rules", func() {
		by := func(ns string) *backflowv1.BackflowPolicy {
			return reconcilePolicy(rule(""), newPolicy(ns, "access-opns-"+ns, "access-unrelated-app"))
		}
		Expect(meta.FindStatusCondition(by("default").Status.Conditions, "Ready").Reason).To(Equal("ArgoCDDefaultNotAllowed"))

		d := rule("")
		d.OperatorNamespace = "default"
		p := reconcilePolicy(d, newPolicy("default", "access-opns-own", visibleApp, hiddenApp))
		Expect(meta.FindStatusCondition(p.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(p.Status.Applications).To(HaveLen(2), "every project")
		Expect(p.Status.ArgoCD.HiddenCount()).To(BeZero())
	})

	It("shows only the Applications of the allowed projects, and only counts the others", func() {
		p := reconcilePolicy(rule("default:"+allowedProj), newPolicy("default", "access-count", visibleApp, hiddenApp, "access-missing-app"))
		Expect(meta.FindStatusCondition(p.Status.Conditions, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(appNames(p.Status)).To(ConsistOf(visibleApp))
		Expect(p.Status.Applications).To(HaveLen(1))
		Expect(p.Status.ArgoCD.HiddenCount()).To(BeEquivalentTo(1))
		Expect(hiddenReason(p.Status)).To(Equal("ProjectNotAllowed"))
		text := shows(p)
		Expect(text).To(ContainSubstring("1 selected application(s) excluded"))
		Expect(text).NotTo(Or(ContainSubstring(hiddenApp), ContainSubstring(hiddenProj)),
			"no name, repository, path or project of an Application the policy may not see")
	})

	It("lets a wildcard rule see every project", func() {
		p := reconcilePolicy(rule("*:*"), newPolicy("default", "access-star", visibleApp, hiddenApp))
		Expect(p.Status.Applications).To(HaveLen(2))
		Expect(p.Status.ArgoCD.HiddenCount()).To(BeZero())
		Expect(hiddenReason(p.Status)).To(BeEmpty())
	})

	It("does not apply the rules to a policy with its own spec.argoCD", func() {
		p := newPolicy("default", "access-own", visibleApp, hiddenApp)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(p), p)).To(Succeed())
		p.Spec.ArgoCD.Server = &backflowv1.ArgoCDServer{
			TokenSecretRef: backflowv1.SecretKeyReference{Name: "tok", Key: "token"},
		}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		got := reconcilePolicy(rule("team-b:other"), p)
		Expect(got.Status.Applications).To(HaveLen(2), "its own token decides what it sees, in Argo CD")
	})

	Context("proposals of an Application the policy may no longer see", func() {
		proposalFor := func(policy *backflowv1.BackflowPolicy, app string) *backflowv1.DriftProposal {
			dp := &backflowv1.DriftProposal{
				ObjectMeta: metav1.ObjectMeta{Namespace: policy.Namespace, GenerateName: "access-dp-",
					Labels: map[string]string{labelPolicy: safeLabel(policy.Name)}},
				Spec: backflowv1.DriftProposalSpec{
					PolicyRef:      backflowv1.LocalObjectRef{Name: policy.Name},
					ApplicationRef: backflowv1.ObjectRef{Name: app, Namespace: argoNS},
					ResourceRef:    backflowv1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "cm"},
					Sources: []backflowv1.ProposalSource{{Position: 1, Role: backflowv1.RoleRender, RepoURL: "https://example.com/r.git", Revision: "abc",
						Path: "apps/demo", Type: backflowv1.SourceDirectory}},
					Changes:    []backflowv1.FieldChange{{Path: "/data/K", Op: backflowv1.OpReplace, Desired: `"a"`, Live: `"b"`}},
					DetectedAt: metav1.Now(),
				},
			}
			create(dp)
			dp.Status.Phase = backflowv1.PhaseMapping
			Expect(k8sClient.Status().Update(ctx, dp)).To(Succeed())
			return dp
		}

		It("are retired by the policy controller and the proposal controller, without Argo CD", func() {
			policy := newPolicy("default", "access-revoke", visibleApp, hiddenApp)
			mine, theirs := proposalFor(policy, visibleApp), proposalFor(policy, hiddenApp)

			reconcilePolicy(rule("default:"+allowedProj), policy)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(theirs), theirs)).To(Succeed())
			Expect(theirs.Status.Signal().AccessRevoked).To(BeTrue())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(mine), mine)).To(Succeed())
			Expect(mine.Status.Signal().AccessRevoked).To(BeFalse(), "the visible Application keeps its proposal")

			r := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: rule("default:" + allowedProj)}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(theirs)})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(theirs), theirs)).To(Succeed())
			Expect(theirs.Status.Phase).To(Equal(backflowv1.PhaseSuperseded))
			Expect(meta.FindStatusCondition(theirs.Status.Conditions, conditionMapped).Reason).To(Equal(reasonAccessRevoked))
			Expect(theirs.Status.Message).NotTo(ContainSubstring(hiddenProj))
		})

		It("are retired by the proposal controller alone when the policy status has not caught up", func() {
			policy := newPolicy("default", "access-guard", hiddenApp)
			dp := proposalFor(policy, hiddenApp)
			r := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: rule("default:" + allowedProj)}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), dp)).To(Succeed())
			Expect(dp.Status.Phase).To(Equal(backflowv1.PhaseSuperseded))
		})

		It("are all retired when the namespace loses its rule", func() {
			policy := newPolicy("default", "access-lost", visibleApp)
			dp := proposalFor(policy, visibleApp)
			reconcilePolicy(rule("team-b:*"), policy)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dp), dp)).To(Succeed())
			Expect(dp.Status.Signal().AccessRevoked).To(BeTrue())
		})

		It("lists the open proposals of a changed policy", func() {
			policy := newPolicy("default", "access-enqueue", visibleApp)
			dp := proposalFor(policy, visibleApp)
			r := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(r.policyProposals(ctx, policy)).To(ConsistOf(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dp)}))
		})
	})

	Context("the parent Application of a child", func() {
		It("says nothing but that the parent is not visible when the policy may not see it", func() {
			child := newApp("access-child", allowedProj)
			child.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "access-parent:argoproj.io/Application:default/access-child"})
			parent := newApp("access-parent", hiddenProj)
			Expect(unstructured.SetNestedSlice(parent.Object, []any{map[string]any{
				"group": "argoproj.io", "kind": "Application", "namespace": argoNS, "name": "access-child"}}, "status", "resources")).To(Succeed())
			create(parent)
			create(child)

			policy := newPolicy("default", "access-parent-policy", "access-child")
			dp := &backflowv1.DriftProposal{Spec: backflowv1.DriftProposalSpec{PolicyRef: backflowv1.LocalObjectRef{Name: policy.Name}}}
			dp.Namespace = policy.Namespace
			r := &DriftProposalReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ArgoCD: rule("default:" + allowedProj)}

			parents, hidden, err := r.parentsOf(ctx, dp, child)
			Expect(err).NotTo(HaveOccurred())
			Expect(hidden).To(BeTrue())
			Expect(parents).To(BeNil(), "not even the parent object is handed on")

			r.ArgoCD = rule("default:*")
			parents, hidden, err = r.parentsOf(ctx, dp, child)
			Expect(err).NotTo(HaveOccurred())
			Expect(hidden).To(BeFalse())
			Expect(parents).To(HaveLen(1))
		})
	})
})
