package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/coglative/talos-in-qemu/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Detect must stay INSIDE create, never hoisted to main: on a host with no
// usable accelerator Detect fails, and teardown of an already-created VM must
// still work. Destroy touching it would make cleanup require a working
// hypervisor — the one thing that must never need one.
func TestDestroyDoesNotProbeThePlatform(t *testing.T) {
	h := &hvf{
		stateRoot: t.TempDir(),
		imageRoot: t.TempDir(),
		detect: func() (*platform.Platform, error) {
			t.Error("Destroy must not probe the host platform")
			return nil, fmt.Errorf("no accelerator on this host")
		},
	}
	m := &unstructured.Unstructured{Object: map[string]interface{}{}}
	m.SetUID("bootstrap-default-gone")
	if err := h.Destroy(context.Background(), m); err != nil {
		t.Fatalf("Destroy of an absent machine must succeed: %v", err)
	}
}

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
