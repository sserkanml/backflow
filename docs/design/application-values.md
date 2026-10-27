# Values set in the Application manifest (roadmap B6)

Status: design, after a spike. No production code yet.

Goal: drift whose value is set in the Argo CD Application itself, not in a file
the source reads, is `Unmapped` today:

- Helm: `spec.source(s).helm.parameters`, `values`, `valuesObject`
  (`fileParameters` are files in Git and are already mapped);
- Kustomize: `spec.source.kustomize.images`, `replicas`, `namePrefix`,
  `nameSuffix`, `namespace`, `commonLabels`, `commonAnnotations`, `patches`
  (`SetByApplication`).

The Application is itself a manifest. In an app-of-apps layout it is a
resource of a *parent* Application whose source is in Git, so the change can
be proposed to the parent's Git file. Backflow's rule stays: **an edit is
proposed only when an Argo CD render proves it**, and Backflow never writes to
the cluster (no temporary Applications, no spec updates, no sync operations).

## Spike

Argo CD v3.5.3 on the kind cluster of `./hack/dev-up.sh`, with the admin
session of that throwaway cluster (not the Backflow account; its token and the
admin token were only ever read from a file, never printed or logged). A
throwaway AppProject `spike`, throwaway Applications and an ApplicationSet,
and a `git` smart-HTTP pod in the cluster as the parent's repository (pushing
to gitlab.com needs the GitLab token, which the spike does not use). Everything
was deleted afterwards (`kubectl -n argocd get app` shows the demo
Applications only). The tooling was two small Go programs using
`internal/argocd` and the engines of this repository (one uploads files to
`GetManifestsWithFiles`, one runs the parent mappings of S3); they are not
committed.

### S1. Is there any API path that renders a modified spec without a write?

**No.** Argo CD publishes 82 REST/gRPC-gateway paths (`GET /swagger.json`). The
ones that render, diff, or preview, with what each takes:

| API | Request fields | Can it override helm/kustomize options? | Writes? | RBAC |
|---|---|---|---|---|
| `GET /applications/{name}/manifests` (`GetManifests`) | `revision`, `revisions[]`, `sourcePositions[]`, `appNamespace`, `project`, `noCache` | No. The spec is the stored one. | No | `applications, get` |
| `POST /applications/manifestsWithFiles` (`GetManifestsWithFiles`, chunked stream) | `query{name, appNamespace, project, checksum}` + the tar of files | No. Files are replaced, the spec is the stored one (this is what Backflow uses). | No | `applications, get` |
| `GET /applications/{name}/server-side-diff` | `appNamespace`, `project`, `targetManifests[]` | No render at all: it dry-run-applies manifests *you supply* and diffs against live. | No (K8s server-side dry run) | `applications, get` |
| `POST /applications/{name}/sync` with `dryRun` | `dryRun`, `revision(s)`, `sourcePositions`, `manifests[]`, `resources`, `syncOptions` | No source override. With `manifests[]` the render is *skipped* and your manifests are used verbatim. | **Yes**: sets `operation` and `status.operationState` on the Application | `applications, sync` |
| `POST /repositories/{repoURL}/appdetails` (`GetAppDetails`) | `source` (a full `ApplicationSource`), `appName`, `appProject`, `sourceIndex` | Accepts the options in the body but refuses a source that is not the Application's own (`permission denied`); and returns chart facts (value files, flattened default parameters), never manifests. | No | `applications, get` (+ `repositories, get`) |
| `POST /applicationsets/generate` | `applicationSet` (a full ApplicationSet) | Not a render: returns the *Application specs* an ApplicationSet would generate. | No | **`applicationsets, create`** |
| `POST /applications?validate=` / `PUT` / `PATCH` | an Application | A write. `validate` only checks the spec. | Yes | create/update |

Evidence (admin session, secrets redacted; output trimmed):

