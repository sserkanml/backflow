package migrate

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LeaderElectionID is the name of the Lease the operator holds (cmd/main.go).
const LeaderElectionID = "7e904218.io"

// operatorLabel selects the Deployment of the chart. A chart with nameOverride labels it differently: --operator-selector.
var operatorLabel = client.MatchingLabels{"app.kubernetes.io/name": "backflow"}

// operatorSelector is the list option that finds the operator's Deployment: the selector the user gave, or the default.
func operatorSelector(selector string) (client.ListOption, error) {
	if selector == "" {
		return operatorLabel, nil
	}
	sel, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("--operator-selector %q: %w", selector, err)
	}
	return client.MatchingLabelsSelector{Selector: sel}, nil
}

// RunningOperatorError says that an operator is running, and what shows it.
type RunningOperatorError struct{ Evidence []string }

func (e *RunningOperatorError) Error() string {
	return "the operator is running, and a migration must not race it: " + strings.Join(e.Evidence, "; ") +
		". Stop it first (kubectl -n <namespace> scale deployment <name> --replicas=0), wait until its pods are gone and run this again"
}

// CheckOperatorStopped returns a *RunningOperatorError while the operator of the chart is running: a Deployment with
// replicas, or pods left from one, or a leader election Lease that is held. namespace limits the search to one namespace;
// empty looks in all. An operator that runs outside the cluster (make run) and does not use leader election cannot be
// seen, and has to be stopped by whoever started it.
func CheckOperatorStopped(ctx context.Context, c client.Reader, namespace string, now time.Time) error {
	return checkOperatorStopped(ctx, c, namespace, "", now)
}

func checkOperatorStopped(ctx context.Context, c client.Reader, namespace, selector string, now time.Time) error {
	var evidence []string

	var deployments appsv1.DeploymentList
	sel, err := operatorSelector(selector)
	if err != nil {
		return err
	}
	opts := []client.ListOption{sel}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := c.List(ctx, &deployments, opts...); err != nil {
		return fmt.Errorf("cannot look for the operator's Deployment: %w", err)
	}
	for _, d := range deployments.Items {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		switch {
		case desired > 0:
			evidence = append(evidence, fmt.Sprintf("Deployment %s/%s has %d replica(s)", d.Namespace, d.Name, desired))
		case d.Status.Replicas > 0:
			evidence = append(evidence, fmt.Sprintf("Deployment %s/%s is scaled to 0 but %d pod(s) are still running", d.Namespace, d.Name, d.Status.Replicas))
		}
	}

	var leases coordinationv1.LeaseList
	leaseOpts := []client.ListOption{}
	if namespace != "" {
		leaseOpts = append(leaseOpts, client.InNamespace(namespace))
	}
	if err := c.List(ctx, &leases, leaseOpts...); err != nil {
		return fmt.Errorf("cannot look for the leader election Lease: %w", err)
	}
	for _, l := range leases.Items {
		if l.Name != LeaderElectionID || !leaseHeld(&l, now) {
			continue
		}
		evidence = append(evidence, fmt.Sprintf("the Lease %s/%s is held by %s", l.Namespace, l.Name, *l.Spec.HolderIdentity))
	}

	if len(evidence) > 0 {
		return &RunningOperatorError{Evidence: evidence}
	}
	return nil
}

// leaseHeld reports whether a Lease has a holder whose time to renew it has not run out.
func leaseHeld(l *coordinationv1.Lease, now time.Time) bool {
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" {
		return false // released
	}
	if l.Spec.RenewTime == nil {
		return true // a holder that never renewed: do not guess
	}
	duration := 15 * time.Second
	if l.Spec.LeaseDurationSeconds != nil {
		duration = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}
	return now.Before(l.Spec.RenewTime.Add(duration))
}

// annotationReplicas remembers how many replicas the operator had before the tool scaled it down.
const annotationReplicas = "migrate.backflow.io/replicas-before"

// stopOperator scales the Deployments of the operator to zero, remembering the replicas in an annotation, and waits until
// their pods and the leader election Lease are gone. A Deployment that is not scaled up is left as it is.
func (r *runner) stopOperator(ctx context.Context) error {
	var deployments appsv1.DeploymentList
	sel, err := operatorSelector(r.opt.OperatorSelector)
	if err != nil {
		return err
	}
	opts := []client.ListOption{sel}
	if r.opt.OperatorNamespace != "" {
		opts = append(opts, client.InNamespace(r.opt.OperatorNamespace))
	}
	if err := r.c.List(ctx, &deployments, opts...); err != nil {
		return fmt.Errorf("cannot look for the operator's Deployment: %w", err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
			continue
		}
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[annotationReplicas] = fmt.Sprint(replicas)
		zero := int32(0)
		d.Spec.Replicas = &zero
		if err := r.c.Update(ctx, d); err != nil {
			return fmt.Errorf("cannot scale %s/%s to 0: %w", d.Namespace, d.Name, err)
		}
		r.printf("operator: Deployment %s/%s scaled from %d to 0 (--restore-operator scales it back)", d.Namespace, d.Name, replicas)
	}
	// Wait for the pods and the Lease: the Lease is held until it expires.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := checkOperatorStopped(ctx, r.c, r.opt.OperatorNamespace, r.opt.OperatorSelector, r.opt.Now())
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// restoreOperator scales back the Deployments that stopOperator scaled down, and takes the annotation away.
func (r *runner) restoreOperator(ctx context.Context) error {
	var deployments appsv1.DeploymentList
	sel, err := operatorSelector(r.opt.OperatorSelector)
	if err != nil {
		return err
	}
	opts := []client.ListOption{sel}
	if r.opt.OperatorNamespace != "" {
		opts = append(opts, client.InNamespace(r.opt.OperatorNamespace))
	}
	if err := r.c.List(ctx, &deployments, opts...); err != nil {
		return fmt.Errorf("cannot look for the operator's Deployment: %w", err)
	}
	restored := 0
	for i := range deployments.Items {
		d := &deployments.Items[i]
		before, ok := d.Annotations[annotationReplicas]
		if !ok {
			continue
		}
		var replicas int32
		if _, err := fmt.Sscan(before, &replicas); err != nil || replicas < 1 {
			replicas = 1
		}
		if d.Spec.Replicas != nil && *d.Spec.Replicas > 0 {
			replicas = *d.Spec.Replicas // Helm or a person already scaled it up
		}
		d.Spec.Replicas = &replicas
		delete(d.Annotations, annotationReplicas)
		if err := r.c.Update(ctx, d); err != nil {
			return fmt.Errorf("cannot scale %s/%s back to %d: %w", d.Namespace, d.Name, replicas, err)
		}
		r.printf("operator: Deployment %s/%s scaled back to %d", d.Namespace, d.Name, replicas)
		restored++
	}
	if restored == 0 {
		r.printf("operator: no Deployment was scaled down by this tool, nothing to restore")
	}
	return nil
}
