package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// The comment on toInt claims int64 is "what the API server path needs":
	// unstructured values from a real client are int64, not the float64 the
	// YAML decoder produces. Nothing above reaches that case, so it is pinned
	// directly — the two arms of toInt must BOTH work or one caller breaks.
	if got := specCPU(map[string]interface{}{"cpu": int64(4)}); got != 4 {
		t.Errorf("specCPU(int64(4)) = %d, want 4 (the API-server path)", got)
	}
}

const (
	// The real x86_64 edk2 vars template size, and the size the padding version
	// of this tool wrote. On x86_64 the poisoned file is what makes QEMU refuse
	// to start with "combined size of system firmware exceeds 8388608 bytes".
	x86VarsSize  = 540672
	poisonedSize = 64 << 20
)

// writeSized writes a file of exactly n bytes whose content is identifiable, so
// a test can tell "left alone" from "rewritten to something the same size".
func writeSized(t *testing.T, path string, n int64, fill byte) {
	t.Helper()
	if err := os.WriteFile(path, []byte{fill}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, n); err != nil {
		t.Fatal(err)
	}
}

// The efivars size-heal is the fix this branch shipped without a test, and it
// has to be right in BOTH directions: heal a poisoned file, and never touch a
// good one. Regenerating unconditionally would silently discard the guest's own
// UEFI boot entries, which is real state and does not come back.
func TestEnsureEFIVars(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, path string)
		rewrite bool
	}{
		{"absent", func(*testing.T, string) {}, true},
		{"poisoned-64MiB", func(t *testing.T, p string) {
			writeSized(t, p, poisonedSize, 'P')
		}, true},
		{"matching-size", func(t *testing.T, p string) {
			writeSized(t, p, x86VarsSize, 'G')
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tmpl := filepath.Join(dir, "OVMF_VARS.fd")
			writeSized(t, tmpl, x86VarsSize, 'T')
			vars := filepath.Join(dir, "efivars.fd")
			tc.setup(t, vars)

			if err := ensureEFIVars(vars, tmpl); err != nil {
				t.Fatalf("ensureEFIVars: %v", err)
			}
			st, err := os.Stat(vars)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if st.Size() != x86VarsSize {
				t.Errorf("size = %d, want the template's %d", st.Size(), x86VarsSize)
			}
			// First byte identifies the SOURCE: 'T' means it came from the
			// template, anything else means the pre-existing file survived.
			b := make([]byte, 1)
			f, err := os.Open(vars)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Read(b); err != nil {
				t.Fatal(err)
			}
			if tc.rewrite && b[0] != 'T' {
				t.Errorf("file was not regenerated from the template (first byte %q)", b[0])
			}
			if !tc.rewrite && b[0] != 'G' {
				t.Errorf("a same-size file must be left ALONE — the guest's UEFI "+
					"boot entries live in it; first byte %q", b[0])
			}
		})
	}
}

// Detect resolves FirmwareVars by statting it, so this is unreachable in
// practice — but it is the difference between a named error and a silent
// nil-deref if that ever stops holding.
func TestEnsureEFIVarsMissingTemplate(t *testing.T) {
	dir := t.TempDir()
	err := ensureEFIVars(filepath.Join(dir, "efivars.fd"), filepath.Join(dir, "absent.fd"))
	if err == nil {
		t.Fatal("a missing nvram template must be an error")
	}
	if !strings.Contains(err.Error(), "absent.fd") {
		t.Errorf("error must name the template, got: %v", err)
	}
}
