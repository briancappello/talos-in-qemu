# Platform Abstraction Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Detect host architecture, hypervisor and UEFI firmware at runtime so TinQ runs unmodified on macOS/arm64 and Linux/amd64.

**Architecture:** A new `platform` package exposes `Detect() (*Platform, error)` returning a struct of resolved facts. `cmd/tinq/main.go` calls it once in `create()` and reads fields. No build tags — every path compiles and is testable on every host. Firmware is discovered through QEMU's `firmware.json` interop registry, with a static fallback table.

**Tech Stack:** Go 1.26, stdlib only (`encoding/json`, `encoding/binary`, `path/filepath`, `os/exec`, `runtime`). No new dependencies.

## Global Constraints

- Go 1.26.0 (`go.mod`). Module path `github.com/coglative/talos-in-qemu`.
- **No new dependencies.** stdlib only.
- **No build tags.** Platform variance is a runtime `switch`, never `_darwin.go`/`_linux.go`.
- Must compile for `darwin/arm64` and `linux/amd64`. Verify with `GOOS=darwin GOARCH=arm64 go build ./...`.
- **No TCG fallback.** Absence of a hardware accelerator is a hard, descriptive error.
- macOS behaviour is written from best guesses and unverifiable here. Every macOS assumption carries a `TODO(macos-verify):` comment.
- Test fixtures are **generated in test code**, never committed binaries. (Refines the spec, which said "checked into the repo" — synthesising them keeps binaries out of git and makes the format self-documenting.)
- `driverkit/` is platform-neutral and must not be modified.

---

## File Structure

| File | Responsibility |
|---|---|
| `platform/platform.go` | `Platform` struct, `Detect()`, GOARCH→arch table |
| `platform/platform_test.go` | arch mapping + `Detect()` wiring |
| `platform/accel.go` | accelerator selection, `/dev/kvm` diagnosis, error text |
| `platform/accel_test.go` | the three failure messages |
| `platform/firmware.go` | descriptor parsing, suitability, lenient machine glob, registry scan, fallback table |
| `platform/firmware_test.go` | SMM trap, microvm trap, priority, glob, fallback |
| `platform/image.go` | ISO9660 walk → `/BOOT/VMLINUZ*` → PE machine |
| `platform/image_test.go` | synthesised ISO fixtures, both arches + malformed |
| `cmd/tinq/main.go` | consumes `Platform`; `edk2Code`/`makeEFIVars` replaced |

---

### Task 1: Architecture mapping

**Files:**
- Create: `platform/platform.go`
- Test: `platform/platform_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Platform struct{...}`; `func archFor(goarch string) (archInfo, error)`; `type archInfo struct{ qemuBinary, machine, console, imageArch, fwArch string }`.

- [ ] **Step 1: Write the failing test**

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/brian/dev/talos-in-qemu && go test ./platform/ -run TestArch -v`
Expected: FAIL — `undefined: archFor`

- [ ] **Step 3: Write minimal implementation**

```go
// Package platform resolves the host-specific facts a QEMU invocation needs:
// which emulator binary, which machine type, which accelerator, and where the
// UEFI firmware lives.
//
// Everything here is decided at RUNTIME rather than by build tags. Build tags
// would leave the macOS path uncompiled on Linux — no type checking, no tests,
// no compile error until someone builds on a Mac. The variance is four values
// and a firmware lookup; that does not justify hiding half the code from the
// compiler.
package platform

import "fmt"

// Platform is the set of host facts main.go needs. Fields are resolved once by
// Detect and then only read.
type Platform struct {
	QEMUBinary   string // qemu-system-x86_64 | qemu-system-aarch64
	Machine      string // q35 | virt
	Accel        string // kvm | hvf
	CPU          string // host — only legal with a hardware accelerator
	FirmwareCode string // read-only pflash
	FirmwareVars string // nvram TEMPLATE, copied verbatim — never padded
	ConsoleArg   string // console=ttyS0 | console=ttyAMA0 (guest hint)
	ImageArch    string // amd64 | arm64 (guest hint, used by the image guard)
}

type archInfo struct {
	qemuBinary string
	machine    string
	console    string
	imageArch  string
	fwArch     string // the "architecture" value used in firmware descriptors
}

// archFor maps Go's arch vocabulary onto QEMU's. They disagree: Go says amd64
// and arm64, QEMU says x86_64 and aarch64, and the firmware registry uses
// QEMU's spelling.
func archFor(goarch string) (archInfo, error) {
	switch goarch {
	case "amd64":
		return archInfo{"qemu-system-x86_64", "q35", "console=ttyS0", "amd64", "x86_64"}, nil
	case "arm64":
		return archInfo{"qemu-system-aarch64", "virt", "console=ttyAMA0", "arm64", "aarch64"}, nil
	}
	return archInfo{}, fmt.Errorf("unsupported host architecture %q: TinQ supports amd64 and arm64", goarch)
}
```

Do **not** import `runtime` in this task — `Detect` arrives in Task 4 and brings
it then. A placeholder such as `var _ = runtime.GOARCH` is dead code and will be
flagged as such.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./platform/ -run TestArch -v`
Expected: PASS (both tests)

- [ ] **Step 5: Commit**

