package cluster

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update rewrites golden files instead of comparing against them:
//
//	go test ./cluster -run Golden -update
//
// A golden diff is a REVIEW ITEM, not noise to regenerate away. The files pin
// what a machine that uses none of the newer spec fields produces, and a change
// there is a change to every existing machine.
var update = flag.Bool("update", false, "rewrite golden files")

// goldenRedact is redact() without the lengths. A fresh bundle is minted per
// run and RSA key encodings vary by a byte between runs, so a length in the
// golden file would flake. Everything that is NOT a secret is still compared
// byte for byte, which is the part a spec change can move.
func goldenRedact(b []byte) []byte {
	s := string(b)
	for _, re := range secretShapes {
		s = re.ReplaceAllString(s, "<redacted>")
	}

	return []byte(s)
}

// compareGolden compares got with testdata/<name>, or rewrites it under -update.
// got must already be free of secrets: it is written to the repository and
// printed on a mismatch.
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
			"A machine that sets none of the newer spec fields must produce exactly this.\n"+
			"If the change is intended, review the diff and run with -update.\n\n got:\n%s",
			path, got)
	}
}

// TestGoldenSingleNodeVMConfig pins the machine config of a plain single-node
// VM: the ConfigInput that cluster.Up builds for a machine file with
// hostForwards and nothing else. Multi-node fields (cluster network, joins,
// hostname) must leave this byte-identical when they are absent.
//
// The Talos version is the one the multi-node work targets. A machinery bump
// moves this file too (the kubelet tag, new defaults), and that is the moment
// to read the diff.
func TestGoldenSingleNodeVMConfig(t *testing.T) {
	g := mustGenerate(t, ConfigInput{
		ClusterName:  "cp0",
		Endpoint:     "https://127.0.0.1:6443",
		APIAddress:   "127.0.0.1",
		TalosVersion: "v1.14.1",
		ConsoleArg:   "console=ttyS0",
		SystemDisk:   DiskRef{Serial: "talos-system"},
	})

	compareGolden(t, "single-node-vm.controlplane.yaml.golden", goldenRedact(g.ControlPlane))
}
