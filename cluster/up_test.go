package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coglative/talos-in-qemu/platform"
)

// THE OUTPUT IS THE FEATURE, so the output is what this file tests.
//
// Almost every step of a bring-up needs a booted VM, and none of the printing
// does. Up therefore takes its VM-facing operations as hooks (upHooks) and its
// destination as an io.Writer, which is what lets the whole ten-step transcript
// — including the two notes that exist to make silent failures visible — be
// asserted here with nothing running.
//
// The same secret rule as config_test.go applies: nothing derived from
// generated material reaches t.Errorf/t.Fatalf except through redact(). The
// fakes below deliberately carry SECRET-SHAPED markers so
// TestUpNeverPrintsSecrets can prove Up does not interpolate any of them.

// The markers are what a leak would look like. They are base64-shaped and long
// enough that redact() covers them too, so even a failure dump of the
// transcript cannot publish them.
// imageTalosVersion is the version the fake ISO reports, and it is
// DELIBERATELY NOT the generator's.
//
// The installer tag is pinned to the IMAGE, and the whole failure that pin
// exists to prevent is Talos substituting the generator's version instead. A
// fixture where the two agree cannot tell the two apart: `TalosVersion:
// GeneratorVersion()` in the wiring produces a byte-identical transcript and
// survives the entire suite. It is older, not newer, so the version guard still
// passes — an older image is exactly what the guard is designed to admit.
const imageTalosVersion = "v1.12.3"

const (
	fakeControlPlane = "controlplane-secret-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	fakeTalosconfig  = "talosconfig-secret-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	fakeSecrets      = "secretsbundle-secret-CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	fakeKubeconfig   = "kubeconfig-secret-DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
)

// recorder captures what Up asked its hooks to do, so a test can assert on
// "storage was never installed" as well as on what got printed.
type recorder struct {
	imageVersion string
	generateErr  error
	failAt       string
	err          error

	called []string
	input  ConfigInput
	// payload is the secret artifact each operation was handed. All four are
	// []byte and every hook takes one, so a swap COMPILES — and applying the
	// talosconfig to a node in maintenance mode, or installing storage with
	// the machine config, fails on the node rather than here.
	payload map[string][]byte
}

func (r *recorder) call(name string, payload ...[]byte) error {
	r.called = append(r.called, name)

	if len(payload) == 1 {
		if r.payload == nil {
			r.payload = map[string][]byte{}
		}

		r.payload[name] = payload[0]
	}

	if r.failAt == name {
		if r.err != nil {
			return r.err
		}

		return fmt.Errorf("%s failed", name)
	}

	return nil
}

func (r *recorder) did(name string) bool {
	for _, c := range r.called {
		if c == name {
			return true
		}
	}

	return false
}

func (r *recorder) hooks() *upHooks {
	return &upHooks{
		detectVersion: func(string) string { return r.imageVersion },
		generateConfig: func(in ConfigInput) (*Generated, error) {
			r.input = in

			// The fake must not accept input the REAL GenerateConfig rejects.
			// A fake more permissive than the thing it stands in for is how
			// "-up boots a VM for an image it has already proven it cannot
			// configure" survived this suite: the whole ten-step transcript
			// ran to success with TalosVersion "", a value GenerateConfig has
			// no branch that accepts. The precondition is the real error
			// function, so the two cannot drift.
			if in.TalosVersion == "" {
				return nil, errUnknownTalosVersion()
			}

			if err := r.call("generateConfig"); err != nil {
				return nil, err
			}

			if r.generateErr != nil {
				return nil, r.generateErr
			}

			return &Generated{
				ControlPlane: []byte(fakeControlPlane),
				Talosconfig:  []byte(fakeTalosconfig),
				Secrets:      []byte(fakeSecrets),
			}, nil
		},
		waitMaintenance: func(context.Context, string, time.Duration) error {
			return r.call("waitMaintenance")
		},
		applyConfig: func(_ context.Context, _ string, config []byte) error {
			return r.call("applyConfig", config)
		},
		waitBootstrapReady: func(_ context.Context, talosconfig []byte, _ string, _ time.Duration) error {
			return r.call("waitBootstrapReady", talosconfig)
		},
		bootstrap: func(_ context.Context, talosconfig []byte, _ string) error {
			return r.call("bootstrap", talosconfig)
		},
		kubeconfig: func(_ context.Context, talosconfig []byte, _ string) ([]byte, error) {
			if err := r.call("kubeconfig", talosconfig); err != nil {
				return nil, err
			}

			return []byte(fakeKubeconfig), nil
		},
		waitNodeReady: func(_ context.Context, kubeconfig []byte, _ time.Duration) error {
			return r.call("waitNodeReady", kubeconfig)
		},
		installStorage: func(_ context.Context, kubeconfig []byte) error {
			return r.call("installStorage", kubeconfig)
		},
	}
}

