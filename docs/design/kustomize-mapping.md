# Kustomize mapping (roadmap B4)

Status: implemented (`internal/kustomize`, wired in `driftproposal_kustomize.go`).
The spike findings below are unchanged; the design section was adjusted where the
implementation differs, see "As implemented".

Backflow maps a live change of a Kustomize Application to the file that owns
it the way it does for Helm: it edits a copy of the repository files, renders
the copy through Argo CD (`GetManifestsWithFiles`) and accepts the edit only
when the render reproduces the live change exactly.

## Spike findings

Run against Argo CD v3.5.3 on the kind cluster, with a throwaway Application
(`path: apps/k/overlays/dev`) and files that were not in Git. Argo CD uses
Kustomize **v5.8.1** (`kustomize version` in the repo-server).

### a. Files outside the Application path

`GetManifestsWithFiles` extracts the uploaded archive as the repository root
and runs `kustomize build` in `<root>/<spec.source.path>`. The Application's
repository is not consulted for the content, so the files may be anything.

- An overlay with `resources: [../../base]` renders; the base lives outside
  the Application path (`apps/k/base`) and is read from the upload.
- Layout: **repository-relative paths**, exactly as in Git (`apps/k/base/...`),
  so `../..` from the overlay resolves inside the archive. Nothing outside the
  archive exists: a base that is not uploaded fails with
  `unable to find one of 'kustomization.yaml' ... in directory` (a render
  failure, not a silent skip). A file set therefore has to be **closed**: every
  local base, component, patch file, generator file and so on reachable from
  the overlay's `kustomization.yaml` must be in the archive.
- The result has `sourceType: Kustomize`. The type is taken from the
  `kustomization.yaml` in the uploaded path.

### b. Build metadata

`buildMetadata: [originAnnotations, transformerAnnotations]` added to the
uploaded overlay `kustomization.yaml` survives Argo CD's render (also when the
Application has `kustomize` options, which Argo CD applies with
`kustomize edit` on the same copy). What comes out:

- `config.kubernetes.io/origin` on every resource: `path: ../../base/deployment.yaml`.
  The path is **relative to the Application's kustomization directory**, not to
  the repository root; it must be joined with the Application path. A resource
  declared in the overlay itself has `path: local.yaml`. A generated resource
  has `configuredIn: kustomization.yaml` + `configuredBy: {kind: ConfigMapGenerator}`
  and no file path (its name carries a content hash, `gen-cf8t4ck7kk`).
- `alpha.config.kubernetes.io/transformations`: a list of
  `configuredIn` + `configuredBy.kind` per transformer that touched the
  resource (`PatchTransformer`, `PrefixTransformer`, `ImageTagTransformer`).
  It does **not** say which patch or which field, and a component's patch is
  listed on resources it did not change. It is a hint for where to look, never
  proof.
- Argo CD's own `argocd.argoproj.io/tracking-id` annotation is added next to
  them. Both metadata annotations are removed before comparing a render with
  the live object.

### c. Application-level `spec.source.kustomize` options

Argo CD applies them on top of the repository's kustomization, so they win:

| Option | Observed |
|---|---|
| `namePrefix` | applied to every name |
| `images: [nginx=nginx:9.9]` | replaces the overlay's `images[].newTag` (render has `nginx:9.9`, overlay says `1.26`) |
| `commonLabels` | added to metadata, selectors and pod template labels |
| `patches` | applied after the overlay's own patches (overlay `replicas: 3`, Application patch `7` renders 7) |

Consequence: a field that an Application-level option sets cannot be changed
by editing Git. A candidate for it would verify only if the render changes,
which it does not, so such a field is `Unmapped`, with the reason that names
the Application option. The renders already include the options, because the
Application object in the cluster is what Argo CD reads.

## Design

### Origin

Locate the owner of a field with one extra render of a copy whose overlay
`kustomization.yaml` has `buildMetadata: [originAnnotations,
transformerAnnotations]` (a locate render). The annotations are read, never
written to Git, and **only the locate render has them**: baseline and
verification renders use the unmodified kustomization, so they are the
renders Argo CD does. Origin is a candidate source, not a verdict: every edit
is still verified by rendering.

### Candidate order

1. Settings that already exist in the overlay: `images` (`newTag`, `newName`,
   `digest`), `replicas`, values inside existing strategic-merge or JSON 6902
   patches, inline `patches`.
