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

package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// BackflowMode decides what Backflow does with a detected drift.
// +kubebuilder:validation:Enum=MergeRequest;DirectCommit;ReportOnly
type BackflowMode string

const (
	// ModeMergeRequest opens a merge request against the source file.
	ModeMergeRequest BackflowMode = "MergeRequest"
	// ModeDirectCommit commits straight to the tracked branch.
	ModeDirectCommit BackflowMode = "DirectCommit"
	// ModeReportOnly only records a DriftProposal; nothing is written to Git.
	ModeReportOnly BackflowMode = "ReportOnly"
)

// BackflowPolicySpec selects Argo CD Applications whose drift is captured
// and decides how that drift flows back to Git.
type BackflowPolicySpec struct {
	// Namespace where Argo CD Applications live.
	// "argocd" for upstream Argo CD, "openshift-gitops" for OpenShift GitOps.
	// +kubebuilder:default=argocd
	ArgoCDNamespace string `json:"argoCDNamespace,omitempty"`

	// Applications this policy applies to.
	Applications ApplicationSelector `json:"applications"`

	// What to do with detected drift.
	// +kubebuilder:default=MergeRequest
	Mode BackflowMode `json:"mode,omitempty"`

	// Argo CD API connection used to read managed resources for drift detection.
	// Drift detection is skipped while this is unset.
	// +optional
	ArgoCD *ArgoCDServer `json:"argoCD,omitempty"`

	// Resource kinds to capture. Empty means every kind the Application manages.
	// +optional
	Include []KindSelector `json:"include,omitempty"`

	// Resource kinds never captured. Evaluated after Include.
	// Secrets are excluded unless explicitly included.
	// +optional
	Exclude []KindSelector `json:"exclude,omitempty"`

	// Fields to ignore when computing drift, in addition to built-in noise
	// (status, managedFields, resourceVersion, platform-injected fields).
	// +optional
	IgnoreFields []IgnoreRule `json:"ignoreFields,omitempty"`

	// Helm-specific behaviour.
	// +optional
	Helm *HelmOptions `json:"helm,omitempty"`

	// Kustomize-specific behaviour.
	// +optional
	Kustomize *KustomizeOptions `json:"kustomize,omitempty"`

	// How drifted fields are traced back to Git, for Directory, Helm and
	// Kustomize sources alike.
	// +optional
	Mapping *MappingOptions `json:"mapping,omitempty"`

	// Merge request settings. Used when Mode is MergeRequest.
	// +optional
	MergeRequest *MergeRequestOptions `json:"mergeRequest,omitempty"`

	// A drift becomes a proposal only after it has stayed the same, at the
	// same synced revision, for this long and across at least two
	// observations. A drift that disappears within the window is never
	// proposed, and several quick edits to the same resource give one
	// proposal for the final state. An explicit 0s waits for the second
	// observation only. Defaults to 30s.
	// +kubebuilder:default="30s"
	// +optional
	BatchWindow *metav1.Duration `json:"batchWindow,omitempty"`
}

// PartialAllowed reports whether a proposal may be mapped partially: the fields
// that can be traced back are proposed and the others left out
// (spec.mapping.allowPartial).
func (s BackflowPolicySpec) PartialAllowed() bool {
	return s.Mapping != nil && s.Mapping.AllowPartial
}

// OverridesAllowed reports whether a Helm edit may add a key to the
// Application's last value file to override a chart default
// (spec.helm.addOverrides, true unless set to false).
func (s BackflowPolicySpec) OverridesAllowed() bool {
	return s.Helm == nil || s.Helm.AddOverrides == nil || *s.Helm.AddOverrides
}

// KustomizeEditsBases reports whether a Kustomize edit may change files outside
// the Application's path, which other Applications may share
// (spec.kustomize.editBases, false unless set).
func (s BackflowPolicySpec) KustomizeEditsBases() bool {
	return s.Kustomize != nil && s.Kustomize.EditBases
}

// KustomizeAddsPatches reports whether a Kustomize edit may add a patch file
// to the Application's overlay as a last resort (spec.kustomize.addPatches,
// true unless set to false).
func (s BackflowPolicySpec) KustomizeAddsPatches() bool {
	return s.Kustomize == nil || s.Kustomize.AddPatches == nil || *s.Kustomize.AddPatches
}

