package kustomize

// Keys and words of kustomization files and resources that more than one file
// of the package spells.
const (
	keyReplicas     = "replicas"
	keyMetadata     = "metadata"
	keyNamespace    = "namespace"
	keyLabels       = "labels"
	keyAnnotations  = "annotations"
	keyPatches      = "patches"
	keyName         = "name"
	textTrue        = "true"
	textNull        = "null"
	keyImages       = "images"
	keyNamePrefix   = "namePrefix"
	keyNameSuffix   = "nameSuffix"
	keyCommonLabels = "commonLabels"
	keyCommonAnnots = "commonAnnotations"
)

// specReplicas is the path of the replica count of a workload.
var specReplicas = []string{"spec", keyReplicas}
