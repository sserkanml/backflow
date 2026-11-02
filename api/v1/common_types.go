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

// SecretKeyReference names a key of a Secret in the namespace of the object that holds it. There
// is no namespace field on purpose: an object can only point at Secrets next to it.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Key of the Secret that holds the value.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// SourceType is how an Argo CD Application source is rendered.
// +kubebuilder:validation:Enum=Directory;Kustomize;Helm;Plugin
type SourceType string

const (
	SourceDirectory SourceType = "Directory"
	SourceKustomize SourceType = "Kustomize"
	SourceHelm      SourceType = "Helm"
	// SourcePlugin is a config management plugin. Backflow cannot map it: the proposal is Unmapped.
	SourcePlugin SourceType = "Plugin"
)

// ActorKind says what kind of identity changed a resource. It is all that is stored when the
// operator is told not to record user names. Unset means unknown.
// +kubebuilder:validation:Enum=Person;Controller
type ActorKind string

const (
	// ActorPerson is a user, or anything that is not a service account.
	ActorPerson ActorKind = "Person"
	// ActorController is a service account.
	ActorController ActorKind = "Controller"
)

// ObjectRef points to a namespaced object.
type ObjectRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
}

// LocalObjectRef points to an object in the namespace of the object that holds the reference.
type LocalObjectRef struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}
