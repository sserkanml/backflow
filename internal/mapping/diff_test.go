package mapping

import (
	"strings"
	"testing"
)

func numbered(prefix string, from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		b.WriteString(prefix)
		b.WriteString(string(rune('a' + i - 1)))
		b.WriteString("\n")
	}
	return b.String()
}

func TestUnifiedDiff(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want string
	}{
		{"identical", "a\nb\n", "a\nb\n", ""},
		{
			"one changed line with context",
			"a\nb\nc\nd\ne\nf\ng\n", "a\nb\nc\nX\ne\nf\ng\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,7 +1,7 @@\n a\n b\n c\n-d\n+X\n e\n f\n g\n",
		},
		{
			"context is limited to three lines",
			"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", "1\n2\n3\n4\n5\nX\n7\n8\n9\n10\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -3,7 +3,7 @@\n 3\n 4\n 5\n-6\n+X\n 7\n 8\n 9\n",
		},
		{
			"distant changes make two hunks",
			"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n", "X\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\nY\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,4 +1,4 @@\n-1\n+X\n 2\n 3\n 4\n@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+Y\n",
		},
		{
			"near changes share a hunk",
			"1\n2\n3\n4\n5\n6\n7\n8\n", "X\n2\n3\n4\n5\n6\n7\nY\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,8 +1,8 @@\n-1\n+X\n 2\n 3\n 4\n 5\n 6\n 7\n-8\n+Y\n",
		},
		{
			"an added line",
			"a\nb\n", "a\nnew\nb\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,2 +1,3 @@\n a\n+new\n b\n",
		},
		{
			"a removed line",
			"a\nold\nb\n", "a\nb\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,3 +1,2 @@\n a\n-old\n b\n",
		},
		{
			"appending to the end",
			"a\n", "a\nb\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,1 +1,2 @@\n a\n+b\n",
		},
		{
			"from empty",
			"", "a\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -0,0 +1,1 @@\n+a\n",
		},
		{
			"a missing final newline is marked",
			"a\nb", "a\nc",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n",
		},
		{
			"only the line ending differs",
			"a\r\nb\r\n", "a\nb\r\n",
			"--- a/f.yaml\n+++ b/f.yaml\n@@ -1,2 +1,2 @@\n-a\r\n+a\n b\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UnifiedDiff("f.yaml", []byte(tt.a), []byte(tt.b)); got != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestUnifiedDiffLargeMiddleFallsBackToABlock(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 2500; i++ {
		a.WriteString("old line\n")
		b.WriteString("new line\n")
	}
	got := UnifiedDiff("f.yaml", []byte("head\n"+a.String()+"tail\n"), []byte("head\n"+b.String()+"tail\n"))
	if !strings.Contains(got, "-old line\n") || !strings.Contains(got, "+new line\n") || !strings.HasPrefix(got, "--- a/f.yaml") {
		t.Errorf("unexpected diff start: %.200s", got)
	}
}
