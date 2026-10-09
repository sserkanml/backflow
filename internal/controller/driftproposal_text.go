package controller

import (
	"fmt"
	"strings"
	"unicode"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

const (
	// defaultCommitterName and defaultCommitterEmail identify Backflow in Git
	// when the ScmConnection sets no committer. The address is deliberately
	// not deliverable.
	defaultCommitterName  = "Backflow"
	defaultCommitterEmail = "backflow@noreply.invalid"

	// trailerProposal marks a commit as the one built for a proposal, so a
	// restarted operator recognises its own direct commit.
	trailerProposal  = "Proposal"
	trailerChangedBy = "Changed-by"

	// maxCellRunes bounds a value shown in the merge request table.
	maxCellRunes = 200
)

// singleLine makes untrusted text (user names, user agents) safe to put on
// one line of a commit message or Markdown paragraph.
func singleLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// resourceLabel is "Kind namespace/name", or "Kind name" for cluster-scoped resources.
func resourceLabel(r backflowv1alpha1.ResourceRef) string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s %s", r.Kind, r.Name)
	}
	return fmt.Sprintf("%s %s/%s", r.Kind, r.Namespace, r.Name)
}

// commitSubject is also the merge request title.
func commitSubject(dp *backflowv1alpha1.DriftProposal) string {
	return fmt.Sprintf("backflow: sync %s from cluster", resourceLabel(dp.Spec.Resource))
}

// commitMessage lists the changed paths and names the proposal. The actor is
// recorded as a trailer: the Kubernetes user has no Git identity, so the
// commit is authored by the committer.
func commitMessage(dp *backflowv1alpha1.DriftProposal) string {
	var b strings.Builder
	b.WriteString(commitSubject(dp))
	b.WriteString("\n\nChanged paths:\n")
	for _, ch := range dp.Spec.Changes {
		fmt.Fprintf(&b, "- %s\n", singleLine(ch.Path))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s: %s\n", trailerProposal, dp.Name)
	if a := dp.Spec.Actor; a != nil && singleLine(a.Username) != "" {
		fmt.Fprintf(&b, "%s: %s\n", trailerChangedBy, singleLine(a.Username))
	}
	return b.String()
}

// hasProposalTrailer reports whether a commit message was built for the proposal.
func hasProposalTrailer(message, proposal string) bool {
	want := trailerProposal + ": " + proposal
	for _, line := range strings.Split(message, "\n") {
		if strings.TrimRight(line, "\r") == want {
			return true
		}
	}
	return false
}

// cell renders a JSON value for a Markdown table cell.
func cell(v, empty string) string {
	if v == "" {
		return empty
	}
	v = singleLine(v)
	if r := []rune(v); len(r) > maxCellRunes {
		v = string(r[:maxCellRunes]) + "..."
	}
	v = strings.ReplaceAll(v, "|", `\|`)
	// Use a code span long enough to hold any backticks in the value.
	fence := "`"
	for strings.Contains(v, fence) {
		fence += "`"
	}
	return fence + " " + v + " " + fence
}

// fenceFor returns a code fence longer than any run of backticks in text.
func fenceFor(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// mergeRequestBody explains the change to the reviewer: what changed, who
// changed it, the diff, which proposal it belongs to, and what merging or
// closing means.
func mergeRequestBody(dp *backflowv1alpha1.DriftProposal, diff string) string {
	var b strings.Builder
	b.WriteString("Backflow found a change made directly in the cluster to a resource managed by Argo CD.\n\n")
	fmt.Fprintf(&b, "- **Resource:** %s\n", singleLine(resourceLabel(dp.Spec.Resource)))
	fmt.Fprintf(&b, "- **Application:** %s/%s\n", singleLine(dp.Spec.Application.Namespace), singleLine(dp.Spec.Application.Name))
	if a := dp.Spec.Actor; a != nil && singleLine(a.Username) != "" {
		fmt.Fprintf(&b, "- **Changed by:** %s", singleLine(a.Username))
		if ua := singleLine(a.UserAgent); ua != "" {
			fmt.Fprintf(&b, " (%s)", ua)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("- **Changed by:** unknown\n")
	}
	fmt.Fprintf(&b, "- **Detected:** %s\n\n", dp.Spec.DetectedAt.UTC().Format("2006-01-02 15:04:05 UTC"))

	b.WriteString("| Path | Git value | Live value |\n|---|---|---|\n")
	for _, ch := range dp.Spec.Changes {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(ch.Path, "-"), cell(ch.Desired, "(absent)"), cell(ch.Live, "(removed)"))
	}

	if diff != "" {
		fence := fenceFor(diff)
		fmt.Fprintf(&b, "\n%sdiff\n%s", fence, diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteString("\n")
		}
		b.WriteString(fence + "\n")
	}

	fmt.Fprintf(&b, "\nDriftProposal: `%s/%s`\n\n", dp.Namespace, dp.Name)
	b.WriteString("Merging this merge request makes the live change permanent: Git then matches the cluster. " +
		"Closing it without merging lets Argo CD revert the cluster to what Git says.\n")
	return b.String()
}
