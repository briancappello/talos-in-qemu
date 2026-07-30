package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// Deliberately complete EXCEPT for device:memory: it carries an
// nvram-template so that the flash requirement is the ONLY thing making it
// unsuitable. Without that, suitable()'s nvram check would mask the flash
// check and TestScanRegistrySkipsNonFlash would pass with no flash check.
const microvmDesc = `{"description":"microvm","interface-types":["uefi"],
"mapping":{"device":"memory","executable":{"filename":"/fw/MICROVM.fd"},
"nvram-template":{"filename":"/fw/MICROVM_VARS.fd"}},
"targets":[{"architecture":"x86_64","machines":["microvm"]}],"features":[]}`

// A second suitable x86_64/q35 descriptor whose paths are distinguishable from
// plainDesc, so a test can prove WHICH of two candidates was selected.
const altPlainDesc = `{"description":"alt","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/ALT_CODE.fd"},
"nvram-template":{"filename":"/fw/ALT_VARS.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":[]}`

const armDesc = `{"description":"aa64","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/QEMU_EFI.fd"},
"nvram-template":{"filename":"/fw/QEMU_VARS.fd"}},
"targets":[{"architecture":"aarch64","machines":["virt-*"]}],
"features":[]}`

// Distros ship a 32-bit OVMF (Debian's 60-edk2-ia32.json) that targets the
// SAME pc-q35-* machines as the 64-bit one and differs only in architecture.
// armDesc cannot exercise the architecture check on its own, because its
// virt-* glob already fails the machine match; this one matches the machine
// exactly, so architecture is the only thing that can reject it.
const ia32Desc = `{"description":"ia32","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/OVMF32_CODE.fd"},
"nvram-template":{"filename":"/fw/OVMF32_VARS.fd"}},
"targets":[{"architecture":"i386","machines":["pc-i440fx-*","pc-q35-*"]}],
"features":[]}`

// Suitable in every respect EXCEPT that it carries no nvram-template. This is
// the real shape of a stateless/read-only build, and the one that would break
// the -pflash PAIR we emit: we would hand QEMU a code image with no vars
// template to copy, so the nvram check is what keeps that unrepresentable.
const noNVRAMDesc = `{"description":"stateless","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/NONV_CODE.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":[]}`

// Suitable EXCEPT that it names no executable: an nvram template with nothing
// to pair it with. Isolates the executable check from the nvram one.
const noCodeDesc = `{"description":"varsonly","interface-types":["uefi"],
"mapping":{"device":"flash","nvram-template":{"filename":"/fw/NOCODE_VARS.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":[]}`

// A complete, well-formed FLASH descriptor that is simply not UEFI. Legacy
// BIOS blobs live in the same registry, so interface-types is the only thing
// separating them from firmware we can drive with -pflash.
const biosDesc = `{"description":"seabios","interface-types":["bios"],
"mapping":{"device":"flash","executable":{"filename":"/fw/BIOS_CODE.fd"},
"nvram-template":{"filename":"/fw/BIOS_VARS.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":[]}`

// secureDesc declares requires-smm AND secure-boot, so it cannot tell the two
// rejections apart: delete either check and the other still rejects it. These
// two split them, and both shapes are real — SMM-only builds exist without
// secure boot, and vice versa.
const smmOnlyDesc = `{"description":"smm","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/SMM_CODE.fd"},
"nvram-template":{"filename":"/fw/SMM_VARS.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":["requires-smm"]}`

const secureBootOnlyDesc = `{"description":"sb","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":"/fw/SB_CODE.fd"},
"nvram-template":{"filename":"/fw/SB_VARS.fd"}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":["secure-boot"]}`

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

