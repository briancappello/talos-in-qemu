package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
	// The distinctness check above is satisfied by the interpolated errno
	// alone, so it would still pass if kvmMissing and kvmNoPerm were collapsed
	// into one branch. "modprobe" appears only in the kvmMissing branch and
	// cannot leak in from an errno string, so it pins that branch to its own
	// remedy.
	if !strings.Contains(missing, "modprobe") {
		t.Errorf("device-missing failure must give its own remedy, got: %s", missing)
	}
	if !strings.Contains(noperm, "usermod") {
		t.Errorf("permission failure must give the actionable fix, got: %s", noperm)
	}
	// kvmNoPerm covers EBUSY too, so the usermod advice must stay a
	// conditional suggestion rather than regress to a stated diagnosis.
	if !strings.Contains(noperm, "if this is a permission error") {
		t.Errorf("permission advice must stay conditional, got: %s", noperm)
	}
	if !strings.Contains(notbuilt, "not built") {
		t.Errorf("not-compiled-in failure must say so, got: %s", notbuilt)
	}
	for _, m := range []string{missing, noperm, notbuilt} {
		if !strings.Contains(m, "hang") {
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
	if !strings.Contains(msg, busy.Error()) {
		t.Errorf("message must quote the underlying error %q, got: %s", busy, msg)
	}
}

// The success path depends on which QEMU binaries the host has, but the error
// path does not: a binary that cannot exist fails identically everywhere. This
// pins the "%s -accel help: %w" contract so the binary name stays in the message.
func TestCompiledAccelsErrorNamesBinary(t *testing.T) {
	const bin = "qemu-system-definitely-not-installed"
	got, err := compiledAccels(bin)
	if err == nil {
		t.Fatalf("compiledAccels(%q) => %v, nil; want an error", bin, got)
	}
	if !strings.Contains(err.Error(), bin) {
		t.Errorf("error must name the binary it tried, got: %s", err)
	}
}

// A qemu binary that never returns would otherwise wedge Detect() forever, and
// a silent hang is the exact failure this whole package exists to prevent — it
// is indistinguishable from the TCG slowness we refuse to fall back to. The
// stand-in execs sleep so the process we start is the process that blocks:
// killing a shell whose child still holds the output pipe would not unblock
// CombinedOutput.
func TestCompiledAccelsTimesOut(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "qemu-system-hangs")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got, err := compiledAccelsWithin(bin, 100*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("compiledAccelsWithin => %v, nil; want a timeout error", got)
	}
	// Bounded just above the 100ms deadline plus the 1s WaitDelay, not at the
	// stand-in's 30s sleep: a loose bound only proves the process eventually
	// died, which the sleep guarantees on its own. This asserts the DEADLINE
	// fired.
	if elapsed > 2*time.Second {
		t.Errorf("took %v: the timeout did not fire", elapsed)
	}
	// "signal: killed" is what the raw exec error says, which reads like a
	// crash. The user needs to know the binary hung and that we gave up.
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("a hang must be reported as a timeout, got: %v", err)
	}
	if !strings.Contains(err.Error(), bin) {
		t.Errorf("error must name the binary, got: %v", err)
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
