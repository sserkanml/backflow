# Values set in the Application

For the other sources see [What maps](overview.md).

Some values are not in any file the source reads: the Application sets them itself,
with `helm.parameters`, `helm.values`/`valuesObject`, or `spec.source.kustomize`
(`images`, `replicas`, `namePrefix`, `nameSuffix`, `namespace`, `commonLabels`,
`commonAnnotations`, `patches`). Argo CD applies them on top of Git, so editing a
value file never changes the result. A drift of such a value ends `Unmapped` with the
reason **`SetByApplication`**, for Helm (single and multi-source) and Kustomize alike.

Backflow does not propose a merge request for it. **No Argo CD API renders an
Application with a changed option** (`GetManifests` and `GetManifestsWithFiles` use the
Application as it is stored; `sync` dry runs and the ApplicationSet generator do not
render, and the first writes to the Application), so a new value of the option cannot be
verified by a render, and Backflow only proposes what a render verifies. What it can prove
it tells you, in `status.mapping.application` and in one sentence, which is also
an Event on the proposal:

> Set by spec.source.helm.parameters[name=config.feature] of Application argocd/child-web,
> defined in apps/children/child.yaml (parent argocd/root). Set it to "gamma" there;
> Backflow cannot verify an Application option by rendering, so no merge request is made.

```yaml
status:
  mapping:
    strategy: Unmapped
    application:
      application: argocd/child-web
      option: spec.source.helm.parameters[name=config.feature]
      optionPath: /spec/source/helm/parameters/0/value
      current: '"beta"'
      suggested: '"gamma"'          # only when the option copies the field exactly
      parent:
        state: Located
        name: argocd/root
        repoURL: https://git.example/apps.git
        revision: 0f3c...
        file: apps/children/child.yaml
        location: /spec/source/helm/parameters/0/value
        diff: |                      # verified by a render of the parent, never applied
          ...
```

**Where the option is defined.** The Application is usually a resource of a parent
Application (app-of-apps). Backflow finds the parent in `status.resources` of the
Applications (the `argocd.argoproj.io/tracking-id` annotation is the fast path), and maps the
child's field to the parent's Git file with the same engines as for any resource
(Directory, Helm, Kustomize), verified by a render of the parent. For a Kustomize parent
it looks for the file that holds the value, not a place to add a patch.

| `parent.state` | Meaning |
|---|---|
| `Located` | The file and location are found and a render of the parent with the edit changes exactly that option. |
| `NotManagedFromGit` | No Application manages it. |
| `GeneratedByApplicationSet` | An ApplicationSet generated it (`name`); change its template or generator. |
| `OutOfSync` | The parent is not synced, so what Git says is not what runs. |
| `Ambiguous` | More than one Application manages it. |
| `NotVerifiable` | The parent has several sources, has no Git source, has another source type, or no verified edit shows the option (the message says which). |

The **suggested value** is given when it can be derived exactly: a parameter or a
`valuesObject` key (it keeps the type of the current value), an image tag of
`spec.source.kustomize.images`, a replica count, a label or annotation. For a string that holds
YAML or JSON (`helm.values`, a Kustomize `patches` entry) only the location is given.
When several options set the drifted fields, the first is in the status and the message
names the others. When only some of the unmapped fields are set by an option, the reason
stays the engine's, and the message adds the sentence.

Nothing is written for this: no commit, no branch, no merge request, no scratch
branch (a parent with several sources is `NotVerifiable` so that nothing needs
one). The parent is read with the `ScmConnection` of the proposal's namespace that
matches its host, or anonymously.