// Each fixture here is a COMPLETE, well-formed x86_64/q35 flash descriptor
// that fails exactly one of suitable()'s remaining rejections. That isolation
// is the point: with the whole set present, deleting any single rejection
// clause from suitable() makes one of these cases start matching, so each
// clause has a test that dies with it.
func TestScanRegistryRejectsUnusableDescriptors(t *testing.T) {
	for _, tc := range []struct{ name, why, body string }{
		{"no-nvram-template", "a code image with no vars template cannot form a -pflash pair", noNVRAMDesc},
		{"no-executable", "a vars template with no code image is not bootable", noCodeDesc},
		{"non-uefi", "a legacy BIOS blob is not UEFI firmware", biosDesc},
		{"requires-smm", "we invoke -machine q35 without smm=on", smmOnlyDesc},
		{"secure-boot", "we enroll no secure-boot keys", secureBootOnlyDesc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDesc(t, dir, "60-edk2.json", tc.body)
			if code, vars, ok := scanRegistry([]string{dir}, "x86_64", "q35"); ok {
				t.Errorf("%s: %s; got code=%q vars=%q", tc.name, tc.why, code, vars)
			}
		})
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
	// Right architecture, wrong machine family: the arch check alone would let
	// this through, so only the machine glob can reject it.
	if _, _, ok := scanRegistry([]string{dir}, "aarch64", "raspi3b"); ok {
		t.Error("a virt-only descriptor must not match -machine raspi3b")
	}

	// The mirror image: right machine, wrong architecture. Booting the 32-bit
	// OVMF on an x86_64 guest is the failure this prevents, and the machine
	// glob cannot catch it because ia32 advertises the very same pc-q35-*.
	ia32 := t.TempDir()
	writeDesc(t, ia32, "60-edk2-ia32.json", ia32Desc)
	if code, _, ok := scanRegistry([]string{ia32}, "x86_64", "q35"); ok {
		t.Errorf("the i386 OVMF must not match an x86_64 guest; got code=%q", code)
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

// Masking is stronger than "first suitable match wins": an earlier directory's
// file REPLACES a later one's same basename even when the earlier file is
// UNSUITABLE. /etc/qemu/firmware/60-edk2.json exists precisely so an admin can
// disable the vendor descriptor of that name, so an unsuitable /etc entry must
// hide the suitable /usr one rather than fall through to it.
func TestScanRegistryEarlierDirectoryMasksWithUnsuitableFile(t *testing.T) {
	etc, usr := t.TempDir(), t.TempDir()
	writeDesc(t, etc, "60-edk2.json", armDesc)   // unsuitable for x86_64
	writeDesc(t, usr, "60-edk2.json", plainDesc) // suitable for x86_64
	if code, _, ok := scanRegistry([]string{etc, usr}, "x86_64", "q35"); ok {
		t.Errorf("/etc must mask /usr/share even when unsuitable; got code=%q", code)
	}
}

// Basename priority outranks directory order for DIFFERENT basenames: the
// merged set is sorted as a whole, so a lower-numbered file in a LATER
// directory still wins. os.ReadDir already returns sorted entries, so this is
// the only shape in which the merge sort decides the outcome.
func TestScanRegistryBasenameBeatsDirectoryOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeDesc(t, first, "60-plain.json", plainDesc)
	writeDesc(t, second, "30-other.json", altPlainDesc)
	code, vars, ok := scanRegistry([]string{first, second}, "x86_64", "q35")
	if !ok {
		t.Fatal("expected a match")
	}
	if code != "/fw/ALT_CODE.fd" || vars != "/fw/ALT_VARS.fd" {
		t.Errorf("30- must outrank 60- across directories: code=%q vars=%q", code, vars)
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

// Registry directories hold more than descriptors: package docs, and — the
// case that actually bites — editor/packaging backups such as
// 50-edk2.json.bak, which a distro upgrade leaves behind. Those must be
// invisible, not merely unparseable: the backup here is VALID JSON for a
// suitable descriptor, and sorts ahead of the real one, so if the extension
// filter goes away it wins the scan and we boot a stale, deselected firmware.
func TestScanRegistryIgnoresNonJSONFiles(t *testing.T) {
	dir := t.TempDir()
	writeDesc(t, dir, "README", "descriptors live here\n")
	writeDesc(t, dir, "50-edk2.json.bak", altPlainDesc)
	writeDesc(t, dir, "60-edk2-plain.json", plainDesc)

	code, vars, ok := scanRegistry([]string{dir}, "x86_64", "q35")
	if !ok {
		t.Fatal("stray non-.json files must not break discovery")
	}
	if code != "/fw/OVMF_CODE.4m.fd" || vars != "/fw/OVMF_VARS.4m.fd" {
		t.Errorf("only .json files are descriptors: code=%q vars=%q", code, vars)
	}
}

func TestScanRegistryEmpty(t *testing.T) {
	if _, _, ok := scanRegistry([]string{t.TempDir()}, "x86_64", "q35"); ok {
		t.Error("empty registry must report not-found so the fallback runs")
	}
}

// An unreadable directory must be SKIPPED, not treated as the end of the scan.
// This is not a hypothetical: registryDirs lists /etc/qemu/firmware first and
// that directory does not exist on a stock Arch or Debian host, so this branch
// runs on every real invocation. If it were terminal, registry discovery would
// silently fall through to the static path table on exactly the hosts the
// registry exists to serve.
func TestScanRegistrySkipsUnreadableDirectory(t *testing.T) {
	good := t.TempDir()
	writeDesc(t, good, "60-edk2.json", plainDesc)
	absent := filepath.Join(t.TempDir(), "no-such-dir")

	code, vars, ok := scanRegistry([]string{absent, good}, "x86_64", "q35")
	if !ok {
		t.Fatal("an unreadable directory must not abort the scan")
	}
	if code != "/fw/OVMF_CODE.4m.fd" || vars != "/fw/OVMF_VARS.4m.fd" {
		t.Errorf("got code=%q vars=%q", code, vars)
	}
}

// The sibling skip: a listed entry whose contents cannot be read. A dangling
// symlink is what a half-removed package leaves behind, and it is hermetic to
// create and independent of uid, unlike a mode-0000 file.
func TestScanRegistrySkipsUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "50-edk2.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeDesc(t, dir, "60-edk2.json", plainDesc)

	code, _, ok := scanRegistry([]string{dir}, "x86_64", "q35")
	if !ok || code != "/fw/OVMF_CODE.4m.fd" {
		t.Errorf("an unreadable descriptor must be skipped, not fatal: ok=%v code=%q", ok, code)
	}
}

// Every other scanRegistry test feeds it fixtures we wrote ourselves, so they
// all agree with each other about descriptor shape and none of them would
// notice if the real schema drifted. This one reads the host's actual registry
// through the actual registryDirs and insists the files it names are really
// there. It skips cleanly where the registry is absent (macOS/Homebrew, which
// is not known to ship descriptors) — that is the fallback table's case.
func TestScanRegistryAgainstHostRegistry(t *testing.T) {
	const dir = "/usr/share/qemu/firmware"
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no firmware registry on this host: %v", err)
	}
	ai, err := archFor(runtime.GOARCH)
	if err != nil {
		t.Skipf("unsupported host arch: %v", err)
	}

	code, vars, ok := scanRegistry(registryDirs, ai.fwArch, ai.machine)
	if !ok {
		t.Fatalf("registry %s exists but resolved nothing for %s/%s", dir, ai.fwArch, ai.machine)
	}
	for _, p := range []string{code, vars} {
		if !fileExists(p) {
			t.Errorf("descriptor names %q, which is not a readable file on this host", p)
		}
	}
	t.Logf("host registry resolved %s/%s to code=%s vars=%s", ai.fwArch, ai.machine, code, vars)
}

// descWithPaths builds a descriptor pointing at real files, so resolveFirmware
// can be asked which SOURCE it preferred rather than only which path it found.
func descWithPaths(code, vars string) string {
	return fmt.Sprintf(`{"description":"tmp","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":%q},"nvram-template":{"filename":%q}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],"features":[]}`, code, vars)
}

// touch creates an empty regular file and returns its path.
func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The registry is the portable source of truth; the table is the last resort.
// Getting this order backwards would pin every distro to a path list that rots.
func TestResolveFirmwarePrefersRegistry(t *testing.T) {
	fw := t.TempDir()
	regCode, regVars := touch(t, fw, "REG_CODE.fd"), touch(t, fw, "REG_VARS.fd")
	tblCode, tblVars := touch(t, fw, "TBL_CODE.fd"), touch(t, fw, "TBL_VARS.fd")

	reg := t.TempDir()
	writeDesc(t, reg, "60-edk2.json", descWithPaths(regCode, regVars))
	table := map[string][][2]string{"x86_64": {{tblCode, tblVars}}}

	code, vars, err := resolveFirmware([]string{reg}, table, "linux", "x86_64", "q35")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != regCode || vars != regVars {
		t.Errorf("got %q/%q, want the registry entry %q/%q", code, vars, regCode, regVars)
	}
}

func TestResolveFirmwareFallsBackWhenRegistryEmpty(t *testing.T) {
	dir := t.TempDir()
	code := touch(t, dir, "edk2-aarch64-code.fd")
	vars := touch(t, dir, "edk2-aarch64-vars.fd")
	table := map[string][][2]string{"aarch64": {{code, vars}}}

	gotCode, gotVars, err := resolveFirmware([]string{t.TempDir()}, table, "darwin", "aarch64", "virt")
	if err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	if gotCode != code || gotVars != vars {
		t.Errorf("got %q/%q want %q/%q", gotCode, gotVars, code, vars)
	}
}

// A descriptor that names a file which is not installed is the one case the
// registry cannot be trusted for — Debian's ovmf descriptors survive the
// package being removed. The scan "succeeds" and the paths are dead, so the
// existence check has to run on the registry result too, not just the table.
func TestResolveFirmwareFallsThroughWhenDescriptorPathsAreDead(t *testing.T) {
	dir := t.TempDir()
	code := touch(t, dir, "TBL_CODE.fd")
	vars := touch(t, dir, "TBL_VARS.fd")

	reg := t.TempDir()
	writeDesc(t, reg, "60-edk2.json", descWithPaths("/gone/DEAD_CODE.fd", "/gone/DEAD_VARS.fd"))
	table := map[string][][2]string{"x86_64": {{code, vars}}}

	gotCode, gotVars, err := resolveFirmware([]string{reg}, table, "linux", "x86_64", "q35")
	if err != nil {
		t.Fatalf("a dead descriptor must fall through to the table, not fail: %v", err)
	}
	if gotCode != code || gotVars != vars {
		t.Errorf("got %q/%q want the table entry %q/%q", gotCode, gotVars, code, vars)
	}
}

// Today's edk2Code() returns a nonexistent path on failure and lets QEMU emit a
// confusing downstream error. The replacement must name every path it tried.
func TestResolveFirmwareErrorListsPathsTried(t *testing.T) {
	table := map[string][][2]string{"x86_64": {{"/nope/CODE.fd", "/nope/VARS.fd"}}}
	reg := t.TempDir()
	writeDesc(t, reg, "60-edk2.json", descWithPaths("/gone/DEAD_CODE.fd", "/gone/DEAD_VARS.fd"))

	_, _, err := resolveFirmware([]string{reg}, table, "linux", "x86_64", "q35")
	if err == nil {
		t.Fatal("expected an error when nothing resolves")
	}
	// The nvram template is listed as well as the code image because a present
	// CODE with a missing VARS is a real packaging shape; naming only the code
	// image sends the user hunting for a file that is already there.
	msg := err.Error()
	for _, want := range []string{
		"/nope/CODE.fd",
		"/nope/VARS.fd",
		"/gone/DEAD_CODE.fd", // named by a descriptor, not delivered
		reg,                  // the registry directory searched
		"x86_64",
	} {
		if !contains(msg, want) {
			t.Errorf("error must mention %q, got: %v", want, msg)
		}
	}
}

// -pflash needs a file. A directory at the expected path resolves nothing, and
// letting it through would hand QEMU an argument it rejects far downstream.
func TestResolveFirmwareRejectsDirectories(t *testing.T) {
	dir := t.TempDir()
	code := filepath.Join(dir, "CODE.fd")
	if err := os.Mkdir(code, 0o755); err != nil {
		t.Fatal(err)
	}
	vars := touch(t, dir, "VARS.fd")
	table := map[string][][2]string{"x86_64": {{code, vars}}}

	if c, v, err := resolveFirmware([]string{t.TempDir()}, table, "linux", "x86_64", "q35"); err == nil {
		t.Errorf("a directory is not firmware; got %q/%q", c, v)
	}
}
