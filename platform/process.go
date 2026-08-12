package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ProcessMatches reports whether pid is a live process that is the qemu of the
// machine whose state dir is dir.
//
// It exists because kill(pid, 0) is not enough. That call proves only that SOME
// signalable process holds the pid — and once a machine can be stopped without
// being destroyed, "state dir present, pidfile stale" becomes the normal
// resting state, consulted on every Observe. After a host reboot the kernel
// reallocates pids from low numbers, so a stale pidfile can name a live,
// unrelated process. Two things then go wrong: a stopped machine reports as
// running and never gets started, and Stop/destroy SIGKILL somebody else's
// process — with several machines, plausibly a different machine's qemu.
//
// dir is the machine's state dir, which is unique per machine and already
// appears in the qemu argv (-pidfile, -drive). Nothing new has to be recorded
// to make this work. It is matched at a path BOUNDARY, not as a bare substring
// — see machineToken.
//
// This assumes qemu runs as the same uid as us: kill(pid, 0) returns EPERM for
// a live process owned by another user, so if qemu were ever launched under a
// different uid (sudo, a systemd unit) this would return false forever and
// Observe would report Stopped while a live VM keeps running.
func ProcessMatches(pid int, dir string) bool {
	if pid <= 0 || dir == "" {
		return false
	}
	// Cheap liveness first: no point shelling out for a pid that is gone.
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	cmdline, err := processCmdline(pid)
	if err != nil {
		// Unreadable command line means we cannot prove it is ours, and an
		// unproven match is the bug this function exists to prevent.
		return false
	}
	return strings.Contains(cmdline, machineToken(dir))
}

// machineToken is the argv needle for a machine's state dir, and the trailing
// separator is the whole point.
//
// A bare dir matches on a PREFIX. Machine UIDs are bootstrap-<ns>-<name> built
// from user-chosen names, so a machine called "t" has a state dir that is a
// strict prefix of the one belonging to "t2" — and a plain substring search
// says machine t's pid matches machine t2's running qemu. Measured against a
// real qemu-shaped argv: `.../bootstrap-default-t` -> true, when it must be
// false. The consequence is the worst one this package exists to prevent: t's
// stale pidfile names t2's pid, so t's Stop/destroy SIGKILLs t2's VM.
//
// Every path qemu carries the dir in is a path UNDER it (-pidfile
// <dir>/qemu.pid, -drive file=<dir>/system.qcow2, -serial file:<dir>/serial.log),
// so the separator is always present for a genuine match and never present for
// a prefix one. Clean first because a caller-supplied root could arrive with a
// trailing slash, which would otherwise build "<dir>//" and match nothing.
//
// This is built HERE rather than at the call sites on purpose. Observe,
// waitGone and halt all ask the same question, and a token assembled three
// times is a token that drifts — one site keeping the bare dir is enough to
// reopen the collision for the path that runs through it.
func machineToken(dir string) string {
	return filepath.Clean(dir) + string(filepath.Separator)
}

// parseCmdline turns /proc's NUL-separated argv into something searchable.
// Pure, so the separation quirk is testable without spawning a process.
func parseCmdline(raw []byte) string {
	return strings.ReplaceAll(string(raw), "\x00", " ")
}

// processCmdline returns pid's full command line, host-specifically.
//
// Linux reads /proc directly: no fork, and it is exact. macOS has no /proc, and
// proc_pidpath yields only the executable — enough to say "some qemu", useless
// for saying WHICH machine's, which is the only question that matters here. So
// macOS shells out to ps. Crude, but this package already shells out to
// qemu-img and qemu-system-*, and correctness beats elegance for a check whose
// failure mode is killing the wrong process.
func processCmdline(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		return linuxCmdline(pid)
	}
	return psCmdline(pid)
}

// psCmdline returns pid's command line by shelling out to ps.
//
// Split out of processCmdline so a test can call it directly instead of going
// through the runtime.GOOS dispatch — that is the only way the -ww regression
// below is covered by CI, which runs on Linux.
//
// -ww disables ps's width limit. Without it ps truncates to the terminal width,
// and a qemu argv is long enough that the state-dir token we search for lands
// past the cut — a running machine would report as stopped, so Observe would
// keep trying to start a VM that is already up. Note that ps only truncates
// when it believes it has a width (a tty, or COLUMNS set); writing to a pipe it
// does not, which is exactly why this bug survived a plain `go test`.
func psCmdline(pid int) (string, error) {
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// linuxCmdline reads /proc, so it only returns anything useful on Linux — but
// it is deliberately untagged: os.ReadFile and strconv compile everywhere, so
// the runtime.GOOS guard above is the only gate needed and the compiler still
// checks this file on a Mac.
func linuxCmdline(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return "", err
	}
	return parseCmdline(b), nil
}
