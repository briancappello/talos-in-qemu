package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
//
// Scope: this heuristic only recovers SUFFIX-FORM aliases, where the alias is
// the tail of the glob's stem ("pc-q35-*"->"q35", "virt-*"->"virt"). It does
// NOT recover QEMU's "pc" alias for "pc-i440fx-*", whose stem does not end in
// "-pc". That is fine because we never invoke -machine pc, but a caller that
// did would need real alias resolution here, not a suffix trim.
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
	if !slices.Contains(d.InterfaceTypes, "uefi") {
		return false
	}
	// Secure-boot firmware needs -machine q35,smm=on plus more. On Arch the
	// secure descriptor sorts FIRST, so without this filter a take-the-first
	// matcher reliably selects firmware that cannot boot as invoked.
	//
	// COUPLED to how this project invokes QEMU: we pass `-machine q35` with no
	// smm=on. If that invocation ever gains smm=on (and the matching pflash
	// wiring), this rejection becomes wrong and must change with it.
	if slices.Contains(d.Features, "requires-smm") || slices.Contains(d.Features, "secure-boot") {
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

// scanRegistry walks the descriptor directories in decreasing priority and
// returns EVERY suitable {code, nvram-template} pair, most-preferred first.
//
// All of them, not just the best one. A descriptor outlives the package that
// installed the files it names — Debian's ovmf descriptors survive the package
// being removed — so the winner can point at nothing. Returning only the first
// suitable candidate meant one rotted descriptor dropped us straight to the
// static path table, which is precisely the thing querying the registry exists
// to avoid. The caller filters on existence and takes the first pair that is
// really installed, so a dead descriptor costs us the NEXT descriptor, not the
// whole registry.
//
// Within the combined set, files sort by BASENAME (lower numeric prefix wins),
// and a file in an earlier directory masks the same basename in a later one.
func scanRegistry(dirs []string, fwArch, machine string) [][2]string {
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
	slices.Sort(names)
	var out [][2]string
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
			out = append(out, [2]string{d.Mapping.Executable.Filename, d.Mapping.NVRAMTemplate.Filename})
		}
	}
	return out
}

// registryDirs is the descriptor search path in decreasing priority, per the
// QEMU interop spec. /etc is the admin override.
var registryDirs = []string{"/etc/qemu/firmware", "/usr/share/qemu/firmware"}

// fallbackTable is used only when the registry yields nothing — the expected
// case on Homebrew, which is not known to ship descriptors. Entries are
// {code, nvram-template} pairs tried in order.
//
// TODO(macos-verify): the /opt/homebrew and /usr/local paths below are the ones
// the pre-refactor edk2Code() used. Unverified from Linux; the file names in
// particular (edk2-x86_64-code.fd paired with edk2-i386-vars.fd) are the ones
// Homebrew's qemu formula is believed to install, and nothing here has been run
// on a Mac.
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

// resolveFirmware finds a usable {code, nvram-template} pair: the descriptor
// registry first, then the static table. The table takes the fallback as a
// parameter rather than reading the package var so tests supply their own
// without mutating package state.
//
// Every candidate is checked for existence, the registry's answers included: a
// descriptor outlives the package that installed the files it names. Both
// sources are lists of candidate pairs filtered the same way, so a rotted
// descriptor costs us the next descriptor rather than the whole registry.
func resolveFirmware(dirs []string, table map[string][][2]string, goos, fwArch, machine string) (string, string, error) {
	var tried []string
	for _, pair := range scanRegistry(dirs, fwArch, machine) {
		if fileExists(pair[0]) && fileExists(pair[1]) {
			return pair[0], pair[1], nil
		}
		tried = append(tried, fmt.Sprintf("%s (nvram %s) - named by a firmware descriptor", pair[0], pair[1]))
	}
	for _, pair := range table[fwArch] {
		if fileExists(pair[0]) && fileExists(pair[1]) {
			return pair[0], pair[1], nil
		}
		tried = append(tried, fmt.Sprintf("%s (nvram %s)", pair[0], pair[1]))
	}
	// Both paths of every pair are listed. A code image that IS installed
	// beside a differently-named vars template is a real packaging shape, and
	// naming only the code image sends the user looking for a file they have.
	//
	// tried can be empty — an architecture with no table entry and no
	// descriptor match reaches here — and a "then these paths:" heading over
	// nothing reads like the list was lost. Say so instead.
	paths := "\n\nthen these paths:\n  " + strings.Join(tried, "\n  ")
	if len(tried) == 0 {
		paths = fmt.Sprintf("\n\nno fallback firmware paths are known for architecture %q", fwArch)
	}
	return "", "", fmt.Errorf(
		"no UEFI firmware found for %s/%s on %s\n\nsearched descriptor registries:\n  %s%s\n\ninstall your distribution's edk2/OVMF package",
		fwArch, machine, goos, strings.Join(dirs, "\n  "), paths)
}

// fileExists rejects directories: -pflash needs a file, and a directory at the
// expected path would only fail much later, inside QEMU.
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
