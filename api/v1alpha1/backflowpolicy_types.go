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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
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

	// Merge request settings. Used when Mode is MergeRequest.
	// +optional
	MergeRequest *MergeRequestOptions `json:"mergeRequest,omitempty"`

	// A drift becomes a proposal only after it has stayed the same, at the
	// same synced revision, for this long and across at least two
	// observations. A drift that disappears within the window is never
	// proposed, and several quick edits to the same resource give one
	// proposal for the final state.
	// +kubebuilder:default="30s"
	BatchWindow metav1.Duration `json:"batchWindow,omitempty"`
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

	// Which values file receives the change when the Application uses several.
	// Defaults to the last file in spec.source.helm.valueFiles.
	// +optional
	TargetValuesFile string `json:"targetValuesFile,omitempty"`

	// Explicit field-to-value mappings. Take precedence over automatic mapping.
	// +optional
	Mappings []HelmFieldMapping `json:"mappings,omitempty"`

	// When no mapping is found, propose the change as an override in the
	// Application's spec.source.helm.valuesObject instead of leaving it unmapped.
	// +kubebuilder:default=false
	FallbackToApplicationOverride bool `json:"fallbackToApplicationOverride,omitempty"`
}

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