// fakePlatform is the host facts Detect would return. NONE of these values may
// be the ones the test binary's own host would produce: a console arg of
// "console=ttyS0" or an OS read from runtime.GOOS would let a hardcoded host
// fact in up.go pass on the developer's machine and fail nowhere.
//
// OS is pinned to "linux" for exactly that reason, and it is the field that
// caught the leak — up.go printed runtime.GOOS, so the transcript said
// "darwin/amd64" on a Mac while every other value on the line was injected.
func fakePlatform() *platform.Platform {
	return &platform.Platform{
		OS:         "linux",
		QEMUBinary: "qemu-system-fake",
		Machine:    "q35",
		Accel:      "kvm",
		CPU:        "host",
		ConsoleArg: "console=ttyFAKE0",
		ImageArch:  "amd64",
	}
}

// upFixture builds an UpOptions wired to a recorder, a temp state dir and a
// buffer, plus the booted flag so "the guard ran before the VM was created"
// is assertable.
type upFixture struct {
	opts UpOptions
	rec  *recorder
	out  *strings.Builder
	dir  string
	// booted records whether Boot was called, and how many times.
	booted int
}

func newFixture(t *testing.T) *upFixture {
	t.Helper()

	f := &upFixture{
		rec: &recorder{imageVersion: imageTalosVersion},
		out: &strings.Builder{},
		dir: t.TempDir(),
	}

	f.opts = UpOptions{
		ClusterName:      "probe",
		ImagePath:        filepath.Join(t.TempDir(), "talos-"+imageTalosVersion+"-amd64.iso"),
		StateDir:         f.dir,
		TalosEndpoint:    "127.0.0.1:50000",
		KubeEndpoint:     "https://127.0.0.1:6443",
		SystemDiskSerial: "talos-system",
		DataDiskSerial:   "talos-data",
		Detect:           func() (*platform.Platform, error) { return fakePlatform(), nil },
		Boot: func() (int, error) {
			f.booted++

			return 163166, nil
		},
		Out:   f.out,
		hooks: f.rec.hooks(),
	}

	return f
}

func (f *upFixture) run(t *testing.T) error {
	t.Helper()

	return Up(context.Background(), f.opts)
}

func (f *upFixture) mustRun(t *testing.T) string {
	t.Helper()

	if err := f.run(t); err != nil {
		t.Fatalf("Up: %s\n%s", redactErr(err), redact(f.out.String()))
	}

	return f.out.String()
}

// wants asserts every fragment is present, and dumps the (redacted) transcript
// once if any is missing.
func wants(t *testing.T, transcript string, fragments ...string) {
	t.Helper()

	for _, want := range fragments {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript does not contain %q\n%s", want, redact(transcript))
		}
	}
}

// The step line is the contract: the number, the total and the label. Asserting
// on the label alone lets two steps swap places and the suite stay green — and
// the ORDER is the thing an operator reads a bring-up transcript for.
func TestUpPrintsTheTenAnnouncedStepsInOrder(t *testing.T) {
	f := newFixture(t)
	transcript := f.mustRun(t)

	steps := []string{
		"[ 1/10] platform",
		"[ 2/10] image",
		"[ 3/10] version guard",
		"[ 4/10] boot",
		"[ 5/10] maintenance",
		"[ 6/10] config",
		"[ 7/10] apply-config",
		"[ 8/10] bootstrap",
		"[ 9/10] kubeconfig",
		"[10/10] storage",
	}

	at := -1

	for _, step := range steps {
		i := strings.Index(transcript, step)
		if i < 0 {
			t.Fatalf("no %q line in the transcript\n%s", step, redact(transcript))
		}

		if i < at {
			t.Errorf("%q is printed out of order\n"+
				"  reason: the transcript is what an operator reads a bring-up by; steps that swap "+
				"places describe a bring-up that did not happen\n%s", step, redact(transcript))
		}

		at = i
	}

	// A reason printed flush left reads as a step of its own, and the
	// transcript's whole shape is "the operation, then why". Every line inside
	// the numbered block is therefore indented past where a step line's text
	// begins.
	for _, line := range strings.Split(transcript[:strings.Index(transcript, "[10/10]")], "\n") {
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}

		if !strings.HasPrefix(line, "        ") {
			t.Errorf("a continuation line is not indented under its step: %q\n"+
				"  reason: at column zero it reads as a step of its own, and the reason stops belonging to the operation", line)
		}
	}

	// The operation each step performed, not just its name.
	wants(t, transcript,
		"linux/amd64", "qemu-system-fake", "kvm",
		"talos-"+imageTalosVersion+"-amd64.iso -> "+imageTalosVersion+" (ISO volume id)",
		"machinery "+GeneratorVersion()+" >= image "+imageTalosVersion,
		"pid 163166", "api 127.0.0.1:50000",
		"controlplane.yaml", "talosconfig",
		"local-path-provisioner "+LocalPathVersion,
	)
}

