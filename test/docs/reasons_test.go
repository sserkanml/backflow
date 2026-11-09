package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// troubleshooting is the page that has to list every reason the operator can set.
const troubleshooting = "docs/operations/troubleshooting.md"

var reasonValue = regexp.MustCompile(`^[A-Z][A-Za-z0-9]+$`)

// codeReasons finds the reasons the Go code can set, by reading its syntax (no package is built):
//
//   - a constant whose name starts with reason or Reason (reasonNotFound = "NotFound");
//   - a composite literal field called Reason or reason (metav1.Condition{Reason: "Resolved"},
//     &checkError{reason: "InvalidSpec"});
//   - an assignment to a field called Reason (cond.Reason = "Authenticated");
//   - the values of the enumerations of the API whose type ends in Reason or State.
//
// A reason is a CamelCase word; the text of a message is not one.
func codeReasons(t *testing.T) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "api", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			ast.Inspect(f, func(n ast.Node) bool {
				for _, r := range reasonsOf(n, rel) {
					if reasonValue.MatchString(r.value) {
						found[r.value] = append(found[r.value], rel+" ("+r.how+")")
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

type reasonFound struct{ value, how string }

// stringLiteral is the value of a string literal.
func stringLiteral(e ast.Expr) (string, bool) {
	if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
		s, err := strconv.Unquote(bl.Value)
		return s, err == nil
	}
	return "", false
}

func isReasonName(name string) bool {
	return strings.HasPrefix(name, "reason") || strings.HasPrefix(name, "Reason")
}

// reasonsOf returns the reasons a syntax node sets.
func reasonsOf(n ast.Node, rel string) []reasonFound {
	var out []reasonFound
	switch x := n.(type) {
	case *ast.ValueSpec:
		for i, name := range x.Names {
			if i >= len(x.Values) || !isReasonName(name.Name) {
				continue
			}
			if s, ok := stringLiteral(x.Values[i]); ok {
				out = append(out, reasonFound{s, name.Name})
			}
		}
	case *ast.KeyValueExpr:
		if id, ok := x.Key.(*ast.Ident); ok && (id.Name == "Reason" || id.Name == "reason") {
			if s, ok := stringLiteral(x.Value); ok {
				out = append(out, reasonFound{s, "field " + id.Name})
			}
		}
	case *ast.AssignStmt:
		for i, l := range x.Lhs {
			sel, ok := l.(*ast.SelectorExpr)
			if !ok || i >= len(x.Rhs) || (sel.Sel.Name != "Reason" && sel.Sel.Name != "reason") {
				continue
			}
			if s, ok := stringLiteral(x.Rhs[i]); ok {
				out = append(out, reasonFound{s, "assignment"})
			}
		}
	case *ast.GenDecl:
		if x.Tok == token.TYPE && strings.HasPrefix(rel, "api/v1/") {
			out = append(out, enumReasons(x)...)
		}
	}
	return out
}

// enumReasons returns the values of the enumerations (+kubebuilder:validation:Enum) of the types of a declaration that
// end in Reason or State.
func enumReasons(x *ast.GenDecl) []reasonFound {
	var out []reasonFound
	for _, spec := range x.Specs {
		ts := spec.(*ast.TypeSpec)
		if !strings.HasSuffix(ts.Name.Name, "Reason") && !strings.HasSuffix(ts.Name.Name, "State") {
			continue
		}
		doc := x.Doc
		if ts.Doc != nil {
			doc = ts.Doc
		}
		if doc == nil {
			continue
		}
		for _, c := range doc.List {
			if _, list, ok := strings.Cut(c.Text, "+kubebuilder:validation:Enum="); ok {
				for v := range strings.SplitSeq(list, ";") {
					out = append(out, reasonFound{strings.TrimSpace(v), "enum " + ts.Name.Name})
				}
			}
		}
	}
	return out
}

// tableReasons are the reasons in the first column of the tables of the page: | `Reason` | ...
func tableReasons(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(troubleshooting)))
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `([A-Za-z0-9]+)` \\|")
	out := map[string]bool{}
	started := false
	for line := range strings.SplitSeq(string(data), "\n") {
		// The tables before the first resource are about where to look, not about reasons.
		if strings.HasPrefix(line, "## ScmConnection") {
			started = true
		}
		if !started {
			continue
		}
		if m := row.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// TestTroubleshootingListsEveryReason fails when the code can set a reason that the troubleshooting page does not
// list, and when the page lists a reason that no code sets, so that the page cannot go stale unnoticed.
func TestTroubleshootingListsEveryReason(t *testing.T) {
	code := codeReasons(t)
	if len(code) < 60 {
		t.Fatalf("only %d reasons found in the code: has the scan broken?", len(code))
	}
	doc := tableReasons(t)
	if len(doc) < 60 {
		t.Fatalf("only %d reasons found in %s: has the format of the tables changed?", len(doc), troubleshooting)
	}
	var missing, stale []string
	for r, where := range code {
		if !doc[r] {
			missing = append(missing, r+"  ("+where[0]+")")
		}
	}
	for r := range doc {
		if _, ok := code[r]; !ok {
			stale = append(stale, r)
		}
	}
	slices.Sort(missing)
	slices.Sort(stale)
	for _, m := range missing {
		t.Errorf("the code sets the reason %s and %s does not list it: add a row to a table", m, troubleshooting)
	}
	for _, s := range stale {
		t.Errorf("%s lists the reason %s and no code sets it: remove the row, or fix its spelling", troubleshooting, s)
	}
	t.Logf("%d reasons in the code, %d in the tables", len(code), len(doc))
}
