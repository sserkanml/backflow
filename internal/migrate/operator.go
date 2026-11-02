package migrate

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LeaderElectionID is the name of the Lease the operator holds (cmd/main.go).
const LeaderElectionID = "7e904218.io"

// operatorLabel selects the Deployment of the chart.
var operatorLabel = client.MatchingLabels{"app.kubernetes.io/name": "backflow"}

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
	var evidence []string

	var deployments appsv1.DeploymentList
	opts := []client.ListOption{operatorLabel}
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