// CARRIED REQUIREMENT 1. CheckVersion returns (checked, err) and `checked` is
// the ONLY signal that the guard never ran: a pre-release volume id such as
// TALOS_V1_14_0_ALPHA reads as "" from InspectImageVersion, CheckVersion
// returns (false, nil), and a caller writing `_, err :=` re-disables the guard
// for exactly the images most likely to break config generation.
//
// The verdict is a REFUSAL, and it lands before Boot. GenerateConfig has no
// branch that accepts an empty version, so announcing this and continuing
// spends a VM, a state dir and the five-minute maintenance wait to arrive at a
// failure the ISO's volume id already proved.
func TestUpRefusesAnImageItCouldNotIdentifyBeforeBooting(t *testing.T) {
	f := newFixture(t)
	f.rec.imageVersion = ""

	err := f.run(t)
	if err == nil {
		t.Fatal("Up continued past an image whose Talos version could not be determined\n" +
			"  reason: GenerateConfig refuses an empty version unconditionally, so this arm is already fatal")
	}

	if f.booted != 0 {
		t.Errorf("the VM was booted %d times for an image the guard could not identify\n"+
			"  reason: failing here costs nothing; failing after the disk exists leaves residue", f.booted)
	}

	if f.rec.did("waitMaintenance") {
		t.Error("Up spent the maintenance budget on an image it had already proven it cannot configure")
	}

	transcript := f.out.String()

	// The operator must still learn WHY, and get the remedy — which lives in
	// the shared refusal, not in the transcript.
	wants(t, transcript,
		"[ 3/10] version guard",
		"REFUSED",
		"could not be determined",
		"TALOS_V1_14_0_ALPHA",
	)
	wants(t, err.Error(), "could not determine the Talos version", "TALOS_V1_13_7")

	// An unknown version must not read as a passing guard.
	if regexp.MustCompile(`(?m)^\[ 3/10\] version guard .*\bok\b`).MatchString(transcript) {
		t.Errorf("the version guard reports ok for an image it could not identify\n%s", redact(transcript))
	}

	// And step 2 must say so too. Printing the empty string leaves a line
	// reading "talos.iso -> (ISO volume id)", which is a blank where the one
	// value the next step depends on should be.
	wants(t, transcript, "-> UNKNOWN")
}

// The other side of it: an image that WAS identified must not print the
// refusal, or the warning becomes noise and stops being read.
func TestUpDoesNotAnnounceASkippedGuardForAKnownImage(t *testing.T) {
	transcript := newFixture(t).mustRun(t)

	for _, unwanted := range []string{"could not be determined", "REFUSED"} {
		if strings.Contains(transcript, unwanted) {
			t.Errorf("a fully identified image printed %q\n"+
				"  reason: a warning that fires on every run is a warning nobody reads", unwanted)
		}
	}
}

// The guard exists to stop a config being generated for a Talos that does not
// exist. Refusing AFTER the VM has been created leaves residue behind for a
// failure that cost nothing to see coming.
func TestUpRefusesAnImageNewerThanTheGeneratorBeforeBooting(t *testing.T) {
	f := newFixture(t)
	f.rec.imageVersion = "v1.99.0"

	err := f.run(t)
	if err == nil {
		t.Fatal("Up generated a cluster from an image newer than the generator\n" +
			"  reason: exceeding the version contract does not error, it silently emits a config for a Talos that does not exist")
	}

	if f.booted != 0 {
		t.Errorf("the VM was booted %d times before the version guard refused\n"+
			"  reason: failing here costs nothing; failing after the disk exists leaves residue", f.booted)
	}

	if f.rec.did("generateConfig") {
		t.Error("a config was generated for an image the guard refused")
	}
}

// CARRIED REQUIREMENT 2. `dataDisk: 40` — the unit omitted — decodes as a
// float64, specDataDisk reads it as "not set", and there is no disk and no
// error. Without this line the first sign of the typo is a PVC that stays
// Pending an hour later.
func TestUpAnnouncesStorageWasSkippedWithoutADataDisk(t *testing.T) {
	f := newFixture(t)
	f.opts.DataDiskSerial = ""

	transcript := f.mustRun(t)

	wants(t, transcript,
		"[10/10] storage",
		"skipped (spec.dataDisk not set)",
	)

	if f.rec.did("installStorage") {
		t.Error("storage was installed with no data disk\n" +
			"  reason: PVCs would land on EPHEMERAL beside etcd, which is the failure the data disk exists to prevent")
	}

	// The two halves of storage must not disagree: no data disk means no user
	// volume in the config either.
	if f.rec.input.DataDiskSerial != "" {
		t.Errorf("GenerateConfig was asked for a user volume on %q with no data disk",
			f.rec.input.DataDiskSerial)
	}

	if strings.Contains(transcript, "userVolume:") {
		t.Errorf("step 6 announces a user volume with no data disk\n%s", redact(transcript))
	}
}

