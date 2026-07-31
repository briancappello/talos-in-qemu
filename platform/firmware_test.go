package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// firstSuitable asks scanRegistry the "which descriptor wins" question most of
// these tests are about. scanRegistry itself returns EVERY suitable candidate
// in priority order — that list is what lets resolveFirmware step past a
// descriptor whose files are gone — but selection order is orthogonal to that,
// and fixtures naming paths like /fw/OVMF_CODE.4m.fd are what make the
// suitable() rejections readable.
func firstSuitable(dirs []string, fwArch, machine string) (string, string, bool) {
	if c := scanRegistry(dirs, fwArch, machine); len(c) > 0 {
		return c[0][0], c[0][1], true
	}
	return "", "", false
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

	code, vars, ok := firstSuitable([]string{dir}, "x86_64", "q35")
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
	if _, _, ok := firstSuitable([]string{dir}, "x86_64", "microvm"); ok {
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
			if code, vars, ok := firstSuitable([]string{dir}, "x86_64", "q35"); ok {
				t.Errorf("%s: %s; got code=%q vars=%q", tc.name, tc.why, code, vars)
			}
		})
	}
}

func TestScanRegistryArchIsolation(t *testing.T) {
	dir := t.TempDir()
	writeDesc(t, dir, "60-arm.json", armDesc)
	if _, _, ok := firstSuitable([]string{dir}, "x86_64", "q35"); ok {
		t.Error("aarch64 descriptor must not match an x86_64 host")
	}
	code, _, ok := firstSuitable([]string{dir}, "aarch64", "virt")
	if !ok || code != "/fw/QEMU_EFI.fd" {
		t.Errorf("aarch64/virt should match: ok=%v code=%q", ok, code)
	}
	// Right architecture, wrong machine family: the arch check alone would let
	// this through, so only the machine glob can reject it.
	if _, _, ok := firstSuitable([]string{dir}, "aarch64", "raspi3b"); ok {
		t.Error("a virt-only descriptor must not match -machine raspi3b")
	}

	// The mirror image: right machine, wrong architecture. Booting the 32-bit
	// OVMF on an x86_64 guest is the failure this prevents, and the machine
	// glob cannot catch it because ia32 advertises the very same pc-q35-*.
	ia32 := t.TempDir()
	writeDesc(t, ia32, "60-edk2-ia32.json", ia32Desc)
	if code, _, ok := firstSuitable([]string{ia32}, "x86_64", "q35"); ok {
		t.Errorf("the i386 OVMF must not match an x86_64 guest; got code=%q", code)
	}
}

