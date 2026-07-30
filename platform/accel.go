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
//
// The underlying error is returned alongside the diagnosis because kvmNoPerm
// covers every errno that is not ENOENT -- EBUSY (another hypervisor holds the
// device) looks identical to EACCES from here, and only the raw error can tell
// the user which one they are actually in. It is nil for kvmOK and non-nil for
// both failure cases, kvmMissing included.
func diagnoseKVM(path string) (kvmDiag, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err == nil {
		f.Close()
		return kvmOK, nil
	}
	if os.IsNotExist(err) {
		return kvmMissing, err
	}
	return kvmNoPerm, err
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

// accelUnavailable explains why no accelerator is usable. diagErr is the raw
// error from diagnoseKVM; it is quoted verbatim so the message never asserts a
// cause the errno does not support.
func accelUnavailable(goos, goarch, accel string, compiled bool, diag kvmDiag, diagErr error) error {
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
		reason = fmt.Sprintf("/dev/kvm exists but could not be opened by uid %d: %v", os.Getuid(), diagErr)
		fix = "if this is a permission error: sudo usermod -aG kvm $USER\n" +
			"       (then log out and back in); if another hypervisor holds\n" +
			"       /dev/kvm, stop it first"
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
