package platform

import (
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

// contains is a shared test helper used by platform_test.go, accel_test.go and
// firmware_test.go.
func contains(s, sub string) bool { return strings.Contains(s, sub) }
