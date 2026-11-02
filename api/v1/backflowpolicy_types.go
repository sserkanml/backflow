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
package v1

import (
	"time"

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

// Condition types and reasons of a BackflowPolicy.
const (
	// PolicyReady is True while the policy resolves its Applications and can detect drift.
	PolicyReady = "Ready"
	// ReasonServerNotAllowed: spec.argoCD.server.url is not in the allowlist the installer set for
	// the operator (--argocd-allowed-servers) and is not the operator's default server. No request
	// is made.
	ReasonServerNotAllowed = "ServerNotAllowed"
)

// BackflowPolicySpec selects Argo CD Applications whose drift is captured
// and decides how that drift flows back to Git.
type BackflowPolicySpec struct {
	// Where the Argo CD Applications are and how to reach the Argo CD API.
	// +kubebuilder:default={}
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Argo CD"
	ArgoCD ArgoCDSpec `json:"argoCD,omitzero"`

	// Applications this policy applies to.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Applications"
	Applications ApplicationSelector `json:"applications"`

	// What to do with detected drift.
	// +kubebuilder:default=MergeRequest
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Mode",xDescriptors="urn:alm:descriptor:com.tectonic.ui:select:MergeRequest,urn:alm:descriptor:com.tectonic.ui:select:DirectCommit,urn:alm:descriptor:com.tectonic.ui:select:ReportOnly"
	Mode BackflowMode `json:"mode,omitempty"`

	// Resource kinds to capture. Empty means every kind the Application manages.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Include"
	Include []KindSelector `json:"include,omitempty"`

	// Resource kinds never captured. Evaluated after Include.
	// Secrets are excluded unless Include names the Secret kind literally.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Exclude"
	Exclude []KindSelector `json:"exclude,omitempty"`

	// Fields to ignore when computing drift, in addition to built-in noise
	// (status, managedFields, resourceVersion, platform-injected fields).
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
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

	// Where and how Backflow writes to Git: the branches, and for a merge request its labels
	// and reviewers.
	// +kubebuilder:default={}
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Git"
	Git GitOptions `json:"git,omitzero"`

	// A drift becomes a proposal only after it has stayed the same, at the
	// same synced revision, for this long and across at least two
	// observations. A drift that disappears within the window is never
	// proposed, and several quick edits to the same resource give one
	// proposal for the final state. An explicit 0s waits for the second
	// observation only. With the actor webhook on, a drift is also held for the time the
	// webhook needs to write its records (at most 5s); the larger counts.
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s') && duration(self) <= duration('24h')",message="batchWindow must be between 0s and 24h"
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Batch window"
	BatchWindow *metav1.Duration `json:"batchWindow,omitempty"`

	// How long Backflow keeps what is finished.
	// +kubebuilder:default={}
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Retention"
	Retention RetentionOptions `json:"retention,omitzero"`
}

// RetentionOptions says how long finished objects are kept.
type RetentionOptions struct {
	// A DriftProposal that is Merged, Rejected, Reverted, Superseded or Failed is deleted this long
	// after it finished. Proposals that are open, or Unmapped, are never deleted. 0s keeps
	// finished proposals for ever. Deleting a proposal does not touch its merge request.
	// +kubebuilder:default="336h"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s') && duration(self) <= duration('8760h')",message="finishedProposals must be between 0s and 8760h"
	// +optional
	FinishedProposals *metav1.Duration `json:"finishedProposals,omitempty"`
}

// DefaultArgoCDNamespace is the namespace of the Applications of a policy that does not name one.
const DefaultArgoCDNamespace = "argocd"

// ApplicationNamespace is the namespace the Applications of the policy are in.
func (s BackflowPolicySpec) ApplicationNamespace() string {
	if s.ArgoCD.Namespace == "" {
		return DefaultArgoCDNamespace
	}
	return s.ArgoCD.Namespace
}

// PartialAllowed reports whether a proposal may be mapped partially: the fields
// that can be traced back are proposed and the others left out
// (spec.mapping.allowPartial).
func (s BackflowPolicySpec) PartialAllowed() bool {
	return s.Mapping != nil && s.Mapping.AllowPartial
}

// ScratchBranchesAllowed reports whether verification may push scratch branches
// (spec.mapping.scratchBranches): Auto allows them except in ReportOnly mode.
func (s BackflowPolicySpec) ScratchBranchesAllowed() bool {
	mode := ScratchBranchesAuto
	if s.Mapping != nil && s.Mapping.ScratchBranches != "" {
		mode = s.Mapping.ScratchBranches
	}
	switch mode {
	case ScratchBranchesAlways:
		return true
	case ScratchBranchesNever:
		return false
	}
	return s.Mode != ModeReportOnly
}

// OverridesAllowed reports whether a Helm edit may add a key to the
// Application's last value file to override a chart default
// (spec.helm.allowValueOverrides, true unless set to false).
func (s BackflowPolicySpec) OverridesAllowed() bool {
	return s.Helm == nil || s.Helm.AllowValueOverrides == nil || *s.Helm.AllowValueOverrides
}

// KustomizeEditsBases reports whether a Kustomize edit may change files outside
// the Application's path, which other Applications may share
// (spec.kustomize.allowBaseEdits, false unless set).
func (s BackflowPolicySpec) KustomizeEditsBases() bool {
	return s.Kustomize != nil && s.Kustomize.AllowBaseEdits
}

// KustomizeAddsPatches reports whether a Kustomize edit may add a patch file
// to the Application's overlay as a last resort (spec.kustomize.allowNewPatches,
// true unless set to false).
func (s BackflowPolicySpec) KustomizeAddsPatches() bool {
	return s.Kustomize == nil || s.Kustomize.AllowNewPatches == nil || *s.Kustomize.AllowNewPatches
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

// DefaultFinishedRetention is how long a finished proposal is kept when the policy does not say.
const DefaultFinishedRetention = 14 * 24 * time.Hour

// FinishedRetention returns how long finished proposals are kept; zero means for ever.
func (s BackflowPolicySpec) FinishedRetention() time.Duration {
	if s.Retention.FinishedProposals == nil {
		return DefaultFinishedRetention
	}
	return s.Retention.FinishedProposals.Duration
}

// AssignsActor reports whether the Kubernetes user who made the change is assigned to the merge
// request when they map to an SCM account (spec.git.mergeRequest.assignActor, true unless false).
func (s BackflowPolicySpec) AssignsActor() bool {
	return s.Git.MergeRequest.AssignActor == nil || *s.Git.MergeRequest.AssignActor
}

// DefaultBranchPrefix is the prefix of the branches Backflow creates when the policy does not say.
const DefaultBranchPrefix = "backflow/"

// ArgoCDSpec says where the Applications are and how to reach the Argo CD API.
type ArgoCDSpec struct {
	// Namespace where Argo CD Applications live.
	// "argocd" for upstream Argo CD, "openshift-gitops" for OpenShift GitOps.
	// +kubebuilder:default=argocd
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Argo CD API connection used to read managed resources for drift detection.
	// When unset, the connection the installer gave the operator (its flags
	// --argocd-server, --argocd-token-secret-* and --argocd-ca-file; the chart
	// values argocd.*) is used, and status.argoCD says so. Drift detection is
	// skipped while neither exists. A policy can only name Secrets of its own
	// namespace; it cannot point the operator at a Secret elsewhere. A server of its own must
	// be in the allowlist the installer set (argocd.allowedServers); otherwise the policy is
	// not Ready (ServerNotAllowed) and no request is made.
	// +optional
	Server *ArgoCDServer `json:"server,omitempty"`
}

// ArgoCDServer describes how to reach the Argo CD API.
type ArgoCDServer struct {
	// Base URL of the Argo CD API server. Override it when the operator runs
	// outside the cluster, e.g. https://localhost:8080 behind a port-forward.
	// +kubebuilder:default="https://argocd-server.argocd.svc"
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+(/\S*)?$`
	URL string `json:"url,omitempty"`

	// Secret key, in this namespace, holding an Argo CD API token with read access to applications.
	TokenSecretRef SecretKeyReference `json:"tokenSecretRef"`

	// Skip verification of the Argo CD server certificate.
	// +kubebuilder:default=false
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`

	// Secret key, in this namespace, holding a PEM CA bundle used to verify the Argo CD server.
	// +optional
	CASecretRef *SecretKeyReference `json:"caSecretRef,omitempty"`
}

// ApplicationSelector picks Argo CD Applications by name or label.
// At least one of Names or Selector must be set. An Application matches when it is named, or
// when its labels match the selector.
// +kubebuilder:validation:XValidation:rule="(has(self.names) && size(self.names) > 0) || has(self.selector)",message="set names, selector, or both"
type ApplicationSelector struct {
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +listType=set
	// +optional
	Names []string `json:"names,omitempty"`
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// KindSelector matches resources by API group and kind.
type KindSelector struct {
	// API group; empty for the core group, "*" for any group.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Group string `json:"group,omitempty"`
	// Kind, or "*" for any kind.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Kind string `json:"kind"`
}

// IgnoreRule removes fields from drift comparison.
type IgnoreRule struct {
	KindSelector `json:",inline"`
	// Restrict to a resource name. Empty matches every name.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Name string `json:"name,omitempty"`
	// JSON pointers to ignore, e.g. /spec/replicas.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=1024
	// +kubebuilder:validation:items:Pattern=`^/`
	// +listType=atomic
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
	AllowBaseEdits bool `json:"allowBaseEdits,omitempty"`

	// As a last resort, when no existing file can take the change, add a
	// strategic merge patch with only the drifted fields to the Application's
	// overlay (backflow-patches/<kind>-<name>.yaml) and list it under patches
	// in the overlay's kustomization.
	// +kubebuilder:default=true
	// +optional
	AllowNewPatches *bool `json:"allowNewPatches,omitempty"`
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

	// Whether verification may push scratch branches to the repository that
	// holds the value files of a multi-source Application. Such an Application
	// is rendered by Argo CD from commits, so each candidate edit is committed to
	// a temporary branch (backflow/scratch/...), rendered by its commit SHA and
	// deleted again; no branch of the source of truth is touched. Auto: allowed
	// for MergeRequest and DirectCommit, not for ReportOnly, whose mapping never
	// writes to Git. Always: allowed in every mode. Never: not allowed. When it is
	// not allowed, a proposal that needs scratch branches is Unmapped with reason
	// ScratchBranchesDisabled.
	// +kubebuilder:default=Auto
	// +optional
	ScratchBranches ScratchBranchesMode `json:"scratchBranches,omitempty"`
}

// ScratchBranchesMode is the value of spec.mapping.scratchBranches.
// +kubebuilder:validation:Enum=Auto;Always;Never
type ScratchBranchesMode string

const (
	ScratchBranchesAuto   ScratchBranchesMode = "Auto"
	ScratchBranchesAlways ScratchBranchesMode = "Always"
	ScratchBranchesNever  ScratchBranchesMode = "Never"
)

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
	// allowValueOverrides) when the Application uses several. It must be one of the
	// Application's value files, as a repository path, relative to the chart
	// directory, or as the Application writes it; otherwise such a change stays
	// Unmapped and says so. Defaults to the last file in
	// spec.source.helm.valueFiles.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	TargetValuesFile string `json:"targetValuesFile,omitempty"`

	// Explicit field-to-value mappings. Take precedence over automatic mapping.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	Mappings []HelmFieldMapping `json:"mappings,omitempty"`

	// When no mapping is found, propose the change as an override in the
	// Application's spec.source.helm.valuesObject instead of leaving it unmapped.
	// +kubebuilder:default=false
	AllowApplicationOverride bool `json:"allowApplicationOverride,omitempty"`

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
	// +kubebuilder:validation:MaxItems=2
	// +listType=set
	// +optional
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
	AllowValueOverrides *bool `json:"allowValueOverrides,omitempty"`
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
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Name string `json:"name,omitempty"`
	// JSON pointer in the rendered resource, e.g. /spec/replicas.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^/`
	FieldPointer string `json:"fieldPointer"`
	// Dotted path in values, e.g. api.replicaCount.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ValuesPath string `json:"valuesPath"`
}

// GitOptions customises what Backflow writes to Git: the branches of a merge request or a
// direct commit, and the merge request itself.
type GitOptions struct {
	// Prefix for branches Backflow creates.
	// +kubebuilder:default="backflow/"
	// +kubebuilder:validation:MaxLength=64
	BranchPrefix string `json:"branchPrefix,omitempty"`

	// Target branch. Defaults to the Application's targetRevision when it is a branch.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	TargetBranch string `json:"targetBranch,omitempty"`

	// Settings of the merge requests. Used when Mode is MergeRequest.
	// +kubebuilder:default={}
	// +optional
	MergeRequest MergeRequestOptions `json:"mergeRequest,omitzero"`
}

// MergeRequestOptions customises the merge requests Backflow opens.
type MergeRequestOptions struct {
	// Labels to put on the merge request.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +listType=set
	// +optional
	Labels []string `json:"labels,omitempty"`

	// Usernames to assign as reviewers.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +listType=set
	// +optional
	Reviewers []string `json:"reviewers,omitempty"`

	// Assign the Kubernetes user who made the change, when they map to an SCM account.
	// It is a pointer so that false can be set: the default is true.
	// +kubebuilder:default=true
	// +optional
	AssignActor *bool `json:"assignActor,omitempty"`
}

// BackflowPolicyStatus summarises what the policy currently covers.
type BackflowPolicyStatus struct {
	// The generation of the spec that the conditions describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Conditions",xDescriptors="urn:alm:descriptor:io.kubernetes.conditions"
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Details of the matched Applications, at most 100. The policy covers all of them even when
	// the list is cut: see applicationCount.
	// +kubebuilder:validation:MaxItems=100
	// +listType=atomic
	// +optional
	Applications []ApplicationSummary `json:"applications,omitempty"`
	// How many Applications the policy covers.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Applications"
	ApplicationCount int32 `json:"applicationCount,omitempty"`
	// True when applications lists fewer Applications than applicationCount.
	// +optional
	ApplicationsTruncated bool `json:"applicationsTruncated,omitempty"`
	// DriftProposals not yet merged, rejected or reverted.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Open proposals"
	OpenProposals int32 `json:"openProposals,omitempty"`
	// Which Argo CD connection the policy uses. Unset while there is none.
	// +optional
	ArgoCD *ArgoCDStatus `json:"argoCD,omitempty"`
}

// ArgoCDSource says where the Argo CD connection of a policy comes from.
// +kubebuilder:validation:Enum=Policy;OperatorDefault
type ArgoCDSource string

const (
	// ArgoCDSourcePolicy is spec.argoCD.server of the policy.
	ArgoCDSourcePolicy ArgoCDSource = "Policy"
	// ArgoCDSourceOperatorDefault is the connection the installer gave the operator.
	ArgoCDSourceOperatorDefault ArgoCDSource = "OperatorDefault"
)

// HiddenReason says why a selected Application is not visible to the policy.
// +kubebuilder:validation:Enum=ProjectNotAllowed;NotVisibleToToken;VisibilityUnknown
type HiddenReason string

const (
	// HiddenProjectNotAllowed: the Argo CD project is not allowed for the policy's namespace
	// (operator default connection).
	HiddenProjectNotAllowed HiddenReason = "ProjectNotAllowed"
	// HiddenNotVisibleToToken: the policy's own token cannot get the Application.
	HiddenNotVisibleToToken HiddenReason = "NotVisibleToToken"
	// HiddenVisibilityUnknown: Argo CD could not be asked whether the policy's token may see them;
	// none is shown until it can.
	HiddenVisibilityUnknown HiddenReason = "VisibilityUnknown"
)

// ArgoCDStatus is the Argo CD connection a policy uses.
type ArgoCDStatus struct {
	// Policy (spec.argoCD.server) or OperatorDefault.
	Source ArgoCDSource `json:"source"`
	// The Argo CD API server it talks to. Never a token.
	URL string `json:"url"`
	// How many Applications the spec selects that the policy may not see, by reason. Only
	// counts are shown, never a name, repository, path or project.
	// +kubebuilder:validation:MaxItems=3
	// +listType=map
	// +listMapKey=reason
	// +optional
	HiddenApplications []HiddenApplications `json:"hiddenApplications,omitempty"`
}

// HiddenApplications is a number of selected Applications that are hidden for a reason.
type HiddenApplications struct {
	Reason HiddenReason `json:"reason"`
	Count  int32        `json:"count"`
}

// UntrackedReason says why Backflow does not track an Application.
// +kubebuilder:validation:Enum=CredentialsInRepoURL
type UntrackedReason string

// UntrackedCredentialsInRepoURL: a repository URL of the Application contains credentials; only
// the redacted URL is kept in the status.
const UntrackedCredentialsInRepoURL UntrackedReason = "CredentialsInRepoURL"

// ApplicationSummary is what Backflow knows about a matched Argo CD Application.
type ApplicationSummary struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Number of resources the Application manages.
	ManagedResources int32 `json:"managedResources"`
	// Why Backflow does not track the Application, or empty when it does.
	// +optional
	UntrackedReason UntrackedReason `json:"untrackedReason,omitempty"`
	// Every source of the Application, in order. One entry for an Application with one source.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Sources []SourceSummary `json:"sources,omitempty"`
}

// First returns the first source, or the zero value.
func (a ApplicationSummary) First() SourceSummary {
	if len(a.Sources) == 0 {
		return SourceSummary{}
	}
	return a.Sources[0]
}

// SourceSummary describes one source of an Application.
type SourceSummary struct {
	// Position of the source in spec.sources, starting at 1 like Argo CD's
	// sourcePositions.
	Position int32 `json:"position"`
	// Repository the source comes from: a Git repository, a Helm repository or
	// an OCI registry. Credentials in it are redacted.
	// +kubebuilder:validation:MaxLength=2048
	RepoURL string `json:"repoURL"`
	// Path inside a Git repository.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Path string `json:"path,omitempty"`
	// Chart name, for a source that comes from a Helm repository or a registry.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Chart string `json:"chart,omitempty"`
	// Branch, tag, chart version or range the source tracks.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`
	// Revision Argo CD resolved and compares against: a commit SHA, or the chart
	// version (status.sync.revisions at this position).
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Revision string `json:"revision,omitempty"`
	// Revision of the last sync (status.operationState.syncResult.revisions).
	// +kubebuilder:validation:MaxLength=255
	// +optional
	SyncedRevision string `json:"syncedRevision,omitempty"`
	// How Argo CD renders the source. Empty for a ref source, which is only referenced.
	// +optional
	Type SourceType `json:"type,omitempty"`
	// Name of a ref source, referenced as $name/... in value files.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Ref string `json:"ref,omitempty"`
	// ScmConnection, in the policy's namespace, used for this repository. Empty for a repository
	// that is not a Git repository, and when none matches.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ScmConnectionName string `json:"scmConnectionName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bfp,categories=backflow
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Applications",type=integer,JSONPath=`.status.applicationCount`
// +kubebuilder:printcolumn:name="Open",type=integer,JSONPath=`.status.openProposals`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackflowPolicy decides which Argo CD Applications are watched for drift
// and how that drift is proposed back to Git. Deleting a policy deletes its DriftProposals (they
// are owned by it); a merge request that is open on the Git host stays open and is no longer
// tracked.
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
