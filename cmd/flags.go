package main

import (
	"context"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	"github.com/sserkanml/backflow/internal/crdcheck"
	"github.com/sserkanml/backflow/internal/metrics"
)

// stringList is a flag that takes comma separated values and can be repeated.
type stringList struct{ values []string }

func (l *stringList) String() string { return strings.Join(l.values, ",") }

func (l *stringList) Set(v string) error {
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			l.values = append(l.values, item)
		}
	}
	return nil
}

//nolint:lll // a kubebuilder marker is one line
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=scmconnections.backflow.io;backflowpolicies.backflow.io;driftproposals.backflow.io,verbs=get

// crdClient is a client of its own for the CRDs (the manager is not built yet when the first check runs).
func crdClient(cfg *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// checkCRDs reads the Backflow CRDs and says whether this version of the operator may run against them, and which
// of them still hold objects stored as v1beta1.
func checkCRDs(cfg *rest.Config) (crdcheck.Result, error) {
	c, err := crdClient(cfg)
	if err != nil {
		return crdcheck.Result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := crdcheck.Check(ctx, c)
	metrics.StorageMigrationPending(len(res.Pending))
	return res, err
}

// migrationWatch looks again, every interval, at whether `backflow migrate` has moved the objects to v1, so that
// the metric and the log follow it without a restart of the operator.
type migrationWatch struct {
	reader   client.Reader
	interval time.Duration
	log      logr.Logger
}

func (w *migrationWatch) Start(ctx context.Context) error {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	pending := true
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := crdcheck.Check(checkCtx, w.reader)
		cancel()
		if err != nil {
			w.log.Error(err, "Cannot check the CustomResourceDefinitions")
			continue
		}
		metrics.StorageMigrationPending(len(res.Pending))
		if pending && len(res.Pending) == 0 {
			w.log.Info("Every Backflow object is stored as v1: backflow migrate has run")
		}
		pending = len(res.Pending) > 0
	}
}

// NeedLeaderElection is false: every replica reports its own metric.
func (w *migrationWatch) NeedLeaderElection() bool { return false }