```bash
git add platform/platform.go platform/platform_test.go
git commit -m "feat(platform): map GOARCH to QEMU binary, machine and console"
```

---

### Task 2: Accelerator detection and its error messages

**Files:**
- Create: `platform/accel.go`
- Test: `platform/accel_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `func accelFor(goos string) (string, error)`; `type kvmDiag int` with `kvmOK`/`kvmMissing`/`kvmNoPerm`; `func diagnoseKVM(path string) kvmDiag`; `func accelUnavailable(goos, goarch, accel string, compiled bool, diag kvmDiag) error`; `func compiledAccels(qemuBinary string) ([]string, error)`.

- [ ] **Step 1: Write the failing test**

```go
package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccelFor(t *testing.T) {
	if a, err := accelFor("linux"); err != nil || a != "kvm" {
		t.Errorf("linux => %q, %v; want kvm", a, err)
	}
	if a, err := accelFor("darwin"); err != nil || a != "hvf" {
		t.Errorf("darwin => %q, %v; want hvf", a, err)
	}
	if _, err := accelFor("plan9"); err == nil {
		t.Error("plan9 should be unsupported")
	}
}

func TestDiagnoseKVM(t *testing.T) {
	if got := diagnoseKVM(filepath.Join(t.TempDir(), "nope")); got != kvmMissing {
		t.Errorf("absent device => %v, want kvmMissing", got)
	}
	p := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(p, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission check is meaningless")
	}
	if got := diagnoseKVM(p); got != kvmNoPerm {
		t.Errorf("unreadable device => %v, want kvmNoPerm", got)
	}
}

// The three failure modes must be distinguishable. "enable hardware
// acceleration" alone does not tell the user which case they are in.
func TestAccelUnavailableMessagesDiffer(t *testing.T) {
	missing := accelUnavailable("linux", "amd64", "kvm", true, kvmMissing).Error()
	noperm := accelUnavailable("linux", "amd64", "kvm", true, kvmNoPerm).Error()
	notbuilt := accelUnavailable("linux", "amd64", "kvm", false, kvmOK).Error()

	if missing == noperm || missing == notbuilt || noperm == notbuilt {
		t.Fatal("the three accelerator failures must produce distinct messages")
	}
	if !contains(noperm, "usermod") {
		t.Errorf("permission failure must give the actionable fix, got: %s", noperm)
	}
	if !contains(notbuilt, "not built") {
		t.Errorf("not-compiled-in failure must say so, got: %s", notbuilt)
	}
	for _, m := range []string{missing, noperm, notbuilt} {
		if !contains(m, "hang") {
			t.Errorf("message must explain why TCG is refused, got: %s", m)
		}
	}
}

func TestParseAccels(t *testing.T) {
	out := "Accelerators supported in QEMU binary:\ntcg\nmshv\nnitro\nkvm\n"
	got := parseAccels(out)
	want := map[string]bool{"tcg": true, "mshv": true, "nitro": true, "kvm": true}
	if len(got) != len(want) {
		t.Fatalf("parseAccels => %v", got)
	}
	for _, a := range got {
		if !want[a] {
			t.Errorf("unexpected accelerator %q", a)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./platform/ -run 'TestAccel|TestDiagnose|TestParseAccels' -v`
Expected: FAIL — `undefined: accelFor`

- [ ] **Step 3: Write minimal implementation**

```go
package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// kvmDiag distinguishes the three ways hardware acceleration is unavailable.
// They need different fixes, so they get different messages.
type kvmDiag int

const (
	kvmOK kvmDiag = iota
	kvmMissing
	kvmNoPerm
)

func accelFor(goos string) (string, error) {
	switch goos {
	case "linux":
		return "kvm", nil
	case "darwin":
		// TODO(macos-verify): HVF is assumed present on any Mac able to run a
		// Homebrew qemu-system binary. Unverified from Linux.
		return "hvf", nil
	}
	return "", fmt.Errorf("unsupported host OS %q: TinQ supports linux (KVM) and darwin (HVF)", goos)
}

// diagnoseKVM reports whether /dev/kvm is usable, and if not, why. Opening
// read-write is the real test: the device can exist and still be unusable.
func diagnoseKVM(path string) kvmDiag {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err == nil {
		f.Close()
		return kvmOK
	}
	if os.IsNotExist(err) {
		return kvmMissing
	}
	return kvmNoPerm
}

// parseAccels reads `qemu-system-X -accel help`, which prints a header line
// followed by one accelerator per line.
func parseAccels(out string) []string {
	var accels []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ":") {
			continue
		}
		accels = append(accels, line)
	}
	return accels
}

// compiledAccels asks the binary what it was BUILT with. That is a different
// question from whether the accelerator is usable right now, and the two
// failures deserve different messages.
func compiledAccels(qemuBinary string) ([]string, error) {
	out, err := exec.Command(qemuBinary, "-accel", "help").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s -accel help: %w", qemuBinary, err)
	}
	return parseAccels(string(out)), nil
}

