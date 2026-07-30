package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// specFromYAML decodes through the SAME path standalone() uses. That routing is
// the entire point: sigs.k8s.io/yaml goes via JSON, so `cpu: 4` arrives as
// float64. A hand-built map[string]interface{}{"cpu": int64(4)} would pass
// against the old broken .(int64) assertion and prove nothing.
func specFromYAML(t *testing.T, doc string) map[string]interface{} {
	t.Helper()
	var obj map[string]interface{}
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	spec, _, _ := unstructured.NestedMap(obj, "spec")
	return spec
}

func TestSpecCPU(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want int
	}{
		{"explicit", "spec:\n  cpu: 4\n", 4},
		{"absent", "spec:\n  memory: 2Gi\n", 2},
		{"zero", "spec:\n  cpu: 0\n", 2},
		{"non-numeric", "spec:\n  cpu: lots\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := specCPU(specFromYAML(t, tc.doc)); got != tc.want {
				t.Errorf("specCPU = %d, want %d", got, tc.want)
			}
		})
	}
}