```text
# GetManifests ignores every attempt to override the source. demo-helm has
# helm.parameters[config.feature]=beta in its spec.
GET /api/v1/applications/demo-helm/manifests
  -> demo-helm-config FEATURE=beta
GET /api/v1/applications/demo-helm/manifests?helm.parameters=config.feature%3Dx
    &source.helm.parameters.0.value=x&spec.source.helm.parameters.0.value=x
  -> demo-helm-config FEATURE=beta            (unknown query parameters are dropped)
```

```text
# GetManifestsWithFiles query message (swagger):
applicationApplicationManifestQueryWithFiles: appNamespace, checksum, name, project
```

```text
# AppDetails with a source that differs from the Application's own:
POST /api/v1/repositories/<repo>/appdetails  {source: {... parameters: [OVERRIDDEN]}}
  -> {"error":"permission denied","code":7}
# with exactly the Application's own source:
  -> {"type":"Helm","helm":{"valueFiles":["values-dev.yaml","values-prod.yaml","values.yaml"],
      "parameters":[{"name":"image.repository",...},{"name":"config.logLevel","value":"debug"},...]}}
     (no "manifests")
```

```text
# Sync dryRun writes to the Application resource.
before: resourceVersion=39795 operation=absent status.operationState=absent
POST /api/v1/applications/spike-sync/sync {"dryRun":true}
after : resourceVersion=39807 status.operationState.phase=Succeeded
        status.operationState.operation.sync.dryRun=true
# with "manifests":[<one ConfigMap>]: syncResult lists only that ConfigMap,
# "configmap/only-this created (dry run)": the source was not rendered.
```

```text
# ApplicationSet generate: a dry run that writes nothing and returns specs only.
POST /api/v1/applicationsets/generate {list generator [dev,prod], helm.parameters: "{{color}}"}
  -> spike-dev  parameters=[config.feature=red]    spike-prod parameters=[config.feature=blue]
kubectl -n argocd get app | wc -l   -> 8 before, 8 after
# With the Backflow account (applications get, repositories get):
GetManifests 200, appdetails (own source) 200,
generate 403 "applicationsets, create", sync dryRun 403 "applications, sync"
```

Other doors, and why they are closed:

- **`Sync` operation `source`/`sources`.** The `SyncOperation` of an Application
  can carry its own source (that is how rollback works), and its dry run would
  render that source. The REST `SyncRequest` has no such field, so the only way
  to set it is to write the Application's `operation`: a write to the cluster.
  Even then the result is a list of resource results ("configured (dry run)"),
  not manifests.
- **The repo-server's `GenerateManifest`.** It takes a full `ManifestRequest`
  and would render any source. It is an internal gRPC service on :8081; the
  shipped NetworkPolicy lets in only `argocd-server`,
  `argocd-application-controller`, `argocd-notifications-controller` and
  `argocd-applicationset-controller`, it is unauthenticated (it trusts the
  network), needs the repository credentials in the request, and its protobufs
  are Argo CD's (this repository has no Argo CD module dependency by design).
  Not an option.
- **Source hydrator (3.x).** `spec.sourceHydrator` makes the controller render
  to a branch of the Git repository: a write to Git and to the cluster, with no
  preview call.
- **A Kubernetes server-side dry run of the Application.** It validates the
  CRD; nothing renders.

**Side finding: `GetManifests` has `noCache`.** Without it the two renders of
the same revision are one cached render; with `noCache=true` each is real
(demo-multi, three random-named test Pods, four calls):

```text
no flag      : grpc-test-htkyp jwt-test-d7urz service-test-rjoi0
no flag      : grpc-test-htkyp jwt-test-d7urz service-test-rjoi0     <- cached, identical
noCache=true : grpc-test-fuymo jwt-test-bqwbk service-test-yrqht
noCache=true : grpc-test-msx5v jwt-test-fky8f service-test-ygehg     <- real renders
```

