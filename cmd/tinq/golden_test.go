package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coglative/talos-in-qemu/driverkit"
	"github.com/coglative/talos-in-qemu/platform"
)

// update rewrites golden files instead of comparing against them:
//
//	go test ./cmd/tinq -run Golden -update
//
// A golden diff is a REVIEW ITEM. These files pin what an existing single-node
// machine file produces, and a change there is a change to every machine
// already on disk.
var update = flag.Bool("update", false, "rewrite golden files")

// compareGolden compares got with testdata/<name>, or rewrites it under -update.
func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (run with -update to create it): %v", path, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file.\n\n"+
			"An existing single-node machine file must produce exactly this.\n"+
			"If the change is intended, review the diff and run with -update.\n\n got:\n%s",
			path, got)
	}
}

// goldenHost is a fixed host, so the golden files do not depend on the
// machine the test runs on. The temp paths are replaced by placeholders.
type goldenHost struct {
	h                *hvf
	root, images, fw string
	code, vars, qemu string
	argv             func() []string
}

func newGoldenHost(t *testing.T, image string) *goldenHost {
	t.Helper()

	g := &goldenHost{root: t.TempDir(), images: t.TempDir(), fw: t.TempDir()}
	g.code = filepath.Join(g.fw, "OVMF_CODE.fd")
	g.vars = filepath.Join(g.fw, "OVMF_VARS.fd")
	writeSized(t, g.code, 1024, 'C')
	writeSized(t, g.vars, x86VarsSize, 'T')
	writeSized(t, filepath.Join(g.images, image), 4096, 'I')
	g.qemu, g.argv = fakeQEMU(t, g.fw)

	g.h = &hvf{
		stateRoot: g.root,
		imageRoot: g.images,
		detect: func() (*platform.Platform, error) {
			return &platform.Platform{
				OS: "linux", QEMUBinary: g.qemu, Machine: "q35", Accel: "kvm", CPU: "host",
				FirmwareCode: g.code, FirmwareVars: g.vars,
				ConsoleArg: "console=ttyS0", ImageArch: "amd64",
			}, nil
		},
	}

	return g
}

// scrub replaces every temp path with a stable placeholder.
func (g *goldenHost) scrub(s string) string {
	return strings.NewReplacer(
		g.root, "<STATE_ROOT>",
		g.images, "<IMAGE_ROOT>",
		g.fw, "<FIRMWARE>",
	).Replace(s)
}

// TestGoldenSingleNodeMachineFile pins what the shipped single-node example
// produces: the whole QEMU argv, and the UpOptions that become its machine
// config. The file is read from examples/ on purpose. It is a machine file an
// operator already has, and it sets none of the multi-node fields
// (clusterNetwork, joins, hostname), so it must not move when they are added.
//
// The config bytes themselves are pinned in cluster/golden_test.go, from the
// ConfigInput these options produce.
func TestGoldenSingleNodeMachineFile(t *testing.T) {
	requireQEMUImg(t)

	m, err := readMachine(filepath.Join("..", "..", "examples", "bootstrap-machine.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// Sizes are shrunk so the test does not write 60 GiB of sparse qcow2.
	// They never reach the argv, so the golden is unaffected.
	spec := m.Object["spec"].(map[string]interface{})
	spec["disk"], spec["dataDisk"] = "64Mi", "32Mi"

	g := newGoldenHost(t, "talos-v1.13.7-amd64.iso")

	if _, err := g.h.create(m, g.h.dir(m)); err != nil {
		t.Fatalf("create: %v", err)
	}

	compareGolden(t, "bootstrap-machine.qemu-argv.golden",
		[]byte(g.scrub(strings.Join(g.argv(), "\n"))+"\n"))

	opts, err := upOptions(g.h, m, driverkit.Absent, nil)
	if err != nil {
		t.Fatalf("upOptions: %v", err)
	}

	if opts.Boot == nil {
		t.Fatal("upOptions returned no Boot function")
	}

	compareGolden(t, "bootstrap-machine.upoptions.golden", []byte(g.scrub(valueFields(t, opts))+"\n"))
}

// valueFields renders every exported, non-func field of v, one per line in
// declaration order, each value JSON-encoded. It goes by reflection so that a field added to the struct
// later appears in the golden file without anyone remembering to list it: a
// new field that is not zero for an old machine file is exactly the drift the
// golden exists to catch. Funcs (Boot) and interfaces (Out) are skipped; their
// behavior is covered by the bring-up tests.
func valueFields(t *testing.T, v any) string {
	t.Helper()

	rv := reflect.ValueOf(v)

	var buf bytes.Buffer

	buf.WriteString("{\n")

	for i := range rv.NumField() {
		f := rv.Type().Field(i)
		if !f.IsExported() || f.Type.Kind() == reflect.Func || f.Type.Kind() == reflect.Interface {
			continue
		}

		b, err := json.Marshal(rv.Field(i).Interface())
		if err != nil {
			t.Fatalf("field %s: %v", f.Name, err)
		}

		fmt.Fprintf(&buf, "  %q: %s\n", f.Name, b)
	}

	buf.WriteString("}")

	return buf.String()
}
