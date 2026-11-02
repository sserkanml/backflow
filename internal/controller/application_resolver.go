package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
)

// appResolver decides which Argo CD Applications a policy may see. The policy controller shows
// the result in its status (capped), and the drift controller works from the full result, never
// from that status: a policy that matches more Applications than the status lists still watches
// every one of them.
type appResolver struct {
	Client     client.Reader
	ArgoCD     *ArgoCDDefaults
	Visibility *ApplicationVisibility
}

// resolveApplications is the one place that decides which Applications a policy
// may see. Everything else (drift detection, mapping, rendering, status, metrics)
// works from its result. A policy without spec.argoCD sees the Applications of
// the Argo CD projects the administrator allowed for its namespace
// (--argocd-default-access); a policy with its own spec.argoCD sees the ones its
// own token can get from Argo CD.
func (r appResolver) resolve(ctx context.Context, policy *backflowv1beta1.BackflowPolicy) (resolution, error) {
	if srv := policy.Spec.ArgoCD.Server; srv != nil {
		// Before anything is asked of the server: one the installer did not allow is never contacted.
		if err := r.ArgoCD.checkServer(srv.URL); err != nil {
			return resolution{}, err
		}
	}
	if usesDefaultAccess(policy.Spec.ArgoCD.Server != nil, r.ArgoCD) && !r.ArgoCD.NamespaceAllowed(policy.Namespace) {
		return resolution{}, &checkError{reason: reasonArgoCDDefaultNotAllowed, msg: defaultNotAllowedMessage(policy.Namespace)}
	}
	apps, err := r.selectApplications(ctx, policy)
	if err != nil {
		return resolution{}, err
	}
	res := resolution{}
	switch {
	case usesDefaultAccess(policy.Spec.ArgoCD.Server != nil, r.ArgoCD):
		res.excludedReason = excludedProjectNotAllowed
		for i := range apps {
			if r.ArgoCD.ProjectAllowed(policy.Namespace, applicationProject(&apps[i])) {
				res.apps = append(res.apps, apps[i])
			} else {
				res.excluded = append(res.excluded, apps[i].GetName())
			}
		}
	case r.Visibility != nil:
		res.excludedReason = excludedNotVisibleToToken
		for i := range apps {
			v, err := tokenVisible(ctx, r.Visibility, r.Client, policy, r.ArgoCD, apps[i].GetName(), apps[i].GetNamespace())
			if err != nil {
				return resolution{}, visibilityError(err)
			}
			switch v {
			case VisibilityVisible:
				res.apps = append(res.apps, apps[i])
			case VisibilityUnknown:
				res.unknown++
			default:
				res.excluded = append(res.excluded, apps[i].GetName())
			}
		}
		if res.unknown > 0 {
			res.excludedReason = excludedVisibilityUnknown
		}
	default:
		res.apps = apps
	}
	return res, nil
}

// visibilityError says why the token of a policy could not be used: a Secret it
// cannot read or that is missing is the policy's to fix and goes to status.
func visibilityError(err error) error {
	if isSecretForbidden(err) {
		return err
	}
	return &checkError{reason: "ArgoCDCredentials", msg: err.Error()}
}

// selectApplications lists Applications in the Argo CD namespace that match
// the policy, whether it may see them or not. When both names and selector are
// set, an Application must match both. Spec problems are returned as *checkError.
func (r appResolver) selectApplications(ctx context.Context, policy *backflowv1beta1.BackflowPolicy) ([]unstructured.Unstructured, error) {
	sel := policy.Spec.Applications
	if len(sel.Names) == 0 && sel.Selector == nil {
		return nil, &checkError{reason: "InvalidSpec", msg: "spec.applications needs at least one of names or selector"}
	}

	opts := []client.ListOption{client.InNamespace(policy.Spec.ApplicationNamespace())}
	if sel.Selector != nil {
		ls, err := metav1.LabelSelectorAsSelector(sel.Selector)
		if err != nil {
			return nil, &checkError{reason: "InvalidSpec", msg: fmt.Sprintf("spec.applications.selector: %v", err)}
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: ls})
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(applicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := r.Client.List(ctx, list, opts...); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, &checkError{reason: "ArgoCDNotFound", msg: "the Argo CD Application CRD is not installed in this cluster"}
		}
		return nil, err
	}

	wanted := make(map[string]bool, len(sel.Names))
	for _, n := range sel.Names {
		wanted[n] = true
	}

	var apps []unstructured.Unstructured
	for _, app := range list.Items {
		if len(wanted) > 0 && !wanted[app.GetName()] {
			continue
		}
		apps = append(apps, app)
	}
	slices.SortFunc(apps, func(a, b unstructured.Unstructured) int { return cmp.Compare(a.GetName(), b.GetName()) })
	return apps, nil
}
