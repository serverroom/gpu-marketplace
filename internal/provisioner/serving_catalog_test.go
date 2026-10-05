package provisioner

import (
	"testing"

	"github.com/serverroom/gpu-marketplace/internal/serving"
)

// The serving catalog names machine kinds as strings, because this package will
// import it; every kind it names must be one Detect can report.
func TestServingCatalogKindsAreProvisionerKinds(t *testing.T) {
	known := map[string]bool{KindQEMUVFIO: true, KindQEMU: true, KindContainer: true}
	for _, m := range serving.Catalog {
		for _, k := range m.Kinds {
			if !known[k] {
				t.Errorf("%s names machine kind %q, which the provisioner does not report", m.ID, k)
			}
		}
	}
}
