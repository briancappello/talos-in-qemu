package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
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

// dataDisk is OPTIONAL and its absence is load-bearing: an unset field must
// produce exactly the machine this tool produced before the field existed. ""
// is the signal for "no second disk", so the empty string and the absent key
// have to arrive here as the same thing.
func TestSpecDataDisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{"set", "spec:\n  dataDisk: 40Gi\n", "40Gi"},
		{"absent", "spec:\n  disk: 20Gi\n", ""},
		{"empty", "spec:\n  dataDisk: \"\"\n", ""},
		// spec.disk must not leak into spec.dataDisk: reading the wrong key
		// would give every single-disk machine a second disk.
		{"disk-only-is-not-a-data-disk", "spec:\n  disk: 20Gi\n  cpu: 4\n", ""},
		// -apply reads this YAML with NO API server in front of it, so the
		// CRD's `type: string` never validates. `dataDisk: 40` (unit omitted)
		// decodes as float64 and must read as "not set" — never a panic, and
		// never a silently coerced 40-byte disk.
		{"unquoted-number-is-not-a-size", "spec:\n  dataDisk: 40\n", ""},
		{"bool-is-not-a-size", "spec:\n  dataDisk: true\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := specDataDisk(specFromYAML(t, tc.doc)); got != tc.want {
				t.Errorf("specDataDisk = %q, want %q", got, tc.want)
			}
		})
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

// requireQEMUImg skips rather than fails: qemu-img is a hard runtime dependency
// of this tool, but a reviewer reading the code on a box without QEMU should not
// see a red suite. Everything it gates is exercised on a host that has it.
func requireQEMUImg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not on PATH")
	}
}

// qcow2VirtualSize reads the virtual size straight out of the qcow2 header
// (magic "QFI\xfb", then big-endian size at offset 24) instead of parsing
// `qemu-img info` prose. The header is a format contract; the prose is not.
func qcow2VirtualSize(t *testing.T, path string) uint64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 32 || string(b[:4]) != "QFI\xfb" {
		t.Fatalf("%s is not a qcow2 image", path)
	}
	return binary.BigEndian.Uint64(b[24:32])
}

// The disk is created ONCE and then never touched again. That is not an
// optimisation: system.qcow2 holds the installed OS and data.qcow2 holds the
// user's PVCs, so a re-create that truncated either would silently destroy a
// running machine on the next reconcile tick — and create() runs on EVERY tick
// where Observe reports absent.
func TestEnsureQcow2(t *testing.T) {
	t.Run("creates-at-the-requested-size", func(t *testing.T) {
		requireQEMUImg(t)
		path := filepath.Join(t.TempDir(), "data.qcow2")
		// Kubernetes says 64Mi, qemu-img says 64M and rejects the "i"
		// outright ("Invalid image size specified!"), so the suffix has to be
		// trimmed — and both spellings mean the same power-of-two bytes, so
		// trimming is exact rather than a rounding.
		if err := ensureQcow2(path, "64Mi"); err != nil {
			t.Fatalf("ensureQcow2: %v", err)
		}
		if got, want := qcow2VirtualSize(t, path), uint64(64<<20); got != want {
			t.Errorf("virtual size = %d, want %d", got, want)
		}
	})

	t.Run("reports-a-qemu-img-failure", func(t *testing.T) {
		requireQEMUImg(t)
		path := filepath.Join(t.TempDir(), "data.qcow2")
		// qemu-img is a trust boundary: it is where a malformed spec.disk
		// lands. Swallowing its exit status would launch the VM against a disk
		// that was never created, and the failure would surface as an
		// unexplained qemu error instead of the bad quantity that caused it.
		err := ensureQcow2(path, "banana")
		if err == nil {
			t.Fatal("a rejected image size must be an error")
		}
		if !strings.Contains(err.Error(), "qemu-img") {
			t.Errorf("error must name qemu-img, got: %v", err)
		}
	})

	t.Run("leaves-an-existing-image-alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "system.qcow2")
		// Deliberately NOT a valid qcow2: if the guard is removed, qemu-img
		// overwrites this and the content check fails. No qemu-img needed for
		// the branch that must not reach it.
		if err := os.WriteFile(path, []byte("the installed OS"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureQcow2(path, "64Mi"); err != nil {
			t.Fatalf("ensureQcow2: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "the installed OS" {
			t.Errorf("an existing disk was rewritten (%q) — that is the installed "+
				"system, or the user's PVCs, gone", b)
		}
	})

	t.Run("reports-a-non-ENOENT-stat-error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: an unreadable directory is still readable")
		}
		dir := filepath.Join(t.TempDir(), "locked")
		if err := os.Mkdir(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		// t.TempDir's cleanup has to be able to descend into it again.
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		// EACCES is not "already there". Reading it as such skips creation and
		// then launches QEMU against a disk that was never made, surfacing as
		// an unexplained qemu error instead of the permission problem.
		if err := ensureQcow2(filepath.Join(dir, "data.qcow2"), "64Mi"); err == nil {
			t.Fatal("an unreadable parent directory must be an error, not a silent skip")
		}
	})
}

// fakeQEMU stands in for the hypervisor so the ARG LIST — which is the real
// product of create() — can be asserted without booting anything. It records
// its argv one arg per line and honours the pidfile contract create() reads
// back, so create() completes exactly as it would against qemu.
func fakeQEMU(t *testing.T, dir string) (bin string, argv func() []string) {
	t.Helper()
	bin = filepath.Join(dir, "fake-qemu")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$0.args\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = -pidfile ]; then echo 4242 > \"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() []string {
		b, err := os.ReadFile(bin + ".args")
		if err != nil {
			t.Fatalf("fake qemu recorded no args: %v", err)
		}
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
}

