package controller

import (
	"fmt"
	"strings"
	"unicode"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/helmmap"
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
	// trailerProposalUID names the object a commit was built for. Proposal
	// names are content hashes and are reused, the UID never is.
	trailerProposalUID = "Proposal-UID"

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

// commitSubject is also the merge request title. left are the changes a
// partial mapping leaves out; the subject says so.
func commitSubject(dp *backflowv1alpha1.DriftProposal, left []leftOutField) string {
	subject := fmt.Sprintf("backflow: sync %s from cluster", resourceLabel(dp.Spec.Resource))
	if len(left) > 0 {
		subject += " (partial)"
	}
	return subject
}

// mappedChanges are the changes of the proposal that the edits make: all of
// them, except those a partial mapping leaves out.
func mappedChanges(dp *backflowv1alpha1.DriftProposal, left []leftOutField) []backflowv1alpha1.FieldChange {
	if len(left) == 0 {
		return dp.Spec.Changes
	}
	skip := make(map[string]bool, len(left))
	for _, l := range left {
		skip[l.path] = true
	}
	out := make([]backflowv1alpha1.FieldChange, 0, len(dp.Spec.Changes))
	for _, ch := range dp.Spec.Changes {
		if !skip[ch.Path] {
			out = append(out, ch)
		}
	}
	return out
}

// commitMessage lists the changed paths and names the proposal. The actor is
// recorded as a trailer: the Kubernetes user has no Git identity, so the
// commit is authored by the committer.
func commitMessage(dp *backflowv1alpha1.DriftProposal, left []leftOutField) string {
	var b strings.Builder
	b.WriteString(commitSubject(dp, left))
	b.WriteString("\n\nChanged paths:\n")
	for _, ch := range mappedChanges(dp, left) {
		fmt.Fprintf(&b, "- %s\n", singleLine(ch.Path))
	}
	b.WriteString("\n")
	if len(left) > 0 {
		b.WriteString("Not included (the next Argo CD sync resets these to their Git values; with automated sync, right after merging):\n")
		for _, l := range left {
			fmt.Fprintf(&b, "- %s\n", singleLine(l.path))
		}
		b.WriteString("\n")
	}
	if rows := helmEditRows(dp); len(rows) > 0 {
		lastFile := ""
		for _, row := range rows {
			if row.file != lastFile {
				fmt.Fprintf(&b, "Value file: %s\n", singleLine(row.file))
				lastFile = row.file
			}
			fmt.Fprintf(&b, "- %s: %s -> %s\n", singleLine(row.key), singleLine(row.old), singleLine(row.new))
		}
		fmt.Fprintf(&b, "\n%s\n\n", helmVerifiedLine(dp, left))
		if lines := collateralLines(dp); len(lines) > 0 {
			b.WriteString("This edit also changes (allowed by the policy's spec.helm.allowCollateralChanges):\n")
			for _, l := range lines {
				fmt.Fprintf(&b, "- %s\n", l)
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "%s: %s\n", trailerProposal, dp.Name)
	fmt.Fprintf(&b, "%s: %s\n", trailerProposalUID, dp.UID)
	if a := dp.Spec.Actor; a != nil && singleLine(a.Username) != "" {
		fmt.Fprintf(&b, "%s: %s\n", trailerChangedBy, singleLine(a.Username))
	}
	return b.String()
}

// hasProposalTrailer reports whether a commit message was built for this
// proposal object: it carries both the name and the UID of the proposal. The
// name alone is not enough, since an earlier proposal for the same drift (the
// user deleted it to retry) has the same name.
func hasProposalTrailer(message string, dp *backflowv1alpha1.DriftProposal) bool {
	wantName := trailerProposal + ": " + dp.Name
	wantUID := trailerProposalUID + ": " + string(dp.UID)
	var name, uid bool
	for line := range strings.SplitSeq(message, "\n") {
		switch strings.TrimRight(line, "\r") {
		case wantName:
			name = true
		case wantUID:
			uid = true
		}
	}
	return name && uid && dp.UID != ""
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
func mergeRequestBody(dp *backflowv1alpha1.DriftProposal, left []leftOutField, diff string) string {
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
	for _, ch := range mappedChanges(dp, left) {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(ch.Path, "-"), cell(ch.Desired, "(absent)"), cell(ch.Live, "(removed)"))
	}

	if rows := helmEditRows(dp); len(rows) > 0 {
		b.WriteString("\n**Helm values**\n\n| File | Key | Old | New |\n|---|---|---|---|\n")
		for _, row := range rows {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", cell(row.file, "-"), cell(row.key, "-"), cell(row.old, "(none)"), cell(row.new, "(none)"))
		}
		fmt.Fprintf(&b, "\n%s\n", helmVerifiedLine(dp, left))
		if c := dp.Status.Mapping.Collateral; len(c) > 0 {
			b.WriteString("\n**This edit also changes** (allowed by the policy's `spec.helm.allowCollateralChanges`):\n\n")
			b.WriteString("| Resource | Path | Current | New |\n|---|---|---|---|\n")
			for _, ch := range c {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", cell(collateralResource(ch), "-"), cell(ch.Path, "-"), cell(ch.Current, "(absent)"), cell(ch.New, "(absent)"))
			}
		}
	}

	if len(left) > 0 {
		b.WriteString("\n**Not included in this merge request**\n\n| Path | Live value | Git value | Reason |\n|---|---|---|---|\n")
		byPath := make(map[string]backflowv1alpha1.FieldChange, len(dp.Spec.Changes))
		for _, ch := range dp.Spec.Changes {
			byPath[ch.Path] = ch
		}
		for _, l := range left {
			ch := byPath[l.path]
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", cell(l.path, "-"), cell(ch.Live, "(removed)"), cell(ch.Desired, "(absent)"), cell(l.reason+": "+l.message, "-"))
		}
		b.WriteString("\n> **Warning:** the next Argo CD sync of the Application resets the fields listed above to their Git values " +
			"(with automated sync, right after this merge request is merged), so those live changes are lost. " +
			"To keep one, port it to Git by hand, in this merge request or before it is merged.\n")
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

// closingComment tells the reviewers why Backflow closes a merge request.
// keptBranch, when set, names a branch that is left in place and why, because
// it holds more than the proposal's own commits.
func closingComment(dp *backflowv1alpha1.DriftProposal, keptBranch, why string) string {
	kept := ""
	if keptBranch != "" {
		kept = fmt.Sprintf("The branch `%s` was not deleted: %s.\n\n", singleLine(keptBranch), singleLine(why))
	}
	return fmt.Sprintf("Backflow is closing this merge request.\n\n%s\n\n%s"+
		"Nothing needs to be done: Git stays the source of truth. DriftProposal: `%s/%s`.\n",
		singleLine(dp.Status.Message), kept, dp.Namespace, dp.Name)
}

// liveRevertedComment tells reviewers why the cluster no longer shows the
// change while the merge request is still open.
func liveRevertedComment(revision string) string {
	return fmt.Sprintf("The cluster was reset to Git by an Argo CD sync of %s. "+
		"Merging this merge request makes the change permanent again.\n", singleLine(revision))
}

// helmRow is one edit of a value file, as shown to reviewers.
type helmRow struct{ file, key, old, new string }

// helmEditRows lists the edits of a proposal mapped with the HelmValues
// strategy: the value file, the key path, and the value before and after. The
// old value is the one the change expected in Git, which is the value the key
// held (candidates are found by that value). Empty for other strategies.
func helmEditRows(dp *backflowv1alpha1.DriftProposal) []helmRow {
	m := dp.Status.Mapping
	if m == nil || m.Strategy != backflowv1alpha1.StrategyHelmValues {
		return nil
	}
	rendered := func(raw string) string {
		if v, ok := scalarOf(raw); ok {
			return helmmap.RenderValue(v)
		}
		return raw
	}
	rows := make([]helmRow, 0, len(m.Edits))
	for _, e := range m.Edits {
		row := helmRow{file: e.File, key: e.Location, new: rendered(e.Value)}
		if i := int(e.ChangeIndex); i >= 0 && i < len(dp.Spec.Changes) {
			row.old = rendered(dp.Spec.Changes[i].Desired)
		}
		rows = append(rows, row)
	}
	return rows
}

// helmVerifiedLine says how a Helm edit was checked and what it changes.
func helmVerifiedLine(dp *backflowv1alpha1.DriftProposal, left []leftOutField) string {
	changes := mappedChanges(dp, left)
	paths := make([]string, len(changes))
	for i, ch := range changes {
		paths[i] = singleLine(ch.Path)
	}
	return fmt.Sprintf("Verified by rendering through Argo CD: this edit changes exactly the drifted fields (%s) and nothing else.",
		strings.Join(paths, ", "))
}

// collateralResource is "Kind namespace/name" of a resource that also changes.
func collateralResource(c backflowv1alpha1.CollateralChange) string {
	return resourceLabel(backflowv1alpha1.ResourceRef{Kind: c.Kind, Namespace: c.Namespace, Name: c.Name})
}

// collateralLines lists, one line each, the changes beyond the drifted fields
// of a proposal mapped with the HelmValues strategy.
func collateralLines(dp *backflowv1alpha1.DriftProposal) []string {
	m := dp.Status.Mapping
	if m == nil || m.Strategy != backflowv1alpha1.StrategyHelmValues {
		return nil
	}
	lines := make([]string, 0, len(m.Collateral))
	for _, c := range m.Collateral {
		lines = append(lines, fmt.Sprintf("%s %s: %s -> %s", singleLine(collateralResource(c)), singleLine(c.Path), singleLine(c.Current), singleLine(c.New)))
	}
	return lines
}
