package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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
	if got, err := diagnoseKVM(filepath.Join(t.TempDir(), "nope")); got != kvmMissing || err == nil {
		t.Errorf("absent device => %v, %v; want kvmMissing and a non-nil error", got, err)
	}
	p := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(p, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission check is meaningless")
	}
	got, err := diagnoseKVM(p)
	if got != kvmNoPerm {
		t.Errorf("unreadable device => %v, want kvmNoPerm", got)
	}
	// kvmNoPerm covers every non-ENOENT errno, so the caller can only tell
	// EACCES from EBUSY if the raw error survives.
	if err == nil {
		t.Error("kvmNoPerm must carry the underlying error")
	}
}

// The three failure modes must be distinguishable. "enable hardware
// acceleration" alone does not tell the user which case they are in.
func TestAccelUnavailableMessagesDiffer(t *testing.T) {
	missing := accelUnavailable("linux", "amd64", "kvm", true, kvmMissing, os.ErrNotExist).Error()
	noperm := accelUnavailable("linux", "amd64", "kvm", true, kvmNoPerm, os.ErrPermission).Error()
	notbuilt := accelUnavailable("linux", "amd64", "kvm", false, kvmOK, nil).Error()

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

// Every non-ENOENT errno lands in kvmNoPerm, so EBUSY ("another hypervisor
// holds /dev/kvm") would otherwise be reported as a group-membership problem
// and send the user down a dead end. The raw error has to be in the message.
func TestAccelUnavailableSurfacesRawError(t *testing.T) {
	busy := &fs.PathError{Op: "open", Path: "/dev/kvm", Err: syscall.EBUSY}
	msg := accelUnavailable("linux", "amd64", "kvm", true, kvmNoPerm, busy).Error()
	if !contains(msg, busy.Error()) {
		t.Errorf("message must quote the underlying error %q, got: %s", busy, msg)
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