// The four reasons below are each a DOCUMENTED failure, not commentary. A step
// that announces the operation and swallows the reason turns this tool back
// into the black box it exists not to be.
func TestUpAnnouncesTheReasonForEveryNonObviousDecision(t *testing.T) {
	transcript := newFixture(t).mustRun(t)

	for _, tc := range []struct {
		what   string
		wants  []string
		reason string
	}{
		{
			"diskSelector by serial",
			[]string{"diskSelector: serial talos-system", "coin flip"},
			"a size matcher picks between the OS target and the data disk once both are large",
		},
		{
			"installer pinned to the image",
			[]string{"installer: ghcr.io/siderolabs/installer:" + imageTalosVersion, "cross-version"},
			"left unset, Talos defaults the installer to OUR version and a fresh install becomes a silent upgrade",
		},
		{
			"console arg for this host",
			[]string{"console=ttyFAKE0", "own cmdline"},
			"the installed system writes its own cmdline and goes silent on serial without it",
		},
		{
			"bootstrap fires while booting",
			[]string{"booting", "running", "deadlock"},
			"waiting for 'running' can never open: the node cannot reach running until etcd exists, and bootstrap is what creates etcd",
		},
	} {
		for _, want := range tc.wants {
			if !strings.Contains(transcript, want) {
				t.Errorf("%s: the transcript does not say %q\n  reason: %s\n%s",
					tc.what, want, tc.reason, redact(transcript))
			}
		}
	}
}

// The console arg is the host's, from Detect. A literal in up.go would read
// correctly on amd64 and put an arm64 node's console on a UART it does not have.
func TestUpCarriesTheHostsConsoleArgIntoTheConfig(t *testing.T) {
	f := newFixture(t)
	f.mustRun(t)

	if f.rec.input.ConsoleArg != "console=ttyFAKE0" {
		t.Errorf("GenerateConfig got ConsoleArg %q, want the host's console=ttyFAKE0\n"+
			"  reason: hardcoding ttyS0 gives an arm64 node a serial console it does not have",
			f.rec.input.ConsoleArg)
	}

	if f.rec.input.SystemDiskSerial != "talos-system" || f.rec.input.DataDiskSerial != "talos-data" {
		t.Errorf("GenerateConfig got serials %q/%q, want the caller's talos-system/talos-data",
			f.rec.input.SystemDiskSerial, f.rec.input.DataDiskSerial)
	}

	if f.rec.input.TalosVersion != imageTalosVersion {
		t.Errorf("GenerateConfig got TalosVersion %q, want the IMAGE's "+imageTalosVersion+"\n"+
			"  reason: the installer tag is written to disk; the generator's own version there is a cross-version install",
			f.rec.input.TalosVersion)
	}

	if f.rec.input.Endpoint != "https://127.0.0.1:6443" {
		t.Errorf("GenerateConfig got Endpoint %q, want the Kubernetes API URL", f.rec.input.Endpoint)
	}

	if f.rec.input.ClusterName != "probe" {
		t.Errorf("GenerateConfig got ClusterName %q, want probe\n"+
			"  reason: name and endpoint are adjacent strings; swapped, everything still generates", f.rec.input.ClusterName)
	}
}

// The paths are printed because hunting for them is the friction this replaces,
// and the three hardened defaults are printed because a Talos cluster is
// production-shaped and a `kind` habit fails against it with no explanation.
func TestUpPrintsTheExportLinesAndTheHardenedDefaults(t *testing.T) {
	f := newFixture(t)
	transcript := f.mustRun(t)

	wants(t, transcript,
		"export TALOSCONFIG="+filepath.Join(f.dir, "talosconfig"),
		"export KUBECONFIG="+filepath.Join(f.dir, "kubeconfig"),
		"kubectl get nodes",
		// taint removed, and WHY
		"allowSchedulingOnControlPlanes",
		"topology correction",
		// PodSecurity still enforced, and HOW to opt a namespace out
		"PodSecurity",
		"baseline",
		"pod-security.kubernetes.io/enforce=privileged",
		// storage state, including that PVCs do not survive teardown
		"does not survive",
	)
}

