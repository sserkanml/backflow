package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1 "github.com/sserkanml/backflow/api/v1"
)

// accessAll matches any namespace or any project in an AccessRule.
const accessAll = "*"

// defaultProject is the Argo CD project of an Application that names none.
const defaultProject = "default"

// AccessRule says which Applications the policies of some namespaces may match
// through the operator's default Argo CD connection: the ones in the listed Argo CD
// projects. "*" matches every namespace or every project.
type AccessRule struct {
	PolicyNamespaces []string
	Projects         []string
}

// ParseAccessRule parses the value of --argocd-default-access:
// "<namespace>[,<namespace>...]:<project>[,<project>...]", for example
// "team-a:team-a", "team-a,team-b:shared,team-a" or "*:*".
func ParseAccessRule(s string) (AccessRule, error) {
	nsPart, projPart, ok := strings.Cut(s, ":")
	if !ok {
		return AccessRule{}, fmt.Errorf("%q: want <namespace>[,<namespace>...]:<project>[,<project>...]", s)
	}
	rule := AccessRule{PolicyNamespaces: splitList(nsPart), Projects: splitList(projPart)}
	if len(rule.PolicyNamespaces) == 0 || len(rule.Projects) == 0 {
		return AccessRule{}, fmt.Errorf("%q: both the namespaces and the projects need at least one entry", s)
	}
	return rule, nil
}

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// String is the flag form of the rule.
func (r AccessRule) String() string {
	return strings.Join(r.PolicyNamespaces, ",") + ":" + strings.Join(r.Projects, ",")
}

func (r AccessRule) coversNamespace(ns string) bool {
	return slices.Contains(r.PolicyNamespaces, accessAll) || slices.Contains(r.PolicyNamespaces, ns)
}

func (r AccessRule) coversProject(project string) bool {
	return slices.Contains(r.Projects, accessAll) || slices.Contains(r.Projects, project)
}

// AccessRules is a repeatable flag.
type AccessRules []AccessRule

func (a *AccessRules) String() string {
	parts := make([]string, len(*a))
	for i, r := range *a {
		parts[i] = r.String()
	}
	return strings.Join(parts, " ")
}

// Set adds one rule.
func (a *AccessRules) Set(s string) error {
	rule, err := ParseAccessRule(s)
	if err != nil {
		return err
	}
	*a = append(*a, rule)
	return nil
}

// NamespaceAllowed reports whether the policies of a namespace may use the
// operator's default connection at all. With no rules, only the policies of the
// operator's own namespace may, for every project.
func (d *ArgoCDDefaults) NamespaceAllowed(ns string) bool {
	if d == nil {
		return true
	}
	if len(d.Access) == 0 {
		return d.OperatorNamespace != "" && ns == d.OperatorNamespace
	}
	return slices.ContainsFunc(d.Access, func(r AccessRule) bool { return r.coversNamespace(ns) })
}

// ProjectAllowed reports whether a policy of the namespace may match the
// Applications of an Argo CD project through the operator's default connection.
func (d *ArgoCDDefaults) ProjectAllowed(ns, project string) bool {
	if d == nil {
		return true
	}
	if project == "" {
		project = defaultProject
	}
	if len(d.Access) == 0 {
		return d.NamespaceAllowed(ns)
	}
	return slices.ContainsFunc(d.Access, func(r AccessRule) bool { return r.coversNamespace(ns) && r.coversProject(project) })
}

// applicationProject is the Argo CD project of an Application.
func applicationProject(app *unstructured.Unstructured) string {
	if p, _, _ := unstructured.NestedString(app.Object, "spec", "project"); p != "" {
		return p
	}
	return defaultProject
}

// usesDefaultAccess reports whether the access rules of the operator's default
// connection apply to the policy: it has no spec.argoCD of its own.
func usesDefaultAccess(hasOwnArgoCD bool, d *ArgoCDDefaults) bool {
	return !hasOwnArgoCD && d != nil
}

