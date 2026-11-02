package migrate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/sserkanml/backflow/internal/migrate/legacy"
)

// The manifests the tests use are objects the v1alpha1 API accepted: decoded strictly into its types, they have no field it
// did not have.
func TestFixturesAreV1alpha1Objects(t *testing.T) {
	fixtures := map[string]string{
		"connection": oldScm, "policy": policyYAML("p1", "30s"), "proposal": oldProposal, "open proposal": dpOpen,
		"merged": dpMergedYAML, "unmapped": dpUnmappedYAML, "superseded": dpSupersededYAML, "rejected": dpRejectedYAML,
	}
	for _, doc := range splitDocuments(teamManifests) {
		if strings.Contains(doc, "backflow.io/v1alpha1") {
			fixtures["team manifest "+strings.Fields(strings.SplitN(doc, "kind:", 2)[1])[0]] = strings.TrimPrefix(doc, "---\n")
		}
	}
	for name, text := range fixtures {
		t.Run(name, func(t *testing.T) {
			raw, err := yaml.YAMLToJSON([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			var kind struct{ Kind string }
			_ = json.Unmarshal(raw, &kind)
			var into any
			switch kind.Kind {
			case "ScmConnection":
				into = &legacy.ScmConnection{}
			case "BackflowPolicy":
				into = &legacy.BackflowPolicy{}
			case "DriftProposal":
				into = &legacy.DriftProposal{}
			default:
				t.Fatalf("kind %q", kind.Kind)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(into); err != nil {
				t.Errorf("not a v1alpha1 object: %v", err)
			}
		})
	}
}
