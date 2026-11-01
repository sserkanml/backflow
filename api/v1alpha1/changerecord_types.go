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

// ActorKind says what kind of identity changed a resource. It is all that is stored
// when the operator is told not to record user names.
// +kubebuilder:validation:Enum=person;controller
type ActorKind string

const (
	// ActorPerson is a user, or anything that is not a service account.
	ActorPerson ActorKind = "person"
	// ActorController is a service account.
	ActorController ActorKind = "controller"
)

// ChangeRecordSpec is who changed which fields of a resource and when, as the
// admission webhook of the operator saw it. It holds the paths of the changed
// fields and never a value.
type ChangeRecordSpec struct {
	// UID of the changed object.
	UID string `json:"uid"`
	// +optional
	Group    string `json:"group,omitempty"`
	Resource string `json:"resource"`
	// The subresource of the request (for example scale). Empty for the resource itself.
	// +optional
	SubResource string `json:"subResource,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// The resourceVersion of the object after the last change merged into this record.
	// +optional
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// +kubebuilder:validation:Enum=UPDATE
	Operation string `json:"operation"`
	// The field manager of the request.
	// +optional
	FieldManager string `json:"fieldManager,omitempty"`
	// The Kubernetes user name. Empty when the operator is told not to record names.
	// +optional
	Username string `json:"username,omitempty"`
	// The groups of the user. Empty when the operator is told not to record names.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	// +optional
	Groups []string `json:"groups,omitempty"`
	// Person or controller (a service account): the only identity stored when names are not.
	Kind ActorKind `json:"kind"`
	// The JSON pointers of the changed fields. Never values.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	Paths []string `json:"paths"`
	// When the first change merged into this record was made, and the last.
	FirstSeen metav1.Time `json:"firstSeen"`
	LastSeen  metav1.Time `json:"lastSeen"`
	// How many changes were merged.
	Count int32 `json:"count"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=chr,categories=backflow
// +kubebuilder:printcolumn:name="Resource",type=string,JSONPath=`.spec.resource`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Manager",type=string,JSONPath=`.spec.fieldManager`
// +kubebuilder:printcolumn:name="Last seen",type=date,JSONPath=`.spec.lastSeen`

// ChangeRecord is written by the actor webhook of the operator, in the operator's
// namespace, and deleted after actorWebhook.retention. It holds user names: it is for
// the operator and its administrators, not for tenants.
type ChangeRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec ChangeRecordSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ChangeRecordList contains a list of ChangeRecord
type ChangeRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ChangeRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ChangeRecord{}, &ChangeRecordList{})
		return nil
	})
}