// The storage note has to tell the truth in BOTH shapes, or it is worse than
// no note: an operator told a StorageClass exists writes a PVC and waits.
func TestUpsStorageNoteMatchesReality(t *testing.T) {
	f := newFixture(t)
	f.opts.DataDiskSerial = ""

	transcript := f.mustRun(t)

	if strings.Contains(transcript, "default StorageClass") {
		t.Errorf("the summary claims a default StorageClass with no data disk\n%s", redact(transcript))
	}

	wants(t, transcript, "no StorageClass")
}

// Every artifact here is secret material: the control plane config carries five
// certificate authorities and the machine token, the talosconfig and kubeconfig
// each carry a CA and a client key, and secrets.yaml is the bundle. 0600 is not
// advisory — the state dir sits under $HOME beside serial.log.
func TestUpWritesEveryArtifactAt0600(t *testing.T) {
	// The verdict must not depend on the runner's umask. Under the usual 022 a
	// 0644 write lands as 0644 and this test fails; under 077 the same write
	// lands as 0600 and it passes — one mutant, two answers, decided by an
	// environment variable nobody set on purpose. Zeroed here so the assertion
	// is about the code.
	restore := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(restore) })

	f := newFixture(t)

	// A re-run over an existing state dir must TIGHTEN the mode. os.WriteFile
	// does not chmod a file that already exists, so a kubeconfig left at 0644
	// by anything else stays world-readable without this.
	loose := filepath.Join(f.dir, "kubeconfig")
	if err := os.WriteFile(loose, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.mustRun(t)

	for name, want := range map[string]string{
		"controlplane.yaml": fakeControlPlane,
		"talosconfig":       fakeTalosconfig,
		"secrets.yaml":      fakeSecrets,
		"kubeconfig":        fakeKubeconfig,
	} {
		path := filepath.Join(f.dir, name)

		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s was not written: %v\n"+
				"  reason: artifacts live in the state dir so -destroy sweeps them and secrets do not outlive the cluster", name, err)

			continue
		}

		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s is mode %04o, want 0600", name, mode)
		}

		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		// Compared without printing either side: both are secret-shaped.
		if string(b) != want {
			t.Errorf("%s holds the wrong artifact (%d bytes, want %d)\n"+
				"  reason: writing the talosconfig into controlplane.yaml produces a node that never installs",
				name, len(b), len(want))
		}
	}
}

// Two earlier tasks on this branch shipped leaks that only a guard test caught.
// Up handles four secret artifacts and prints a transcript, which is the
// obvious place for the fifth.
func TestUpNeverPrintsSecrets(t *testing.T) {
	secretsOf := []string{fakeControlPlane, fakeTalosconfig, fakeSecrets, fakeKubeconfig}

	t.Run("on-success", func(t *testing.T) {
		transcript := newFixture(t).mustRun(t)

		for _, secret := range secretsOf {
			if strings.Contains(transcript, secret) {
				t.Errorf("a secret artifact was printed to the transcript (%d chars)\n"+
					"  reason: the transcript goes to a terminal, a CI log and whatever gets pasted into an issue",
					len(secret))
			}
		}
	})

	// Every step after the config exists holds secret material in a local, and
	// a failure is where an error message goes looking for context to add.
	for _, step := range []string{"applyConfig", "waitBootstrapReady", "bootstrap", "kubeconfig", "waitNodeReady", "installStorage"} {
		t.Run("when-"+step+"-fails", func(t *testing.T) {
			f := newFixture(t)
			f.rec.failAt = step

			err := f.run(t)
			if err == nil {
				t.Fatalf("a failing %s did not fail the bring-up", step)
			}

			for _, secret := range secretsOf {
				if strings.Contains(err.Error(), secret) || strings.Contains(f.out.String(), secret) {
					t.Errorf("a secret artifact reached the output of a failing %s (%d chars)", step, len(secret))
				}
			}
		})
	}
}

// A bring-up that dies half way leaves a VM and a state dir, and v1 has no
// resume. Saying so is the difference between a retry that works and a second
// -up against a node that is no longer in maintenance mode.
func TestUpSaysHowToRecoverFromAMidFlightFailure(t *testing.T) {
	f := newFixture(t)
	f.rec.failAt = "bootstrap"

	err := f.run(t)
	if err == nil {
		t.Fatal("a failing bootstrap did not fail the bring-up")
	}

	for _, want := range []string{"-destroy", "not resumable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %s\n"+
				"  reason: re-running -up against a node past maintenance mode waits out the whole timeout for nothing",
				want, redactErr(err))
		}
	}
}

// upOperations is every operation Up performs against a node or a cluster, in
// the order the node requires, paired with the step line each one is announced
// under. It drives the two tests below, so a hook added without an announcement
// — or announced without being checked — cannot slip through either.
var upOperations = []struct {
	op   string
	step string
}{
	{"waitMaintenance", "[ 5/10] maintenance"},
	{"generateConfig", "[ 6/10] config"},
	{"applyConfig", "[ 7/10] apply-config"},
	{"waitBootstrapReady", "[ 7/10] apply-config"},
	{"bootstrap", "[ 8/10] bootstrap"},
	{"kubeconfig", "[ 9/10] kubeconfig"},
	{"waitNodeReady", "[ 9/10] kubeconfig"},
	{"installStorage", "[10/10] storage"},
}