That is a simpler fix than the marker commit `scratch.RenderFresh` used to make,
and it was adopted afterwards (see "Hooks and the manifest cache" in
`multi-source-mapping.md`): `GetManifestsByRevisions` sends `noCache=true` for
the two renders of the unmodified files.

### S2. Helm precedence as Argo CD applies it

Rendered through `GetManifestsWithFiles` against a stored Application whose
source had all layers set, with an uploaded value file that sets the same keys
(the chart is the coverage suite's podinfo-like chart):

```text
uploaded values-prod.yaml : replicaCount 9, ui.message from-file,  ui.color #999999
helm.values (inline)      : replicaCount 3, ui.message from-values, ui.color #111111
helm.parameters           : ui.message from-parameter
-> replicas 3, PODINFO_UI_MESSAGE=from-parameter, PODINFO_UI_COLOR=#111111

+ helm.fileParameters {ui.message: msg.txt}  (uploaded msg.txt = from-fileparameter)
-> PODINFO_UI_MESSAGE=from-fileparameter     (beats the parameter)
- parameters removed -> PODINFO_UI_MESSAGE=from-fileparameter
- fileParameters and values removed -> replicas 9, from-file, #999999

helm.values "from-values" + helm.valuesObject "from-valuesobject"
-> PODINFO_UI_MESSAGE=from-valuesobject      (valuesObject wins over values)
```

So: **valueFiles < values < valuesObject < parameters < fileParameters.** An
uploaded value file never overrides `values`, `valuesObject` or a parameter:
the Application's own layers win whatever the files say. The only *file* that
sits on top is the one a `fileParameters` entry names, and that file is
uploaded like any other, so an edit of it verifies (already supported by
`CollectFiles`/`FieldsFor`). This is why `helm.parameters` today ends in
`NoVerifiedCandidate` with "the value is set by helm.parameters at <key> in the
Application, not by a value file" (`helmmap.Result.Explain`).

For Kustomize the stored options also win over the repository's
kustomization (`kustomize-mapping.md`, spike c).

### S3. From the child Application to the parent's Git file

**Finding the parent.** A real app-of-apps (parent `spike-parent`, Directory
source `apps/children` in a Git repo served inside the cluster) gave:

```text
child-web  metadata.annotations:
  argocd.argoproj.io/tracking-id: spike-parent:argoproj.io/Application:argocd/child-web
  (no labels: the annotation tracking method, the default of v3)
spike-parent status.resources: argoproj.io/Application argocd/child-web Synced
```

The id is `<parent app>:<group>/<kind>:<namespace>/<name>`; for a parent in a
namespace other than the controller's it is `<namespace>_<name>` (the documented
format; only the same-namespace form was exercised). With
`application.resourceTrackingMethod: label` the owner is in
`app.kubernetes.io/instance` instead (not exercised here; the documented
format). Backflow reads the child (it has it already, as the Application of
the proposal), parses the annotation, reads the parent Application and checks
that the parent's `status.resources` lists the child (a stale tracking-id on a
child that someone detached is then caught).

**ApplicationSet children.** A child generated by an ApplicationSet has an
`ownerReferences` entry (`kind: ApplicationSet`, `controller: true`) and **no
tracking-id and no labels**:

```text
set-dev ownerRefs=[{"kind":"ApplicationSet","name":"spike-set","controller":true,...}]
        annotations=  labels=
```

**Can the existing engine map a change of the child Application resource to
the parent's file, verified by rendering the parent?** Yes, for all three
parent source types, with no change to the engine. A synthetic change
`/spec/source/helm/parameters/0/value: "beta" -> "gamma"` on the resource
`argoproj.io/Application argocd/child-gen`, against stored parent Applications
in the cluster and uploaded files, through the real `ArgoRenderer`:

