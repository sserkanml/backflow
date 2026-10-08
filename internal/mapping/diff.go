package mapping

import (
	"fmt"
	"strings"
)

const diffContext = 3

// maxLCSCells bounds the line-diff table. Beyond it the changed region is
// shown as one removed and one added block, which is still a correct diff.
const maxLCSCells = 4_000_000

type diffOp struct {
	kind byte // ' ', '-' or '+'
	line string
}

// UnifiedDiff returns a unified diff of a file, or "" when nothing changed.
func UnifiedDiff(name string, a, b []byte) string {
	if string(a) == string(b) {
		return ""
	}
	ops := diffLines(splitLines(string(a)), splitLines(string(b)))

	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)

	// Group changes into hunks with surrounding context.
	for i := 0; i < len(ops); {
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i == len(ops) {
			break
		}
		start := max(i-diffContext, 0)
		end := i
		for {
			for end < len(ops) && ops[end].kind != ' ' {
				end++
			}
			// Extend over the equal run if another change follows soon.
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run < len(ops) && run-end <= 2*diffContext {
				end = run
				continue
			}
			end = min(end+diffContext, len(ops))
			break
		}
		writeHunk(&out, ops, start, end)
		i = end
	}
	return out.String()
}

func writeHunk(out *strings.Builder, ops []diffOp, start, end int) {
	aLine, bLine := 1, 1
	for _, op := range ops[:start] {
		if op.kind != '+' {
			aLine++
		}
		if op.kind != '-' {
			bLine++
		}
	}
	aCount, bCount := 0, 0
	for _, op := range ops[start:end] {
		if op.kind != '+' {
			aCount++
		}
		if op.kind != '-' {
			bCount++
		}
	}
	// An empty range is addressed by the line before it.
	aStart, bStart := aLine, bLine
	if aCount == 0 {
		aStart--
	}
	if bCount == 0 {
		bStart--
	}
	fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
	for _, op := range ops[start:end] {
		out.WriteByte(op.kind)
		out.WriteString(op.line)
		if !strings.HasSuffix(op.line, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
}

// splitLines splits keeping the line endings, so a changed line ending counts.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func diffLines(a, b []string) []diffOp {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	am, bm := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]

	var ops []diffOp
	for _, l := range a[:prefix] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, diffMiddle(am, bm)...)
	for _, l := range a[len(a)-suffix:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

// diffMiddle diffs the part without common prefix and suffix using a
// longest-common-subsequence table.
func diffMiddle(a, b []string) []diffOp {
	var ops []diffOp
	if len(a)*len(b) > maxLCSCells {
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	// lcs[i][j] is the LCS length of a[i:] and b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}