// An earlier directory masks a same-named file in a later one.
func TestScanRegistryDirectoryPrecedence(t *testing.T) {
	etc, usr := t.TempDir(), t.TempDir()
	writeDesc(t, usr, "60-edk2.json", plainDesc)
	writeDesc(t, etc, "60-edk2.json", armDesc)
	code, _, ok := firstSuitable([]string{etc, usr}, "aarch64", "virt")
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
	if code, _, ok := firstSuitable([]string{etc, usr}, "x86_64", "q35"); ok {
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
	code, vars, ok := firstSuitable([]string{first, second}, "x86_64", "q35")
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

	code, _, ok := firstSuitable([]string{dir}, "x86_64", "q35")
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

	code, vars, ok := firstSuitable([]string{dir}, "x86_64", "q35")
	if !ok {
		t.Fatal("stray non-.json files must not break discovery")
	}
	if code != "/fw/OVMF_CODE.4m.fd" || vars != "/fw/OVMF_VARS.4m.fd" {
		t.Errorf("only .json files are descriptors: code=%q vars=%q", code, vars)
	}
}

func TestScanRegistryEmpty(t *testing.T) {
	if _, _, ok := firstSuitable([]string{t.TempDir()}, "x86_64", "q35"); ok {
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

	code, vars, ok := firstSuitable([]string{absent, good}, "x86_64", "q35")
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

	code, _, ok := firstSuitable([]string{dir}, "x86_64", "q35")
	if !ok || code != "/fw/OVMF_CODE.4m.fd" {
		t.Errorf("an unreadable descriptor must be skipped, not fatal: ok=%v code=%q", ok, code)
	}
}

// Every other scanRegistry test feeds it fixtures we wrote ourselves, so they
// all agree with each other about descriptor shape and none of them would
// notice if the real schema drifted. This one reads the host's actual registry
// through the actual registryDirs and insists the files it names are really
// there.
//
// The precondition is EVERY directory in registryDirs, not one hardcoded path.
// It used to stat only /usr/share/qemu/firmware, so on macOS it reported SKIP —
// and that skip is precisely what hid the missing Homebrew descriptor directory
// until the branch was run on a Mac. A test whose reach is narrower than the
// code it covers announces success for the case it cannot see.
func TestScanRegistryAgainstHostRegistry(t *testing.T) {
	var present []string
	for _, d := range registryDirs {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			present = append(present, d)
		}
	}
	if len(present) == 0 {
		t.Skipf("no firmware registry on this host; searched: %v", registryDirs)
	}
	ai, err := archFor(runtime.GOARCH)
	if err != nil {
		t.Skipf("unsupported host arch: %v", err)
	}

	code, vars, ok := firstSuitable(registryDirs, ai.fwArch, ai.machine)
	if !ok {
		// NOT a failure. A registry that holds only descriptors for OTHER
		// architectures is normal — cross-arch qemu is common, so an aarch64
		// box with x86 OVMF installed has the directory and matches nothing.
		// Falling through to the static table is exactly what it is for, and
		// Detect() still succeeds there. Only a descriptor that matched and
		// then named files that are absent is a real defect, and that is the
		// t.Errorf below.
		t.Skipf("registries %v hold no descriptor for this host's %s/%s", present, ai.fwArch, ai.machine)
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

// A rotted descriptor must cost us the NEXT DESCRIPTOR, not the whole registry.
// Taking only the first suitable candidate meant one dead entry dropped us
// straight to the static path table — inverting the branch's own rationale, that
// querying the registry beats a table that rots. Here 50- is suitable but names
// files that are not installed, 60- is suitable and real, and the table entry is
// real too: the second descriptor must win over both.
func TestResolveFirmwareTriesTheNextDescriptorWhenTheFirstIsDead(t *testing.T) {
	fw := t.TempDir()
	liveCode, liveVars := touch(t, fw, "LIVE_CODE.fd"), touch(t, fw, "LIVE_VARS.fd")
	tblCode, tblVars := touch(t, fw, "TBL_CODE.fd"), touch(t, fw, "TBL_VARS.fd")

	reg := t.TempDir()
	writeDesc(t, reg, "50-edk2-dead.json", descWithPaths("/gone/DEAD_CODE.fd", "/gone/DEAD_VARS.fd"))
	writeDesc(t, reg, "60-edk2-live.json", descWithPaths(liveCode, liveVars))
	table := map[string][][2]string{"x86_64": {{tblCode, tblVars}}}

	code, vars, err := resolveFirmware([]string{reg}, table, "linux", "x86_64", "q35")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code == tblCode {
		t.Fatalf("fell through to the static table; the second descriptor was installed and usable")
	}
	if code != liveCode || vars != liveVars {
		t.Errorf("got %q/%q, want the second descriptor %q/%q", code, vars, liveCode, liveVars)
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
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %q, got: %v", want, msg)
		}
	}
}

// An architecture with no table entry leaves nothing to list, and the naive
// format string still printed the "then these paths:" heading over an empty
// bullet — which reads as though the list was lost rather than empty. The
// heading must not appear with nothing under it.
func TestResolveFirmwareErrorHasNoDanglingPathSection(t *testing.T) {
	table := map[string][][2]string{"x86_64": {{"/nope/CODE.fd", "/nope/VARS.fd"}}}

	_, _, err := resolveFirmware([]string{t.TempDir()}, table, "linux", "riscv64", "virt")
	if err == nil {
		t.Fatal("expected an error for an architecture with no firmware")
	}
	msg := err.Error()
	if strings.Contains(msg, "then these paths:") {
		t.Errorf("no paths were tried, so the heading must not appear, got:\n%s", msg)
	}
	// The generic shape of the defect: a bullet indent followed by nothing.
	if strings.Contains(msg, "\n  \n") || strings.HasSuffix(msg, "\n  ") {
		t.Errorf("message has an empty bullet, got:\n%q", msg)
	}
	// It must still say which architecture came up empty and stay actionable.
	for _, want := range []string{"riscv64", "edk2/OVMF"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %q, got:\n%s", want, msg)
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

// The bug this pins: fallbackTable named edk2-aarch64-vars.fd, which QEMU has
// never shipped. resolveFirmware requires BOTH halves of a pair, so the missing
// vars template discarded the code image beside it and every aarch64 entry for
// Homebrew went dead — Detect() failed outright on macOS.
//
// A code image that IS installed next to a vars template that is NOT is the
// exact signature, and it is checkable on any host without knowing which
// packages are present: entries for a prefix that does not exist are simply
// absent on both halves and say nothing either way.
func TestFallbackTablePairsAreInstalledTogether(t *testing.T) {
	for arch, pairs := range fallbackTable {
		for _, p := range pairs {
			if fileExists(p[0]) && !fileExists(p[1]) {
				t.Errorf("%s: code %q is installed but its nvram template %q is not,\n"+
					"so resolveFirmware discards the pair and this entry is dead",
					arch, p[0], p[1])
			}
		}
	}
}

// The host check above only speaks where the files are installed, so on a Linux
// CI box every Homebrew entry is silent and a reintroduced edk2-aarch64-vars.fd
// would sail through. Pin the naming convention itself: edk2 vars templates are
// shared across an architecture FAMILY, so the aarch64 code image pairs with the
// arm vars template and the x86_64 code image pairs with the i386 one. Both are
// confirmed by QEMU's own descriptors.
func TestFallbackVarsTemplatesUseFamilySharedNames(t *testing.T) {
	for _, tc := range []struct{ arch, code, vars string }{
		{"aarch64", "edk2-aarch64-code.fd", "edk2-arm-vars.fd"},
		{"x86_64", "edk2-x86_64-code.fd", "edk2-i386-vars.fd"},
	} {
		var found int
		for _, p := range fallbackTable[tc.arch] {
			if filepath.Base(p[0]) != tc.code {
				continue
			}
			found++
			if got := filepath.Base(p[1]); got != tc.vars {
				t.Errorf("%s: %s must pair with %s, got %s (QEMU ships no per-sub-arch vars template)",
					tc.arch, tc.code, tc.vars, got)
			}
		}
		if found == 0 {
			t.Errorf("%s: no fallback entry names %s at all", tc.arch, tc.code)
		}
	}
}
