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


// ScmProvider is the type of Git hosting service.
// +kubebuilder:validation:Enum=gitlab;github
type ScmProvider string

const (
	ScmProviderGitLab ScmProvider = "gitlab"
	ScmProviderGitHub ScmProvider = "github"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ScmConnectionSpec defines the desired state of ScmConnection
type ScmConnectionSpec struct {
	// Provider type.
	Provider ScmProvider `json:"provider"`

	// Base URL of the provider, e.g. https://gitlab.example.com or https://github.com.
	// +kubebuilder:validation:Pattern=`^https?://.+`
	URL string `json:"url"`

	// Repository hosts this connection is used for. A repository URL from an
	// Argo CD Application is matched against these hosts. Defaults to the host of URL.
	// +optional
	Hosts []string `json:"hosts,omitempty"`

	// Secret key holding an API token with permission to push branches and
	// open merge requests (GitLab: api + write_repository; GitHub: contents + pull_requests).
	TokenSecretRef corev1.SecretKeySelector `json:"tokenSecretRef"`

	// Optional Secret key holding a PEM CA bundle for self-hosted providers.
	// +optional
	CASecretRef *corev1.SecretKeySelector `json:"caSecretRef,omitempty"`

	// Identity used as Git committer. The Kubernetes user who made the change
	// is recorded as the author when known.
	// +optional
	Committer *GitIdentity `json:"committer,omitempty"`
}

// GitIdentity is a Git name/email pair.
type GitIdentity struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Format=email
	Email string `json:"email"`
}

// ScmConnectionStatus reports whether the connection works.
type ScmConnectionStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Account the token authenticated as.
	// +optional
	AuthenticatedAs string `json:"authenticatedAs,omitempty"`

	// Last time the token was verified.
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=scm
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="User",type=string,JSONPath=`.status.authenticatedAs`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ScmConnection is a connection to a Git hosting service.
type ScmConnection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScmConnectionSpec   `json:"spec,omitempty"`
	Status ScmConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ScmConnectionList contains a list of ScmConnection
type ScmConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ScmConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ScmConnection{}, &ScmConnectionList{})
		return nil
	})
}
