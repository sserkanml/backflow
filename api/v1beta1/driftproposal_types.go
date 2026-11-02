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
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// MaxChanges is how many field changes a DriftProposal holds. A drift with more is created
// Unmapped (reason TooManyChanges) with the first MaxChanges and spec.truncated set.
const MaxChanges = 256

// Condition types of a DriftProposal.
const (
	ConditionMapped       = "Mapped"
	ConditionProposed     = "Proposed"
	ConditionLiveReverted = "LiveReverted"
	ConditionCleanedUp    = "CleanedUp"
)

// ReasonTooManyChanges is the reason of Mapped=False for a proposal whose drift has more than
// MaxChanges field changes.
const ReasonTooManyChanges = "TooManyChanges"

// DriftProposalSpec is written by the operator when drift is detected.
// It is immutable after creation; a newer drift on the same resource
// creates a new proposal and supersedes this one.
type DriftProposalSpec struct {
	// Policy, in this namespace, that captured this drift.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Policy"
	PolicyRef LocalObjectRef `json:"policyRef"`

	// Argo CD Application that owns the resource.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Application"
	ApplicationRef ObjectRef `json:"applicationRef"`

	// The drifted resource.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Resource"
	ResourceRef ResourceRef `json:"resourceRef"`

	// Every source of the Application, in order, each at the revision Argo CD compared against
	// when the drift was observed. The edits of the proposal are written to the one with role
	// Values, or, when there is none, to the one with role Render.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Sources []ProposalSource `json:"sources"`

	// Field-level changes between Git (desired) and the cluster (live), at most 256.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Changes"
	Changes []FieldChange `json:"changes"`

	// True when the drift had more than 256 changes and only the first 256 are listed. Such a
	// proposal is created Unmapped (reason TooManyChanges): nothing is proposed to Git for it.
	// +optional
	Truncated bool `json:"truncated,omitempty"`

	// Who made the change in the cluster, when known: from the actor webhook.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Actor"
	Actor *Actor `json:"actor,omitempty"`

	// The managedFields writers of the drifted fields, most recent first. The first is the
	// latest writer. Argo CD's own managers are never listed.
	// +kubebuilder:validation:MaxItems=5
	// +listType=atomic
	// +optional
	Managers []ManagerWrite `json:"managers,omitempty"`

	// When the drift was first observed.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Detected at"
	DetectedAt metav1.Time `json:"detectedAt"`
}

