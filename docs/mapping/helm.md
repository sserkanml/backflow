# Helm sources

Backflow's mapping of a drift of an Application whose chart is in Git. For the other sources see [What maps](overview.md).

For an Application with a Helm source (a chart in the Git repository), Backflow
traces a drifted value back to the **value file** that feeds it and proposes
the change to that file. It does not guess: a candidate key is only used after
Argo CD itself has rendered the chart with the edit.

How a proposal is made:

1. Backflow finds the keys in the Application's value files (`helm.valueFiles`,
   in Argo CD's order and with its globbing, `$ARGOCD_APP_*` variables and
   `ignoreMissingValueFiles`) whose effective value equals the value Git had
   for the drifted field. A key that a later file overrides is not a candidate.
   When no edit of such a key verifies, Backflow also tries the keys that hold
   a **part** of the value: after `kubectl set image` an image
   `nginxinc/nginx-unprivileged:1.27-alpine` became `...:1.28-alpine`, and the
   key `image.tag` holds `1.27-alpine`. What changed is the part of the old
   string that is left after the common beginning and end of the two strings
   are taken away; a key whose value occurs in the old string and covers that
   whole part is a candidate, and its new value is the same stretch cut out of
   the new string (so a registry change finds `image.registry`, and a value that
   occurs twice gives two candidates). The keys that hold the whole value are
   always tried first.
2. It renders the chart through Argo CD (`GetManifestsWithFiles`) with the files
   of the proposal's revision, once for the unmodified files (twice, to find
   fields that change on every render) and once for each candidate edit.
3. An edit is accepted only when every field it changes, in **all** rendered
   resources, is one of the drifted fields and now has its live value. When
   several drifted fields are fed by the same key, one edit covers all of them;
   when the fields need different keys, the edits are combined and rendered once
   more, and the result must differ from the unmodified render by exactly the
   drifted fields. Edits in several value files go into one commit.
4. The merge request and the commit message list the value file, each key path
   with its old and new value, and say that the edit was verified by rendering
   through Argo CD.

**Charts with `file://` dependencies are rendered faster.** When a chart depends on
another chart in the repository (`repository: file://../common`), Argo CD's repo server
renders what it receives with `helm template` and, only when Helm says a dependency is
"missing in charts/", runs `helm dependency build` and renders again. An upload is a
fresh directory every time, so it builds on every render. Backflow therefore uploads the
dependencies already unpacked under `charts/<chart name>/` (aliases stay in the parent
`Chart.yaml`; a dependency that is already in `charts/` as a directory or a `.tgz` is
never added a second time; remote dependencies are left alone, and a chart with a remote
dependency that is not in `charts/` is rendered as before). A render went from about
150 ms to about 65 ms on a kind cluster with Argo CD v3.5.3.

It never changes a result. The first render of a chart (identified by the content of
the chart and its `file://` dependencies, not of its value files, and by the version of
Argo CD) is made both ways and the resources are compared; the quick way is used from
then on only if they are identical. Otherwise the render stays the one Argo CD does by
itself, the reason is logged once, and the decision is kept in memory. For example a
dependency of a dependency that is not committed in its `charts/` is built by Argo CD
without its own dependency (Helm does not build those), so the unpacked set would
render more than Argo CD does, and it is not used. `--unpack-file-dependencies=false`
(chart value `unpackFileDependencies: false`) switches the whole thing off. The metrics
`backflow_renders_total` and `backflow_render_duration_seconds` have a `deps` label
(`none`, `built`, `unpacked`), and `backflow_deps_equivalence_checks_total` counts the
checks (`identical`, `different`, `error`).

