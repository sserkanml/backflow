package chart

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type workflowFile struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string         `json:"name"`
			Uses string         `json:"uses"`
			Run  string         `json:"run"`
			With map[string]any `json:"with"`
		} `json:"steps"`
	} `json:"jobs"`
}

// hack/olm-catalog.sh lists the earlier releases from the git tags and refuses a clone that does not have them (a
// shallow one would leave versions out without a word). So every job that renders the catalog, directly or through a
// script that does (hack/test-olm.sh), must check out all the tags: actions/checkout with fetch-depth: 0 and
// fetch-tags: true. actionlint cannot know this; the kind job of olm.yml failed on it once.
func TestWorkflowsThatRenderTheCatalogFetchAllTheTags(t *testing.T) {
	// What reaches the catalog: the script, make catalog, and the scripts of hack/ that call either.
	reach := []string{"olm-catalog.sh", "make catalog", "make -C \"$ROOT\" catalog"}
	scripts, _ := filepath.Glob("../../hack/*.sh")
	for _, s := range scripts {
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range reach[:3] {
			if strings.Contains(string(data), token) && filepath.Base(s) != "olm-catalog.sh" {
				reach = append(reach, filepath.Base(s))
				break
			}
		}
	}
	workflows, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil || len(workflows) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	found := 0
	for _, w := range workflows {
		data, err := os.ReadFile(w)
		if err != nil {
			t.Fatal(err)
		}
		var wf workflowFile
		if err := yaml.Unmarshal(data, &wf); err != nil {
			t.Fatalf("%s: %v", w, err)
		}
		for name, job := range wf.Jobs {
			reaches := false
			for _, step := range job.Steps {
				for _, token := range reach {
					if regexp.MustCompile(regexp.QuoteMeta(token)).MatchString(step.Run) {
						reaches = true
					}
				}
			}
			if !reaches {
				continue
			}
			found++
			fetched := false
			for _, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, "actions/checkout@") {
					continue
				}
				depth, tags := step.With["fetch-depth"], step.With["fetch-tags"]
				if (depth == 0 || depth == float64(0)) && tags == true {
					fetched = true
				}
			}
			if !fetched {
				t.Errorf("%s, job %s: it renders the catalog (hack/olm-catalog.sh) but its actions/checkout has not got "+
					"fetch-depth: 0 and fetch-tags: true, so the script refuses the clone", filepath.Base(w), name)
			}
		}
	}
	if found < 3 {
		t.Errorf("found %d jobs that render the catalog (release.yml, image-main.yml, olm.yml): "+
			"the test no longer recognises them", found)
	}
}