// ResourceRef identifies a Kubernetes resource.
type ResourceRef struct {
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Group string `json:"group,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Version string `json:"version"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SourceRole is what a source of an Application is for.
// +kubebuilder:validation:Enum=Render;Values;Reference
type SourceRole string

const (
	// RoleRender is the source that renders: the only source, or a Helm chart.
	RoleRender SourceRole = "Render"
	// RoleValues is the ref source whose files hold the values; Backflow writes
	// its edits there.
	RoleValues SourceRole = "Values"
	// RoleReference is any other ref source.
	RoleReference SourceRole = "Reference"
)

// ProposalSource is one source of the Application of a proposal.
type ProposalSource struct {
	// Position in spec.sources of the Application, from 1.
	// +kubebuilder:validation:Minimum=1
	Position int32      `json:"position"`
	Role     SourceRole `json:"role"`
	// +kubebuilder:validation:MaxLength=2048
	RepoURL string `json:"repoURL"`
	// Revision Argo CD resolved and compared against: a commit SHA, or the chart
	// version of a chart from a Helm repository or a registry.
	// +kubebuilder:validation:MaxLength=255
	Revision string `json:"revision"`
	// Branch, tag, chart version or range the source tracks.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// Path inside a Git repository.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Path string `json:"path,omitempty"`
	// Chart name, for a chart from a Helm repository or a registry.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Chart string `json:"chart,omitempty"`
	// Name of a ref source, referenced as $name/... in value files.
	// +kubebuilder:validation:MaxLength=253
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
// +kubebuilder:validation:XValidation:rule="self.op != 'Add' || !has(self.desired)",message="an Add has no desired value"
// +kubebuilder:validation:XValidation:rule="self.op != 'Remove' || !has(self.live)",message="a Remove has no live value"
type FieldChange struct {
	// JSON pointer in the resource, e.g. /spec/replicas.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^/`
	Path string          `json:"path"`
	Op   ChangeOperation `json:"op"`
	// JSON-encoded value in Git. Empty for Add.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Desired string `json:"desired,omitempty"`
	// JSON-encoded value in the cluster. Empty for Remove.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Live string `json:"live,omitempty"`
}

// Actor says who changed the resource in the cluster, as far as Backflow can tell. It comes from
// the admission webhook of the operator (actorWebhook.enabled), only from a record whose
// manager, time and fields agree with the managedFields; without the webhook, or with no such
// record, there is no actor. The tools and controllers that wrote the fields are in
// spec.managers.
type Actor struct {
	// The Kubernetes username, from a verified record of the actor webhook. Empty: the
	// webhook is off, saw no matching change, or was told not to record names.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Username string `json:"username,omitempty"`
	// The groups of that user, under the same conditions.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +listType=atomic
	// +optional
	Groups []string `json:"groups,omitempty"`
	// Person or controller (a service account), from a verified record of the actor webhook.
	// It is set even when names are not recorded.
	// +optional
	Kind ActorKind `json:"kind,omitempty"`
}

// ManagedFieldsOperation is the operation of a managedFields entry.
// +kubebuilder:validation:Enum=Update;Apply
type ManagedFieldsOperation string

const (
	ManagedUpdate ManagedFieldsOperation = "Update"
	ManagedApply  ManagedFieldsOperation = "Apply"
)

// ManagerWrite is a field manager that wrote drifted fields, from the managedFields of the live
// object: a tool (kubectl-patch, kubectl-edit, oc, Mozilla for a browser console) or a controller
// (kube-controller-manager for an HPA).
type ManagerWrite struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Manager string `json:"manager"`
	// The operation of that managedFields entry.
	// +optional
	Operation ManagedFieldsOperation `json:"operation,omitempty"`
	// When that entry was written. Some entries (a subresource such as scale) have no time.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
	// The drifted paths (JSON pointers) it wrote.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=1024
	// +listType=atomic
	// +optional
	Fields []string `json:"fields,omitempty"`
}

// ProposalPhase is the lifecycle stage of a proposal. It summarises the conditions; consumers
// must tolerate values that are added later.
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

// Finished reports whether the phase is a final one that retention applies to: not open, and not
// Unmapped, which stays for a person to look at.
func (p ProposalPhase) Finished() bool {
	switch p {
	case PhaseMerged, PhaseRejected, PhaseReverted, PhaseSuperseded, PhaseFailed:
		return true
	}
	return false
}

// MappingStrategy is how the change was traced back to source. A proposal that is not mapped has
// none.
// +kubebuilder:validation:Enum=Direct;KustomizePatch;HelmValues;ApplicationOverride
type MappingStrategy string

const (
	StrategyDirect              MappingStrategy = "Direct"
	StrategyKustomizePatch      MappingStrategy = "KustomizePatch"
	StrategyHelmValues          MappingStrategy = "HelmValues"
	StrategyApplicationOverride MappingStrategy = "ApplicationOverride"
)

// Mapping records where each change lands in the source. Why a proposal is not mapped is the
// reason of its Mapped condition.
type Mapping struct {
	// How the change was traced back to source. Empty for a proposal that is not mapped.
	// +optional
	Strategy MappingStrategy `json:"strategy,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	Edits []SourceEdit `json:"edits,omitempty"`
	// True when a re-render with the edits reproduced the live state exactly.
	// +optional
	Verified bool `json:"verified,omitempty"`
	// Unified diff of the edited files, truncated to 32 KiB.
	// +kubebuilder:validation:MaxLength=32768
	// +optional
	Diff string `json:"diff,omitempty"`
	// Changes the edits also make beyond the drifted fields, which the policy
	// allowed (spec.helm.allowCollateralChanges). Set only for HelmValues.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	Collateral []CollateralChange `json:"collateral,omitempty"`
	// Fields of spec.changes the edits leave out, with the reason for each. Set
	// only for a partial mapping (spec.mapping.allowPartial of the policy): the
	// edits then cover the other changes, and these fields stay different from
	// Git after the merge request is merged.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	Unmapped []UnmappedField `json:"unmapped,omitempty"`
	// Where the value is set in the Application, when an option of the
	// Application (helm.parameters, kustomize.images, ...) decides a drifted
	// field. Set only when the Mapped condition has reason SetByApplication.
	// Nothing is proposed: no Argo CD API renders an Application with a changed
	// spec, so the edit cannot be verified by a render of the child.
	// +optional
	ApplicationOption *ApplicationValue `json:"applicationOption,omitempty"`
}

// ApplicationParentState says what is known about the Git file that defines the
// Application whose option sets the value.
// +kubebuilder:validation:Enum=Located;NotManagedFromGit;GeneratedByApplicationSet;NotVerifiable;OutOfSync;Ambiguous;NotVisible
type ApplicationParentState string

const (
	// ParentLocated: the parent Application renders the child from Git and a
	// render of the parent with the edit changes exactly the option.
	ParentLocated ApplicationParentState = "Located"
	// ParentNotManagedFromGit: no Application manages the child.
	ParentNotManagedFromGit ApplicationParentState = "NotManagedFromGit"
	// ParentGeneratedByApplicationSet: an ApplicationSet generated the child.
	ParentGeneratedByApplicationSet ApplicationParentState = "GeneratedByApplicationSet"
	// ParentNotVerifiable: the parent exists but its Git file cannot be found
	// with certainty (several sources, no Git source, no verified edit).
	ParentNotVerifiable ApplicationParentState = "NotVerifiable"
	// ParentOutOfSync: the parent is not synced, so its Git is not what runs.
	ParentOutOfSync ApplicationParentState = "OutOfSync"
	// ParentAmbiguous: more than one Application manages the child.
	ParentAmbiguous ApplicationParentState = "Ambiguous"
	// ParentNotVisible: the parent exists, or may exist, but the policy may not see
	// it. Nothing else about it is said.
	ParentNotVisible ApplicationParentState = "NotVisible"
)

// ApplicationValue says which option of an Argo CD Application sets a drifted
// value, and where that option is defined in Git.
type ApplicationValue struct {
	// The Application whose option sets the value.
	ApplicationRef ObjectRef `json:"applicationRef"`
	// The option, such as spec.source.helm.parameters[name=image.tag].
	// +kubebuilder:validation:MaxLength=1024
	Option string `json:"option"`
	// JSON pointer of the option's value in the Application resource, such as
	// /spec/source/helm/parameters/0/value.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	OptionPath string `json:"optionPath,omitempty"`
	// JSON-encoded value the option has now.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Current string `json:"current,omitempty"`
	// JSON-encoded value that would reproduce the live state, when it can be
	// derived exactly (a parameter, a valuesObject key, an image tag, a replica
	// count, a label). Empty when it cannot, or when the option is a string that
	// holds YAML (helm.values, kustomize patches).
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Suggested string `json:"suggested,omitempty"`
	// The parent Application and its Git file.
	// +optional
	Parent *ParentApplication `json:"parent,omitempty"`
}

// ParentApplication is the Application that defines the child in Git.
type ParentApplication struct {
	State ApplicationParentState `json:"state"`
	// The parent Application as namespace/name, or the ApplicationSet that
	// generated the child.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Name string `json:"name,omitempty"`
	// Git repository and commit the parent was rendered at.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	RepoURL string `json:"repoURL,omitempty"`
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Revision string `json:"revision,omitempty"`
	// File that defines the option and where in it (a key path for Helm, a JSON
	// pointer otherwise).
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	File string `json:"file,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Location string `json:"location,omitempty"`
	// Unified diff of the edit that sets the suggested value, verified by a
	// render of the parent. Empty when there is no suggested value.
	// +kubebuilder:validation:MaxLength=32768
	// +optional
	Diff string `json:"diff,omitempty"`
	// Why the state is what it is, for people.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`
}

// UnmappedField is a drifted field that a partial mapping leaves out.
type UnmappedField struct {
	// JSON pointer of the field in the resource.
	// +kubebuilder:validation:MaxLength=1024
	Path string `json:"path"`
	// Machine-readable reason, such as UnsupportedChange or NoVerifiedCandidate. The set of
	// reasons grows; consumers must tolerate values they do not know.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	Reason string `json:"reason"`
	// Why the field could not be traced back to Git, for people.
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message"`
}

// CollateralChange is a field the edit changes although it did not drift.
type CollateralChange struct {
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Group string `json:"group,omitempty"`
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// JSON pointer in the rendered resource.
	// +kubebuilder:validation:MaxLength=1024
	Path string `json:"path"`
	// JSON-encoded value before the edit (the value of the unmodified render).
	// +kubebuilder:validation:MaxLength=16384
	Current string `json:"current"`
	// JSON-encoded value after the edit.
	// +kubebuilder:validation:MaxLength=16384
	New string `json:"new"`
}

// SourceEdit is one edit in a Git file.
type SourceEdit struct {
	// Repository the edit is written to: the repository of the source with role Values, or of the
	// only source.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	RepoURL string `json:"repoURL,omitempty"`
	// File path relative to the repository root.
	// +kubebuilder:validation:MaxLength=1024
	File string `json:"file"`
	// For Helm values, the dotted values path; otherwise a JSON pointer in the file.
	// +kubebuilder:validation:MaxLength=1024
	Location string `json:"location"`
	// JSON-encoded new value.
	// +kubebuilder:validation:MaxLength=16384
	Value string `json:"value"`
	// Index into spec.changes this edit satisfies.
	// +kubebuilder:validation:Minimum=0
	ChangeIndex int32 `json:"changeIndex"`
}

// MergeRequestRef points to the merge request opened for this proposal.
type MergeRequestRef struct {
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// Provider-side number (GitLab iid, GitHub PR number).
	Number int64 `json:"number"`
	// Provider-reported state, e.g. opened, merged, closed. The set is the provider's: consumers
	// must tolerate values they do not know.
	// +kubebuilder:validation:MaxLength=64
	State string `json:"state"`
}

// GitStatus is what the proposal did in Git.
type GitStatus struct {
	// Branch Backflow pushes (or is about to push) for the merge request. It is
	// recorded before the push, so a branch or merge request left behind by a
	// crash can still be found and cleaned up.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	MergeRequest *MergeRequestRef `json:"mergeRequest,omitempty"`
	// Commit that contains the change, once merged or committed directly.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
	// Bookkeeping of declined pushes.
	// +optional
	Push *PushStatus `json:"push,omitempty"`
}

// PushStatus counts the pushes the Git server declined. A decline during an outage of the Git
// host looks like one of a protected branch, so a few declines are retried with a growing delay
// before the proposal fails. Kept here so the count survives an operator restart.
type PushStatus struct {
	// How many times the Git server declined the push of this proposal.
	// +optional
	Rejections int32 `json:"rejections,omitempty"`
	// When the push was last declined; the next attempt waits from here.
	// +optional
	LastRejectedAt *metav1.Time `json:"lastRejectedAt,omitempty"`
}

// Signals are facts that the policy and drift controllers leave for the proposal controller,
// which turns them into a phase. They are in status, which users cannot write, and not in
// annotations, which they can. Only those two controllers write this object (with a merge patch
// of just this object) and the proposal controller only reads it.
type Signals struct {
	// The name of the newer proposal that replaced this one.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SupersededBy string `json:"supersededBy,omitempty"`
	// Why the proposal has no reason to exist any more (for example: its drift is gone before
	// it was proposed). The proposal becomes Reverted with this message.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reverted string `json:"reverted,omitempty"`
	// The revision of an Argo CD sync that reset the live resource to Git while the proposal was
	// open. Cleared when the live change returns.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	LiveRevertedAtRevision string `json:"liveRevertedAtRevision,omitempty"`
	// True when the policy may no longer see the Application, so the proposal is retired.
	// +optional
	AccessRevoked bool `json:"accessRevoked,omitempty"`
	// True once the rejection of a Rejected proposal was reported (an event), so it is not
	// reported again.
	// +optional
	RejectionReported bool `json:"rejectionReported,omitempty"`
}

// DriftProposalStatus tracks the proposal through mapping and review.
type DriftProposalStatus struct {
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Phase",xDescriptors="urn:alm:descriptor:io.kubernetes.phase"
	Phase ProposalPhase `json:"phase,omitempty"`
	// +optional
	Mapping *Mapping `json:"mapping,omitempty"`
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Git"
	Git *GitStatus `json:"git,omitempty"`
	// Proposal that replaced this one.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SupersededBy string `json:"supersededBy,omitempty"`
	// Human-readable explanation of the current phase: the message of the condition that caused it.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Message"
	Message string `json:"message,omitempty"`
	// When the proposal reached a final phase (Merged, Rejected, Reverted, Superseded, Failed).
	// spec.retention.finishedProposals of the policy counts from here.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Finished at"
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Facts left by the policy and drift controllers; see Signals.
	// +optional
	Signals *Signals `json:"signals,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Conditions",xDescriptors="urn:alm:descriptor:io.kubernetes.conditions"
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dp;drift,categories=backflow
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.spec.applicationRef.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.resourceRef.kind`
// +kubebuilder:printcolumn:name="Resource",type=string,JSONPath=`.spec.resourceRef.name`
// +kubebuilder:printcolumn:name="Actor",type=string,JSONPath=`.spec.actor.username`
// +kubebuilder:printcolumn:name="Changed by",type=string,JSONPath=`.spec.managers[0].manager`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.status.mapping.strategy`,priority=1
// +kubebuilder:printcolumn:name="Mapped",type=string,JSONPath=`.status.conditions[?(@.type=="Mapped")].reason`,priority=1
// +kubebuilder:printcolumn:name="Proposed",type=string,JSONPath=`.status.conditions[?(@.type=="Proposed")].reason`,priority=1
// +kubebuilder:printcolumn:name="MR",type=string,JSONPath=`.status.git.mergeRequest.url`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DriftProposal is a captured difference between Git and the cluster,
// and its journey back to Git. Its spec is an immutable observation, not a desired state.
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