// reasonArgoCDDefaultNotAllowed is the reason of a policy that may not use the
// operator's default Argo CD connection.
const reasonArgoCDDefaultNotAllowed = "ArgoCDDefaultNotAllowed"

// defaultNotAllowedMessage names the chart value that grants the access. It names
// no Application.
func defaultNotAllowedMessage(ns string) string {
	return fmt.Sprintf("BackflowPolicies in namespace %q may not use the operator's default Argo CD connection. "+
		"Give the policy its own spec.argoCD, or ask the administrator to add the namespace to the chart value "+
		"argocd.defaultAccess (flag --argocd-default-access).", ns)
}

// mayManage reports whether the proposal's policy may see the Application. For a
// policy that uses the operator's default connection it is the rule of its namespace;
// for a policy with its own token it is what that token can get from Argo CD.
func (r *DriftProposalReconciler) mayManage(ctx context.Context, policy *backflowv1.BackflowPolicy,
	app *unstructured.Unstructured) (bool, error) {
	if usesDefaultAccess(policy.Spec.ArgoCD.Server != nil, r.ArgoCD) {
		return r.ArgoCD.ProjectAllowed(policy.Namespace, applicationProject(app)), nil
	}
	v, err := tokenVisible(ctx, r.Visibility, r.Client, policy, r.ArgoCD, app.GetName(), app.GetNamespace())
	if err != nil {
		return false, err
	}
	if v == VisibilityUnknown {
		// Not asked yet and Argo CD cannot be reached: retry later, say nothing, decide nothing.
		return false, errVisibilityUnknown
	}
	return v == VisibilityVisible, nil
}

// errVisibilityUnknown retries the lookup that asked; it is not a verdict.
var errVisibilityUnknown = errors.New("could not ask Argo CD whether the policy's token may see the Application")

// accessRevoked reports whether the proposal's Application is one its policy may not
// see. It reads the policy and the Application from the cache; it calls no Argo CD.
func (r *DriftProposalReconciler) accessRevoked(ctx context.Context, dp *backflowv1.DriftProposal) bool {
	policy, err := r.policyOf(ctx, dp)
	if err != nil || !usesDefaultAccess(policy.Spec.ArgoCD.Server != nil, r.ArgoCD) {
		return false
	}
	if !r.ArgoCD.NamespaceAllowed(policy.Namespace) {
		return true
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	ns := dp.Spec.ApplicationRef.Namespace
	if ns == "" {
		ns = policy.Spec.ApplicationNamespace()
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: dp.Spec.ApplicationRef.Name}, app); err != nil {
		return false
	}
	return !r.ArgoCD.ProjectAllowed(policy.Namespace, applicationProject(app))
}

// policyProposals enqueues the open proposals of a policy that changed, so that a
// proposal of an Application the policy may no longer see is retired, and a held
// one goes on.
func (r *DriftProposalReconciler) policyProposals(ctx context.Context, obj client.Object) []reconcile.Request {
	policy, ok := obj.(*backflowv1.BackflowPolicy)
	if !ok {
		return nil
	}
	var list backflowv1.DriftProposalList
	if err := r.List(ctx, &list, client.InNamespace(policy.Namespace)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.PolicyRef.Name == policy.Name && isOpenPhase(p.Status.Phase) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
		}
	}
	return requests
}

// retireIfRevoked ends the proposal when its policy may not see its Application (see
// accessRevoked). done is true when it did, and the proposal needs nothing more.
func (r *DriftProposalReconciler) retireIfRevoked(ctx context.Context, dp, orig *backflowv1.DriftProposal) (done bool, res ctrl.Result, err error) {
	if !r.accessRevoked(ctx, dp) {
		return false, ctrl.Result{}, nil
	}
	setAccessRevoked(dp)
	if err := r.patchStatus(ctx, dp, orig); err != nil {
		return true, ctrl.Result{}, err
	}
	if r.Writer != nil && needsCleanup(dp) {
		res, err = r.cleanup(ctx, dp)
	}
	return true, res, err
}
