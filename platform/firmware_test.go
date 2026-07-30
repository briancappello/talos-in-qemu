package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// `-machine q35` is an ALIAS of pc-q35-11.0, but descriptors glob on
// "pc-q35-*". A plain filepath.Match misses for exactly the two machine types
// we use, silently defeating registry discovery.
func TestMachineMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, machine string
		want             bool
	}{
		{"pc-q35-*", "q35", true},
		{"virt-*", "virt", true},
		{"virt", "virt", true},
		{"pc-q35-*", "pc-q35-11.0", true},
		{"pc-i440fx-*", "q35", false},
		{"pc-q35-*", "virt", false},
	} {
		if got := machineMatches(tc.pattern, tc.machine); got != tc.want {
			t.Errorf("machineMatches(%q,%q)=%v want %v", tc.pattern, tc.machine, got, tc.want)
		}
	}
}

func writeDesc(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const secureDesc = `{"description":"secure","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/SEC_CODE.fd"},
"nvram-template":{"filename":"/fw/SEC_VARS.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":["requires-smm","secure-boot"]}`

const plainDesc = `{"description":"plain","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/OVMF_CODE.4m.fd"},
"nvram-template":{"filename":"/fw/OVMF_VARS.4m.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-i440fx-*","pc-q35-*"]}],
"features":["acpi-s3"]}`

const microvmDesc = `{"description":"microvm","interface-types":["uefi"],
"mapping":{"device":"memory","executable":{"filename":"/fw/MICROVM.fd"}},
"targets":[{"architecture":"x86_64","machines":["microvm"]}],"features":[]}`

const armDesc = `{"description":"aa64","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/QEMU_EFI.fd"},
"nvram-template":{"filename":"/fw/QEMU_VARS.fd"}},
"targets":[{"architecture":"aarch64","machines":["virt-*"]}],
"features":[]}`

// The secure descriptor sorts FIRST by priority. Taking the first match picks
// firmware that needs -machine q35,smm=on and fails without it.
func TestScanRegistrySkipsSecureBootTrap(t *testing.T) {
	dir := t.TempDir()
	writeDesc(t, dir, "50-edk2-secure.json", secureDesc)
	writeDesc(t, dir, "60-edk2-plain.json", plainDesc)

	code, vars, ok := scanRegistry([]string{dir}, "x86_64", "q35")
	if !ok {
		t.Fatal("expected a match")
	}
	if code != "/fw/OVMF_CODE.4m.fd" || vars != "/fw/OVMF_VARS.4m.fd" {
		t.Errorf("selected the secure-boot entry: code=%q vars=%q", code, vars)
	}
}

func TestScanRegistrySkipsNonFlash(t *testing.T) {
	dir := t.TempDir()
	writeDesc(t, dir, "60-microvm.json", microvmDesc)
	if _, _, ok := scanRegistry([]string{dir}, "x86_64", "microvm"); ok {
		t.Error("device:memory entries are not usable as pflash")
	}
}

func TestScanRegistryArchIsolation(t *testing.T) {
	dir := t.TempDir()
	writeDesc(t, dir, "60-arm.json", armDesc)
	if _, _, ok := scanRegistry([]string{dir}, "x86_64", "q35"); ok {
		t.Error("aarch64 descriptor must not match an x86_64 host")
	}
	code, _, ok := scanRegistry([]string{dir}, "aarch64", "virt")
	if !ok || code != "/fw/QEMU_EFI.fd" {
		t.Errorf("aarch64/virt should match: ok=%v code=%q", ok, code)
	}
}

// An earlier directory masks a same-named file in a later one.
func TestScanRegistryDirectoryPrecedence(t *testing.T) {
	etc, usr := t.TempDir(), t.TempDir()
	writeDesc(t, usr, "60-edk2.json", plainDesc)
	writeDesc(t, etc, "60-edk2.json", armDesc)
	code, _, ok := scanRegistry([]string{etc, usr}, "aarch64", "virt")
	if !ok || code != "/fw/QEMU_EFI.fd" {
		t.Errorf("/etc must mask /usr/share for the same basename: ok=%v code=%q", ok, code)
	}
}

// One unparseable file on a user's system must not break discovery for every
// other descriptor in the directory.
func TestScanRegistrySkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	writeDesc(t, dir, "50-broken.json", "{not json")
	writeDesc(t, dir, "60-edk2-plain.json", plainDesc)

	code, _, ok := scanRegistry([]string{dir}, "x86_64", "q35")
	if !ok || code != "/fw/OVMF_CODE.4m.fd" {
		t.Errorf("a malformed descriptor must be skipped, not fatal: ok=%v code=%q", ok, code)
	}
}

func TestScanRegistryEmpty(t *testing.T) {
	if _, _, ok := scanRegistry([]string{t.TempDir()}, "x86_64", "q35"); ok {
		t.Error("empty registry must report not-found so the fallback runs")
	}
}