// A failed step must fail the bring-up, must not be announced as done, and
// nothing after it may run.
//
// Every operation is exercised, not one representative: a swallowed error is
// invisible in the happy path, and "the wait for the maintenance API returned
// an error and we applied the config anyway" is a bring-up that then fails four
// steps later against a node that was never listening.
func TestUpStopsAtTheFirstFailedStep(t *testing.T) {
	for i, tc := range upOperations {
		t.Run(tc.op, func(t *testing.T) {
			f := newFixture(t)
			f.rec.failAt = tc.op

			err := f.run(t)
			if err == nil {
				t.Fatalf("a failing %s did not fail the bring-up\n"+
					"  reason: a swallowed error here is announced as a step that succeeded, and "+
					"the transcript becomes something an operator debugs against instead of from", tc.op)
			}

			transcript := f.out.String()

			if strings.Contains(transcript, tc.step) {
				t.Errorf("%q was announced although %s failed\n%s", tc.step, tc.op, redact(transcript))
			}

			for _, later := range upOperations[i+1:] {
				if f.rec.did(later.op) {
					t.Errorf("%s ran after %s failed", later.op, tc.op)
				}
			}
		})
	}
}

// Both endpoints are the HOST side of a qemu forward, and both come from
// spec.hostForwards. A missing one is not discovered until a wait spends its
// whole budget on an address that is not there.
func TestUpRefusesWithoutTheForwardedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear func(*UpOptions)
		want  string
	}{
		{"talos", func(o *UpOptions) { o.TalosEndpoint = "" }, "50000"},
		{"kubernetes", func(o *UpOptions) { o.KubeEndpoint = "" }, "6443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.clear(&f.opts)

			err := f.run(t)
			if err == nil {
				t.Fatalf("Up ran with no %s endpoint", tc.name)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the guest port %s: %s\n"+
					"  reason: the fix is a spec.hostForwards entry, and the message is the only thing that says which",
					tc.want, redactErr(err))
			}

			if f.booted != 0 {
				t.Error("the VM was booted before the endpoints were checked")
			}
		})
	}
}

// Both defaults in Up are invisible to every test above, because every test
// above supplies both — and their absence is not a wrong answer, it is a nil
// dereference in production only. This drives a bring-up with NEITHER supplied
// and stops it at step 3, which is now the last point before anything needs a
// node: the image does not exist, the REAL detectVersion reads it as unknown,
// and the version guard refuses.
//
// Reaching that refusal is what proves both defaults: hooks nil resolved to
// realHooks (or hooks.detectVersion would have panicked) and Out nil resolved
// to os.Stdout (or p.step would have).
func TestUpDefaultsToStdoutAndTheRealOperations(t *testing.T) {
	err := Up(context.Background(), UpOptions{
		ClusterName:      "probe",
		ImagePath:        filepath.Join(t.TempDir(), "absent.iso"),
		StateDir:         t.TempDir(),
		TalosEndpoint:    "127.0.0.1:50000",
		KubeEndpoint:     "https://127.0.0.1:6443",
		SystemDiskSerial: "talos-system",
		Detect:           func() (*platform.Platform, error) { return fakePlatform(), nil },
		// Must never run: step 3 refuses an unidentifiable image before
		// anything is created.
		Boot: func() (int, error) { return 0, errors.New("Boot was reached for an image the guard refused") },
		// Out and hooks are deliberately left nil.
	})
	if err == nil || !strings.Contains(err.Error(), "could not determine the Talos version") {
		t.Fatalf("Up did not refuse the unidentifiable image with its own defaults: %s", redactErr(err))
	}
}

// realHooks is the wiring, and a nil entry in it is a nil call at whichever
// step reaches it — five minutes into a bring-up, on a node that is by then
// halfway installed.
func TestRealHooksAreAllWired(t *testing.T) {
	h := reflect.ValueOf(*realHooks())

	for i := range h.NumField() {
		if h.Field(i).IsNil() {
			t.Errorf("realHooks().%s is nil", h.Type().Field(i).Name)
		}
	}

	if h.NumField() == 0 {
		t.Fatal("upHooks has no fields; this test is asserting nothing")
	}
}

