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
