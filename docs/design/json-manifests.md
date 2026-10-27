# JSON manifests (roadmap A3)

Status: implemented (`internal/jsonedit`, `internal/mapping/jsondocs.go`).

Drift whose source is a JSON file is mapped like YAML. This note records what Argo CD
does with JSON, and the choices that follow.

## What Argo CD v3.5.3 does with JSON

Read from `reposerver/repository/repository.go` (`findManifests`,
`getObjsFromYAMLOrJSON`, `GenerateManifests`) of the tag; Argo CD is not a dependency.

| Case | Argo CD |
|---|---|
| Files read | names matching `^.*\.(yaml\|yml\|json\|jsonnet)$`, case-sensitive, after `directory.include`/`exclude` |
| A `.json` file | `json.NewDecoder(...).Decode(&unstructured.Unstructured)`: **one object**. If `decoder.More()` is true the sync fails with "Found multiple objects in "x.json". Only single objects are allowed in JSON files." |
| A JSON array in a `.json` file | does not decode into an object: the sync fails (unless the file is an OCI manifest, which is not a Kubernetes resource) |
| An object with an `items` array | `obj.IsList()` is "has an `items` that is a `[]interface{}`", whatever the `kind`. `EachListItem` expands it, and the items are the resources. An item that is not an object is an error |
| `"items": null` | `isNullList`: no resources |
| JSON in a `.yaml`/`.yml` file | read by `splitYAMLOrJSON` (a YAML-or-JSON decoder), several documents per file; it is YAML as far as Backflow is concerned |
| Encoding | `utfutil.OpenFile(..., UTF8)`: a byte order mark is dropped and UTF-16 is converted |
| `.jsonnet` | evaluated, when manifest generation is enabled |

## What Backflow does

- `Locate` reads a `.json` file the same way: exactly one object, or the items of a
  list; a file Argo CD would reject (two values, an array) is "could not parse" and
  defines nothing. Document numbers are the position of the item in a list, 0 for an
  object. A resource in an item is edited with the pointer `/items/N` in front of its
  field path.
- `jsonedit` edits the bytes in place (README: JSON files). Its result is parsed and
  compared with the intended object; a duplicate key on the path is `ErrAmbiguous`.
  Documents with a UTF-16 byte order mark are refused; a UTF-8 one is kept.
- `mapping.Apply` and `mapping.VerifyFile` dispatch on the extension; `Verify` of a
  list also checks that no other item changed. The Kustomize writer uses `jsonedit` for
  JSON 6902 patch files (a list at the top) and `mapping.Apply` for resources and strategic
  merge patches; the Helm writers go through `mapping.Apply` too, which now takes
  `.json` value files, and do not fall back to the YAML encoder for them.
- `ErrUnsupportedFileFormat` is left for Jsonnet.

## Coverage and tests

Unit tests of `jsonedit` (indentation, compact, tabs, CRLF, numbers, escapes, empty
containers, nested arrays of objects), of `Locate`/`Map` for the Argo CD cases above, of
the Kustomize tiers on `.json` files, and of the Helm writers. The coverage suite has the
same manifests as YAML and as JSON (Directory), a Kustomize overlay of `.json` files and
the `webapp` chart with a JSON values file; the YAML and JSON variants give the same
outcome case by case. `hack/test-merge-request.sh` section h checks the merge request
of a JSON ConfigMap against the real demo repository.