// The artifacts are the point of the state dir, so a write that fails must stop
// the bring-up rather than continue into a cluster nobody can then reach: a
// talosconfig that was never written is a cluster with no way in.
func TestUpReportsAFailureToWriteAnArtifact(t *testing.T) {
	t.Run("state-dir-missing", func(t *testing.T) {
		f := newFixture(t)
		f.opts.StateDir = filepath.Join(f.dir, "not-created")

		err := f.run(t)
		if err == nil {
			t.Fatal("Up continued after failing to write the generated config")
		}

		if f.rec.did("applyConfig") {
			t.Error("a config was applied that was never written to the state dir\n" +
				"  reason: the node then installs from a config the operator has no copy of")
		}

		if strings.Contains(f.out.String(), "[ 6/10] config") {
			t.Errorf("step 6 announced \"wrote controlplane.yaml, talosconfig, secrets.yaml\" and then failed to\n"+
				"  reason: announcing before doing turns the transcript into something to debug rather than debug from\n%s",
				redact(f.out.String()))
		}
	})

	// Step 9's write has its own guard, and the happy path cannot reach it:
	// step 6 writes to the same directory and succeeds first. Blocking exactly
	// one filename is what makes the later guard reachable.
	t.Run("kubeconfig-path-blocked", func(t *testing.T) {
		f := newFixture(t)

		blocked := filepath.Join(f.dir, "kubeconfig")
		if err := os.MkdirAll(filepath.Join(blocked, "occupied"), 0o755); err != nil {
			t.Fatal(err)
		}

		err := f.run(t)
		if err == nil {
			t.Fatal("Up continued after failing to write the kubeconfig")
		}

		if _, statErr := os.Stat(filepath.Join(f.dir, "controlplane.yaml")); statErr != nil {
			t.Fatalf("step 6 did not get far enough for this to be step 9's guard: %v", statErr)
		}

		if f.rec.did("installStorage") {
			t.Error("storage was installed with no kubeconfig on disk")
		}

		if strings.Contains(f.out.String(), "[ 9/10] kubeconfig") {
			t.Errorf("step 9 announced a kubeconfig it failed to write\n%s", redact(f.out.String()))
		}
	})
}

// Boot failing is the one error that is NOT mid-flight residue in the sense the
// note describes — but Detect failing before it is not either, and both have to
// come back as errors rather than a transcript that stops silently.
func TestUpReportsAFailureToDetectTheHost(t *testing.T) {
	f := newFixture(t)
	f.opts.Detect = func() (*platform.Platform, error) { return nil, errors.New("no accelerator on this host") }

	err := f.run(t)
	if err == nil {
		t.Fatal("Up continued with no host platform")
	}

	if !strings.Contains(err.Error(), "no accelerator") {
		t.Errorf("the detect failure was replaced rather than reported: %s", redactErr(err))
	}
}

func TestUpReportsAFailureToBoot(t *testing.T) {
	f := newFixture(t)
	f.opts.Boot = func() (int, error) { return 0, errors.New("qemu: exit status 1") }

	err := f.run(t)
	if err == nil {
		t.Fatal("Up continued with no VM")
	}

	if !strings.Contains(err.Error(), "qemu") {
		t.Errorf("the boot failure was replaced rather than reported: %s", redactErr(err))
	}

	if f.rec.did("waitMaintenance") {
		t.Error("Up waited for the maintenance API of a VM that never started")
	}
}

// Ordering inside a step is invisible in the transcript and load-bearing
// everywhere else: applying the config before the maintenance API answers gets
// a connection refused, and bootstrapping before the installed system is back
// gets a certificate from a node still in maintenance mode.
func TestUpRunsTheOperationsInTheOrderTheNodeRequires(t *testing.T) {
	f := newFixture(t)
	f.mustRun(t)

	want := []string{
		"waitMaintenance",
		"generateConfig",
		"applyConfig",
		"waitBootstrapReady",
		"bootstrap",
		"kubeconfig",
		"waitNodeReady",
		"installStorage",
	}

	if len(f.rec.called) != len(want) {
		t.Fatalf("operations = %v, want %v", f.rec.called, want)
	}

	for i := range want {
		if f.rec.called[i] != want[i] {
			t.Errorf("operation %d = %q, want %q\n"+
				"  reason: %s", i, f.rec.called[i], want[i], orderReason(want[i]))
		}
	}
}