// DefaultBatchWindow is the batch window of a policy that does not set one.
const DefaultBatchWindow = 30 * time.Second

// EffectiveBatchWindow returns the batch window to use: the configured one,
// including an explicit 0s, or DefaultBatchWindow when unset. The API server
// fills in the default for stored objects; this covers objects that did not
// go through it.
func (s BackflowPolicySpec) EffectiveBatchWindow() time.Duration {
	if s.BatchWindow == nil {
		return DefaultBatchWindow
	}
	return s.BatchWindow.Duration
}

// ArgoCDServer describes how to reach the Argo CD API.
type ArgoCDServer struct {
	// Base URL of the Argo CD API server. Override it when the operator runs
	// outside the cluster, e.g. https://localhost:8080 behind a port-forward.
	// +kubebuilder:default="https://argocd-server.argocd.svc"
	URL string `json:"url,omitempty"`

	// Secret key holding an Argo CD API token with read access to applications.
	TokenSecretRef corev1.SecretKeySelector `json:"tokenSecretRef"`

	// Skip verification of the Argo CD server certificate.
	// +kubebuilder:default=false
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`

	// Secret key holding a PEM CA bundle used to verify the Argo CD server.
	// +optional
	CASecretRef *corev1.SecretKeySelector `json:"caSecretRef,omitempty"`
}

// ApplicationSelector picks Argo CD Applications by name or label.
// At least one of Names or Selector must be set.
type ApplicationSelector struct {
	// +optional
	Names []string `json:"names,omitempty"`
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// KindSelector matches resources by API group and kind.
type KindSelector struct {
	// API group; empty for the core group, "*" for any group.
	// +optional
	Group string `json:"group,omitempty"`
	// Kind, or "*" for any kind.
	Kind string `json:"kind"`
}

// IgnoreRule removes fields from drift comparison.
type IgnoreRule struct {
	KindSelector `json:",inline"`
	// Restrict to a resource name. Empty matches every name.
	// +optional
	Name string `json:"name,omitempty"`
	// JSON pointers to ignore, e.g. /spec/replicas.
	// +kubebuilder:validation:MinItems=1
	JSONPointers []string `json:"jsonPointers"`
}

// KustomizeOptions configures how drift in Kustomize-rendered resources is
// mapped back to files. An edit is looked for, in this order, in the settings
// of the Application's own kustomization (images, replicas, labels, generator
// literals), in the patches it already has, in the resource files inside the
// Application's path, in files outside it (shared bases), and last in a new
// patch file; every edit is proven by rendering through Argo CD.
type KustomizeOptions struct {
	// Allow edits to files outside the Application's path: a base or a
	// component that other Applications may use too. A change there reaches
	// every Application that includes the file, so the merge request names them.
	// By default only files inside the Application's path are edited.
	// +kubebuilder:default=false
	EditBases bool `json:"editBases,omitempty"`

	// As a last resort, when no existing file can take the change, add a
	// strategic merge patch with only the drifted fields to the Application's
	// overlay (backflow-patches/<kind>-<name>.yaml) and list it under patches
	// in the overlay's kustomization.
	// +kubebuilder:default=true
	// +optional
	AddPatches *bool `json:"addPatches,omitempty"`
}

// MappingOptions configures how a proposal is mapped to files in Git.
type MappingOptions struct {
	// Propose the fields that can be mapped when others of the same resource
	// cannot. By default any field that cannot be traced back to Git makes the
	// whole proposal Unmapped, and the fields that could have been mapped are
	// lost with it. With this on, the proposal maps the verified fields only and
	// lists the others in status.mapping.unmapped (Mapped=True, reason
	// PartiallyMapped); the merge request says that it is partial and lists what
	// it leaves out. Those fields stay different from Git after the merge and are
	// reported as a new proposal. A proposal that maps no field is still
	// Unmapped.
	// +kubebuilder:default=false
	AllowPartial bool `json:"allowPartial,omitempty"`
}

// HelmMappingMode controls how rendered fields are traced back to values.
// +kubebuilder:validation:Enum=Auto;ExplicitOnly
type HelmMappingMode string

const (
	// HelmMappingAuto uses marker rendering plus explicit mappings.
	HelmMappingAuto HelmMappingMode = "Auto"
	// HelmMappingExplicitOnly only uses mappings declared in this policy.
	HelmMappingExplicitOnly HelmMappingMode = "ExplicitOnly"
)

// HelmOptions configures how drift in Helm-rendered resources is mapped
// back to values.
type HelmOptions struct {
	// +kubebuilder:default=Auto
	MappingMode HelmMappingMode `json:"mappingMode,omitempty"`

	// Which values file receives an override of a chart default (see
	// addOverrides) when the Application uses several. It must be one of the
	// Application's value files, as a repository path, relative to the chart
	// directory, or as the Application writes it; otherwise such a change stays
	// Unmapped and says so. Defaults to the last file in
	// spec.source.helm.valueFiles.
	// +optional
	TargetValuesFile string `json:"targetValuesFile,omitempty"`

	// Explicit field-to-value mappings. Take precedence over automatic mapping.
	// +optional
	Mappings []HelmFieldMapping `json:"mappings,omitempty"`

	// When no mapping is found, propose the change as an override in the
	// Application's spec.source.helm.valuesObject instead of leaving it unmapped.
	// +kubebuilder:default=false
	FallbackToApplicationOverride bool `json:"fallbackToApplicationOverride,omitempty"`

	// Changes beyond the drifted fields that an edit of a value file may also
	// make, for a Helm edit that is otherwise proven right by rendering. By
	// default none: an edit is accepted only when it changes exactly the
	// drifted fields. A chart often derives more than one field from one value
	// (the image tag also goes into a version label), and without this the
	// proposal stays unmapped, with the nearest edit in its message.
	//
	// Labels allows changes to metadata.labels and spec.template.metadata.labels
	// of any rendered resource, Annotations the same for annotations. Selectors
	// are never allowed (spec.selector, matchLabels, matchExpressions, a
	// Service's spec.selector), nor a change to a drifted field that does not
	// end at its live value. The changes that were accepted are listed in the
	// merge request.
	// +optional
	// +listType=set
	AllowCollateralChanges []HelmCollateralKind `json:"allowCollateralChanges,omitempty"`

	// When the value a drifted field had in Git comes only from the chart's
	// own defaults (the chart's values.yaml, or that of a dependency under its
	// name or alias, or global.*), propose adding that key, with the live
	// value, to the Application's last value file, which wins over the
	// defaults. Existing lines of the value files are edited first; an
	// override is only tried when none of those verifies, and it is verified
	// by rendering like any other edit. Keys under a list in the defaults are
	// not overridden. Set it to false to leave such proposals Unmapped.
	// Defaults to true.
	// +kubebuilder:default=true
	// +optional
	AddOverrides *bool `json:"addOverrides,omitempty"`
}

// HelmCollateralKind names a kind of change beyond the drifted fields.
// +kubebuilder:validation:Enum=Labels;Annotations
type HelmCollateralKind string

const (
	// CollateralLabels allows changes to labels.
	CollateralLabels HelmCollateralKind = "Labels"
	// CollateralAnnotations allows changes to annotations.
	CollateralAnnotations HelmCollateralKind = "Annotations"
)

// HelmFieldMapping ties a field of a rendered resource to a values key.
type HelmFieldMapping struct {
	KindSelector `json:",inline"`
	// +optional
	Name string `json:"name,omitempty"`
	// JSON pointer in the rendered resource, e.g. /spec/replicas.
	FieldPointer string `json:"fieldPointer"`
	// Dotted path in values, e.g. api.replicaCount.
	ValuesPath string `json:"valuesPath"`
}

// MergeRequestOptions customises the merge requests Backflow opens.
type MergeRequestOptions struct {
	// Prefix for branches Backflow creates.
	// +kubebuilder:default="backflow/"
	BranchPrefix string `json:"branchPrefix,omitempty"`

	// Target branch. Defaults to the Application's targetRevision when it is a branch.
	// +optional
	TargetBranch string `json:"targetBranch,omitempty"`

	// +optional
	Labels []string `json:"labels,omitempty"`

	// Usernames to assign as reviewers.
	// +optional
	Reviewers []string `json:"reviewers,omitempty"`

	// Assign the Kubernetes user who made the change, when they map to an SCM account.
	// +kubebuilder:default=true
	AssignActor bool `json:"assignActor,omitempty"`
}

// BackflowPolicyStatus summarises what the policy currently covers.
type BackflowPolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Applications currently matched.
	// +optional
	MatchedApplications []string `json:"matchedApplications,omitempty"`
	// Details of each matched Application.
	// +optional
	Applications []ApplicationSummary `json:"applications,omitempty"`
	// DriftProposals not yet merged, rejected or reverted.
	// +optional
	OpenProposals int32 `json:"openProposals,omitempty"`
}

// ApplicationSummary is what Backflow knows about a matched Argo CD Application.
type ApplicationSummary struct {
	Name string `json:"name"`
	// Git repository the Application deploys from.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`
	// Path inside the repository; for Helm repositories, the chart name.
	// +optional
	Path string `json:"path,omitempty"`
	// Branch or tag the Application tracks.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// Commit Argo CD last synced.
	// +optional
	SyncedRevision string `json:"syncedRevision,omitempty"`
	// How Argo CD renders the source: Directory, Kustomize, Helm or Plugin.
	// +optional
	SourceType string `json:"sourceType,omitempty"`
	// Number of resources the Application manages.
	ManagedResources int32 `json:"managedResources"`
	// ScmConnection used for this repository. Empty when none matches.
	// +optional
	ScmConnection string `json:"scmConnection,omitempty"`
	// Why Backflow does not track the Application, or empty when it does.
	// CredentialsInRepoURL: a repository URL of the Application contains
	// credentials; only the redacted URL is kept here.
	// +optional
	Reason string `json:"reason,omitempty"`
	// Sources lists every source of an Application that has several, in order.
	// The fields above keep describing the first source. Empty for an
	// Application with one source.
	// +optional
	// +listType=atomic
	Sources []SourceSummary `json:"sources,omitempty"`
}

