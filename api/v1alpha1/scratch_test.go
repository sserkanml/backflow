package v1alpha1

import "testing"

func TestScratchBranchesAllowed(t *testing.T) {
	mapping := func(m ScratchBranchesMode) *MappingOptions { return &MappingOptions{ScratchBranches: m} }
	tests := []struct {
		name string
		spec BackflowPolicySpec
		want bool
	}{
		{"Auto by default, MergeRequest", BackflowPolicySpec{Mode: ModeMergeRequest}, true},
		{"Auto by default, DirectCommit", BackflowPolicySpec{Mode: ModeDirectCommit}, true},
		{"Auto by default, ReportOnly", BackflowPolicySpec{Mode: ModeReportOnly}, false},
		{"Auto spelled out, ReportOnly", BackflowPolicySpec{Mode: ModeReportOnly, Mapping: mapping(ScratchBranchesAuto)}, false},
		{"Always, ReportOnly", BackflowPolicySpec{Mode: ModeReportOnly, Mapping: mapping(ScratchBranchesAlways)}, true},
		{"Never, MergeRequest", BackflowPolicySpec{Mode: ModeMergeRequest, Mapping: mapping(ScratchBranchesNever)}, false},
		{"mapping without the field, MergeRequest", BackflowPolicySpec{Mode: ModeMergeRequest, Mapping: &MappingOptions{AllowPartial: true}}, true},
	}
	for _, tt := range tests {

		if got := tt.spec.ScratchBranchesAllowed(); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}
