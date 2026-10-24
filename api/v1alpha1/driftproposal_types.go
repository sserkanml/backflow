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

	// Where the resource comes from in Git. For an Application with several
	// sources this is its first source; see Sources for the others.
	Source SourceRef `json:"source"`

	// Every source of an Application that has several, in order, each at the
	// revision Argo CD compared against when the drift was observed. The edits of
	// the proposal are written to the one with role Values.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Sources []ApplicationSource `json:"sources,omitempty"`

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

// SourceRole is what a source of a multi-source Application is for.
// +kubebuilder:validation:Enum=Render;Values;Reference
type SourceRole string

const (
	// RoleRender is the source that renders: a Helm chart.
	RoleRender SourceRole = "Render"
	// RoleValues is the ref source whose files hold the values; Backflow writes
	// its edits there.
	RoleValues SourceRole = "Values"
	// RoleReference is any other ref source.
	RoleReference SourceRole = "Reference"
)

// ApplicationSource is one source of a multi-source Application.
type ApplicationSource struct {
	// Position in spec.sources of the Application, from 1.
	Position int32      `json:"position"`
	Role     SourceRole `json:"role"`
	RepoURL  string     `json:"repoURL"`
	// Revision Argo CD resolved and compared against: a commit SHA, or the chart
	// version of a chart from a Helm repository or a registry.
	Revision string `json:"revision"`
	// Branch, tag, chart version or range the source tracks.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// Path inside a Git repository.
	// +optional
	Path string `json:"path,omitempty"`
	// Chart name, for a chart from a Helm repository or a registry.
	// +optional
	Chart string `json:"chart,omitempty"`
	// Name of a ref source, referenced as $name/... in value files.
	// +optional
	Ref string `json:"ref,omitempty"`
	// How Argo CD renders the source. Empty for a ref source.
	// +optional
	Type SourceType `json:"type,omitempty"`
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
	// Repository the edits are written to, when the Application has several
	// sources: the repository of the source with role Values.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`
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
	// Changes the edits also make beyond the drifted fields, which the policy
	// allowed (spec.helm.allowCollateralChanges). Set only for HelmValues.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Collateral []CollateralChange `json:"collateral,omitempty"`
	// Fields of spec.changes the edits leave out, with the reason for each. Set
	// only for a partial mapping (spec.mapping.allowPartial of the policy): the
	// edits then cover the other changes, and these fields stay different from
	// Git after the merge request is merged.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Unmapped []UnmappedField `json:"unmapped,omitempty"`
}

// UnmappedField is a drifted field that a partial mapping leaves out.
type UnmappedField struct {
	// JSON pointer of the field in the resource.
	Path string `json:"path"`
	// Machine-readable reason, such as UnsupportedChange or NoVerifiedCandidate.
	Reason string `json:"reason"`
	// Why the field could not be traced back to Git, for people.
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message"`
}

// CollateralChange is a field the edit changes although it did not drift.
type CollateralChange struct {
	// +optional
	Group string `json:"group,omitempty"`
	Kind  string `json:"kind"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// JSON pointer in the rendered resource.
	Path string `json:"path"`
	// JSON-encoded value before the edit (the value of the unmodified render).
	Current string `json:"current"`
	// JSON-encoded value after the edit.
	New string `json:"new"`
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
	// Branch Backflow pushes (or is about to push) for the merge request. It is
	// recorded before the push, so a branch or merge request left behind by a
	// crash can still be found and cleaned up.
	// +optional
	Branch string `json:"branch,omitempty"`
	// Commit that contains the change, once merged or committed directly.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
	// Proposal that replaced this one.
	// +optional
	SupersededBy string `json:"supersededBy,omitempty"`
	// Human-readable explanation of the current phase.
	// +optional
	Message string `json:"message,omitempty"`
	// How many times the Git server declined the push of this proposal. A
	// decline during an outage of the Git host looks like one of a protected
	// branch, so a few declines are retried with a growing delay before the
	// proposal fails. Kept here so the count survives an operator restart.
	// +optional
	PushRejections int32 `json:"pushRejections,omitempty"`
	// When the push was last declined; the next attempt waits from here.
	// +optional
	LastPushRejectedAt *metav1.Time `json:"lastPushRejectedAt,omitempty"`
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
// +kubebuilder:printcolumn:name="Mapped",type=string,JSONPath=`.status.conditions[?(@.type=="Mapped")].reason`,priority=1
// +kubebuilder:printcolumn:name="Proposed",type=string,JSONPath=`.status.conditions[?(@.type=="Proposed")].reason`,priority=1
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