2. Files inside the Application's own path (the resource file named by the
   origin, a patch file, a generator file).
3. Shared bases and components outside the path, only when
   `spec.kustomize.editBases` is true (default false): a change there affects
   every Application that uses the base.
4. A new strategic-merge patch in the overlay, when `spec.kustomize.addPatches`
   is true (default true). The patch file is added to the overlay and listed in
   its `kustomization.yaml`.

### Verification

The same rules as Helm, shared in `internal/rendermap`: baseline rendered
twice with nondeterministic fields masked, the edit set must reproduce the
drift exactly, edit sets that give byte-identical files are one mapping, two
that give different files are `AmbiguousCandidates`, untried candidates are
tracked and confirmed by renders, Secrets stay `Unmapped`, partial results
follow `allowPartial`.

### Unmapped, with a reason

- Remote bases and resources (URLs, `github.com/...//`): not closed, not
  readable offline.
- Fields set by Application-level kustomize options (see c).
- Hashed generators: the render changes the generated name, so the edit also
  changes other fields (`ValueAffectsOtherFields`).
- Plugins, KRM functions, `helmCharts`: the output is not a function of the
  files we can edit and render.

### File closure and Git-ahead

`kustomize.RelevantFiles` returns the files a Kustomize source reads
(recursively through `resources`, `bases`, `components`, patches, generator
files and envs, `replacements`, `openapi`, `crds`) and whether the set is
closed. It is used by the render upload and by the Git-ahead check, so a
commit that touches a file outside the Application path but inside the closure
is seen as a change of the source.

## As implemented

Differences from the design above, and what was added:

- **Candidate order has five tiers**, in `internal/rendermap` as `Found.Later`:
  (1) settings of the Application's kustomization, (2) values inside existing patches,
  (3) the resource file in the path, (4) files outside the path with `editBases`,
  (5) a new Backflow patch with `addPatches`. A later tier is rendered only when no
  edit of an earlier one verifies, so a value held by both a patch and the base is
  edited in the patch and never ambiguous.
- **Shared bases.** A tier 4 file that another Application of any BackflowPolicy builds
  is tried after tier 5 (`Policy.OtherUsers`, computed from the `RelevantFiles` of the
  Applications in the policies' status). A file nobody else builds stays before tier 5.
  Applications outside every policy cannot be known; the text says so.
- **Components and shared files outside the path** are tier 4 whatever they hold
  (patch, setting or resource), because editing them is editing a shared file.
- **Application options** (`spec.source.kustomize`) are parsed (`kustomize.Options`)
  and a field they set is blocked before any render: reason `SetByApplication`, naming
  the option. The renderer used by the tests applies them with `kustomize edit`.
- **Hashed generators** are blocked with `ValueAffectsOtherFields` (the message names
  the name hash); unhashed ones are tier 1 (literals, env-file lines).
- **Origin is lost** when a patch replaces `metadata.annotations`. The resource file
  is then unknown (no tier 3/4 candidate) and a new patch is tried for the rendered
  name and the name without the prefixes and suffixes of the kustomizations.
- **New patches keep list order.** kustomize places the items of a patch first, so the
  patch lists the items before the changed one by merge key. A whole-list replacement
  (a removed env var) is a patch with the items of the live list plus `$patch: delete`
  for the missing ones; a list without a merge key (tolerations) is the whole live list.
- **Writers** build on `mapping.Apply`; a replaced scalar is replaced in place (which
  keeps the layout of lists), inline patches are rewritten inside their block scalar,
  JSON 6902 patches that are JSON strings are re-written as compact JSON. The Helm YAML
  editor was not moved: it is tied to Helm's value parsing.
- **Engine hooks** added for this: `Candidate.Variant/Meta/NewFile`, `Written.Also`
  (a new patch file is listed in the kustomization), `ContextWriter` (the writer sees
  the files of the set so far), `Outcome.Applied`, `Found.Blocked`, `ErrRenderFailed`
  worded for any source.
- **Git.** `gitrepo.CommitMeta.Create` lists the files a commit may add; the controller
  passes the new patch file, so it and the edited `kustomization.yaml` are one commit.
- **Fingerprint** of a mapping includes `spec.kustomize` of the policy and the whole
  `spec.source` of the Application (so `spec.source.kustomize` too). At a moved head the
  mapping is reused when none of the files the build reads (at either revision) changed.