// Every hook takes a []byte of secret material, and there are four different
// ones in flight. A swap COMPILES, and each swap is a failure the node reports
// rather than this package: the talosconfig applied as a machine config is
// rejected by a node already installing, and the machine config handed to the
// storage installer is a kubeconfig parse error nine steps in.
func TestUpHandsEachOperationTheRightArtifact(t *testing.T) {
	f := newFixture(t)
	f.mustRun(t)

	for _, tc := range []struct {
		op, want, describe string
	}{
		{"applyConfig", fakeControlPlane, "the machine config is what a node in maintenance mode installs from"},
		{"waitBootstrapReady", fakeTalosconfig, "the wait authenticates with the cluster PKI, which only the talosconfig carries"},
		{"bootstrap", fakeTalosconfig, "bootstrap is a Talos API call, not a Kubernetes one"},
		{"kubeconfig", fakeTalosconfig, "the kubeconfig is FETCHED over the Talos API using the talosconfig"},
		{"waitNodeReady", fakeKubeconfig, "node readiness is the Kubernetes API's answer, not Talos's"},
		{"installStorage", fakeKubeconfig, "the manifest goes to the Kubernetes API"},
	} {
		got, ok := f.rec.payload[tc.op]
		if !ok {
			t.Errorf("%s was never called", tc.op)

			continue
		}

		// Lengths and identity only: printing either side is the leak the
		// guard test below exists to prevent.
		if string(got) != tc.want {
			t.Errorf("%s was handed the wrong artifact (%d bytes, want the %d-byte one)\n  reason: %s",
				tc.op, len(got), len(tc.want), tc.describe)
		}
	}
}

func orderReason(op string) string {
	switch op {
	case "waitMaintenance":
		return "the config cannot be applied before the maintenance API answers"
	case "waitBootstrapReady":
		return "bootstrap needs the INSTALLED system's apid, which maintenance mode cannot satisfy"
	case "installStorage":
		return "the provisioner cannot be applied before the node is Ready"
	}

	return "the node requires this order"
}

// A refusal from the version guard must reach the operator with the guard's own
// message — that message is where the remedy is.
func TestUpKeepsTheVersionGuardsExplanation(t *testing.T) {
	f := newFixture(t)
	f.rec.imageVersion = "v1.99.0"

	err := f.run(t)
	if err == nil {
		t.Fatal("no refusal")
	}

	wants(t, err.Error(), "v1.99.0", GeneratorVersion())
}

// ── the kexec workaround is macOS/arm64-ONLY ────────────────────────────────
//
// Two host facts gate it, and each one is asserted on a platform the test binary
// is not running on. That is the whole reason OS and ImageArch are resolved onto
// Platform instead of read from runtime: a workaround for someone else's host
// has to be provable from this one.

// hostPlatform returns the fixture platform with OS and guest arch overridden,
// so each case below states the host it is about instead of inheriting it.
func hostPlatform(os, arch string) func() (*platform.Platform, error) {
	return func() (*platform.Platform, error) {
		p := fakePlatform()
		p.OS = os
		p.ImageArch = arch

		return p, nil
	}
}

// The fixture's platform says linux, so this asserts the Linux behaviour while
// running on a Mac — which is the only way this assertion means anything.
func TestUpLeavesKexecAloneOnLinux(t *testing.T) {
	f := newFixture(t)
	f.opts.Detect = hostPlatform("linux", "arm64")

	transcript := f.mustRun(t)

	if f.rec.input.DisableKexec {
		t.Error("kexec was disabled on a linux host\n" +
			"  reason: kexec works under KVM and skips a firmware boot; disabling it there is a\n" +
			"  tax paid for a macOS bug")
	}

	if strings.Contains(transcript, "kexec_load_disabled") {
		t.Errorf("transcript announces the workaround on linux\n%s", redact(transcript))
	}
}

// An INTEL Mac has nothing to work around: the guest bug is arm64's, and
// upstream gates its own workaround on the architecture too. Disabling kexec
// here would cost a firmware boot for a bug that is not present.
func TestUpLeavesKexecAloneOnAnIntelMac(t *testing.T) {
	f := newFixture(t)
	f.opts.Detect = hostPlatform("darwin", "amd64")

	transcript := f.mustRun(t)

	if f.rec.input.DisableKexec {
		t.Error("kexec was disabled on darwin/amd64\n" +
			"  reason: the kexec bug is arm64's — upstream gates on TargetArch == arm64. An\n" +
			"  Intel Mac pays a firmware boot for nothing")
	}

	if strings.Contains(transcript, "kexec_load_disabled") {
		t.Errorf("transcript announces the workaround on darwin/amd64\n%s", redact(transcript))
	}
}

// On macOS/arm64 the sysctl must be requested, and the transcript must say so: a
// bring-up that silently changed the node's reboot behaviour is one nobody can
// account for later.
func TestUpDisablesKexecOnAppleSilicon(t *testing.T) {
	f := newFixture(t)
	f.opts.Detect = hostPlatform("darwin", "arm64")

	transcript := f.mustRun(t)

	if !f.rec.input.DisableKexec {
		t.Error("kexec was left enabled on darwin/arm64\n" +
			"  reason: Talos kexecs into the installed kernel, and under QEMU on macOS that\n" +
			"  path dies in the guest — the node never boots what it just installed")
	}

	wants(t, transcript, "kexec_load_disabled=1", "darwin/arm64 host", "MAINTENANCE")
}