// SourceSummary describes one source of a multi-source Application.
type SourceSummary struct {
	// Position of the source in spec.sources, starting at 1 like Argo CD's
	// sourcePositions.
	Position int32 `json:"position"`
	// Repository the source comes from: a Git repository, a Helm repository or
	// an OCI registry.
	RepoURL string `json:"repoURL"`
	// Path inside a Git repository.
	// +optional
	Path string `json:"path,omitempty"`
	// Chart name, for a source that comes from a Helm repository or a registry.
	// +optional
	Chart string `json:"chart,omitempty"`
	// Branch, tag, chart version or range the source tracks.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// Revision Argo CD resolved and compares against: a commit SHA, or the chart
	// version (status.sync.revisions at this position).
	// +optional
	Revision string `json:"revision,omitempty"`
	// Revision of the last sync (status.operationState.syncResult.revisions).
	// +optional
	SyncedRevision string `json:"syncedRevision,omitempty"`
	// How Argo CD renders the source: Directory, Kustomize, Helm or Plugin.
	// Empty for a ref source, which is only referenced.
	// +optional
	SourceType string `json:"sourceType,omitempty"`
	// Name of a ref source, referenced as $name/... in value files.
	// +optional
	Ref string `json:"ref,omitempty"`
	// ScmConnection used for this repository. Empty for a repository that is
	// not a Git repository, and when none matches.
	// +optional
	ScmConnection string `json:"scmConnection,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bfp
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Open",type=integer,JSONPath=`.status.openProposals`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackflowPolicy decides which Argo CD Applications are watched for drift
// and how that drift is proposed back to Git.
type BackflowPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackflowPolicySpec   `json:"spec,omitempty"`
	Status BackflowPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BackflowPolicyList contains a list of BackflowPolicy.
type BackflowPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackflowPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BackflowPolicy{}, &BackflowPolicyList{})
		return nil
	})
}
