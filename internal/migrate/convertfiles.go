package migrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// ConvertFiles rewrites the backflow.io/v1alpha1 ScmConnection and BackflowPolicy manifests in YAML files to v1, for the
// users who keep them in Git and let Argo CD apply them. A file that does not mention backflow.io/v1alpha1 is not read
// further. Of a multi-document file only the documents that are converted are written again: the others, and the separators,
// are copied as they are. A document that is converted keeps its comments and the order of its keys (the YAML is written
// again, with two spaces of indentation).
//
// The conversion of a document is checked against the one `backflow migrate` does to an object (Convert): when the YAML that
// the node edit made is not the same spec, the document is reported and left alone.

// FilesResult is what a run over files found.
type FilesResult struct {
	// Files counts the files that were read.
	Files int
	// Changed are the files that were rewritten, or that would be with Check.
	Changed []string
	// Manual are the documents that need a person: a template, an anchor, a shape the tool does not know.
	Manual []string
}

// Pending reports whether anything is left to do or to look at.
func (r *FilesResult) Pending() bool { return len(r.Changed) > 0 || len(r.Manual) > 0 }

var (
	// v1alpha1 backflow objects of the two kinds that live in Git; the text check is for files that are not YAML (Helm templates).
	mentionsOld  = regexp.MustCompile(`backflow\.io/v1(alpha1|beta1)`)
	mentionsKind = regexp.MustCompile(`(?m)^\s*kind:\s*['"]?(ScmConnection|BackflowPolicy)\b`)
	separator    = regexp.MustCompile(`^---(\s|$)`)
)

// ConvertManifest converts the documents of one file. It returns the new content (the same bytes when nothing changed), a line
// for each document it converted, and one for each that it could not.
func ConvertManifest(src []byte) (out []byte, converted, manual []string) {
	if !mentionsOld.Match(src) {
		return src, nil, nil
	}
	var buf bytes.Buffer
	for _, seg := range splitDocuments(string(src)) {
		text, did, why := convertDocument(seg)
		buf.WriteString(text)
		if did != "" {
			converted = append(converted, did)
		}
		if why != "" {
			manual = append(manual, why)
		}
	}
	return buf.Bytes(), converted, manual
}

