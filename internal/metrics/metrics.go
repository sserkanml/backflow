// Package metrics holds the Prometheus metrics of Backflow. They are registered
// on controller-runtime's registry, so they are served by the metrics endpoint
// of the manager next to its own, and they all start with backflow_.
//
// Two rules hold for every metric here.
//
//   - The labels are of low cardinality: a policy, an application, a source type,
//     a reason, a phase, a provider, a host, an endpoint, a status class. Never the
//     name of a resource, a proposal, a file or a branch, a full repository URL,
//     a token or a Secret. The values that come from outside are bounded as well
//     (see bound), so a surprise cannot make the series grow without end.
//   - A metric never blocks and never fails: the functions below recover from a
//     panic of the client library and return nothing, and the callers ignore them.
//
// The package imports nothing of Backflow, so any package can use it.
package metrics

import (
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "backflow"

// The label names, used more than once.
const (
	lPolicy      = "policy"
	lApplication = "application"
	lProvider    = "provider"
	lResult      = "result"
	lName        = "name"
	lNamespace   = "namespace"
	lSourceType  = "source_type"
	lRenderer    = "renderer"
	lEndpoint    = "endpoint"
	lHost        = "host"
	lReason      = "reason"
	lAction      = "action"
	resultError  = "error"
)

// Registry is where the metrics are registered: controller-runtime's.
var Registry = ctrlmetrics.Registry

var (
	driftsDetected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "drifts_detected_total",
		Help: "Drifts that became a proposal, by policy, application and source type.",
	}, []string{lPolicy, lApplication, lSourceType})
	mappingResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "mapping_results_total",
		Help: "Outcomes of mapping a proposal to Git, by source type, strategy and reason (Mapped, or why the proposal is Unmapped); a mapping that is tried again later is not counted.",
	}, []string{lSourceType, "strategy", lReason})
	mergeRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "merge_requests_total",
		Help: "Merge requests by provider and action: opened, merged, closed, push_rejected.",
	}, []string{lProvider, lAction})

	verificationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "verification_duration_seconds",
		Help:    "Time to verify the edits of one proposal by rendering, by source type and result (mapped, unmapped, retry).",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300, 600},
	}, []string{lSourceType, lResult})
	renders = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "renders_total",
		Help: "Renders through Argo CD by renderer (upload: GetManifestsWithFiles, revisions: by commit) and result (ok, failed, error).",
	}, []string{lRenderer, lResult})
	renderDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "render_duration_seconds",
		Help:    "Time of one render through Argo CD, by renderer.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 40, 80},
	}, []string{lRenderer})
	scratchBranches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "scratch_branches_total",
		Help: "Scratch branches by action: pushed, deleted, swept (found left behind and deleted), push_failed.",
	}, []string{lAction})

	argocdRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "argocd_requests_total",
		Help: "Requests to the Argo CD API by endpoint and status class (2xx, 4xx, 5xx, error).",
	}, []string{lEndpoint, "code_class"})
	argocdDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "argocd_request_duration_seconds",
		Help:    "Duration of requests to the Argo CD API, by endpoint.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80},
	}, []string{lEndpoint})
	scmRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "scm_requests_total",
		Help: "Requests to the API of a Git host by provider, host and status class (2xx, 4xx, 5xx, error).",
	}, []string{lProvider, lHost, "code_class"})
	scmDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "scm_request_duration_seconds",
		Help:    "Duration of requests to the API of a Git host, by provider and host.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{lProvider, lHost})
	secretReads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "secret_reads_total",
		Help: "Reads of a Secret from the API server (not from the short memory of the operator) by result: ok, forbidden, notfound, error.",
	}, []string{lResult})

	scmConnectionReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "scm_connection_ready",
		Help: "1 when an ScmConnection is Ready (its token is accepted), else 0.",
	}, []string{lNamespace, lName})
	policyReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "policy_ready",
		Help: "1 when a BackflowPolicy is Ready, else 0.",
	}, []string{lNamespace, lName})
	policyApplications = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "policy_applications",
		Help: "Argo CD Applications a BackflowPolicy selects.",
	}, []string{lNamespace, lName})
	detectionPaused = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "detection_paused",
		Help: "1 while drift detection for an Application is paused, by the reason (GitAhead, ApplicationChanged, Settling).",
	}, []string{lPolicy, lApplication, lReason})
	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info",
		Help: "Always 1; the version and the commit of the operator.",
	}, []string{"version", "commit"})
)

func init() {
	Registry.MustRegister(driftsDetected, mappingResults, mergeRequests, verificationDuration, renders, renderDuration,
		scratchBranches, argocdRequests, argocdDuration, scmRequests, scmDuration, secretReads,
		scmConnectionReady, policyReady, policyApplications, detectionPaused, buildInfo)
}

