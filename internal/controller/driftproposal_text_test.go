package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
)

func textProposal() *backflowv1alpha1.DriftProposal {
	return &backflowv1alpha1.DriftProposal{
		ObjectMeta: metav1.ObjectMeta{Name: "dp-abc", Namespace: "backflow-system"},
		Spec: backflowv1alpha1.DriftProposalSpec{
			Application: backflowv1alpha1.ObjectRef{Name: "demo-app", Namespace: "argocd"},
			Resource:    backflowv1alpha1.ResourceRef{Version: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "demo-config"},
			Changes: []backflowv1alpha1.FieldChange{
				{Path: "/data/LOG_LEVEL", Op: backflowv1alpha1.OpReplace, Desired: `"info"`, Live: `"debug"`},
				{Path: "/data/NEW", Op: backflowv1alpha1.OpAdd, Live: `"x"`},
				{Path: "/data/OLD", Op: backflowv1alpha1.OpRemove, Desired: `"y"`},
			},
			Actor:      &backflowv1alpha1.Actor{Username: "alice", UserAgent: "kubectl/v1.30"},
			DetectedAt: metav1.NewTime(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)),
		},
	}
}

func TestCommitSubject(t *testing.T) {
	dp := textProposal()
	if got, want := commitSubject(dp), "backflow: sync ConfigMap demo/demo-config from cluster"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	dp.Spec.Resource = backflowv1alpha1.ResourceRef{Kind: "ClusterRole", Name: "viewer"}
	if got, want := commitSubject(dp), "backflow: sync ClusterRole viewer from cluster"; got != want {
		t.Errorf("cluster-scoped subject = %q, want %q", got, want)
	}
}

func TestCommitMessage(t *testing.T) {
	want := "backflow: sync ConfigMap demo/demo-config from cluster\n\n" +
		"Changed paths:\n- /data/LOG_LEVEL\n- /data/NEW\n- /data/OLD\n\n" +
		"Proposal: dp-abc\nChanged-by: alice\n"
	if got := textProposal(); commitMessage(got) != want {
		t.Errorf("message =\n%q\nwant\n%q", commitMessage(got), want)
	}

	t.Run("no actor, no trailer", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Actor = nil
		if got := commitMessage(dp); strings.Contains(got, trailerChangedBy) || !strings.Contains(got, "Proposal: dp-abc\n") {
			t.Errorf("message = %q", got)
		}
	})
	t.Run("hostile actor cannot inject trailers", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Actor.Username = "mallory\nProposal: other\nSigned-off-by: root"
		got := commitMessage(dp)
		if strings.Contains(got, "\nSigned-off-by") || hasProposalTrailer(got, "other") {
			t.Errorf("injection through the username: %q", got)
		}
	})
}

func TestHasProposalTrailer(t *testing.T) {
	msg := commitMessage(textProposal())
	if !hasProposalTrailer(msg, "dp-abc") {
		t.Error("own trailer not found")
	}
	for _, other := range []string{"dp-ab", "dp-abcd", "dp-abc "} {
		if hasProposalTrailer(msg, other) {
			t.Errorf("%q matched", other)
		}
	}
	if !hasProposalTrailer("subject\r\n\r\nProposal: dp-abc\r\n", "dp-abc") {
		t.Error("CRLF message not matched")
	}
	if hasProposalTrailer("subject only", "dp-abc") {
		t.Error("matched a message without trailer")
	}
}

func TestMergeRequestBody(t *testing.T) {
	dp := textProposal()
	diff := "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n"
	body := mergeRequestBody(dp, diff)
	for _, want := range []string{
		"**Resource:** ConfigMap demo/demo-config",
		"**Application:** argocd/demo-app",
		"**Changed by:** alice (kubectl/v1.30)",
		"**Detected:** 2026-03-04 05:06:07 UTC",
		"| Path | Git value | Live value |",
		"| ` /data/LOG_LEVEL ` | ` \"info\" ` | ` \"debug\" ` |",
		"| ` /data/NEW ` | (absent) | ` \"x\" ` |",
		"| ` /data/OLD ` | ` \"y\" ` | (removed) |",
		"```diff\n" + diff + "```\n",
		"DriftProposal: `backflow-system/dp-abc`",
		"makes the live change permanent",
		"lets Argo CD revert the cluster",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}

	t.Run("unknown actor", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Actor = nil
		if got := mergeRequestBody(dp, diff); !strings.Contains(got, "**Changed by:** unknown") {
			t.Errorf("body = %s", got)
		}
	})
	t.Run("no diff, no code block", func(t *testing.T) {
		if got := mergeRequestBody(dp, ""); strings.Contains(got, "```") {
			t.Errorf("body = %s", got)
		}
	})
	t.Run("a diff holding backticks cannot close the fence", func(t *testing.T) {
		tricky := "+  note: ```\n+  more\n"
		got := mergeRequestBody(dp, tricky)
		if !strings.Contains(got, "````diff\n"+tricky+"````\n") {
			t.Errorf("body = %s", got)
		}
	})
	t.Run("hostile values stay in their cell", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Changes[0].Live = "\"a|b`c\"\n| injected | row |"
		got := mergeRequestBody(dp, "")
		if strings.Count(got, "\n|") != 5 { // header, separator and three change rows
			t.Errorf("table rows broke:\n%s", got)
		}
		if !strings.Contains(got, `a\|b`) {
			t.Errorf("pipe not escaped:\n%s", got)
		}
	})
	t.Run("long values are cut", func(t *testing.T) {
		dp := textProposal()
		dp.Spec.Changes[0].Live = `"` + strings.Repeat("x", 5000) + `"`
		if got := mergeRequestBody(dp, ""); len(got) > 3000 || !strings.Contains(got, "...") {
			t.Errorf("len = %d", len(got))
		}
	})
}

func TestSingleLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":            "plain",
		"  a \t b\n c\r\n": "a b c",
		"bell\x07here":     "bell here",
		"":                 "",
	} {
		if got := singleLine(in); got != want {
			t.Errorf("singleLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClosingCommentMentionsAKeptBranch(t *testing.T) {
	dp := &backflowv1alpha1.DriftProposal{}
	dp.Name, dp.Namespace = "dp", "ns"
	dp.Status.Message = "A newer drift replaced this proposal."
	if got := closingComment(dp, "", ""); strings.Contains(got, "not deleted") {
		t.Errorf("no branch kept, but the comment says so:\n%s", got)
	}
	got := closingComment(dp, "backflow/dp", "someone else pushed to it")
	if !strings.Contains(got, "The branch `backflow/dp` was not deleted: someone else pushed to it.") {
		t.Errorf("comment = %q", got)
	}
}