// splitDocuments cuts a file at its "---" lines. A separator line belongs to the document that follows it.
func splitDocuments(src string) []string {
	var segs []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(src, "\n") {
		if separator.MatchString(line) && cur.Len() > 0 {
			segs = append(segs, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		segs = append(segs, cur.String())
	}
	return segs
}

// convertDocument returns the text of the document after the conversion. did names what was converted, why what could not be.
func convertDocument(seg string) (text, did, why string) {
	head, body := "", seg
	if separator.MatchString(seg) {
		line, rest, _ := strings.Cut(seg, "\n")
		head, body = line+"\n", rest
		if !strings.Contains(seg, "\n") {
			head, body = line, ""
		}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		if mentionsOld.MatchString(body) && mentionsKind.MatchString(body) {
			return seg, "", "a document with backflow.io/v1alpha1 that is not plain YAML (a template?): convert it by hand"
		}
		return seg, "", ""
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return seg, "", ""
	}
	apiVersion := scalarOf(root, "apiVersion")
	kind := ObjectKind(scalarOf(root, "kind"))
	if (apiVersion != oldAPIVersion && apiVersion != betaAPIVersion) || (kind != KindScmConnection && kind != KindPolicy) {
		return seg, "", ""
	}
	if apiVersion == betaAPIVersion {
		// v1beta1 has the schema of v1: only the apiVersion changes, and nothing else in the document is touched.
		return head + replaceAPIVersion(root, body), fmt.Sprintf("%s %s: converted (1 change(s))",
			kind, scalarOf(mapOf(root, keyMetadata), keyName)), ""
	}
	name := scalarOf(mapOf(root, "metadata"), "name")
	label := string(kind) + " " + name
	if hasAlias(root) {
		return seg, "", label + ": uses YAML anchors or aliases: convert it by hand"
	}

	// What the object conversion makes of it, to check against and to take the notes from.
	var asMapping obj
	if err := sigsyaml.Unmarshal([]byte(body), &asMapping); err != nil {
		return seg, "", label + ": " + err.Error()
	}
	want, err := Convert(kind, asMapping)
	if err != nil {
		return seg, "", label + ": " + err.Error()
	}

	switch kind {
	case KindScmConnection:
		convertScmNode(root)
	case KindPolicy:
		convertPolicyNode(root)
	}
	setScalar(root, "apiVersion", newAPIVersion)
	deleteKey(root, "status")

	var enc bytes.Buffer
	e := yaml.NewEncoder(&enc)
	e.SetIndent(2)
	if err := e.Encode(&doc); err != nil {
		return seg, "", label + ": " + err.Error()
	}
	_ = e.Close()
	var got obj
	if err := sigsyaml.Unmarshal(enc.Bytes(), &got); err != nil {
		return seg, "", label + ": " + err.Error()
	}
	if !sameSpec(got[keySpec], want.Object[keySpec]) {
		return seg, "", label + ": the edit of the YAML does not give what the conversion of the object gives: convert it by hand"
	}
	notes := 0
	for _, n := range want.Notes {
		if !strings.HasPrefix(n, "metadata.annotations[") && !strings.HasPrefix(n, "status") {
			notes++
		}
	}
	return head + enc.String(), fmt.Sprintf("%s: converted (%d change(s))", label, notes), ""
}

// replaceAPIVersion changes backflow.io/v1beta1 to backflow.io/v1 at the place of the apiVersion value, and nowhere else.
func replaceAPIVersion(root *yaml.Node, body string) string {
	i := keyIndex(root, "apiVersion")
	if i < 0 {
		return body
	}
	value := root.Content[i+1]
	lines := strings.SplitAfter(body, "\n")
	if value.Line < 1 || value.Line > len(lines) {
		return body
	}
	line := lines[value.Line-1]
	before, after, found := strings.Cut(line, betaAPIVersion)
	if !found {
		return body
	}
	lines[value.Line-1] = before + newAPIVersion + after
	return strings.Join(lines, "")
}

func sameSpec(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Node helpers. A mapping node keeps its keys and values in Content, one after the other.

func keyIndex(m *yaml.Node, key string) int {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func mapOf(m *yaml.Node, key string) *yaml.Node {
	if i := keyIndex(m, key); i >= 0 && m.Content[i+1].Kind == yaml.MappingNode {
		return m.Content[i+1]
	}
	return nil
}

func scalarOf(m *yaml.Node, key string) string {
	if i := keyIndex(m, key); i >= 0 && m.Content[i+1].Kind == yaml.ScalarNode {
		return m.Content[i+1].Value
	}
	return ""
}

func setScalar(m *yaml.Node, key, value string) {
	if i := keyIndex(m, key); i >= 0 {
		m.Content[i+1].Value = value
	}
}

func deleteKey(m *yaml.Node, key string) {
	if i := keyIndex(m, key); i >= 0 {
		m.Content = append(m.Content[:i], m.Content[i+2:]...)
	}
}

func renameKey(m *yaml.Node, from, to string) {
	if i := keyIndex(m, from); i >= 0 {
		m.Content[i].Value = to
	}
}

// takeKey removes a key from a mapping and returns its key and value nodes.
func takeKey(m *yaml.Node, key string) (k, v *yaml.Node) {
	i := keyIndex(m, key)
	if i < 0 {
		return nil, nil
	}
	k, v = m.Content[i], m.Content[i+1]
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	return k, v
}

func newKey(name string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
}

func newMapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

func hasAlias(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	return slices.ContainsFunc(n.Content, hasAlias)
}

// dedupeSequence removes repeated scalars of a sequence, keeping the first.
func dedupeSequence(seq *yaml.Node) {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	seen := map[string]bool{}
	kept := seq.Content[:0]
	for _, c := range seq.Content {
		if c.Kind == yaml.ScalarNode {
			if seen[c.Value] {
				continue
			}
			seen[c.Value] = true
		}
		kept = append(kept, c)
	}
	seq.Content = kept
}

func seqOf(m *yaml.Node, key string) *yaml.Node {
	if i := keyIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}

func convertScmNode(root *yaml.Node) {
	spec := mapOf(root, keySpec)
	if spec == nil {
		return
	}
	if i := keyIndex(spec, "provider"); i >= 0 {
		if to, ok := providers[spec.Content[i+1].Value]; ok {
			spec.Content[i+1].Value = to
		}
	}
	dedupeSequence(seqOf(spec, "hosts"))
	for _, k := range []string{"tokenSecretRef", "caSecretRef"} {
		deleteKey(mapOf(spec, k), "optional")
	}
}

func convertPolicyNode(root *yaml.Node) {
	spec := mapOf(root, keySpec)
	if spec == nil {
		return
	}
	convertArgoCDNode(spec)
	for _, r := range [][3]string{{"helm", "fallbackToApplicationOverride", "allowApplicationOverride"}, {"helm", "addOverrides", "allowValueOverrides"},
		{"kustomize", "editBases", "allowBaseEdits"}, {"kustomize", "addPatches", "allowNewPatches"}} {
		renameKey(mapOf(spec, r[0]), r[1], r[2])
	}
	dedupeSequence(seqOf(mapOf(spec, "applications"), "names"))
	if mr := mapOf(spec, "mergeRequest"); mr != nil {
		git := newMapping()
		req := newMapping()
		for _, k := range []string{"branchPrefix", "targetBranch"} {
			if key, val := takeKey(mr, k); key != nil {
				git.Content = append(git.Content, key, val)
			}
		}
		for _, k := range []string{keyLabels, keyReviewers, "assignActor"} {
			if key, val := takeKey(mr, k); key != nil {
				if k == keyReviewers || k == keyLabels {
					dedupeSequence(val)
				}
				req.Content = append(req.Content, key, val)
			}
		}
		if len(req.Content) > 0 {
			git.Content = append(git.Content, newKey("mergeRequest"), req)
		}
		i := keyIndex(spec, "mergeRequest")
		spec.Content[i].Value = "git"
		spec.Content[i+1] = git
	}
}

// convertArgoCDNode makes spec.argoCD of spec.argoCDNamespace and the old spec.argoCD: {namespace, server: {...}}.
func convertArgoCDNode(spec *yaml.Node) {
	nsKey, nsVal := takeKey(spec, "argoCDNamespace")
	if nsKey == nil && mapOf(spec, "argoCD") == nil {
		return
	}
	argo := newMapping()
	if nsKey != nil {
		nsKey.Value = keyNamespace
		argo.Content = append(argo.Content, nsKey, nsVal)
	}
	if i := keyIndex(spec, "argoCD"); i >= 0 {
		oldKey, server := spec.Content[i], spec.Content[i+1]
		for _, k := range []string{"tokenSecretRef", "caSecretRef"} {
			deleteKey(mapOf(server, k), "optional")
		}
		argo.Content = append(argo.Content, newKey("server"), server)
		oldKey.Value = "argoCD"
		spec.Content[i+1] = argo
		return
	}
	// There was only argoCDNamespace: put the new key where the old one was (at the end, if that cannot be known).
	spec.Content = append(spec.Content, newKey("argoCD"), argo)
}

// ConvertFilesRun converts the YAML files under the paths (files, or directories with .yaml and .yml files). With check
// nothing is written.
func ConvertFilesRun(paths []string, check bool, out io.Writer) (*FilesResult, error) {
	res := &FilesResult{}
	files, err := collectFiles(paths)
	if err != nil {
		return res, err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return res, err
		}
		res.Files++
		converted, newData, manual := convertFile(data)
		for _, m := range manual {
			res.Manual = append(res.Manual, f+": "+m)
			_, _ = fmt.Fprintf(out, "%s: NEEDS A PERSON: %s\n", f, m)
		}
		if len(converted) == 0 {
			continue
		}
		res.Changed = append(res.Changed, f)
		verb := "converted"
		if check {
			verb = "would be converted"
		}
		for _, c := range converted {
			_, _ = fmt.Fprintf(out, "%s: %s (%s)\n", f, strings.Replace(c, ": converted", ": "+verb, 1), "backflow.io/v1alpha1 or v1beta1 -> v1")
		}
		if !check {
			if err := writeKeepingMode(f, newData); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func convertFile(data []byte) (converted []string, out []byte, manual []string) {
	out, converted, manual = ConvertManifest(data)
	return converted, out, manual
}

func collectFiles(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if base := d.Name(); path != p && (base == ".git" || base == "node_modules" || base == "vendor") {
					return filepath.SkipDir
				}
				return nil
			}
			if ext := filepath.Ext(path); ext == ".yaml" || ext == ".yml" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// writeKeepingMode writes the file through a temporary file in the same directory, with the mode the file had.
func writeKeepingMode(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".convert-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

const convertFilesUsage = `usage: backflow migrate convert-files [--check] [paths...]

Rewrites the ScmConnection and BackflowPolicy manifests of backflow.io/v1alpha1 and backflow.io/v1beta1 in YAML files to backflow.io/v1,
in place (a v1beta1 manifest only gets the new apiVersion: the schema is the same).
The paths are files or directories (.yaml and .yml files below them); the default is the current directory. Documents of other
kinds and files that do not mention backflow.io/v1alpha1 are not touched, byte for byte. A converted document keeps its
comments and the order of its keys.

Use it on the Git repository that Argo CD applies the Backflow objects from, and merge the change before the upgrade
(docs/operations/upgrade-1.0.md): after it the old manifests either fail to sync or, written through v1alpha1, lose fields.

--check changes nothing and exits 1 when a file would change or a document needs a person (a template, a YAML anchor), for CI.

flags:
`

// ConvertFilesMain runs `migrate convert-files` and returns the exit code.
func ConvertFilesMain(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("convert-files", flag.ContinueOnError)
	fl.SetOutput(stderr)
	check := fl.Bool("check", false, "change nothing; exit 1 if a file would change")
	fl.Usage = func() {
		_, _ = fmt.Fprint(stderr, convertFilesUsage)
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitDone
		}
		return ExitError
	}
	paths := fl.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	res, err := ConvertFilesRun(paths, *check, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ERROR: %v\n", err)
		return ExitError
	}
	_, _ = fmt.Fprintf(stdout, "%d file(s) read, %d %s, %d document(s) need a person\n", res.Files, len(res.Changed),
		map[bool]string{true: "would change", false: "changed"}[*check], len(res.Manual))
	if res.Pending() && (*check || len(res.Manual) > 0) {
		return ExitError
	}
	return ExitDone
}
