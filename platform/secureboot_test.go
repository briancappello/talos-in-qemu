package platform

import (
	"fmt"
	"strings"
	"testing"
)

// secureDescWithPaths is descWithPaths' secure-boot twin: same shape, but
// carrying the two features QEMU's own 50-edk2-ovmf-x86_64-secure-4m.json
// advertises.
func secureDescWithPaths(code, vars string) string {
	return fmt.Sprintf(`{"description":"secure","interface-types":["uefi"],
"mapping":{"device":"flash","executable":{"filename":%q},"nvram-template":{"filename":%q}},
"targets":[{"architecture":"x86_64","machines":["pc-q35-*"]}],
"features":["requires-smm","secure-boot"]}`, code, vars)
}

// A secure-boot request must NOT be satisfied by plain firmware. Silently
// booting non-secure OVMF is the failure this mode exists to prevent: the guest
// comes up with SecureBoot permanently unavailable, and the damage surfaces much
// later as a TPM key sealed against a PCR 7 that changes the moment SecureBoot
// is switched on.
func TestSecureBootRejectsPlainFirmware(t *testing.T) {
	reg := t.TempDir()
	fw := t.TempDir()
	code, vars := touch(t, fw, "plain-code.fd"), touch(t, fw, "vars.fd")
	writeDesc(t, reg, "10-plain.json", descWithPaths(code, vars))

	if _, _, err := resolveFirmware([]string{reg}, map[string][][2]string{}, "linux", "x86_64", "q35", true); err == nil {
		t.Fatal("secure-boot request was satisfied by plain firmware")
	}
}

// The mirror image: a plain machine must never be handed secure firmware. tinq
// emits smm=on only when secureBoot is set, and secure OVMF hangs without it.
func TestPlainRequestRejectsSecureFirmware(t *testing.T) {
	reg := t.TempDir()
	fw := t.TempDir()
	code, vars := touch(t, fw, "sec-code.fd"), touch(t, fw, "vars.fd")
	writeDesc(t, reg, "10-secure.json", secureDescWithPaths(code, vars))

	if _, _, err := resolveFirmware([]string{reg}, map[string][][2]string{}, "linux", "x86_64", "q35", false); err == nil {
		t.Fatal("plain request was satisfied by secure-boot firmware")
	}
}

// With both installed, each mode picks its own.
func TestSecureBootPicksSecureDescriptor(t *testing.T) {
	reg := t.TempDir()
	fw := t.TempDir()
	secCode := touch(t, fw, "sec-code.fd")
	plainCode := touch(t, fw, "plain-code.fd")
	vars := touch(t, fw, "vars.fd")

	writeDesc(t, reg, "10-secure.json", secureDescWithPaths(secCode, vars))
	writeDesc(t, reg, "20-plain.json", descWithPaths(plainCode, vars))

	gotCode, _, err := resolveFirmware([]string{reg}, map[string][][2]string{}, "linux", "x86_64", "q35", true)
	if err != nil {
		t.Fatalf("secure: %v", err)
	}
	if gotCode != secCode {
		t.Fatalf("secure picked %q, want %q", gotCode, secCode)
	}

	gotCode, _, err = resolveFirmware([]string{reg}, map[string][][2]string{}, "linux", "x86_64", "q35", false)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if gotCode != plainCode {
		t.Fatalf("plain picked %q, want %q", gotCode, plainCode)
	}
}

// The secure fallback table must pair a secure CODE image with the BLANK vars
// template, never a *_VARS.secboot.fd. A preloaded store already carries
// Microsoft's PK, so the guest is NOT in setup mode: sd-boot's
// `secure-boot-enroll` silently does nothing, and our UKI -- signed by a key
// absent from that db -- fails to validate.
func TestSecureFallbackTableUsesBlankVarsTemplate(t *testing.T) {
	table := secureFallbackTable["x86_64"]
	if len(table) == 0 {
		t.Fatal("no secure fallback entries for x86_64")
	}
	for _, pair := range table {
		code, vars := pair[0], pair[1]
		if !strings.Contains(code, "secboot") {
			t.Errorf("code image %q does not look like secure-boot firmware", code)
		}
		if strings.Contains(vars, "secboot") {
			t.Errorf("vars template %q is preloaded; setup mode needs the blank store", vars)
		}
	}
}