// maxValuesPerLabel bounds the distinct values one label name takes: more, and
// the value is recorded as "other". The real sets (applications, policies,
// hosts, reasons) are far smaller; this is a guard against a surprise.
const maxValuesPerLabel = 500

// maxLabelLength cuts a value that is longer.
const maxLabelLength = 63

var (
	boundMu sync.Mutex
	bounded = map[string]map[string]struct{}{}
)

// bound returns a safe value for the label: never empty, cut to a short length,
// and "other" once the label has taken maxValuesPerLabel distinct values.
func bound(label, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "none"
	}
	if len(value) > maxLabelLength {
		value = value[:maxLabelLength]
	}
	boundMu.Lock()
	defer boundMu.Unlock()
	seen := bounded[label]
	if seen == nil {
		seen = map[string]struct{}{}
		bounded[label] = seen
	}
	if _, ok := seen[value]; ok {
		return value
	}
	if len(seen) >= maxValuesPerLabel {
		return "other"
	}
	seen[value] = struct{}{}
	return value
}

// safe runs f and swallows a panic: a metric must never fail the code it measures.
func safe(f func()) {
	defer func() { _ = recover() }()
	f()
}

// PolicyLabel is the value of the policy label: namespace/name.
func PolicyLabel(namespace, name string) string { return namespace + "/" + name }

// CodeClass is the status class of a response: 2xx, 3xx, 4xx, 5xx, or error when
// there is no response (err is set, or the status is 0).
func CodeClass(status int, err error) string {
	switch {
	case err != nil && status == 0, status <= 0:
		return resultError
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	}
	return "5xx"
}

// The reasons of SetDetectionPaused.
const (
	PausedGitAhead           = "GitAhead"
	PausedApplicationChanged = "ApplicationChanged"
	PausedSettling           = "Settling"
)

// appsSeen are the Applications of each policy that have a series by Application,
// for ForgetApplicationsExcept.
var (
	appsMu   sync.Mutex
	appsSeen = map[string]map[string]struct{}{}
)

func seeApplication(policy, application string) {
	appsMu.Lock()
	defer appsMu.Unlock()
	if appsSeen[policy] == nil {
		appsSeen[policy] = map[string]struct{}{}
	}
	appsSeen[policy][application] = struct{}{}
}

// ForgetApplicationsExcept removes the series of the Applications of a policy
// that are not in keep.
func ForgetApplicationsExcept(policy string, keep map[string]bool) {
	appsMu.Lock()
	var gone []string
	for app := range appsSeen[policy] {
		if !keep[app] {
			gone = append(gone, app)
			delete(appsSeen[policy], app)
		}
	}
	appsMu.Unlock()
	for _, app := range gone {
		ForgetApplication(policy, app)
	}
}

// DriftDetected counts a drift that became a proposal.
func DriftDetected(policy, application, sourceType string) {
	seeApplication(policy, application)
	safe(func() {
		driftsDetected.WithLabelValues(bound(lPolicy, policy), bound(lApplication, application), bound(lSourceType, sourceType)).Inc()
	})
}

// MappingResult counts the outcome of mapping a proposal.
func MappingResult(sourceType, strategy, reason string) {
	safe(func() {
		mappingResults.WithLabelValues(bound(lSourceType, sourceType), bound("strategy", strategy), bound(lReason, reason)).Inc()
	})
}

// The actions of MergeRequest.
const (
	MROpened       = "opened"
	MRMerged       = "merged"
	MRClosed       = "closed"
	MRPushRejected = "push_rejected"
)

// MergeRequest counts a merge request action.
func MergeRequest(provider, action string) {
	safe(func() { mergeRequests.WithLabelValues(bound(lProvider, provider), bound(lAction, action)).Inc() })
}

// Verification records the time to verify the edits of a proposal.
func Verification(sourceType, result string, d time.Duration) {
	safe(func() {
		verificationDuration.WithLabelValues(bound(lSourceType, sourceType), bound(lResult, result)).Observe(d.Seconds())
	})
}

// The renderers of Render.
const (
	RendererUpload    = "upload"
	RendererRevisions = "revisions"
)

// Render records one render through Argo CD: err is nil for a render that worked,
// failed says the source did not render (a verdict about the source, not about
// Argo CD), anything else is an error.
func Render(renderer string, err error, failed bool, d time.Duration) {
	safe(func() {
		result := "ok"
		switch {
		case err != nil && failed:
			result = "failed"
		case err != nil:
			result = resultError
		}
		renders.WithLabelValues(bound(lRenderer, renderer), result).Inc()
		renderDuration.WithLabelValues(bound(lRenderer, renderer)).Observe(d.Seconds())
	})
}

