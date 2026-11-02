package migrate

import (
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/sserkanml/backflow/config/crd"
)

// releaseCRD reads the CRD of this release from the binary.
func releaseCRD(name string) (*apiextensionsv1.CustomResourceDefinition, error) {
	plural, group, ok := strings.Cut(name, ".")
	if !ok {
		return nil, fmt.Errorf("%q is not the name of a CRD", name)
	}
	data, err := crd.Bases.ReadFile("bases/" + group + "_" + plural + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("this release has no CRD %s: %w", name, err)
	}
	out := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("the CRD %s of this release: %w", name, err)
	}
	return out, nil
}
