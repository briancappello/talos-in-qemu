package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The live-process cases run against THIS test binary, so they exercise the
// real host-specific path on whatever platform runs them — which is the only
// way the macOS branch gets covered at all without a macOS CI runner.
func TestProcessMatchesOurself(t *testing.T) {
	// The DIRECTORY holding the test binary stands in for a state dir: argv0 is
	// a path under it, exactly as every qemu path is a path under the machine's
	// state dir. Passing the bare basename would not exercise the boundary rule
	// machineToken enforces.
	dir := filepath.Dir(os.Args[0])
	if !ProcessMatches(os.Getpid(), dir) {
		t.Fatalf("ProcessMatches(self, %q) = false, want true", dir)
	}
}

func TestProcessMatchesRejectsWrongToken(t *testing.T) {
	if ProcessMatches(os.Getpid(), "/definitely-not-in-our-argv-9f3a2b") {
		t.Fatal("ProcessMatches matched a dir that is not in our command line")
	}
}

func TestProcessMatchesRejectsDeadPid(t *testing.T) {
	// PID 0 is never a normal process; negative pids are signal-group syntax.
	for _, pid := range []int{0, -1, -12345} {
		if ProcessMatches(pid, "/anything") {
			t.Fatalf("ProcessMatches(%d) = true, want false", pid)
		}
	}
}

// The prefix collision, with the fixture that makes it concrete: machine "t"
// and machine "t2" in the same site. UIDs are bootstrap-<ns>-<name> and names
// come from the user, so t/t2 is an ordinary pair, not a contrivance.
//
// Only t2's qemu runs. If t's stale pidfile happens to name t2's pid, a bare
// substring match tells machine t that t2's process is its own — and t's
// Stop/destroy then SIGKILLs t2's VM. That is the exact failure the design doc
// names as the worst case, so it is pinned against a REAL process with a
// qemu-shaped argv rather than against a hand-written string.
func TestProcessMatchesDoesNotMatchAPrefixOfAnotherMachine(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "site-a", "bootstrap-default-t")
	dirB := filepath.Join(root, "site-a", "bootstrap-default-t2")

	// The dir appears only as the prefix of paths UNDER it, as qemu's does.
	// The `while` loop stops sh from exec'ing away and taking the argv with it.
	b := exec.Command("sh", "-c", "while :; do sleep 1; done",
		"-pidfile", filepath.Join(dirB, "qemu.pid"),
		"-drive", "if=none,id=sys,format=qcow2,file="+filepath.Join(dirB, "system.qcow2"))
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Process.Kill(); _ = b.Wait() })

	// The kernel publishes arg_start/arg_end a moment after exec returns, so a
	// live process reads with an EMPTY cmdline for a short window. Waiting for
	// the argv to become visible is synchronisation, not a workaround.
	deadline := time.Now().Add(2 * time.Second)
	for !ProcessMatches(b.Process.Pid, dirB) {
		if time.Now().After(deadline) {
			t.Fatalf("decoy %d never carried %q in its argv, so nothing below is exercised",
				b.Process.Pid, dirB)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if ProcessMatches(b.Process.Pid, dirA) {
		t.Errorf("machine %q matched machine %q's qemu — its Stop/destroy would SIGKILL the wrong VM",
			filepath.Base(dirA), filepath.Base(dirB))
	}
}

func TestMachineTokenEndsAtAPathBoundary(t *testing.T) {
	argv := "qemu-system-x86_64 -pidfile /state/site-a/bootstrap-default-t2/qemu.pid"
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"/state/site-a/bootstrap-default-t", false}, // prefix of another machine
		{"/state/site-a/bootstrap-default-t2", true},
		{"/state/site-a/bootstrap-default-t2/", true}, // a trailing slash must not break it
	} {
		if got := strings.Contains(argv, machineToken(tc.dir)); got != tc.want {
			t.Errorf("machineToken(%q) = %q, matched %v, want %v", tc.dir, machineToken(tc.dir), got, tc.want)
		}
	}
}

// TestHelperProcess is not a real test. It is the stdlib re-exec trick: the
// test binary runs itself with GO_WANT_HELPER_PROCESS set to get a child with
// an argv we control. Under `go test` the variable is unset and this returns
// immediately.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	// Stay alive long enough for the parent to read our argv. The parent kills
	// us; this bound only stops an orphan from lingering forever.
	time.Sleep(time.Minute)
	os.Exit(0)
}

// TestPsCmdlineSurvivesNarrowTerminal is the regression test for the missing
// -ww: ps truncates its output to the terminal width, and a qemu argv is long
// enough that the state-dir token lands past the cut. Truncated output means
// ProcessMatches returns false for a live machine, so Observe reports Stopped
// and reconcile tries to boot a VM that is already running.
//
// COLUMNS is what makes this reproducible. ps only truncates when it thinks it
// has a width — from a tty, or from COLUMNS. Writing to a pipe, as exec.Command
// does, it does not truncate, which is exactly why the bug survived a plain
// `go test`. Setting COLUMNS forces the truncating path; exec.Command inherits
// os.Environ(), so t.Setenv reaches the ps child.
//
// Scope, honestly: this proves the behaviour of procps ps (Linux), where the
// test actually runs. psCmdline is only reached on non-Linux hosts, i.e. macOS
// and its BSD ps, which no CI runner here executes. "repeat -w for unlimited
// width" is the documented BSD idiom, so -ww is the right flag on both — but
// only a Mac proves that half.
func TestPsCmdlineSurvivesNarrowTerminal(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skipf("no ps on this host: %v", err)
	}
	t.Setenv("COLUMNS", "80")

	const token = "state-dir-token-9f3a2b"
	// Padding puts the token far past column 80, where an unfixed ps would cut
	// it off. "--" stops the test binary's flag parsing so the padding and
	// token are inert positional args.
	padding := strings.Repeat("x", 400)
	helper := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", padding, token)
	helper.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if err := helper.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	got, err := psCmdline(helper.Process.Pid)
	if err != nil {
		t.Fatalf("psCmdline(%d): %v", helper.Process.Pid, err)
	}
	// Guard against a vacuous pass: if the argv never got long, ps had nothing
	// to truncate and the assertion below would hold with or without -ww.
	if len(got) <= 80 {
		t.Fatalf("helper argv is only %d bytes, too short to exercise truncation: %q", len(got), got)
	}
	if !strings.Contains(got, token) {
		t.Fatalf("psCmdline dropped the token past column 80 (got %d bytes): %q", len(got), got)
	}
}

// parseCmdline is pure, so the NUL-separation quirk is testable without
// spawning anything.
func TestParseCmdlineJoinsNulSeparatedArgs(t *testing.T) {
	raw := []byte("qemu-system-x86_64\x00-pidfile\x00/state/dir/qemu.pid\x00")
	got := parseCmdline(raw)
	if !strings.Contains(got, "/state/dir") {
		t.Fatalf("parseCmdline(%q) = %q, want it to contain /state/dir", raw, got)
	}
	if strings.Contains(got, "\x00") {
		t.Fatalf("parseCmdline left NUL bytes in %q", got)
	}
}