// The actions of ScratchBranches.
const (
	ScratchPushed     = "pushed"
	ScratchDeleted    = "deleted"
	ScratchSwept      = "swept"
	ScratchPushFailed = "push_failed"
)

// ScratchBranches counts scratch branches by action; n is how many.
func ScratchBranches(action string, n int) {
	if n <= 0 {
		return
	}
	safe(func() { scratchBranches.WithLabelValues(bound(lAction, action)).Add(float64(n)) })
}

// ArgoCDRequest records a request to the Argo CD API. status is the HTTP status,
// or 0 when there was none.
func ArgoCDRequest(endpoint string, status int, err error, d time.Duration) {
	safe(func() {
		ep := bound(lEndpoint, endpoint)
		argocdRequests.WithLabelValues(ep, CodeClass(status, err)).Inc()
		argocdDuration.WithLabelValues(ep).Observe(d.Seconds())
	})
}

// SCMRequest records a request to the API of a Git host.
func SCMRequest(provider, host string, status int, err error, d time.Duration) {
	safe(func() {
		p, h := bound(lProvider, provider), bound(lHost, host)
		scmRequests.WithLabelValues(p, h, CodeClass(status, err)).Inc()
		scmDuration.WithLabelValues(p, h).Observe(d.Seconds())
	})
}

// SecretRead counts a read of a Secret from the API server by its result (see
// SecretResult).
func SecretRead(result string) {
	safe(func() { secretReads.WithLabelValues(result).Inc() })
}

// SecretResult is the result label for the error of a read of a Secret, given
// whether it was forbidden or not found.
func SecretResult(err error, forbidden, notFound bool) string {
	switch {
	case err == nil:
		return "ok"
	case forbidden:
		return "forbidden"
	case notFound:
		return "notfound"
	}
	return resultError
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// SetSCMConnection records whether an ScmConnection is Ready.
func SetSCMConnection(namespace, name string, ready bool) {
	safe(func() {
		scmConnectionReady.WithLabelValues(bound(lNamespace, namespace), bound(lName, name)).Set(b2f(ready))
	})
}

// ForgetSCMConnection removes the series of a deleted ScmConnection.
func ForgetSCMConnection(namespace, name string) {
	safe(func() { scmConnectionReady.DeleteLabelValues(namespace, name) })
}

// SetPolicy records whether a BackflowPolicy is Ready and how many Applications
// it selects.
func SetPolicy(namespace, name string, ready bool, applications int) {
	safe(func() {
		ns, n := bound(lNamespace, namespace), bound(lName, name)
		policyReady.WithLabelValues(ns, n).Set(b2f(ready))
		policyApplications.WithLabelValues(ns, n).Set(float64(applications))
	})
}

// ForgetPolicy removes every series of a deleted BackflowPolicy.
func ForgetPolicy(namespace, name string) {
	safe(func() {
		policyReady.DeleteLabelValues(namespace, name)
		policyApplications.DeleteLabelValues(namespace, name)
		policy := PolicyLabel(namespace, name)
		detectionPaused.DeletePartialMatch(prometheus.Labels{lPolicy: policy})
		driftsDetected.DeletePartialMatch(prometheus.Labels{lPolicy: policy})
		appsMu.Lock()
		delete(appsSeen, policy)
		appsMu.Unlock()
	})
}

// SetDetectionPaused records why drift detection for an Application is paused
// ("" when it is not): only the current reason has a series, set to 1.
func SetDetectionPaused(policy, application, reason string) {
	if reason != "" {
		seeApplication(policy, application)
	}
	safe(func() {
		p, a := bound(lPolicy, policy), bound(lApplication, application)
		detectionPaused.DeletePartialMatch(prometheus.Labels{lPolicy: p, lApplication: a})
		if reason != "" {
			detectionPaused.WithLabelValues(p, a, bound(lReason, reason)).Set(1)
		}
	})
}

// ForgetApplication removes the series of an Application a policy no longer has.
func ForgetApplication(policy, application string) {
	appsMu.Lock()
	delete(appsSeen[policy], application)
	appsMu.Unlock()
	safe(func() {
		detectionPaused.DeletePartialMatch(prometheus.Labels{lPolicy: policy, lApplication: application})
		driftsDetected.DeletePartialMatch(prometheus.Labels{lPolicy: policy, lApplication: application})
	})
}

// SetBuildInfo records the version and the commit of the operator.
func SetBuildInfo(version, commit string) {
	safe(func() {
		buildInfo.Reset()
		buildInfo.WithLabelValues(bound("version", version), bound("commit", commit)).Set(1)
	})
}