**Hooks and names chosen at random.** Resources with a `helm.sh/hook` or
`argocd.argoproj.io/hook` annotation (a chart's test Pods, `PreSync` Jobs) are not
what the Application manages, so verification leaves them out of every comparison,
for Helm, Kustomize, Directory and multi-source Applications alike. A resource that
is not a hook but is named at random by the chart (`randAlphaNum` in its name) is
found by the two renders of the unmodified files: a resource that exists in one
render only is a name that is not stable, and adding or removing a resource of its
kind in that namespace is not counted as a change. The two renders are real renders:
for a multi-source Application, whose renders Argo CD answers from its manifest cache
for a revision it has rendered before, they are asked for with `noCache=true`, so the
cache cannot make them one (no scratch branch is needed for them). Uploaded files
(single-source Helm and Kustomize) are never cached.

Like Directory proposals, a Helm proposal is made against the head of the target
branch. When the head is the revision the proposal was mapped at, or only files
the mapping does not depend on changed since (the files a render reads for Helm,
everything below the directory for a Directory source), the mapping is reused
and nothing is rendered again; the operator log says which ("Reusing the mapping"
or "Mapping again", with the reason). Otherwise the head is mapped again, and
when it no longer renders the old values the proposal is `Superseded`.

When the change cannot be traced back with certainty the proposal is `Unmapped`,
and the `Mapped` condition says why (`NoVerifiedCandidate`,
`AmbiguousCandidates`, `EditsInteract`, `RenderMismatch`,
`NondeterministicField`, `SecretNotVerifiable`, `TooManyCandidates`,
`RenderBudgetExceeded`, `RenderFailed`, `SymlinkedValueFile`, `MissingValueFile`
and others). When a key reproduces the drifted field but also changes other
fields, the reason is `ValueAffectsOtherFields`, even if other keys failed for
other reasons: the message names the nearest key with the fields it would move
(path, current value, value after the edit) and says how many other candidates
were rejected, so a person can decide.

Argo CD must allow the `applications, get` permission to Backflow's account (the
read-only account in the install steps has it) and must be reachable over
**HTTP/2**: the render is a gRPC call on the same port as the API, so a proxy
that only speaks HTTP/1.1 in front of Argo CD makes mapping wait and retry
(`ArgoCDUnreachable`). Nothing is applied to the cluster.

## Defaults that only the chart defines

An environment often changes a value it never set: the replica count of a chart
that nobody overrides, a timeout, the pull policy. The old value then comes only
from the chart's own defaults (its `values.yaml`, or that of a dependency under
its name or alias, and `global.*`), and that file is not the Application's
configuration, so Backflow does not edit it. Instead it **adds an override**: the
key, with the live value, is appended to the Application's last value file (the
one with the highest precedence), where it wins over the defaults.

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  helm:
    allowValueOverrides: true        # the default; false leaves such drift Unmapped