```text
Helm parent (child generated by a template; helmmap.FieldsFor + Verify): MAPPED, 3 renders
  charts/parentchart/values.yaml      -feature: beta   +feature: gamma
Kustomize parent (child in a base resource file; kustomize.Locate + Verify, editBases): MAPPED, 3 renders
  parent/base/child.yaml              -value: beta     +value: gamma
Directory parent (mapping.Map, no render needed): MAPPED
  apps/children/child-gen.yaml        -value: beta     +value: gamma
```

The verification is Argo CD's: the parent render with the edit changes exactly
the child's `parameters[0].value` to `gamma` and nothing else. For the Helm
parent the key found is the parent chart's own `feature`, by the usual
candidates-by-value search plus render.

What this proves, and what it does not: it proves the **Git side** (the edit
is the right one in the parent's repository, and has no other effect on the
parent's resources). It does **not** prove the **semantic side**: that a child
with `config.feature=gamma` renders the *live* workload value. That needs a
render of the child with a spec that does not exist yet, which S1 shows is
impossible without a write.

### S4. What can be proven without a render path

Provable, with renders that are possible:

1. **No file can be the cause.** Every value-file candidate was tried by
   render and none reproduces the field (the existing engine; the rejected
   ones are the proof).
2. **Which Application option masks the key.** The layered values (`Layers`)
   show a key whose effective value comes from `helm.parameters`/`values`/
   `valuesObject`, and S2 gives the precedence as fact. `Controlled` already
   says so. For Kustomize, `kustomize.Options` names the option
   (`SetsImage`, replicas, labels, prefix).
3. **Where in Git the option lives**, for a child with a parent that
   verifies as in S3: the file, and the JSON pointer of the option inside the
   Application resource (`/spec/source/helm/parameters/0/value`), with the
   current Git value (read from the render of the parent, so it is the value
   Argo CD sees, not a regex hit).
4. **That the parent edit is exact** (S3).

Not provable:

5. **That setting the option to the live value reproduces the live field.**
   If the field is the parameter's value copied unchanged, this is almost
   certain, and "the field equals the parameter value, and it is the only
   Application option or file key equal to it" is a strong hint. It is still
   a guess: a template can transform (`upper`, `printf`, `default`), feed the
   same value to two fields (the existing `ValueAffectsOtherFields` problem:
   an edit would change fields that did not drift), or the equality can be a
   coincidence. Rewriting the uploaded chart's templates to probe the
   dependency was considered and rejected: `tpl`, `include`, `$.Values`,
   `index .Values "a" "b"` and subcharts make it unreliable.

## Options

**(A) Full verification through an API.** Rejected: S1 found none. The two
nearest are not renders (`ApplicationSet.generate` returns specs, and needs
`applicationsets, create`, a permission that lets Backflow create
ApplicationSets for real) or are writes (`sync dryRun`, `PATCH`).

**(B) Diagnosis only: Unmapped with the exact Git location and the value to
set, no merge request.** Everything it says is proven (S4 1–4); the one
unproven link (S4 5) is stated in the text as the reason there is no merge
request, and the decision stays with a person.

**(C) Opt-in unverified merge request** (`spec.mapping.allowUnverified`).
The merge request would carry a verified Git edit and an unverified claim. It
breaks "never guess a mapping" in its letter, and a wrong guess is expensive:
the parent merges, the child's spec changes, Argo CD applies it to the
cluster and **may change more than the drifted field** (a parameter that feeds
two fields), which is exactly what the render check exists to prevent. If it
is ever built it needs, at least: the opt-in naming the option kinds, a label
in the title and body, the field-equality guard of S4 5, and a post-merge
check (the child must show no drift for the field after the parent syncs;
otherwise the proposal becomes `Merged` with a warning condition and an
Event). That is not verification, only detection after the fact.

**(D) Local `helm template`/`kustomize build`.** Rejected: it would not be
Argo CD's render. Argo CD passes the release name, namespace, `--kube-version`,
`--api-versions`, `--include-crds`/`skipCrds`, schema flags, `ARGOCD_APP_*`
variables, repository and OCI credentials, `passCredentials`, plugin and
CMP generators, `argocd-cm` kustomize build options and per-version binaries.
Reproducing them is the "re-implement Argo CD's work" the project forbids, the
operator image would carry two more binaries, and a mismatch would not show as
an error but as a wrong "verified" label. It also needs network access to the
chart repositories.

## Recommendation: (B), with a prepared diff

Build **B** now. It is cheap, uses only what is proven, and turns a dead end
("set by helm.parameters") into a one-step action. Keep **C** out until there
is demand, and then only with the guards above.

What the user gets, for a drift on `Deployment web` whose field comes from a
parameter of the Application `argocd/child-web`, which is a resource of the
Application `argocd/spike-parent`:

> `ApplicationOption` — /spec/template/spec/containers/0/env/1/value is set by
> `spec.source.helm.parameters[0]` (`config.feature`) of the Application
> `argocd/child-web`, which Git cannot change from the chart's value files.
> The Application is defined in `charts/parentchart/values.yaml` (key
> `feature`) of `https://…/parent.git` (Application `argocd/spike-parent`),
> where setting `feature` to `"gamma"` makes the parent render the parameter
> `gamma` (verified by rendering the parent). Not proposed: Backflow cannot
> render the child with a changed parameter, so it cannot prove the new value
> reproduces the live field. `status.mapping.application.diff` has the edit.

### API and status

No new spec field in v1 (diagnosis is always on). `status.mapping` of an
`Unmapped` proposal gains an optional block (kubebuilder, `omitempty`):

```yaml
status:
  mapping:
    strategy: Unmapped
    application:                       # new, optional
      option: spec.source.helm.parameters[0]      # JSON-pointer-like path in the Application
      name: config.feature                          # the parameter / key / image the option names
      appliedValue: '"beta"'                        # what the Application sets (JSON)
      suggestedValue: '"gamma"'                     # the live value
      parent:
        application: argocd/spike-parent
        repoURL: https://…/parent.git
        revision: 0f3c…                              # the commit the parent was rendered at
        state: Located                               # Located | NotFound | NotInGit | OutOfSync | GeneratedByApplicationSet | Ambiguous | NotVerifiable
        file: charts/parentchart/values.yaml        # set when Located
        location: feature                            # key path (Helm), JSON pointer (Directory/Kustomize)
        diff: |                                      # bounded like status.mapping.diff
          …
      owner: ApplicationSet argocd/spike-set        # set when generated
```

Reasons (all `Unmapped`, `Mapped` condition): keep `SetByApplication` for the
Kustomize options and use it for Helm too (today the Helm case is
`NoVerifiedCandidate` with an explanation; it becomes `SetByApplication` when
the layer analysis names an Application option as the owner of *every*
unmapped field, and stays `NoVerifiedCandidate` otherwise). The message always
ends with where to look. `status.mapping.application.parent.state` carries
the finer answer:

| state | meaning |
|---|---|
| `Located` | The parent's Git edit is found and verified by the parent's render. |
| `NotFound` | No tracking-id/label, or the parent is not readable, or its `status.resources` does not list the child. The message still names the option. |
| `NotInGit` | The parent is Located but its render has no `Application` resource of that name at a path the edit can reach (child created by `kubectl`, or by a plugin). |
| `OutOfSync` | The parent is not Synced at the revision rendered: the child's cluster spec is not what Git says, so the parent's file is not the owner of the value that is live. No suggestion. |
| `GeneratedByApplicationSet` | `ownerReferences` names an ApplicationSet. |
| `Ambiguous` | Two candidate keys of the parent verify with different files (the engine's `AmbiguousCandidates`). |
| `NotVerifiable` | The engine could not verify any edit (the reason is the engine's). |

### How the parent mapping reuses `rendermap`

New code is small and sits in the controller (`driftproposal_application.go`)
and one helper package (`internal/appvalues`: option resolution and tracking-id
parsing); the mapping itself is the existing engines, as S3 shows.

1. **Resolve the option** (`appvalues.Resolve(app, source, fields)`): for each
   unmapped field, which option of the child sets it.
   - Helm: from the existing `Source.Layers` and `Result.Controlled` (the layer
     name and key path already come out of `inlineFor`); `parameters` match by
     name, `values`/`valuesObject` by path in the parsed YAML/JSON.
   - Kustomize: `kustomize.Options` (`SetsImage` → `images[i]`; `replicas[name]`;
     `namePrefix`/`nameSuffix`/`namespace`; `commonLabels[key]`;
     `commonAnnotations[key]`; an inline `patches[i].patch` is *named* but gets
     no suggested value in v1: the value lives inside a YAML/JSON string).
   - The *suggested value* is only set when the field equals the option's value
     unchanged (strings equal, S4 5). For an image the suggested value is the
     live image with the entry's own form (`name=newName:newTag`), taken the
     way T1 of `kustomize` splits an image. Otherwise the block carries the
     option and the location, with an empty `suggestedValue` and a message that
     says the value is derived by the chart.
2. **Find the parent** (`appvalues.ParentOf(child)`): the tracking-id
   annotation, else `app.kubernetes.io/instance`; read the parent through the
   Kubernetes API (the operator already lists Applications; add `get` on
   Applications, which it has); require the child in `status.resources`.
3. **Map the child's option in the parent**: build a `FieldChange` on the
   resource `argoproj.io/Application <ns>/<name>` at the option's pointer
   (`Desired` = the value in the *live child spec*, which equals the parent's
   rendered value when the parent is Synced; `Live` = the suggested value) and
   call the engine of the parent's source type: `mapping.Map` (Directory),
   `helmmap.FieldsFor`+`Verify`, `kustomize.Locate`+`Verify` — exactly what the
   spike did. The parent is rendered through the same `rendermap.Renderer`
   (`ArgoRenderer`, or `scratch` for a multi-source parent), the same
   `Options` (`AllowPartial` is irrelevant: one field).
4. **Record, never write.** The result goes in `status.mapping.application`;
   no commit, no branch, no scratch branch (a multi-source parent would need
   scratch branches to render; in v1 a multi-source parent is `NotVerifiable`
   so that diagnosis never pushes anything).
5. **Policy and access.** The child's policy governs. The parent's repository
   is read with the `ScmConnection` that matches its host (the machinery of
   `repositoryAuth`), or anonymously; no policy has to select the parent.

### Git-ahead

For B nothing is written, so Git-ahead only decides *when the suggestion is
trusted*: the child's own pause (the existing Git-ahead check on the child's
repository) is unchanged, and the parent contributes a second condition:
`parent.state` is `OutOfSync` when the parent is not Synced at the commit that
was rendered, or when files of the parent's source changed after the parent's
last sync (`lookAt*` for the parent's source type, with the parent's
`RelevantFiles`). Then no suggestion is made. The block is recomputed whenever
the proposal is mapped again (the remap-at-head path already does that); the
fingerprint of the mapping gains the parent's `spec.source` and the child's
`spec.source` options.

If C is ever built, the parent's pause has to be a real pause of the child's
proposals (a pending merge of the parent changes the child's spec, and drift
detection of the child would see the old spec as live drift): that is a new
state for the pause logic, and one reason not to build C now.

### ApplicationSet children

v1: `GeneratedByApplicationSet`, no suggestion. The Unmapped message names the
ApplicationSet (`ownerReferences`), and, best-effort, which of its list
elements or template fields holds the value (string search in the
ApplicationSet read through the Kubernetes API; this needs `get` on
ApplicationSets in the operator's ClusterRole, read-only).

A real verification does exist for these children and is a candidate for a
later version: change the ApplicationSet in the parent's Git
(`argoproj.io/ApplicationSet` is a resource of the parent like any other, the
engines map it), then call `applicationsets/generate` with the edited
ApplicationSet and check that the generated child's spec has the new option
value. It is Argo CD's own generator, writes nothing, and proves the child
*spec* (not the child's manifests, so the semantic link of S4 5 stays open). It
needs `applicationsets, create` for the Backflow account, so it would be opt-in
(`spec.argoCD.allowApplicationSetGenerate`), with that permission called out as
dangerous (it lets the holder create ApplicationSets).

### Test plan

- **Unit** (`internal/appvalues`, table-driven): tracking-id parsing (plain,
  `ns_name`, other group/kind, malformed), parent resolution against fake
  Applications (annotation, label, detached child, parent not listing the
  child), option resolution (`parameters`, `values`, `valuesObject` with
  nested keys and arrays, `images` forms `name=new:tag`, `name:tag`, digests,
  `replicas`, prefix/suffix, labels, inline `patches` named without a value),
  suggested value only for unchanged copies.
- **Engine reuse**: the three parents of the spike as tests with a fake
  renderer (Helm parent via `helmmap`, Kustomize parent via `kustomize` with a
  base resource and with an overlay patch, Directory parent), plus: the
  parent edit that would also change another resource is rejected
  (`ValueAffectsOtherFields`), a parent where two keys verify
  (`AmbiguousCandidates`), a parent that renders nothing for the child
  (`NotInGit`).
- **envtest** (`driftproposal_application_test.go`): a child Application with a
  tracking annotation and a parent Application in the cluster, fake renderer;
  the proposal ends `Unmapped` with `SetByApplication` and a `Located` block
  with file and diff; `OutOfSync`; `GeneratedByApplicationSet`; parent in
  another namespace; no repository access (`RepositoryAuthFailed` is held, not
  a verdict, as everywhere).
- **Coverage suite**: a `parent` chart per source type whose templates emit a
  child Application, with the same scenarios as the Helm cases but on the
  child's options; the table gets the states above as outcomes.
- **Spike as a script**: `hack/test-application-values.sh` (same conventions as
  `test-helm.sh`; the in-cluster `git` pod of this spike replaces gitlab.com
  for the parent, so it needs no `GITLAB_TOKEN` and can run in CI against kind).
  Sections: parameter of a Directory parent; parameter of a Helm parent;
  Kustomize `images` option; ApplicationSet child; parent OutOfSync; secrets
  (no token in the log or the status).

### Open questions

1. **Is a diagnosis enough?** If people want the merge request anyway, C needs
   a decision on what "unverified" may cover. The safest subset is the case
   where the *same* key is also in a value file and was proven by render to be
   masked (the field really is that key's, and the Application's layer just
   overrides it); there the claim "the parameter feeds the field" has one
   more leg than a name coincidence. Not proof.
2. **Label tracking.** The format of `app.kubernetes.io/instance` and the
   `resourceTrackingMethod` setting were not exercised on this cluster
   (annotation only).
3. **Multi-source parents.** v1 gives up on them; supporting them means the
   scratch renderer for the parent, which pushes branches to the parent's
   repository (an ScmConnection with write access to a repository the policy
   did not name).
4. **Parent outside the operator's reach.** The parent can live in another
   namespace or project and in a repository with no ScmConnection; `NotFound`
   is the answer, but a policy-level allow-list of parents might be wanted.
5. **Inline Kustomize patches** (`spec.source.kustomize.patches[].patch`) hold
   YAML/JSON inside a string; naming them is easy, a suggested value needs the
   same string editing as `internal/kustomize` inline patches. Deferred.
6. **Argo CD upgrade.** If a later Argo CD adds a spec-override render (for
   example `source` in `ManifestQuery`), A becomes possible and B would be
   upgraded to propose the merge request; the status block is designed so
   that `state: Located` plus a verified child render is the only addition.
