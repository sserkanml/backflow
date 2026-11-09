# JSON files

For the other sources see [What maps](overview.md).

A manifest, a patch or a value file that is JSON is edited as JSON, **in place**: only
the bytes of the edit change. Indentation (spaces or tabs, any width), key order, the
text of numbers (`1.10` stays `1.10` unless the value changes), string escapes, line
endings (CRLF), a byte order mark and the final newline (or its absence) stay as they
are. A changed value is written with correct escaping; a new member goes after the last
one with the indentation of its siblings (or on the same line in a one-line object);
a removed member or element takes its comma with it and leaves no blank line; a map or
list that is replaced is written again in the surrounding style. Every result is parsed and
compared with the object the edit was meant to give, and a key defined twice on the path
of an edit (which JSON parsers read differently) is `Ambiguous`, never guessed.

What is JSON follows Argo CD (v3.5.3, `getObjsFromYAMLOrJSON`):

- **Directory sources.** A file whose name ends in `.json` holds **one object**; a
  second value in the file, or an array at the top, makes Argo CD fail, so Backflow
  reads such a file as defining nothing. An object with an `items` array is a list (any
  `kind`, `List` usually): its items are the resources, and `"items": null` is an empty
  list. A `.yaml` file with JSON in it is read as YAML and edited as YAML.
- **Kustomize sources.** `.json` resource files, strategic merge patches and JSON 6902
  patches (a JSON list) are edited as JSON, whichever tier they come in. Settings in
  `kustomization.yaml`, and new Backflow patches, stay YAML.
- **Helm sources.** A value file with a `.json` extension is read by Helm as YAML and
  edited by Backflow as JSON.

Limits: UTF-16 files are not edited (Argo CD reads them, Backflow refuses them), and
Jsonnet is not supported.
