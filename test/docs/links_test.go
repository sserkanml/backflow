// Package docs checks the documentation of the repository: every relative link of every
// Markdown file, and its anchor, resolves; and the troubleshooting page lists every
// reason the code can set. No network is used: a link to another site is not followed.
package docs

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// root is the repository root, from test/docs.
const root = "../.."

// skipDirs are the directories no document of the project lives in.
var skipDirs = map[string]bool{".git": true, "bin": true, "node_modules": true, "tmp": true, "vendor": true}

// skipFiles are Markdown files that are not part of the repository (CLAUDE.md is a local file).
var skipFiles = map[string]bool{"CLAUDE.md": true}

// markdownFiles returns every Markdown file of the repository, as slash separated paths from the root.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, ".md") && !skipFiles[rel] {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

var (
	headingRE = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	// linkRE matches an inline link or image: [text](target) or [text](target "title"). The text may
	// contain code spans with brackets, which is rare; a link whose text has a ] is not recognised.
	linkRE = regexp.MustCompile(`!?\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	// htmlAnchorRE matches <a name="x"> and <a id="x">.
	htmlAnchorRE = regexp.MustCompile(`<a\s+(?:name|id)="([^"]+)"`)
	codeSpanRE   = regexp.MustCompile("`[^`]*`")
)

// slug is the anchor GitHub makes of a heading: the text in lower case, without anything but
// letters, digits, spaces, hyphens and underscores, and with the spaces turned into hyphens.
func slug(heading string) string {
	// Markup that does not show: code spans keep their text, links their label.
	heading = strings.ReplaceAll(heading, "`", "")
	heading = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`).ReplaceAllString(heading, "$1")
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// parsed is a Markdown file as far as links are concerned.
type parsed struct {
	anchors map[string]bool
	links   []link
}

type link struct {
	line   int
	target string
}

// parse reads the headings and the links of a Markdown file, leaving out fenced code.
func parse(t *testing.T, rel string) *parsed {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	p := &parsed{anchors: map[string]bool{}}
	seen := map[string]int{}
	fence := ""
	for i, line := range strings.Split(string(data), "\n") {
		trim := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			marker := trim[:3]
			switch fence {
			case "":
				fence = marker
			case marker:
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := headingRE.FindStringSubmatch(line); m != nil {
			s := slug(m[2])
			if n := seen[s]; n > 0 {
				p.anchors[s+"-"+itoa(n)] = true
			} else {
				p.anchors[s] = true
			}
			seen[s]++
		}
		for _, m := range htmlAnchorRE.FindAllStringSubmatch(line, -1) {
			p.anchors[m[1]] = true
		}
		// Links are looked for outside code spans.
		for _, m := range linkRE.FindAllStringSubmatch(codeSpanRE.ReplaceAllString(line, ""), -1) {
			p.links = append(p.links, link{line: i + 1, target: m[1]})
		}
	}
	return p
}

func itoa(n int) string { return strconv.Itoa(n) }

// external reports whether a link target leaves the repository (a URL or an address).
func external(target string) bool {
	return regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`).MatchString(target) || strings.HasPrefix(target, "//")
}

// TestMarkdownLinksResolve fails for every relative link of a Markdown file whose file does not
// exist, and for every anchor that is not a heading of the file it points to.
func TestMarkdownLinksResolve(t *testing.T) {
	files := markdownFiles(t)
	if len(files) < 5 {
		t.Fatalf("only %d Markdown files found: is the root %q right?", len(files), root)
	}
	cache := map[string]*parsed{}
	get := func(rel string) *parsed {
		if p, ok := cache[rel]; ok {
			return p
		}
		p := parse(t, rel)
		cache[rel] = p
		return p
	}
	checked := 0
	for _, rel := range files {
		for _, l := range get(rel).links {
			if external(l.target) {
				continue
			}
			checked++
			file, anchor, _ := strings.Cut(l.target, "#")
			dest := rel
			if file != "" {
				dest = path.Clean(path.Join(path.Dir(rel), file))
			}
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dest)))
			if err != nil {
				t.Errorf("%s:%d: %q does not exist (%s)", rel, l.line, l.target, dest)
				continue
			}
			if anchor == "" {
				continue
			}
			if info.IsDir() || !strings.HasSuffix(dest, ".md") {
				// An anchor into a file that is not Markdown (a line of a source file, say) is not checked.
				continue
			}
			if !get(dest).anchors[anchor] {
				t.Errorf("%s:%d: the anchor #%s is not a heading of %s", rel, l.line, anchor, dest)
			}
		}
	}
	t.Logf("%d Markdown files, %d relative links checked", len(files), checked)
}
