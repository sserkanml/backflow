# Kustomize sources

For the other sources see [What maps](overview.md).

For an Application with a Kustomize source (an overlay in the Git repository),
Backflow traces a drifted field back to the file or setting that decides it and
proposes the change there. Like Helm, nothing is guessed: an edit is only used
after Argo CD has rendered the edited files and the render reproduces the
drifted fields, and nothing else.

How a proposal is made:

1. **The closed file set.** Backflow follows the Application's `kustomization.yaml`
   through `resources`, `bases`, `components`, patches, generator files and envs,
   `replacements`, `openapi` and `crds` (`kustomize.RelevantFiles`), including
   bases outside the Application's path, and uploads exactly those files to Argo
   CD. A remote base or resource, `helmCharts`, a symbolic link or a reference
   that does not exist makes the set not closed: the proposal is `Unmapped`
   (`KustomizeSourceNotClosed`) and the message says which reference.
2. **Where each resource comes from.** One extra render of a copy whose overlay
   has `buildMetadata: [originAnnotations, transformerAnnotations]` tells which file
   declares each resource and which transformers touched it. This only orders and
   seeds the candidates; the annotations are never written to Git and never used
   as proof, and every verification render uses the unmodified files.
3. **Tiers, tried in this order** (a later tier is only tried when no edit of an
   earlier one verifies):

   | Tier | Where the edit goes | Strategy |
   |---|---|---|
   | 1 | A setting of the Application's own `kustomization.yaml`: `images` (`newName`, `newTag`, `digest`, also when the drift is only a part of the image string), `replicas`, `labels` / `commonLabels` / `commonAnnotations`, and the literals and env-file lines of a generator **without** a name hash | `KustomizePatch` |
   | 2 | A value inside a patch the overlay already has: a strategic merge patch file or inline patch (list items are matched by their merge key: containers and env vars by name, ports by `containerPort`, ...), or a JSON 6902 operation, as a file or as a string (kept in block style, or re-written as compact JSON when it was JSON) | `KustomizePatch` |
   | 3 | The resource file inside the Application's path | `Direct` |
   | 4 | A file outside the path (a shared base or component), **only with `spec.kustomize.allowBaseEdits`** | `Direct` |
   | 5 | A **new patch**: `<overlay>/backflow-patches/<kind>-<name>.yaml` with only the drifted fields (a removed list item as `$patch: delete`, a removed key as `null`), listed under `patches:` in the overlay's `kustomization.yaml`; an existing file of that name is extended and keeps its comments. Only with `spec.kustomize.allowNewPatches` | `KustomizePatch` |

   Comments, key order and quoting of the edited files are kept.
4. The merge request and the commit say what kind of edit each one is, that it was
   verified by rendering, and, for a new patch file, that the file was added. The
   new file and the edited `kustomization.yaml` are in the same commit.

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  kustomize:
    allowBaseEdits: false     # default; true allows edits of files outside the Application's path
    allowNewPatches: true     # default; false leaves what no setting, patch or file holds Unmapped
```

`spec.mapping.allowPartial` and `spec.helm.allowCollateralChanges` apply to
Kustomize sources the same way as to Helm ones.

## Shared bases

A base is usually shared by several environments, so editing it changes them too.
Backflow works out which other Applications build a file outside the Application's
path (the files each Application of **any** BackflowPolicy reads):

- If **no other Application** builds the file, tier 4 comes before tier 5.
- If **others do**, a new patch (tier 5) is tried first; the shared file is edited
  only when that cannot be verified or `allowNewPatches` is false. The merge request and
  the status list the others ("Merging this also changes: X, Y at their next sync").
- Applications that no policy covers cannot be known, so every merge request that
  edits a file outside the path says that other Applications outside Backflow's
  policies may use it.

## What is Unmapped, and why

- **Hashed generators.** `configMapGenerator` / `secretGenerator` without
  `disableNameSuffixHash` put a hash of the data in the name. Editing the data
  changes the name and every field that refers to it, so the proposal is
  `ValueAffectsOtherFields` and the message says so. Set `disableNameSuffixHash` (in
  `generatorOptions` or on the generator) to map the data.
- **Options of the Application.** What `spec.source.kustomize` sets (`images`,
  `replicas`, `namePrefix`, `nameSuffix`, `namespace`, `commonLabels`,
  `commonAnnotations`, `patches`) is applied by Argo CD on top of Git, so a change of
  Git cannot change it: `SetByApplication`, naming the option and where it is defined
  (see [Values set in the Application](application-values.md)).
- **Remote bases, `helmCharts`, plugins and KRM functions:** the output is not a
  function of the files Backflow can edit and render.
- **Secrets**, as for Helm (`SecretNotVerifiable`).
- A value no setting, patch or file holds, with `allowNewPatches: false`
  (`NoVerifiedCandidate`); the message names a file that holds the value but is
  outside the path while `allowBaseEdits` is off.
- Changing the **merge key** of a list item (a Service's `port`, a container's `name`)
  cannot be expressed as a patch; it needs `allowBaseEdits` to edit the file itself.

Git-ahead works as for Helm: a commit to any file the kustomization builds from, in
or outside the path, pauses detection for the Application until it is synced; a
commit to another file does not.

The render needs Argo CD's Kustomize (v5.8.1 at the time of writing); `make coverage`
and the render tests use the same version, installed by `make kustomize`.
`make test` installs it too, and with `CI=true` a missing binary fails the tests
instead of skipping them. `test/coverage/REPORT.md` has the coverage by source type.
