package kustomize

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// Description is how a verified mapping reads in the status of a proposal and
// in a merge request.
type Description struct {
	// Strategy is KustomizePatch when the edits are a setting, a patch value
	// or a new patch, and Direct when they are the fields of a resource file.
	Strategy v1alpha1.MappingStrategy
	// Tiers are the tiers of the edits, without duplicates, in order.
	Tiers []int
	// Edited lists "file: key" of every edit; for an inline patch the key
	// starts with the pointer of the patch in the kustomization, and for a
	// JSON 6902 patch it is a JSON pointer.
	Edited []string
	// AddedFiles are the new patch files.
	AddedFiles []string
	// SharedFiles are the edited files outside the Application's path.
	SharedFiles []string
	// Users maps a shared file to the other Applications of Backflow's
	// policies that build it.
	Users map[string][]string
	// Lines are short sentences for the merge request, one per kind of edit.
	Lines []string
}

// Describe describes the edits of an outcome.
func Describe(out *rendermap.Outcome) Description {
	d := Description{Strategy: v1alpha1.StrategyDirect}
	for _, a := range out.Applied {
		e, ok := a.Candidate.Meta.(*Edit)
		if !ok {
			continue
		}
		if e.Kind != KindResource {
			d.Strategy = v1alpha1.StrategyKustomizePatch
		}
		if !slices.Contains(d.Tiers, e.Tier) {
			d.Tiers = append(d.Tiers, e.Tier)
		}
		d.Edited = append(d.Edited, a.Candidate.File+": "+a.Candidate.Path.String())
		if e.Kind == KindNewPatch && a.Candidate.NewFile && !slices.Contains(d.AddedFiles, a.Candidate.File) {
			d.AddedFiles = append(d.AddedFiles, a.Candidate.File)
		}
		if e.Shared && !slices.Contains(d.SharedFiles, a.Candidate.File) {
			d.SharedFiles = append(d.SharedFiles, a.Candidate.File)
			if len(e.Users) > 0 {
				if d.Users == nil {
					d.Users = map[string][]string{}
				}
				d.Users[a.Candidate.File] = e.Users
			}
		}
		line := tierLine(e)
		if !slices.Contains(d.Lines, line) {
			d.Lines = append(d.Lines, line)
		}
	}
	slices.Sort(d.Tiers)
	return d
}

func tierLine(e *Edit) string {
	switch e.Tier {
	case TierSetting:
		return "Changes " + e.Note + ", a setting of the kustomization."
	case TierPatch:
		return "Changes the value in the existing patch: " + e.Note + "."
	case TierResource:
		return "Changes the resource file: " + e.Note + "."
	case TierBase:
		return "Changes a file outside the Application's path: " + e.Note + "."
	}
	return "Adds a new patch file: " + e.Note + "."
}

// Text is the explanation for a merge request or commit: what kind of edit
// each one is, and a warning for every file outside the Application's path.
func (d Description) Text() string {
	var b strings.Builder
	for _, l := range d.Lines {
		b.WriteString(l + "\n")
	}
	if len(d.AddedFiles) > 0 {
		fmt.Fprintf(&b, "\nA new patch file was added (%s) and listed in the kustomization of the Application.\n", strings.Join(d.AddedFiles, ", "))
	}
	for _, f := range d.SharedFiles {
		fmt.Fprintf(&b, "\nWarning: %s is outside the Application's path.", f)
		if apps := d.Users[f]; len(apps) > 0 {
			fmt.Fprintf(&b, " Merging this also changes: %s at their next sync.", strings.Join(apps, ", "))
		}
		b.WriteString(" Other Applications outside Backflow's policies may use this file.\n")
	}
	return strings.TrimSpace(b.String())
}
