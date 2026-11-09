# What maps

Mapping is how a drifted field becomes an edit to a file in Git. It runs in every mode, `ReportOnly` included, reads the
repository at the revision Argo CD synced (never the head of the branch), and **never writes to Git by itself**: the result is
`status.mapping` of the `DriftProposal` (the strategy, the files, the edits, a unified `diff`, `verified`), and the `Mapped` condition.

Nothing is guessed. Every candidate edit is applied to a copy of the source files and Argo CD renders the copy; the edit is used
only if the render reproduces the drifted fields exactly and changes nothing else (see
[verification by render](../concepts.md#verification-by-render)). What cannot be shown is `Unmapped`, with a reason.

## By source type

| Source of the Application | What is edited | `status.mapping.strategy` | Details |
|---|---|---|---|
| **Directory** (plain manifests, YAML or JSON) | The single document that defines the resource, in place: only the bytes of the changed `key: value` entry are written again, so comments, quoting and the other lines stay. `directory.recurse`, `include` and `exclude` are honoured. | `Direct` | [JSON files](json.md); [Limitations](../reference/limitations.md) |
| **Kustomize** (an overlay in Git) | The file or setting that decides the field, found through the closed set of files the kustomization reads, in five tiers: a setting of the overlay (`images`, `replicas`, labels), a value in a patch it already has, the resource file in its path, a shared base (only with `spec.kustomize.allowBaseEdits`), or a new patch (`allowNewPatches`). | `KustomizePatch` | [Kustomize](kustomize.md) |
| **Helm**, chart in Git | The value file key that feeds the field (also a part of a value, such as an image tag), or a new key in the last value file when only the chart's default holds the old value. | `HelmValues` | [Helm](helm.md) |
| **Helm, several sources**: a chart from a Helm repository or an OCI registry, with its value files in a Git repository (`$values/...`) | The value files of the `ref` source, verified by rendering the edited commit through Argo CD (scratch branches) | `HelmValues` | [Charts from a Helm repository or a registry](multi-source.md) |
| **Values the Application sets itself** (`helm.parameters`, `helm.values`, `kustomize.images`, ...) | Nothing is written: no render can verify a changed option. `status.mapping.application` says which option, where it is defined, and the value to set. | `Unmapped` (`SetByApplication`) | [Values set in the Application](application-values.md) |
| **Plugin**, Jsonnet | Plugin sources are detected, but their proposals end `Unmapped` (`UnsupportedSourceType`); Jsonnet files cannot be edited and Jsonnet output is not read. | `Unmapped` | [Limitations](../reference/limitations.md) |

An Application with `spec.sources` in any shape other than one Helm source plus Git `ref` sources gets one `UnsupportedSource`
Event and no proposals.

## Partial proposals

A proposal is all or nothing by default. With `spec.mapping.allowPartial: true` a policy proposes the fields that can be proven and lists the
others: [Partial proposals](partial-proposals.md).

## What stays Unmapped

A proposal ends `Unmapped` when the operator looked and could not map the change with certainty. The reason is in
`status.mapping.reason` and in the `Mapped` condition; a person decides what to do. The common ones:

- the resource is **not found** in the files, or defined in **more than one place** (`NotFound`, `Ambiguous`);
- no candidate edit **verifies** by render (`NoVerifiedCandidate`, `VerificationFailed`), or more than one does (`AmbiguousCandidates`),
  or the render shows the edit also changes something else (`ValueAffectsOtherFields`, `EditsInteract`);
- the value is **set by the Application** and not by a file (`SetByApplication`);
- the file is **not YAML or JSON**, or the Kustomize file set is **not closed** (a remote base, `helmCharts`, a symbolic link);
- the drifted field is in a **Secret**, whose values Argo CD masks, so no render can verify it (`SecretNotVerifiable`).

Every reason, what it means and what to do about it is in the table of [Troubleshooting](../operations/troubleshooting.md).

A reason that says the operator **could not look** is different: `RepositoryUnavailable`, `RepositoryAuthFailed`,
`RepositoryNotFound`, `ApplicationNotFound` and the Argo CD ones keep the proposal in `Mapping` and are retried, with a delay that
grows to ten minutes, until the problem is gone.
