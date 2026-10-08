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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// SourceType is how the drifted resource is produced from Git.
// +kubebuilder:validation:Enum=Directory;Kustomize;Helm
type SourceType string

const (
	SourceDirectory SourceType = "Directory"
	SourceKustomize SourceType = "Kustomize"
	SourceHelm      SourceType = "Helm"
)

// DriftProposalSpec is written by the operator when drift is detected.
// It is immutable after creation; a newer drift on the same resource
// creates a new proposal and supersedes this one.
type DriftProposalSpec struct {
	// Policy that captured this drift.
	PolicyName string `json:"policyName"`

	// Argo CD Application that owns the resource.
	Application ObjectRef `json:"application"`

	// The drifted resource.
	Resource ResourceRef `json:"resource"`

	// Where the resource comes from in Git.
	Source SourceRef `json:"source"`

	// Field-level changes between Git (desired) and the cluster (live).
	// +kubebuilder:validation:MinItems=1
	Changes []FieldChange `json:"changes"`

	// Who made the change in the cluster, when known.
	// +optional
	Actor *Actor `json:"actor,omitempty"`

	// When the drift was first observed.
	DetectedAt metav1.Time `json:"detectedAt"`
}

// ObjectRef points to a namespaced object.
type ObjectRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ResourceRef identifies a Kubernetes resource.
type ResourceRef struct {
	// +optional
	Group   string `json:"group,omitempty"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// SourceRef describes the Git source of the resource as known by Argo CD.
type SourceRef struct {
	RepoURL string `json:"repoURL"`
	// Revision Argo CD last synced (commit SHA).
	Revision string `json:"revision"`
	// Branch or tag the Application tracks.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// Path inside the repository; for Helm repos, the chart name.
	// +optional
	Path string     `json:"path,omitempty"`
	Type SourceType `json:"type"`
}

// ChangeOperation is the kind of change on a field.
// +kubebuilder:validation:Enum=Add;Replace;Remove
type ChangeOperation string

const (
	OpAdd     ChangeOperation = "Add"
	OpReplace ChangeOperation = "Replace"
	OpRemove  ChangeOperation = "Remove"
)

// FieldChange is a single field difference.
type FieldChange struct {
	// JSON pointer in the resource, e.g. /spec/replicas.
	Path string          `json:"path"`
	Op   ChangeOperation `json:"op"`
	// JSON-encoded value in Git. Empty for Add.
	// +optional
	Desired string `json:"desired,omitempty"`
	// JSON-encoded value in the cluster. Empty for Remove.
	// +optional
	Live string `json:"live,omitempty"`
}

// Actor is the Kubernetes identity that changed the resource.
type Actor struct {
	Username string `json:"username"`
	// +optional
	Groups []string `json:"groups,omitempty"`
	// How the change was made, e.g. kubectl, console, or a controller name.
	// +optional
	UserAgent string `json:"userAgent,omitempty"`
}

// ProposalPhase is the lifecycle stage of a proposal.
// +kubebuilder:validation:Enum=Detected;Mapping;Unmapped;Proposed;Merged;Rejected;Reverted;Superseded;Failed
type ProposalPhase string

const (
	PhaseDetected   ProposalPhase = "Detected"
	PhaseMapping    ProposalPhase = "Mapping"
	PhaseUnmapped   ProposalPhase = "Unmapped"
	PhaseProposed   ProposalPhase = "Proposed"
	PhaseMerged     ProposalPhase = "Merged"
	PhaseRejected   ProposalPhase = "Rejected"
	PhaseReverted   ProposalPhase = "Reverted"
	PhaseSuperseded ProposalPhase = "Superseded"
	PhaseFailed     ProposalPhase = "Failed"
)

// MappingStrategy is how the change was traced back to source.
// +kubebuilder:validation:Enum=Direct;KustomizePatch;HelmValues;ApplicationOverride;Unmapped
type MappingStrategy string

const (
	StrategyDirect              MappingStrategy = "Direct"
	StrategyKustomizePatch      MappingStrategy = "KustomizePatch"
	StrategyHelmValues          MappingStrategy = "HelmValues"
	StrategyApplicationOverride MappingStrategy = "ApplicationOverride"
	StrategyUnmapped            MappingStrategy = "Unmapped"
)

// Mapping records where each change lands in the source.
type Mapping struct {
	Strategy MappingStrategy `json:"strategy"`
	// +optional
	Edits []SourceEdit `json:"edits,omitempty"`
	// True when a re-render with the edits reproduced the live state exactly.
	// +optional
	Verified bool `json:"verified,omitempty"`
	// Unified diff of the edited files, truncated to 32 KiB.
	// +kubebuilder:validation:MaxLength=32768
	// +optional
	Diff string `json:"diff,omitempty"`
	// Why the change could not be mapped. Set only when strategy is Unmapped.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// SourceEdit is one edit in a Git file.
type SourceEdit struct {
	// File path relative to the repository root.
	File string `json:"file"`
	// For Helm values, the dotted values path; otherwise a JSON pointer in the file.
	Location string `json:"location"`
	// JSON-encoded new value.
	Value string `json:"value"`
	// Index into spec.changes this edit satisfies.
	ChangeIndex int32 `json:"changeIndex"`
}

// MergeRequestRef points to the merge request opened for this proposal.
type MergeRequestRef struct {
	URL string `json:"url"`
	// Provider-side number (GitLab iid, GitHub PR number).
	Number int64  `json:"number"`
	Branch string `json:"branch"`
	// Provider-reported state, e.g. opened, merged, closed.
	State string `json:"state"`
}

// DriftProposalStatus tracks the proposal through mapping and review.
type DriftProposalStatus struct {
	// +optional
	Phase ProposalPhase `json:"phase,omitempty"`
	// +optional
	Mapping *Mapping `json:"mapping,omitempty"`
	// +optional
	MergeRequest *MergeRequestRef `json:"mergeRequest,omitempty"`
	// Commit that contains the change, once merged or committed directly.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
	// Proposal that replaced this one.
	// +optional
	SupersededBy string `json:"supersededBy,omitempty"`
	// Human-readable explanation of the current phase.
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dp
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.spec.application.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.resource.kind`
// +kubebuilder:printcolumn:name="Resource",type=string,JSONPath=`.spec.resource.name`
// +kubebuilder:printcolumn:name="Actor",type=string,JSONPath=`.spec.actor.username`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.status.mapping.strategy`,priority=1
// +kubebuilder:printcolumn:name="MR",type=string,JSONPath=`.status.mergeRequest.url`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DriftProposal is a captured difference between Git and the cluster,
// and its journey back to Git.
type DriftProposal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   DriftProposalSpec   `json:"spec,omitempty"`
	Status DriftProposalStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DriftProposalList contains a list of DriftProposal
type DriftProposalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DriftProposal `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DriftProposal{}, &DriftProposalList{})
		return nil
	})
}