```

- The key goes to the last value file of the Application, or to the file named in
  `spec.helm.targetValuesFile`. That file must be one of the Application's
  resolved value files (a repository path, relative to the chart directory, or
  as the Application writes it); otherwise the proposal stays `Unmapped` and the
  message says which file was named and which ones the Application uses.
- Missing intermediate maps are created, an existing parent map gets the entry
  appended in its own indentation, and the rest of the file (comments, quoting,
  key order) is not touched. The new value keeps the type of the default.
- Edits of keys a value file already defines are always tried first; an override
  is only tried when none of them verifies. It is verified by rendering through
  Argo CD like any other edit: it must change exactly the drifted fields, and the
  collateral and partial rules apply unchanged.
- Whole values and parts of a value (the image tag of `repository:tag`) both
  work. A key under a list in the defaults is out of scope: a list is replaced
  as a whole, and the proposal stays `Unmapped` with that reason.
- A key that the Application sets inline (`helm.values`, `helm.parameters`, ...)
  stays `Unmapped` (`SetByApplication`; see [Values set in the Application](application-values.md), which says
  where it is defined); so does an Application without any value file.
- The merge request body has an **Overrides a chart default** table (value file,
  key added, new value, chart default, and the file that defines it), the commit
  message says the same, and the table of Helm values shows the old value as
  `(not set; chart default ...)`.

## Maps and lists

Annotations, labels, tolerations, a node selector, env vars and resources are
maps and lists in the rendered resource, and the value files hold them under
keys such as `podAnnotations`, `tolerations`, `extraEnvVars` or `resources`.
Which key feeds which field is written in the templates, not in the values, so
Backflow compares: it merges the layers of values the way Helm does, and looks
for keys whose content fits what the resource has in Git (its base render).
Every candidate is then verified by a render, exactly like a scalar edit: it
must produce the live state of the field and change nothing else.

What is found:

- **A key that holds what Git renders**: `tolerations` that equals the rendered
  list is set to the live list.
- **A stretch of a longer list**: a chart builds `env` from entries of its own
  and the user's `extraEnvVars`. The values list is the stretch of the rendered
  list it feeds (an empty one sits at the end or at the start), and the edit
  changes only that stretch: adding or removing an env var edits `extraEnvVars`.
- **A key added to or removed from a map**: a pod annotation or label, a node
  selector. The key is added to the values map that is a part of the rendered
  map (`podAnnotations`, `podLabels`, `nodeSelector`), or removed from it when a
  value file defines it.
- **An empty key that takes the place of what the chart generates**: `resources:
  {}` next to `resourcesPreset`. A drifted limit is brought to the live state by
  setting the whole `resources` map to what is live.

Where the key goes follows the scalar rules: edits of keys a value file already
defines come first; a key that only the chart's defaults hold is an override in
the Application's last value file (or `spec.helm.targetValuesFile`), which
needs `spec.helm.allowValueOverrides`. Maps and lists are written in block style, the
rest of the file (comments, order, siblings, quoting) stays, and the result is
checked to parse back to exactly the intended value. Strings that YAML 1.1
reads as other types (`yes`, `no`, `on`, `y`, `0777`, `1:30`) are quoted,
because Helm parses with a 1.1 parser.

Names decide which keys are rendered first, never which one is right: a key is
a candidate when its name fits the field (the words of the field and of the
resource's name, and `count` for replicas), and a field with more than ten
candidates is narrowed to the best-fitting ones only when that leaves a clear
tier. The keys that are left out are not forgotten, because a proposal is only
mapped when no other key could have worked too:

- a key that no template, helper or value file of the chart (and of its
  dependencies, packaged ones included) reads cannot be the one, and is dropped;
- when an edit verifies, the other keys that a template may read and that
  compete with it are rendered too, up to 30 renders and within the render
  budget; if one of them verifies as well, the proposal is `AmbiguousCandidates`;
- if there are more than can be rendered, or the render budget runs out, the
  proposal is `TooManyCandidates` and names the best keys that were not tried.

If two edits verify and give different files, the proposal is
`AmbiguousCandidates` however the keys were found; only edits that write the
same bytes to the same files are one mapping.

Limits:

- **Hardcoded in a template.** A list or map that a template writes itself, with
  no value behind it, has no key to edit (`NoVerifiedCandidate`).
- **Entries a helper generates.** An env var, annotation or label that the
  chart generates cannot be changed from the values: only the user's part of a
  list or map is editable.
- **A value that feeds more than the drifted field.** The same `containerPorts`
  key may feed a NetworkPolicy, or `master.count` the env of the replicas: the
  proposal is `Unmapped` with `ValueAffectsOtherFields`, naming what else moves.
- **A key removed that only the defaults define** cannot be removed from the
  Application; setting it to null is not proposed.
- **Ambiguous subtrees.** Two keys that both reproduce the change and give
  different files are never picked between.

## Label and annotation changes beside an edit

A chart often puts one value in more than one place: the image tag is also the
`app.kubernetes.io/version` label of the Deployment and of its pods. Editing the
tag then changes more than the field that drifted, and by default such an edit
is not accepted: the proposal is `Unmapped`, and its message names the nearest
edit and the other fields it would change. A policy can accept this for labels
and annotations:

```yaml
apiVersion: backflow.io/v1
kind: BackflowPolicy
spec:
  helm:
    allowCollateralChanges: [Labels]        # and/or Annotations
