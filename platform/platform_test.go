package platform

import (
	"runtime"
	"strings"
	"testing"
)

func TestArchFor(t *testing.T) {
	for _, tc := range []struct {
		goarch, binary, machine, console, imageArch, fwArch string
	}{
		{"amd64", "qemu-system-x86_64", "q35", "console=ttyS0", "amd64", "x86_64"},
		{"arm64", "qemu-system-aarch64", "virt", "console=ttyAMA0", "arm64", "aarch64"},
	} {
		got, err := archFor(tc.goarch)
		if err != nil {
			t.Fatalf("archFor(%q): unexpected error %v", tc.goarch, err)
		}
		if got.qemuBinary != tc.binary || got.machine != tc.machine ||
			got.console != tc.console || got.imageArch != tc.imageArch || got.fwArch != tc.fwArch {
			t.Errorf("archFor(%q) = %+v, want binary=%s machine=%s console=%s imageArch=%s fwArch=%s",
				tc.goarch, got, tc.binary, tc.machine, tc.console, tc.imageArch, tc.fwArch)
		}
	}
}

func TestArchForUnsupported(t *testing.T) {
	_, err := archFor("riscv64")
	if err == nil {
		t.Fatal("expected error for riscv64, got nil")
	}
	if !contains(err.Error(), "riscv64") {
		t.Errorf("error must name the detected arch, got: %v", err)
	}
}

// Detect is the only function here that reads the real host, so it is the only
// one whose wiring no fixture can check: every field could be resolved
// correctly and still be assigned to the wrong struct member. This runs it for
// real and cross-checks the result against the pieces it is built from. It
// skips where the host genuinely cannot run a VM (no QEMU, no accelerator, no
// firmware) — that is a fact about the machine, not a regression, and Detect's
// job there is precisely to refuse.
func TestDetectOnThisHost(t *testing.T) {
	p, err := Detect()
	if err != nil {
		t.Skipf("this host cannot launch a VM, which Detect reported as:\n%v", err)
	}

	ai, err := archFor(runtime.GOARCH)
	if err != nil {
		t.Fatalf("Detect succeeded on an arch archFor rejects: %v", err)
	}
	accel, err := accelFor(runtime.GOOS)
	if err != nil {
		t.Fatalf("Detect succeeded on an OS accelFor rejects: %v", err)
	}

	for _, c := range []struct{ field, got, want string }{
		{"QEMUBinary", p.QEMUBinary, ai.qemuBinary},
		{"Machine", p.Machine, ai.machine},
		{"ConsoleArg", p.ConsoleArg, ai.console},
		{"ImageArch", p.ImageArch, ai.imageArch},
		{"Accel", p.Accel, accel},
		{"CPU", p.CPU, "host"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	// Detect promising firmware that is not on disk is the old edk2Code()
	// failure mode: QEMU discovers it instead of us, several layers down.
	for _, f := range []struct{ field, path string }{
		{"FirmwareCode", p.FirmwareCode},
		{"FirmwareVars", p.FirmwareVars},
	} {
		if !fileExists(f.path) {
			t.Errorf("%s = %q, which is not a readable file", f.field, f.path)
		}
	}
	t.Logf("Detect() = %+v", *p)
}

// contains is a shared test helper used by platform_test.go, accel_test.go and
// firmware_test.go.
func contains(s, sub string) bool { return strings.Contains(s, sub) }
