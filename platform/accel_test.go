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