const machineDoc = `apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata:
  name: cp0
  namespace: default
spec:
  site: testsite
  role: talos-cp
  image: talos.iso
  cpu: 4
  memory: 2Gi
  disk: 64Mi
  hostForwards:
    - hostPort: 50000
      guestPort: 50000
`

// The qemu argv is asserted WHOLE, position by position, not by substring.
// Order is semantics here — bootindex decides the install lifecycle, and a
// -device that drifts away from the -drive it names is a machine that does not
// start. A Contains-based test passes happily through both mistakes.
//
// The no-dataDisk case is the branch's hard constraint: it must be exactly the
// argv this tool emitted before dataDisk existed, plus serial= and nothing else.
func TestCreateQEMUArgs(t *testing.T) {
	requireQEMUImg(t)
	for _, tc := range []struct {
		name, doc string
		dataDisk  bool
		sysSize   uint64
	}{
		{"without-dataDisk", machineDoc, false, 64 << 20},
		{"with-dataDisk", machineDoc + "  dataDisk: 32Mi\n", true, 64 << 20},
		// The CRD marks spec.disk required, but -apply reads a file with NO
		// API server in front of it, so nothing validates that schema on the
		// bootstrap path. The default is reachable, therefore it is pinned.
		{"disk-unset-falls-back-to-16Gi", strings.Replace(machineDoc, "  disk: 64Mi\n", "", 1), false, 16 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, imageRoot := t.TempDir(), t.TempDir()
			fw := t.TempDir()
			code := filepath.Join(fw, "OVMF_CODE.fd")
			vars := filepath.Join(fw, "OVMF_VARS.fd")
			writeSized(t, code, 1024, 'C')
			writeSized(t, vars, x86VarsSize, 'T')
			image := filepath.Join(imageRoot, "talos.iso")
			writeSized(t, image, 4096, 'I')
			bin, argv := fakeQEMU(t, fw)

			h := &hvf{
				stateRoot: root,
				imageRoot: imageRoot,
				detect: func() (*platform.Platform, error) {
					return &platform.Platform{
						QEMUBinary: bin, Machine: "q35", Accel: "kvm", CPU: "host",
						FirmwareCode: code, FirmwareVars: vars,
						ConsoleArg: "console=ttyS0", ImageArch: "amd64",
					}, nil
				},
			}
			var obj map[string]interface{}
			if err := yaml.Unmarshal([]byte(tc.doc), &obj); err != nil {
				t.Fatal(err)
			}
			m := &unstructured.Unstructured{Object: obj}
			m.SetUID("bootstrap-default-cp0")
			dir := h.dir(m)

			pid, err := h.create(m, dir)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if pid != 4242 {
				t.Errorf("pid = %d, want the 4242 the fake wrote to -pidfile", pid)
			}

			want := []string{
				"-machine", "q35,accel=kvm", "-cpu", "host",
				"-smp", "4",
				"-m", "2048",
				"-drive", "if=pflash,format=raw,readonly=on,file=" + code,
				"-drive", "if=pflash,format=raw,file=" + filepath.Join(dir, "efivars.fd"),
				"-drive", "if=none,id=sys,format=qcow2,file=" + filepath.Join(dir, "system.qcow2"),
				"-device", "virtio-blk-pci,drive=sys,serial=talos-system,bootindex=0",
				"-drive", "if=none,id=cd,media=cdrom,file=" + image,
				"-device", "virtio-blk-pci,drive=cd,bootindex=1",
				"-netdev", "user,id=n0,hostfwd=tcp:127.0.0.1:50000-:50000",
				"-device", "virtio-net-pci,netdev=n0",
				"-display", "none",
				"-serial", "file:" + filepath.Join(dir, "serial.log"),
				"-pidfile", filepath.Join(dir, "qemu.pid"),
				"-daemonize",
			}
			dataPath := filepath.Join(dir, "data.qcow2")
			if tc.dataDisk {
				want = append(want,
					"-drive", "if=none,id=data,format=qcow2,file="+dataPath,
					"-device", "virtio-blk-pci,drive=data,serial=talos-data")
			}

			got := argv()
			if len(got) != len(want) {
				t.Fatalf("argv has %d args, want %d\n got: %q\nwant: %q", len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
				}
			}

			// The data disk must never be a BOOT CANDIDATE. Firmware walks
			// every bootindex it is given; hand one to the PVC disk and a
			// blank-disk machine can try to boot the wrong device — and the
			// install-loop halt this repo already documents is exactly what a
			// boot-order mistake looks like from the console.
			for _, a := range got {
				if strings.Contains(a, "drive=data") && strings.Contains(a, "bootindex") {
					t.Errorf("the data disk carries a bootindex (%q) — it must never "+
						"be a boot candidate", a)
				}
			}

			// Each disk must be sized from ITS OWN spec field. The sizes never
			// reach the argv, so nothing above would notice the two being
			// crossed — and a data disk silently sized from spec.disk is a
			// StorageClass that runs out of room for no visible reason.
			if got := qcow2VirtualSize(t, filepath.Join(dir, "system.qcow2")); got != tc.sysSize {
				t.Errorf("system.qcow2 virtual size = %d, want %d (spec.disk)", got, tc.sysSize)
			}
			if _, err := os.Stat(dataPath); tc.dataDisk != (err == nil) {
				t.Errorf("data.qcow2 present=%v, want %v", err == nil, tc.dataDisk)
			} else if err == nil {
				if got, want := qcow2VirtualSize(t, dataPath), uint64(32<<20); got != want {
					t.Errorf("data.qcow2 virtual size = %d, want %d (spec.dataDisk)", got, want)
				}
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
