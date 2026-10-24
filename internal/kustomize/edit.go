package kustomize

import "github.com/sserkanml/backflow/api/v1alpha1"

// The tiers of places an edit can go, in the order they are tried.
const (
	// TierSetting: a setting of the Application's own kustomization (images,
	// replicas, labels, generator literals).
	TierSetting = 1
	// TierPatch: a value inside a patch the Application's kustomization already
	// has.
	TierPatch = 2
	// TierResource: a resource file inside the Application's path.
	TierResource = 3
	// TierBase: a file outside the Application's path (a shared base), only
	// with spec.kustomize.editBases.
	TierBase = 4
	// TierNewPatch: a new patch file added to the Application's kustomization,
	// the last resort (spec.kustomize.addPatches).
	TierNewPatch = 5
)

// The kinds of Edit.
const (
	KindSetting        = "setting"
	KindPatchFile      = "patch-file"
	KindPatchInline    = "patch-inline"
	KindJSON6902File   = "json6902-file"
	KindJSON6902Inline = "json6902-inline"
	KindResource       = "resource"
	KindNewPatch       = "new-patch"
	patchesDirectory   = "backflow-patches"
)

// Edit says what a candidate does to its file. It is the Meta of a
// rendermap.Candidate.
type Edit struct {
	Tier int
	Kind string
	// Document is the YAML document of the file (the resource file, the patch
	// file) that is edited.
	Document int
	// Holder is, for an inline patch, the JSON pointer in the kustomization of
	// the string that holds the patch; Pointer is then inside that string.
	Holder string
	// Pointer is the JSON pointer of the field in the edited document.
	Pointer string
	Op      v1alpha1.ChangeOperation
	// Desired and Live are the JSON texts of the value in the render and in
	// the cluster; Live is empty for a removal.
	Desired, Live string
	// Shared is set for an edit of a file outside the Application's path.
	Shared bool
	// Kustomization is, for a new patch, the kustomization that lists it.
	Kustomization string
	// Patch is, for a new patch, what this edit adds to the patch file: a
	// strategic merge patch of one resource with only the drifted fields.
	Patch map[string]any
	// Note is a short phrase for the merge request: what the edit changes
	// ("images[nginx].newTag of apps/k/overlays/dev/kustomization.yaml").
	Note string
}
