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
