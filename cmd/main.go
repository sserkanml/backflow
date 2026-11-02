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

package main

import (
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	records "github.com/sserkanml/backflow/api/records/v1alpha1"
	backflowv1beta1 "github.com/sserkanml/backflow/api/v1beta1"
	"github.com/sserkanml/backflow/internal/controller"
	"github.com/sserkanml/backflow/internal/gitrepo"
	"github.com/sserkanml/backflow/internal/helmmap"
	"github.com/sserkanml/backflow/internal/metrics"
	"github.com/sserkanml/backflow/internal/version"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(backflowv1beta1.AddToScheme(scheme))
	utilruntime.Must(records.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var repoCacheDir string
	var mrPollInterval, renderBudget time.Duration
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var enableLeaderElection bool
	var scmRecheckInterval, secretCacheTTL time.Duration
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var argoCD controller.ArgoCDDefaults
	var trustedCAFile string
	var argoAllowedServers, scmAllowedHosts stringList
	var actor actorOptions
	var unpackFileDeps bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&repoCacheDir, "repo-cache-dir", "",
		"Directory where Git repositories are cached for mapping. Defaults to <os.TempDir()>/backflow-repos.")
	flag.DurationVar(&mrPollInterval, "merge-request-poll-interval", 2*time.Minute,
		"How often the state of an open merge request is polled.")
	flag.DurationVar(&renderBudget, "render-budget", 2*time.Minute,
		"The time all renders of one verification may take together, for each proposal of a Helm or Kustomize source. "+
			"Rendering goes through Argo CD, which can take seconds per render.")
	flag.DurationVar(&scmRecheckInterval, "scm-recheck-interval", controller.DefaultSCMRecheckInterval,
		"How often a working ScmConnection is verified again. Secrets are not watched, so this is how long a rotated "+
			"token or CA bundle can go unnoticed (a change of the ScmConnection itself is verified at once).")
	flag.DurationVar(&secretCacheTTL, "secret-cache-ttl", controller.DefaultSecretTTL,
		"How long a Secret that was read by name stays in memory. 0 reads it from the API server every time.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.StringVar(&argoCD.Server, "argocd-server", "",
		"The Argo CD API server that BackflowPolicies without spec.argoCD use, e.g. "+
			"https://openshift-gitops-server.openshift-gitops.svc. Set by the installer only: a policy cannot change it.")
	flag.StringVar(&argoCD.TokenSecretNamespace, "argocd-token-secret-namespace", "",
		"The namespace of the Secret that holds the Argo CD token for --argocd-server.")
	flag.StringVar(&argoCD.TokenSecretName, "argocd-token-secret-name", "",
		"The name of that Secret (OpenShift GitOps writes <user>-local-user).")
	flag.StringVar(&argoCD.TokenSecretKey, "argocd-token-secret-key", controller.DefaultArgoCDTokenKey,
		"The key of the token in that Secret.")
	flag.StringVar(&argoCD.CAFile, "argocd-ca-file", "",
		"A PEM CA bundle file that verifies --argocd-server. It is read again when it changes, no restart is needed.")
	flag.BoolVar(&argoCD.InsecureSkipTLSVerify, "argocd-insecure-skip-tls-verify", false,
		"Do not verify the certificate of --argocd-server. For development only (a port-forward to a kind cluster); "+
			"the Helm chart has no value for it.")
	flag.Var(&argoAllowedServers, "argocd-allowed-servers",
		"Argo CD servers a BackflowPolicy may name in spec.argoCD.server.url, besides --argocd-server (always allowed): "+
			"comma separated URLs such as https://argocd.example.com or https://*.argo.corp:8443. Empty: none. A policy "+
			"that names another server is not Ready (ServerNotAllowed) and no request is made. Repeatable.")
	flag.Var(&scmAllowedHosts, "scm-allowed-hosts",
		"Git hosts a ScmConnection may name in spec.url: comma separated host names such as gitlab.com or *.example.com. "+
			"Empty: none. A connection to another host is not Ready (HostNotAllowed) and no request is made. Repeatable.")
	flag.Var(&argoCD.Access, "argocd-default-access",
		"Which Applications the policies of which namespaces may match through --argocd-server: "+
			"<namespace>[,<namespace>...]:<project>[,<project>...] (\"*\" matches any), for example team-a:team-a. "+
			"Repeatable. Without it only the policies of --operator-namespace may use the default connection, for all projects.")
	flag.StringVar(&argoCD.OperatorNamespace, "operator-namespace", operatorNamespace(),
		"The namespace the operator runs in (defaults to the one of its service account). Its policies may use "+
			"--argocd-server for every project when --argocd-default-access is not set.")
	actor.bind()
	flag.BoolVar(&unpackFileDeps, "unpack-file-dependencies", true,
		"Upload the file:// dependencies of a Helm chart unpacked under charts/, so that Argo CD does not run "+
			"helm dependency build on every render. Only used for a chart whose render was shown to be identical "+
			"both ways; --unpack-file-dependencies=false switches it off.")
	flag.StringVar(&trustedCAFile, "trusted-ca-file", "",
		"A PEM CA bundle file of extra roots for the connections to GitLab and GitHub of ScmConnections that have "+
			"no caSecretRef (for example the trusted CA bundle OpenShift injects). Read again when it changes.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := argoCD.Validate(); err != nil {
		setupLog.Error(err, "Invalid Argo CD flags")
		os.Exit(1)
	}
	controller.SetArgoCDAllowedServers(argoAllowedServers.values)
	controller.SetScmAllowedHosts(scmAllowedHosts.values)
	setupLog.Info("Allowlists", "argoCDServers", argoAllowedServers.values, "scmHosts", scmAllowedHosts.values)

	// Objects stored in an older version would be read with the fields of this version only: do
	// not start until they have been migrated.
	restConfig := ctrl.GetConfigOrDie()
	if err := checkCRDs(restConfig); err != nil {
		setupLog.Error(err, "The CustomResourceDefinitions are not ready for this version of the operator")
		os.Exit(1)
	}
	if argoCD.Configured() {
		setupLog.Info("Policies without spec.argoCD use the default Argo CD connection", "server", argoCD.Server,
			"tokenSecret", argoCD.TokenSecretNamespace+"/"+argoCD.TokenSecretName, "caFile", argoCD.CAFile)
	}
	controller.UseTrustedCAFile(trustedCAFile)
	if err := actor.validate(argoCD.OperatorNamespace); err != nil {
		setupLog.Error(err, "Invalid actor webhook flags")
		os.Exit(1)
	}

	// Fail at startup, not after minutes of retries, when the repository
	// cache cannot be written (for example a read-only root file system
	// without a mounted volume).
	if err := gitrepo.EnsureWritable(repoCacheDir); err != nil {
		setupLog.Error(err, "Repository cache is unusable; mount a writable volume and set --repo-cache-dir")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production: mount a certificate and pass
	// --metrics-cert-path.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		Cache:                  actor.cacheOptions(argoCD.OperatorNamespace),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "7e904218.io",
		// Argo CD Applications are read as unstructured objects (the Argo CD Go
		// module is not imported). The client bypasses the cache for
		// unstructured objects unless told otherwise, so without this every
		// Get/List of an Application, once per application in the drift
		// controller and once per older policy in the policy controller, would
		// hit the API server although the controllers already watch them.
		//
		// Secrets are never cached: the operator may only get them by name.
		Client: controller.ClientOptions(),
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Secrets are read by name straight from the API server (no informer, and
	// the operator holds only "get" on them); every controller uses this client.
	visibility := &controller.ApplicationVisibility{}
	k8sClient := controller.NewSecretClient(mgr.GetClient(), mgr.GetAPIReader(), secretCacheTTL)
	if err := (&controller.ScmConnectionReconciler{
		Client:          k8sClient,
		Scheme:          mgr.GetScheme(),
		RecheckInterval: scmRecheckInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "scmconnection")
		os.Exit(1)
	}
	if err := (&controller.BackflowPolicyReconciler{
		Client:     k8sClient,
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorder("backflow-policy"),
		ArgoCD:     &argoCD,
		Visibility: visibility,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "backflowpolicy")
		os.Exit(1)
	}
	repoCache := gitrepo.NewCache(repoCacheDir)
	var fileDeps *helmmap.UnpackedDeps
	if unpackFileDeps {
		fileDeps = helmmap.NewUnpackedDeps()
	}
	if err := (&controller.DriftProposalReconciler{
		Client:                 k8sClient,
		Scheme:                 mgr.GetScheme(),
		Repos:                  controller.CacheReader{Cache: repoCache},
		Writer:                 repoCache,
		Recorder:               mgr.GetEventRecorder("backflow-driftproposal"),
		TrackInterval:          mrPollInterval,
		UsernameInMergeRequest: actor.usernameInMR,
		RenderBudget:           renderBudget,
		ArgoCD:                 &argoCD,
		Visibility:             visibility,
		FileDeps:               fileDeps,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "driftproposal")
		os.Exit(1)
	}
	var actors *controller.ActorRecords
	if actor.enabled {
		var err error
		if actors, err = actor.setup(mgr, k8sClient, argoCD.OperatorNamespace); err != nil {
			setupLog.Error(err, "Failed to set up the actor webhook")
			os.Exit(1)
		}
		setupLog.Info("The actor webhook is on", "path", actor.path, "namespace", argoCD.OperatorNamespace,
			"recordUsername", actor.recordUsername, "retention", actor.retention.String())
	}
	if err := (&controller.DriftReconciler{
		Client:   k8sClient,
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("backflow-drift"),
		Git:      repoCache,
		ArgoCD:   &argoCD,
		Actors:   actors,

		Visibility: visibility,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "drift")
		os.Exit(1)
	}
	sweeper := &controller.ScratchSweeper{Client: k8sClient, Git: repoCache, ArgoCD: &argoCD, Visibility: visibility}
	if err := mgr.Add(sweeper); err != nil {
		setupLog.Error(err, "Failed to add the scratch branch sweeper")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	metrics.SetBuildInfo(version.Get())
	if err := metrics.RegisterProposals(controller.ProposalCounts(mgr.GetCache())); err != nil {
		setupLog.Error(err, "Failed to register the proposals metric")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// operatorNamespace is the namespace of the pod: POD_NAMESPACE, else the one of the
// service account; empty when the operator runs outside a cluster.
func operatorNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
