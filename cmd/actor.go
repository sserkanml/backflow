package main

import (
	"errors"
	"flag"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/actorwebhook"
	"github.com/sserkanml/backflow/internal/controller"
)

// actorOptions are the flags of the actor webhook (chart values actorWebhook.*).
type actorOptions struct {
	enabled        bool
	path           string
	retention      time.Duration
	flushInterval  time.Duration
	maxRecords     int
	tolerance      time.Duration
	recordUsername bool
	usernameInMR   bool
}

func (o *actorOptions) bind() {
	flag.BoolVar(&o.enabled, "actor-webhook", false,
		"Serve the observe-only admission webhook that tells who changed a resource, and use its records to name "+
			"the user of a proposal. The webhook always answers allowed. Needs --operator-namespace and the webhook "+
			"certificate flags.")
	flag.StringVar(&o.path, "actor-webhook-path", "/observe",
		"The URL path of the actor webhook. The chart makes it unguessable and puts it in the webhook configuration.")
	flag.DurationVar(&o.retention, "actor-retention", actorwebhook.DefaultRetention, "How long a ChangeRecord is kept.")
	flag.DurationVar(&o.flushInterval, "actor-flush-interval", actorwebhook.DefaultFlushInterval,
		"How often the records of a pod are written to ChangeRecords.")
	flag.IntVar(&o.maxRecords, "actor-max-records", actorwebhook.DefaultMaxRecords,
		"The most ChangeRecords (and records buffered in a pod). Beyond it the oldest are dropped.")
	flag.DurationVar(&o.tolerance, "actor-tolerance", actorwebhook.DefaultTolerance,
		"How far the time of a record may be from the managedFields entry it is checked against.")
	flag.BoolVar(&o.recordUsername, "actor-record-username", true,
		"Store user names and groups in the records. false stores only whether a person or a controller "+
			"(a service account) made the change.")
	flag.BoolVar(&o.usernameInMR, "username-in-merge-request", false,
		"Put the user name of the actor into the text of merge requests and commits. Git keeps it for ever.")
}

func (o *actorOptions) validate(operatorNamespace string) error {
	switch {
	case !o.enabled:
		return nil
	case operatorNamespace == "":
		return errors.New("--actor-webhook needs --operator-namespace: the ChangeRecords live there")
	case !strings.HasPrefix(o.path, "/") || len(o.path) < 2:
		return errors.New("--actor-webhook-path must be a path starting with /")
	}
	return nil
}

// cacheOptions restrict the informer of ChangeRecords to the operator's namespace, so that a
// Role in that namespace is all the operator needs.
func (o *actorOptions) cacheOptions(operatorNamespace string) cache.Options {
	if !o.enabled {
		return cache.Options{}
	}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&backflowv1alpha1.ChangeRecord{}: {Namespaces: map[string]cache.Config{operatorNamespace: {}}},
	}}
}

// setup registers the handler on the webhook server and adds the flusher and the sweeper.
// The webhook runs in every replica; only the leader sweeps.
func (o *actorOptions) setup(mgr manager.Manager, c client.Client, namespace string) (*controller.ActorRecords, error) {
	buffer := actorwebhook.NewBuffer(o.maxRecords)
	handler := actorwebhook.NewHandler(actorwebhook.Options{Buffer: buffer, RecordUsername: o.recordUsername})
	mgr.GetWebhookServer().Register(o.path, handler)
	if err := mgr.Add(&actorwebhook.Flusher{
		Client: c, Reader: mgr.GetAPIReader(), Buffer: buffer, Namespace: namespace, Interval: o.flushInterval,
	}); err != nil {
		return nil, err
	}
	sweeper := &actorwebhook.Sweeper{Client: c, Namespace: namespace, Retention: o.retention, Max: o.maxRecords}
	if err := mgr.Add(sweeper); err != nil {
		return nil, err
	}
	return &controller.ActorRecords{Reader: c, Namespace: namespace, Tolerance: o.tolerance}, nil
}
