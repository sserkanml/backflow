package main

import (
	"context"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sserkanml/backflow/internal/crdcheck"
)

// stringList is a flag that takes comma separated values and can be repeated.
type stringList struct{ values []string }

func (l *stringList) String() string { return strings.Join(l.values, ",") }

func (l *stringList) Set(v string) error {
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			l.values = append(l.values, item)
		}
	}
	return nil
}

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=scmconnections.backflow.io;backflowpolicies.backflow.io;driftproposals.backflow.io,verbs=get

// checkCRDs reads the Backflow CRDs with a client of its own (the manager is not built yet) and
// says whether this version of the operator may run against them.
func checkCRDs(cfg *rest.Config) error {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return crdcheck.Check(ctx, c)
}