func accelUnavailable(goos, goarch, accel string, compiled bool, diag kvmDiag) error {
	var reason, fix string
	switch {
	case !compiled:
		reason = fmt.Sprintf("this QEMU build does not include %s (it was not built with it)", accel)
		fix = "install a QEMU package built with " + accel + " support"
	case goos == "linux" && diag == kvmMissing:
		reason = "/dev/kvm does not exist"
		fix = "load the kvm module (modprobe kvm_intel or kvm_amd), or enable\n" +
			"       virtualization in firmware; in a VM, enable nested virtualization"
	case goos == "linux" && diag == kvmNoPerm:
		reason = fmt.Sprintf("/dev/kvm exists but is not usable by uid %d", os.Getuid())
		fix = "sudo usermod -aG kvm $USER   (then log out and back in)"
	default:
		reason = accel + " is not available"
		fix = "ensure hardware virtualization is enabled on this host"
	}
	return fmt.Errorf(`no hardware accelerator available on %s/%s

  %s
  fix: %s

TinQ requires KVM (Linux) or HVF (macOS). Talos under TCG emulation is
slow enough to be indistinguishable from a hang, so falling back to it
would turn "this host cannot do that" into "TinQ is broken"`,
		goos, goarch, reason, fix)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./platform/ -run 'TestAccel|TestDiagnose|TestParseAccels' -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Commit**

```bash
git add platform/accel.go platform/accel_test.go
git commit -m "feat(platform): detect accelerator, refuse TCG with an actionable error"
```

---

### Task 3: Firmware descriptor matching

**Files:**
- Create: `platform/firmware.go`
- Test: `platform/firmware_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type descriptor struct{...}`; `func machineMatches(pattern, machine string) bool`; `func (d *descriptor) suitable(fwArch, machine string) bool`; `func scanRegistry(dirs []string, fwArch, machine string) (code, vars string, found bool)`.

- [ ] **Step 1: Write the failing test**

```go
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

func TestScanRegistryEmpty(t *testing.T) {
	if _, _, ok := scanRegistry([]string{t.TempDir()}, "x86_64", "q35"); ok {
		t.Error("empty registry must report not-found so the fallback runs")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./platform/ -run 'TestMachineMatches|TestScanRegistry' -v`
Expected: FAIL — `undefined: machineMatches`

- [ ] **Step 3: Write minimal implementation**

```go
package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// descriptor is QEMU's firmware interop schema (docs/interop/firmware.json),
// the same registry libvirt consumes. Querying it is what makes this portable
// across distros: Debian's OVMF_CODE_4M.fd, Fedora's ovmf/OVMF_CODE.fd and
// SUSE's ovmf-x86_64-code.bin all self-describe through this one schema, so we
// never maintain a path table that rots.
type descriptor struct {
	Description    string   `json:"description"`
	InterfaceTypes []string `json:"interface-types"`
	Mapping        struct {
		Device     string `json:"device"`
		Executable struct {
			Filename string `json:"filename"`
		} `json:"executable"`
		NVRAMTemplate struct {
			Filename string `json:"filename"`
		} `json:"nvram-template"`
	} `json:"mapping"`
	Targets []struct {
		Architecture string   `json:"architecture"`
		Machines     []string `json:"machines"`
	} `json:"targets"`
	Features []string `json:"features"`
}

// machineMatches compares a descriptor's machine glob against the machine type
// we actually pass to QEMU.
//
// The subtlety that breaks the obvious implementation: `q35` is an ALIAS of
// `pc-q35-11.0`, and descriptors glob on `pc-q35-*`. filepath.Match("pc-q35-*",
// "q35") is FALSE, so a naive matcher misses for both machine types we use and
// silently falls through to the static table. Trimming the trailing "-*" and
// comparing against the alias recovers it without matching unrelated families.
func machineMatches(pattern, machine string) bool {
	if ok, _ := filepath.Match(pattern, machine); ok {
		return true
	}
	if base := strings.TrimSuffix(pattern, "-*"); base != pattern {
		return base == machine || strings.HasSuffix(base, "-"+machine)
	}
	return false
}

func (d *descriptor) suitable(fwArch, machine string) bool {
	if d.Mapping.Device != "flash" || d.Mapping.Executable.Filename == "" ||
		d.Mapping.NVRAMTemplate.Filename == "" {
		return false
	}
	if !slicesContains(d.InterfaceTypes, "uefi") {
		return false
	}
	// Secure-boot firmware needs -machine q35,smm=on plus more. On Arch the
	// secure descriptor sorts FIRST, so without this filter a take-the-first
	// matcher reliably selects firmware that cannot boot as invoked.
	if slicesContains(d.Features, "requires-smm") || slicesContains(d.Features, "secure-boot") {
		return false
	}
	for _, t := range d.Targets {
		if t.Architecture != fwArch {
			continue
		}
		for _, m := range t.Machines {
			if machineMatches(m, machine) {
				return true
			}
		}
	}
	return false
}

func slicesContains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// scanRegistry walks the descriptor directories in decreasing priority.
// Within the combined set, files sort by BASENAME (lower numeric prefix wins),
// and a file in an earlier directory masks the same basename in a later one.
func scanRegistry(dirs []string, fwArch, machine string) (string, string, bool) {
	seen := map[string]string{} // basename -> full path of the winning file
	var names []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasSuffix(n, ".json") {
				continue
			}
			if _, dup := seen[n]; dup {
				continue // earlier directory wins
			}
			seen[n] = filepath.Join(dir, n)
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(seen[n])
		if err != nil {
			continue
		}
		var d descriptor
		if err := json.Unmarshal(b, &d); err != nil {
			continue // a malformed descriptor must not break discovery
		}
		if d.suitable(fwArch, machine) {
			return d.Mapping.Executable.Filename, d.Mapping.NVRAMTemplate.Filename, true
		}
	}
	return "", "", false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./platform/ -run 'TestMachineMatches|TestScanRegistry' -v`
Expected: PASS (6 tests)

- [ ] **Step 5: Commit**

```bash
git add platform/firmware.go platform/firmware_test.go
git commit -m "feat(platform): discover UEFI firmware via QEMU's interop registry"
```

---

### Task 4: Firmware fallback table and `Detect()`

**Files:**
- Modify: `platform/firmware.go` (append)
- Modify: `platform/platform.go` (append `Detect`)
- Modify: `platform/firmware_test.go`, `platform/platform_test.go` (append)

**Interfaces:**
- Consumes: `archFor` (Task 1), `accelFor`/`diagnoseKVM`/`compiledAccels`/`accelUnavailable` (Task 2), `scanRegistry` (Task 3).
- Produces: `func resolveFirmware(dirs []string, table map[string][][2]string, goos, fwArch, machine string) (code, vars string, err error)`; `func Detect() (*Platform, error)`.

The fallback table is a **parameter, not a global read inside the function**, so
tests supply their own without mutating package state. `fallbackTable` remains a
package var holding the real data; only `Detect` reads it.

- [ ] **Step 1: Write the failing test**

```go
func TestResolveFirmwareFallsBackWhenRegistryEmpty(t *testing.T) {
	dir := t.TempDir()
	code := filepath.Join(dir, "edk2-aarch64-code.fd")
	vars := filepath.Join(dir, "edk2-aarch64-vars.fd")
	for _, p := range []string{code, vars} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	table := map[string][][2]string{"aarch64": {{code, vars}}}

	gotCode, gotVars, err := resolveFirmware([]string{t.TempDir()}, table, "darwin", "aarch64", "virt")
	if err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	if gotCode != code || gotVars != vars {
		t.Errorf("got %q/%q want %q/%q", gotCode, gotVars, code, vars)
	}
}

// Today's edk2Code() returns a nonexistent path on failure and lets QEMU emit a
// confusing downstream error. The replacement must name every path it tried.
func TestResolveFirmwareErrorListsPathsTried(t *testing.T) {
	table := map[string][][2]string{"x86_64": {{"/nope/CODE.fd", "/nope/VARS.fd"}}}

	_, _, err := resolveFirmware([]string{t.TempDir()}, table, "linux", "x86_64", "q35")
	if err == nil {
		t.Fatal("expected an error when nothing resolves")
	}
	if !contains(err.Error(), "/nope/CODE.fd") {
		t.Errorf("error must list paths tried, got: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./platform/ -run TestResolveFirmware -v`
Expected: FAIL — `undefined: fallbackTable`

- [ ] **Step 3: Write minimal implementation**

Append to `platform/firmware.go`:

```go
// registryDirs is the descriptor search path in decreasing priority, per the
// QEMU interop spec. /etc is the admin override.
var registryDirs = []string{"/etc/qemu/firmware", "/usr/share/qemu/firmware"}

// fallbackTable is used only when the registry yields nothing — the expected
// case on Homebrew, which is not known to ship descriptors. Entries are
// {code, nvram-template} pairs tried in order.
//
// TODO(macos-verify): the Homebrew paths below are the ones the pre-refactor
// edk2Code() used. Unverified from Linux.
var fallbackTable = map[string][][2]string{
	"aarch64": {
		{"/opt/homebrew/share/qemu/edk2-aarch64-code.fd", "/opt/homebrew/share/qemu/edk2-aarch64-vars.fd"},
		{"/usr/local/share/qemu/edk2-aarch64-code.fd", "/usr/local/share/qemu/edk2-aarch64-vars.fd"},
		{"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/AAVMF/AAVMF_VARS.fd"},
		{"/usr/share/edk2/aarch64/QEMU_EFI.fd", "/usr/share/edk2/aarch64/QEMU_VARS.fd"},
	},
	"x86_64": {
		{"/opt/homebrew/share/qemu/edk2-x86_64-code.fd", "/opt/homebrew/share/qemu/edk2-i386-vars.fd"},
		{"/usr/local/share/qemu/edk2-x86_64-code.fd", "/usr/local/share/qemu/edk2-i386-vars.fd"},
		{"/usr/share/edk2/x64/OVMF_CODE.4m.fd", "/usr/share/edk2/x64/OVMF_VARS.4m.fd"},
		{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
		{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
		{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
	},
}

// resolveFirmware takes the fallback table as a parameter so tests can supply
// their own without mutating package state.
func resolveFirmware(dirs []string, table map[string][][2]string, goos, fwArch, machine string) (string, string, error) {
	if code, vars, ok := scanRegistry(dirs, fwArch, machine); ok {
		if fileExists(code) && fileExists(vars) {
			return code, vars, nil
		}
	}
	var tried []string
	for _, pair := range table[fwArch] {
		tried = append(tried, pair[0])
		if fileExists(pair[0]) && fileExists(pair[1]) {
			return pair[0], pair[1], nil
		}
	}
	return "", "", fmt.Errorf(
		"no UEFI firmware found for %s/%s on %s\n\nsearched descriptor registries:\n  %s\n\nthen these paths:\n  %s\n\ninstall your distribution's edk2/OVMF package",
		fwArch, machine, goos,
		strings.Join(dirs, "\n  "), strings.Join(tried, "\n  "))
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
```

Add `"fmt"` to the import block of `firmware.go`.

Append to `platform/platform.go`:

```go
// Detect resolves every host fact needed to launch a VM. It fails rather than
// degrading: a wrong guess here surfaces as a silent hang inside QEMU, which is
// far more expensive to diagnose than an error at startup.
func Detect() (*Platform, error) {
	ai, err := archFor(runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	accel, err := accelFor(runtime.GOOS)
	if err != nil {
		return nil, err
	}

	compiled := false
	if accels, err := compiledAccels(ai.qemuBinary); err == nil {
		compiled = slicesContains(accels, accel)
	} else {
		return nil, fmt.Errorf("%s not runnable: %w\n\ninstall QEMU (%s)", ai.qemuBinary, err, ai.qemuBinary)
	}

	diag := kvmOK
	if runtime.GOOS == "linux" {
		diag = diagnoseKVM("/dev/kvm")
	}
	if !compiled || diag != kvmOK {
		return nil, accelUnavailable(runtime.GOOS, runtime.GOARCH, accel, compiled, diag)
	}

	code, vars, err := resolveFirmware(registryDirs, fallbackTable, runtime.GOOS, ai.fwArch, ai.machine)
	if err != nil {
		return nil, err
	}

	return &Platform{
		QEMUBinary:   ai.qemuBinary,
		Machine:      ai.machine,
		Accel:        accel,
		CPU:          "host", // only legal with a hardware accelerator, verified above
		FirmwareCode: code,
		FirmwareVars: vars,
		ConsoleArg:   ai.console,
		ImageArch:    ai.imageArch,
	}, nil
}
```

Add `"fmt"` and `"runtime"` to `platform.go`'s imports (Task 1 left it importing only `fmt`).

- [ ] **Step 4: Run tests**

Run: `go test ./platform/ -v && go vet ./platform/`
Expected: PASS, vet clean

- [ ] **Step 5: Commit**

```bash
git add platform/
git commit -m "feat(platform): firmware fallback table and Detect() entry point"
```

---

### Task 5: Image architecture inspection

**Files:**
- Create: `platform/image.go`
- Test: `platform/image_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func InspectImageArch(path string) string` returning `"amd64"`, `"arm64"` or `""` (unknown).

- [ ] **Step 1: Write the failing test**

```go
package platform

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

const isoSector = 2048

func isoDirRecord(name string, extent, size uint32, isDir bool) []byte {
	n := []byte(name)
	rl := 33 + len(n)
	if rl%2 != 0 {
		rl++
	}
	b := make([]byte, rl)
	b[0] = byte(rl)
	binary.LittleEndian.PutUint32(b[2:], extent)
	binary.BigEndian.PutUint32(b[6:], extent)
	binary.LittleEndian.PutUint32(b[10:], size)
	binary.BigEndian.PutUint32(b[14:], size)
	if isDir {
		b[25] = 0x02
	}
	binary.LittleEndian.PutUint16(b[28:], 1)
	binary.BigEndian.PutUint16(b[30:], 1)
	b[32] = byte(len(n))
	copy(b[33:], n)
	return b
}

// synthISO builds the smallest ISO9660 that exercises the real code path:
// PVD at sector 16 -> root dir -> BOOT dir -> VMLINUZ with a PE header.
func synthISO(t *testing.T, machine uint16, kernelName string) string {
	t.Helper()
	const (
		rootLBA   = 19
		bootLBA   = 21
		kernelLBA = 25
		total     = 27
	)
	img := make([]byte, total*isoSector)

	pvd := img[16*isoSector:]
	pvd[0] = 1
	copy(pvd[1:], "CD001")
	pvd[6] = 1
	copy(pvd[156:], isoDirRecord("\x00", rootLBA, isoSector, true))

	root := img[rootLBA*isoSector:]
	o := 0
	for _, r := range [][]byte{
		isoDirRecord("\x00", rootLBA, isoSector, true),
		isoDirRecord("\x01", rootLBA, isoSector, true),
		isoDirRecord("BOOT", bootLBA, isoSector, true),
	} {
		copy(root[o:], r)
		o += len(r)
	}

	kernel := make([]byte, 512)
	copy(kernel, "MZ")
	binary.LittleEndian.PutUint32(kernel[0x3c:], 0x40)
	copy(kernel[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(kernel[0x44:], machine)

	boot := img[bootLBA*isoSector:]
	o = 0
	for _, r := range [][]byte{
		isoDirRecord("\x00", bootLBA, isoSector, true),
		isoDirRecord("\x01", rootLBA, isoSector, true),
		isoDirRecord(kernelName, kernelLBA, uint32(len(kernel)), false),
	} {
		copy(boot[o:], r)
		o += len(r)
	}
	copy(img[kernelLBA*isoSector:], kernel)

	p := filepath.Join(t.TempDir(), "test.iso")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInspectImageArch(t *testing.T) {
	if got := InspectImageArch(synthISO(t, 0x8664, "VMLINUZ.;1")); got != "amd64" {
		t.Errorf("x86_64 kernel => %q, want amd64", got)
	}
	if got := InspectImageArch(synthISO(t, 0xaa64, "VMLINUZ.;1")); got != "arm64" {
		t.Errorf("aarch64 kernel => %q, want arm64", got)
	}
}

// Unknown must never be an error: it disables the guard, it does not break the
// run. Every one of these is a valid image we simply cannot classify.
func TestInspectImageArchUnknownIsSilent(t *testing.T) {
	dir := t.TempDir()

	notISO := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(notISO, make([]byte, 4*isoSector), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := InspectImageArch(notISO); got != "" {
		t.Errorf("non-ISO => %q, want empty", got)
	}
	if got := InspectImageArch(filepath.Join(dir, "absent.iso")); got != "" {
		t.Errorf("missing file => %q, want empty", got)
	}
	if got := InspectImageArch(synthISO(t, 0x8664, "OTHER.;1")); got != "" {
		t.Errorf("ISO without a kernel => %q, want empty", got)
	}
	if got := InspectImageArch(synthISO(t, 0x1c0, "VMLINUZ.;1")); got != "" {
		t.Errorf("unrecognised PE machine => %q, want empty", got)
	}
}

// Runs against the real Talos images when present; skipped otherwise so the
// suite stays runnable on any machine.
func TestInspectImageArchRealISOs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	// Subtests, not a bare loop: t.Skip terminates the whole test, so skipping
	// inside a map range would let one missing ISO silently drop the other
	// assertion — and map order is random, so which one is nondeterministic.
	for _, tc := range []struct{ name, want string }{
		{"talos-v1.9.5-amd64.iso", "amd64"},
		{"talos-v1.9.5-arm64.iso", "arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(home, ".hvf", "images", tc.name)
			if _, err := os.Stat(p); err != nil {
				t.Skipf("%s not present", p)
			}
			if got := InspectImageArch(p); got != tc.want {
				t.Errorf("%s => %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./platform/ -run TestInspectImage -v`
Expected: FAIL — `undefined: InspectImageArch`

- [ ] **Step 3: Write minimal implementation**

```go
package platform

import (
	"encoding/binary"
	"os"
	"strings"
)

const sectorSize = 2048

// InspectImageArch reports the architecture of a Talos boot ISO, or "" when it
// cannot tell. It NEVER returns an error: unknown disables the mismatch guard
// rather than rejecting an image we merely fail to understand.
//
// Three plausible cheaper methods are wrong, verified against real v1.9.5
// images:
//
//   - ESP boot filenames: Talos ships BOTH BOOTX64.EFI and BOOTAA64.EFI, as
//     real PE binaries with contradictory machine types, in the SAME amd64 ISO.
//   - whole-file PE machine histogram: ambiguous — amd64 has {0x8664:4,
//     0xaa64:2}, arm64 has {0x8664:3, 0xaa64:3}.
//   - the arm64 Image magic at 0x38: ABSENT from the arm64 ISO, because that
//     kernel is an EFI-stub PE rather than a raw Image.
//
// Only the kernel at /BOOT/VMLINUZ* is authoritative. Reads ~8 KB, not the
// whole 100 MB file.
func InspectImageArch(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	pvd := make([]byte, sectorSize)
	if _, err := f.ReadAt(pvd, 16*sectorSize); err != nil {
		return ""
	}
	if string(pvd[1:6]) != "CD001" {
		return ""
	}

	rootExtent, rootLen := recordExtent(pvd[156:])
	bootExtent, bootLen, ok := findChild(f, rootExtent, rootLen, func(n string) bool {
		return n == "BOOT"
	})
	if !ok {
		return ""
	}
	kExtent, _, ok := findChild(f, bootExtent, bootLen, func(n string) bool {
		return strings.HasPrefix(n, "VMLINUZ")
	})
	if !ok {
		return ""
	}

	head := make([]byte, 1024)
	if _, err := f.ReadAt(head, int64(kExtent)*sectorSize); err != nil {
		return ""
	}
	switch peMachine(head) {
	case 0x8664:
		return "amd64"
	case 0xaa64:
		return "arm64"
	}
	return ""
}

func recordExtent(rec []byte) (uint32, uint32) {
	if len(rec) < 18 {
		return 0, 0
	}
	return binary.LittleEndian.Uint32(rec[2:6]), binary.LittleEndian.Uint32(rec[10:14])
}

// findChild walks one ISO9660 directory extent looking for a matching entry.
func findChild(f *os.File, extent, length uint32, match func(string) bool) (uint32, uint32, bool) {
	if length == 0 || length > 1<<20 {
		return 0, 0, false
	}
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, int64(extent)*sectorSize); err != nil {
		return 0, 0, false
	}
	for off := 0; off < len(buf); {
		rl := int(buf[off])
		if rl == 0 || off+rl > len(buf) || rl < 33 {
			break
		}
		rec := buf[off : off+rl]
		nameLen := int(rec[32])
		if 33+nameLen <= rl {
			name := string(rec[33 : 33+nameLen])
			if match(name) {
				e, l := recordExtent(rec)
				return e, l, true
			}
		}
		off += rl
	}
	return 0, 0, false
}

func peMachine(head []byte) uint16 {
	if len(head) < 0x40 || head[0] != 'M' || head[1] != 'Z' {
		return 0
	}
	lfanew := int(binary.LittleEndian.Uint32(head[0x3c:0x40]))
	if lfanew <= 0 || lfanew+6 > len(head) {
		return 0
	}
	if string(head[lfanew:lfanew+4]) != "PE\x00\x00" {
		return 0
	}
	return binary.LittleEndian.Uint16(head[lfanew+4 : lfanew+6])
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./platform/ -run TestInspectImage -v`
Expected: PASS, including `TestInspectImageArchRealISOs` against the real ISOs

- [ ] **Step 5: Commit**

```bash
git add platform/image.go platform/image_test.go
git commit -m "feat(platform): detect image arch from the kernel's PE header"
```

---

### Task 6: Wire `platform` into `main.go`

**Files:**
- Modify: `cmd/tinq/main.go` — replace lines 279-284 (args head), 307-319 (binary), 356-378 (`edk2Code`/`makeEFIVars`)

**Interfaces:**
- Consumes: `platform.Detect()`, `platform.InspectImageArch()`, `Platform` fields (Tasks 1-5).
- Produces: no new exported API.

- [ ] **Step 1: Add the import and detect early in `create`**

In the import block add `"github.com/coglative/talos-in-qemu/platform"`.

At the top of `func (h *hvf) create(m *unstructured.Unstructured, dir string) (int, error)`, immediately after `spec, _, _ := unstructured.NestedMap(m.Object, "spec")`:

```go
	// Resolve host facts BEFORE creating any state. Failing here costs nothing;
	// failing after the disk exists leaves residue behind.
	p, err := platform.Detect()
	if err != nil {
		return 0, err
	}
```

The existing `if err := os.MkdirAll(dir, 0o755); err != nil {` below it needs no
change — its `err` is scoped to the `if`, which is legal alongside the outer
`err`. Confirm with `go build ./...`.

- [ ] **Step 2: Warn on image/host arch mismatch**

Immediately after the existing image resolution block (after the `os.Stat(image)` check):

```go
	// A wrong-arch image boots to UEFI, finds no bootable media, and sits there
	// with no console output and no API — indistinguishable from a hang unless
	// we say so. Warn only; detection returning "" must never block a valid
	// image we simply cannot classify.
	if got := platform.InspectImageArch(image); got != "" && got != p.ImageArch {
		log.Printf("warning: image is %s but host is %s\n"+
			"  the VM will start, reach UEFI, find no bootable media, and sit\n"+
			"  there with no console output and no API.\n"+
			"  this is not a hang — it is the wrong image: %s", got, p.ImageArch, image)
	}
```

- [ ] **Step 3: Replace the hardcoded args**

Change the head of the `args` slice from:

```go
		"-machine", "virt,accel=hvf", "-cpu", "host",
```

to:

```go
		"-machine", p.Machine + ",accel=" + p.Accel, "-cpu", p.CPU,
```

Change the pflash code line from `"-drive", "if=pflash,format=raw,readonly=on,file=" + edk2Code(),` to:

```go
		"-drive", "if=pflash,format=raw,readonly=on,file=" + p.FirmwareCode,
```

Change `cmd := exec.Command("qemu-system-aarch64", args...)` to:

```go
	cmd := exec.Command(p.QEMUBinary, args...)
```

Change the EFI vars creation call from `makeEFIVars(varsPath)` to `makeEFIVars(varsPath, p.FirmwareVars)`.

- [ ] **Step 4: Delete `edk2Code` and rewrite `makeEFIVars`**

Delete `func edk2Code() string { ... }` entirely. Replace `makeEFIVars` with:

```go
// makeEFIVars copies the firmware's own nvram template VERBATIM.
//
// The previous version padded to 64 MiB, which was correct on aarch64 only by
// coincidence — edk2's aarch64 vars template genuinely is 67108864 bytes. The
// x86_64 template is 540672 bytes, and padding it makes QEMU refuse to start:
//
//	combined size of system firmware exceeds 8388608 bytes
//
// Copying the template is correct on both arches for the SAME reason instead of
// being right on one by accident.
func makeEFIVars(path, template string) error {
	b, err := os.ReadFile(template)
	if err != nil {
		return fmt.Errorf("read nvram template %s: %w", template, err)
	}
	return os.WriteFile(path, b, 0o644)
}
```

Remove `"strings"` from imports only if `go build` reports it unused (it is still used elsewhere; verify).

- [ ] **Step 5: Build for both platforms and run everything**

```bash
go build ./... && go vet ./... && go test ./... 
GOOS=darwin GOARCH=arm64 go build ./... && echo "darwin/arm64 OK"
GOOS=linux GOARCH=amd64 go build ./... && echo "linux/amd64 OK"
```

Expected: all succeed; `go test ./...` passes.

- [ ] **Step 6: Commit**

```bash
git add cmd/tinq/main.go
git commit -m "feat(tinq): resolve qemu binary, machine, accel and firmware at runtime"
```

---

### Task 7: Remove dead code (Tier 3 — separate commit)

**Files:**
- Modify: `cmd/tinq/main.go:416-419`

- [ ] **Step 1: Confirm it is unreferenced**

Run: `grep -nE '[^a-zA-Z]nested\(' cmd/tinq/main.go`
Expected: only the definition on line 416. (`nestedSlice` is a different symbol and IS used.)

- [ ] **Step 2: Delete the function**

```go
func nested(m *unstructured.Unstructured, f ...string) int64 {
	v, _, _ := unstructured.NestedInt64(m.Object, f...)
	return v
}
```

- [ ] **Step 3: Verify**

Run: `go build ./... && go vet ./...`
Expected: clean

- [ ] **Step 4: Commit separately**

```bash
git add cmd/tinq/main.go
git commit -m "chore: remove unused nested() helper"
```

---

### Task 8: End-to-end verification and README

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Boot a real Talos VM on this host**

```bash
cat > /tmp/tinq-linux-test.yaml <<'YAML'
apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata:
  name: linux-cp0
  namespace: default
spec:
  site: linux-local
  role: talos-cp
  image: talos-v1.9.5-amd64.iso
  cpu: 4
  memory: 6Gi
  disk: 20Gi
  hostForwards:
    - { hostPort: 50000, guestPort: 50000 }
    - { hostPort: 6443,  guestPort: 6443 }
YAML
go run ./cmd/tinq -apply /tmp/tinq-linux-test.yaml
```

Expected: `created: map[apiEndpoint:127.0.0.1:50000 pid:<n> stateDir:...]`

- [ ] **Step 2: Confirm the guest actually booted**

```bash
sleep 30
tail -40 ~/.hvf/linux-local/*/serial.log
```

Expected: Talos kernel output, not an empty file and not a UEFI shell prompt. An empty log means the VM never booted — investigate before proceeding.

- [ ] **Step 3: Confirm the arch guard fires on the wrong image**

```bash
sed -e 's/talos-v1.9.5-amd64.iso/talos-v1.9.5-arm64.iso/' \
    -e 's/linux-local/linux-wrongarch/' \
    -e 's/linux-cp0/linux-wrongarch0/' \
    -e 's/hostPort: 50000/hostPort: 50010/' \
    -e 's/hostPort: 6443/hostPort: 6453/' \
    /tmp/tinq-linux-test.yaml > /tmp/tinq-wrongarch-test.yaml
go run ./cmd/tinq -apply /tmp/tinq-wrongarch-test.yaml
```

Expected: the `warning: image is arm64 but host is amd64` block appears. Distinct
ports are required — the first VM still holds 50000/6443, and QEMU would fail to
bind, masking the warning you are trying to observe.

- [ ] **Step 4: Tear down BOTH VMs**

```bash
go run ./cmd/tinq -destroy /tmp/tinq-wrongarch-test.yaml
go run ./cmd/tinq -destroy /tmp/tinq-linux-test.yaml
ls ~/.hvf/            # linux-local and linux-wrongarch must both be gone
pgrep -a qemu-system-x86_64 || echo "no stray qemu processes"
```

- [ ] **Step 5: Update the README**

- `## Install`: replace "Requires macOS on Apple silicon" with Linux (KVM) and macOS (HVF) instructions, including `metal-amd64.iso` vs `metal-arm64.iso`.
- `## From a booted VM to a cluster`: note that `console=ttyAMA0` is arm64; x86_64 uses `console=ttyS0`.
- `## Status`: delete the "Apple silicon only" bullet; delete "No tests" and state what is now covered.

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "docs: linux support, arch-specific console and image names"
```

---

## Self-Review

**Spec coverage:** arch mapping → T1; accelerator + error text → T2; firmware registry incl. SMM/microvm/priority traps → T3; fallback table + `Detect` → T4; image inspection + boundaries → T5; all seven `main.go` integration points → T6 (points 1-6) and T7 (point 7, dead code); testing strategy → T1-T5; end-to-end proof → T8. No spec section is unimplemented.

**Placeholder scan:** no TBD/TODO-as-instruction. The `TODO(macos-verify):` markers are deliberate, specified by the spec's global constraints, and grep-able for the macOS agent.

**Type consistency:** `archInfo` field names (`qemuBinary`, `machine`, `console`, `imageArch`, `fwArch`) are consistent T1→T4. `scanRegistry` returns `(string, string, bool)` in T3 and is consumed that way in T4. `resolveFirmware` returns `(string, string, error)` in T4. `makeEFIVars` gains its second parameter in T6 at both call site and definition. `slicesContains` is defined in T3 and reused in T4. `contains` is a test helper defined in T1 and reused in T2/T4.

**One deviation from the spec, recorded deliberately:** the spec said ISO and descriptor fixtures would be committed. The plan synthesises them in test code instead — no binaries in git, and the generator documents the on-disk format. The real-ISO integration test still runs against `~/.hvf/images/` when present.