```

With it, an edit may also change, on any rendered resource,
`metadata.labels` and `spec.template.metadata.labels` (`Labels`), and
`metadata.annotations` and `spec.template.metadata.annotations`
(`Annotations`); for a CronJob also the pod template of its jobs,
`spec.jobTemplate.spec.template.metadata.labels` and `.annotations`. The edit is still proven by rendering through Argo CD; the
strict rule (exactly the drifted fields) is tried first, and this is used only
when it finds nothing. **Never allowed, whatever the policy says:** any selector
(`spec.selector`, its `matchLabels` and `matchExpressions`, a Service's
`spec.selector`), because changing one can orphan the pods of a workload, and a
change to a field that drifted but does not end at its live value. A tag that
also feeds a selector label is therefore always `Unmapped`.

The changes that were accepted are recorded in `status.mapping.collateral` and
listed in the merge request body and in the commit message ("This edit also
changes: ..."), each with the resource, the path, the value now and the value
after the edit, so the reviewer sees them before merging.

Limits of Helm mapping:

- **Live-only additions are not reported by Argo CD.** A field that exists
  only in the live object (added with `kubectl edit` or `patch` and absent from
  Git) is not detected on its own, because Argo CD's diff preserves it. Such an
  addition is mapped when the same resource also has a reported drift. Removals
  and value changes are always detected.
- **Chart in Git, or a chart from a Helm repository or a registry with its
  value files in a Git ref source.** A chart from a Helm repository or an OCI
  registry with its values set only in the Application is `Unmapped`
  (`UnsupportedSourceType`); see the next section for the supported shape with
  several sources. Value files that are URLs are not edited; Backflow reports
  that a value may come from one.
- **The chart's own `values.yaml` is never edited.** A value that exists only
  there is overridden in the Application's last value file (see above); with
  `spec.helm.allowValueOverrides: false` it is `Unmapped` (`NoVerifiedCandidate`,
  "chart's own defaults"), as are keys under a list in the defaults.
- **Inline settings win and are not edited.** A key set by `helm.values`,
  `helm.valuesObject`, `helm.parameters` or `helm.fileParameters` on the
  Application overrides any value file, and that setting lives in the Argo CD
  Application, not in the chart repository. The proposal is `Unmapped` and
  says which layer sets it.
- **Derived fields.** Only values that equal the old value are candidates, so a
  field a template computes (`upper`, `printf`, a sum) has no candidate of its
  own. An edit of the key it derives from is accepted only when every field it
  moves is a drifted field; if the template also moves one that did not drift
  (a version label that follows `image.tag`), the proposal is `Unmapped` with
  that near miss. The reason is that merging the edit would make Git disagree
  with the cluster on a field nobody changed. Labels and annotations that follow
  the value can be accepted by the policy, see below.
- **Secret resources are never proposed.** Argo CD masks the data of Secrets,
  and a Secret value must not be put into Git (`SecretNotVerifiable`).
- **Fields that change on every render** (`randAlphaNum`, `now`) cannot be
  verified (`NondeterministicField`); resources with a random name are ignored
  when comparing renders.
- **Values keep their type.** An integer stays an integer, a boolean a boolean,
  a quoted string stays quoted, when the live value reads as that type and
  writing it that way renders back to exactly the same string; otherwise it is
  written as a quoted string. A tag `1.10` in a file is a number that renders
  as `1.1`, so the new tag `1.20` is written as `"1.20"`, not as `1.20`, which
  would render as `1.2`. YAML anchors and merge keys are read, but
  a key that exists only through a merge key (`<<: *base`) cannot be edited. A
  list is replaced by a later file as a whole, as Helm does; an item is added
  to or removed from a list by setting the list (see Maps and lists).
- **Bounded work.** At most 10 candidate keys per field and two minutes of
  rendering per proposal. A symbolic link in the chart, a value file or a
  `file://` dependency makes the Application unmappable and pauses detection
  while Git is ahead.
- **Argo CD renders from its own settings.** Backflow sends the files, not the
  Application: Helm version, `helm.releaseName`, `kubeVersion` and API versions
  are those of Argo CD. A `.argocd-source*.yaml` file in the chart directory is
  not modelled (`ArgoSourceOverrides`).
