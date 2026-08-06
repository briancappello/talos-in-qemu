package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// renderedFor builds a real generated config, so the refusals below are matched
// against the encoder's actual output rather than a hand-written approximation
// of it. A hand-written fixture would keep passing after an upstream change
// moved the very fields those regexps look for.
func renderedFor(t *testing.T, mutate func(*ConfigInput)) []byte {
	t.Helper()

	in := testInput()
	if mutate != nil {
		mutate(&in)
	}

	return mustGenerate(t, in).ControlPlane
}

// THE POINT OF THE VERB'S REFUSALS. Talos accepts a config naming a different
// install disk or EPHEMERAL cap and does NOTHING about it — the partitions are
// already written. Applying it would leave the state dir describing a layout the
// disk does not have, and the first sign is a PVC that never binds.
func TestReconfigureRefusesInstallTimeChanges(t *testing.T) {
	current := renderedFor(t, nil)

	for _, tc := range []struct {
		name   string
		mutate func(*ConfigInput)
		want   string
	}{
		{
			"the install disk, by serial",
			func(in *ConfigInput) { in.SystemDisk = DiskRef{Serial: "some-other-disk"} },
			"the install disk",
		},
		{
			// The realistic hardware case: a manifest switched from naming the
			// target by serial to naming it by WWID. Same disk, possibly — but
			// nothing here can prove that, and guessing wrong reinstalls.
			"the install disk, swapped to a wwid",
			func(in *ConfigInput) { in.SystemDisk = DiskRef{WWID: "naa.5000c5001b82df21"} },
			"the install disk",
		},
		{
			"the EPHEMERAL cap being introduced",
			func(in *ConfigInput) { in.DataDiskSerial = ""; in.EphemeralMaxSize = "120GB" },
			"the EPHEMERAL cap",
		},
		{
			"the data disk",
			func(in *ConfigInput) { in.DataDiskSerial = "another-data-disk" },
			"the user volume's disk",
		},
		{
			"the user volume being removed",
			func(in *ConfigInput) { in.DataDiskSerial = "" },
			"the user volume's disk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := reconfigureRefusals(current, renderedFor(t, tc.mutate))
			if err == nil {
				t.Fatalf("reconfigure accepted a change to %s\n"+
					"  reason: it is decided when the disk is partitioned, and Talos silently "+
					"ignores a config that says otherwise", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name what changed (%q): %v", tc.want, err)
			}

			// The remedy is the whole message: there is no way to apply this
			// to a running machine, so the refusal has to say what is.
			if !strings.Contains(err.Error(), "wipe the machine") {
				t.Errorf("the refusal does not give the remedy: %v", err)
			}
		})
	}
}

// The changes this verb EXISTS for must pass. A refusal list that also refuses
// the reason anyone runs the verb is a refusal list nobody can use.
func TestReconfigureAllowsChangesARunningNodeCanTake(t *testing.T) {
	current := renderedFor(t, nil)

	for _, tc := range []struct {
		name   string
		mutate func(*ConfigInput)
	}{
		{
			// The case that motivated the whole thing: an adopted node needed
			// a registry mirror and the only way to get one was a disk wipe.
			"adding a registry mirror",
			func(in *ConfigInput) {
				in.Registries = []RegistryMirror{{Host: "reg.lan:5000", Endpoint: "http://reg.lan:5000"}}
			},
		},
		{
			"changing an existing mirror's endpoint",
			func(in *ConfigInput) {
				in.Registries = []RegistryMirror{{Host: "reg.lan:5000", Endpoint: "https://reg.lan:5000", InsecureSkipVerify: true}}
			},
		},
		{
			"nothing at all",
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := reconfigureRefusals(current, renderedFor(t, tc.mutate)); err != nil {
				t.Errorf("reconfigure refused a change a running node can take: %v", err)
			}
		})
	}
}

// A machine that was never brought up has no node to reconfigure, and
// generating its FIRST config here would leave a state dir claiming a machine
// exists that was never installed or bootstrapped.
func TestReconfigureRefusesAMachineThatWasNeverBroughtUp(t *testing.T) {
	err := reconfigureRunAgainst(t, t.TempDir())
	if err == nil {
		t.Fatal("reconfigure ran against a machine with no talosconfig")
	}

	if !strings.Contains(err.Error(), "never been brought up") {
		t.Errorf("the refusal does not say the machine was never brought up: %v", err)
	}

	for _, want := range []string{"tinq up", "tinq adopt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not point at %q: %v", want, err)
		}
	}
}

// The secrets bundle is what keeps a regenerated config trusted by a node that
// is already running. Without it there is nothing to do but mint a new PKI,
// which is the one outcome this path exists to prevent — so a state dir missing
// it is a refusal, never a fresh generation.
func TestReconfigureRefusesWithoutTheSecretsBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "talosconfig"), []byte("context: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := reconfigureRunAgainst(t, dir)
	if err == nil {
		t.Fatal("reconfigure ran against a state dir with no secrets bundle")
	}

	if !strings.Contains(err.Error(), "secrets bundle") {
		t.Errorf("the refusal does not name the missing secrets bundle: %v", err)
	}
}

// reconfigureRunAgainst runs Reconfigure against a state dir, with an endpoint
// nothing listens on. Every test using it asserts a refusal that must land
// BEFORE the node is dialled, so a bound of a few seconds against a wait
// measured in minutes is itself the assertion.
func reconfigureRunAgainst(t *testing.T, dir string) error {
	t.Helper()

	return runBounded(t, 10*time.Second, func() error {
		_, err := Reconfigure(context.Background(), ReconfigureOptions{
			ClusterName: "probe",
			StateDir:    dir,
			// Nothing is listening here and nothing needs to be.
			TalosEndpoint: "127.0.0.1:1",
			KubeEndpoint:  "https://127.0.0.1:6443",
			APIAddress:    "127.0.0.1",
			SystemDisk:    DiskRef{Serial: "talos-system"},
		})

		return err
	})
}
