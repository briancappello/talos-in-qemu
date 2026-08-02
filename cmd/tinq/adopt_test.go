package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coglative/talos-in-qemu/driverkit"
	"github.com/coglative/talos-in-qemu/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ── adopt: the baremetal substrate ──────────────────────────────────────────

func baremetalMachine() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "machine.hvf.fleet.io/v1alpha1",
		"kind":       "TalosMachine",
		"metadata":   map[string]interface{}{"name": "bm0", "namespace": "default"},
		"spec": map[string]interface{}{
			"site": "lab", "role": "talos-cp",
			"baremetal": map[string]interface{}{
				"endpoint":         "192.168.1.50",
				"systemDiskSerial": "S1",
			},
		},
	}}
}

func TestIsBaremetalKeysOnTheSpecBlock(t *testing.T) {
	if !isBaremetal(baremetalMachine()) {
		t.Error("a machine with spec.baremetal was not recognised as baremetal")
	}

	qemu := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"image": "talos.iso"},
	}}
	if isBaremetal(qemu) {
		t.Error("a machine with no spec.baremetal was treated as baremetal\n" +
			"  reason: presence of the block IS the discriminator")
	}
}

func TestBaremetalEndpointsUseTalosDefaultPorts(t *testing.T) {
	m := baremetalMachine()

	if got := baremetalTalosEndpoint(m); got != "192.168.1.50:50000" {
		t.Errorf("talos endpoint = %q, want 192.168.1.50:50000\n"+
			"  reason: there is no forward on hardware; apid serves its own default port", got)
	}

	if got := baremetalKubeEndpoint(m); got != "https://192.168.1.50:6443" {
		t.Errorf("kube endpoint = %q, want https://192.168.1.50:6443", got)
	}
}

func TestVMVerbsRefuseABaremetalMachine(t *testing.T) {
	for _, verb := range []string{"apply", "up", "stop", "destroy"} {
		err := refuseWrongSubstrate(baremetalMachine(), verb)
		if err == nil {
			t.Errorf("%s accepted a baremetal machine\n"+
				"  reason: destroy in particular would delete the only talosconfig "+
				"that can reach a node it cannot destroy", verb)
			continue
		}
		if !strings.Contains(err.Error(), "adopt") {
			t.Errorf("%s's refusal does not name the verb that does work: %s", verb, err)
		}
	}
}

func TestAdoptRefusesAQEMUMachine(t *testing.T) {
	qemu := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"image": "talos.iso"},
	}}
	if err := refuseWrongSubstrate(qemu, "adopt"); err == nil {
		t.Error("adopt accepted a machine with no spec.baremetal")
	}
}

// The refusal has to be WIRED IN, not merely written. Every assertion above
// calls refuseWrongSubstrate directly, and all four would stay green with the
// call deleted from standalone — which is the only place a user reaches it.
//
// It also pins the ORDER, and the fixture is what makes that assertable: the
// state root is a regular FILE, so Observe's stat of <root>/<site>/<uid>/
// system.qcow2 fails with ENOTDIR rather than ENOENT and standalone returns
// "observe: ...". Seeing the refusal instead proves nothing observed first —
// which matters because Observe stats a qcow2 and would call a machine that is
// a machine Absent.
func TestStandaloneRefusesABaremetalMachineBeforeObserving(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "machine.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata: {name: bm0, namespace: default}
spec:
  site: lab
  baremetal:
    endpoint: 192.168.1.50
    systemDiskSerial: S1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &hvf{stateRoot: root, imageRoot: t.TempDir(),
		detect: func() (*platform.Platform, error) {
			t.Error("a refusal must not probe the host: the machine is not this host's guest")
			return nil, fmt.Errorf("no accelerator")
		}}

	for _, verb := range []string{"apply", "up", "stop", "destroy"} {
		t.Run(verb, func(t *testing.T) {
			err := standalone(context.Background(), d, path, verb)
			if err == nil {
				t.Fatalf("standalone %s ran against a baremetal machine", verb)
			}
			// LOAD-BEARING ON hvf.Observe RETURNING AN ERROR FOR ENOTDIR.
			// This assertion can only tell "refused first" from "observed
			// first" because a stat failure that is not ENOENT propagates —
			// see Observe's `return driverkit.Absent, nil, err`. Soften that
			// to Absent for every stat error and standalone stops erroring in
			// Observe at all, so this branch goes vacuous while staying green
			// and the ordering it exists to pin is silently unasserted.
			// Change Observe there and this test needs a new lever.
			if strings.HasPrefix(err.Error(), "observe:") {
				t.Fatalf("standalone %s observed before refusing: %v\n"+
					"  reason: Observe stats system.qcow2 and reports hardware as "+
					"Absent, which is a meaningless answer", verb, err)
			}
			if !strings.Contains(err.Error(), "adopt") {
				t.Errorf("standalone %s's refusal does not name the verb that works: %v", verb, err)
			}
		})
	}
}

// adopt is reachable, spelled the way the docs spell it, and refuses a VM
// through the same door a user opens. A verb missing from AddCommand compiles.
func TestAdoptIsRegisteredAndRefusesAVM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.yaml")
	if err := os.WriteFile(path, []byte(machineDoc), 0o644); err != nil {
		t.Fatal(err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"adopt", "--state-root", t.TempDir(), path})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	err := root.Execute()
	if err == nil {
		t.Fatal("adopt ran against a machine with no spec.baremetal")
	}
	if strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("adopt is not registered: %v", err)
	}
	// The REFUSAL, specifically, and not the "endpoint is required" that a
	// machine which got past it fails on next — that one also says
	// "spec.baremetal", so a looser assertion here is green against an adopt
	// that accepted a VM and then tripped over the missing block anyway.
	if !strings.Contains(err.Error(), "`tinq up` is the verb that builds it") {
		t.Errorf("adopt's refusal does not send a VM to the verb that builds it: %v", err)
	}
}

// A PORT IN THE ENDPOINT IS A HANG, and that is why it is refused rather than
// left to fail somewhere. The two ports are Talos's own and adopt appends them,
// so "10.0.0.5:50000" becomes "10.0.0.5:50000:50000" — measured: ten minutes of
// the maintenance budget spent on an address nothing can dial, with only
// "waiting for the Talos maintenance API" printed to explain it.
//
// The refusal must land BEFORE the wait, which is what the deadline asserts:
// this test may not take ten minutes to discover a typo.
func TestAdoptRefusesAnEndpointCarryingAPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata: {name: bm0, namespace: default}
spec:
  site: lab
  baremetal:
    endpoint: 10.0.0.5:50000
    systemDiskSerial: S1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	d := &hvf{stateRoot: root, imageRoot: t.TempDir()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := adoptMachine(ctx, d, path)
	if err == nil {
		t.Fatal("adopt accepted an endpoint with a port in it")
	}
	if ctx.Err() != nil {
		t.Fatalf("adopt dialled before checking the address it was given: %v", err)
	}
	if !strings.Contains(err.Error(), "10.0.0.5:50000") {
		t.Errorf("the refusal does not quote the endpoint it rejected: %v", err)
	}
	// The check sits before MkdirAll, and this is the property that buys.
	// Without it the deadline assertion above is equally green against a
	// refusal that has already carved out a state dir for a machine it just
	// rejected — residue named after a typo, left for the operator to find.
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatalf("state root: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("a refused adopt created %d entries under the state root, want 0", len(entries))
	}
}

// THE CONTROLLER REACHES Destroy WITH NO SUBSTRATE CHECK IN FRONT OF IT.
// refuseWrongSubstrate guards the CLI verbs only, and driverkit's reconcile
// handles a deletion timestamp before it Observes — so `kubectl delete
// talosmachine bm0`, on the machine the docs tell you to register after adopt,
// lands here directly. Sweeping the state dir there deletes the sole
// talosconfig for a node that left maintenance mode when it was adopted and
// can never be adopted again: the machine survives, the key to it does not.
//
// Both halves matter. The QEMU subtest is the regression guard — this is the
// method the controller calls on every delete tick, and a guard that also
// stops sweeping VMs has traded one leak for another.
func TestDestroyForgetsHardwareAndStillSweepsAVM(t *testing.T) {
	// seed builds a state dir with one file in it and returns both paths.
	seed := func(t *testing.T, h *hvf, m *unstructured.Unstructured, name string) (string, string) {
		t.Helper()
		dir := h.dir(m)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte("not a secret, a fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir, f
	}

	t.Run("baremetal", func(t *testing.T) {
		h := &hvf{stateRoot: t.TempDir(), imageRoot: t.TempDir(),
			detect: func() (*platform.Platform, error) {
				t.Error("forgetting a machine must not probe the host")
				return nil, fmt.Errorf("no accelerator")
			}}
		m := baremetalMachine()
		m.SetUID("bm0-uid")
		dir, cfg := seed(t, h, m, "talosconfig")

		if err := h.Destroy(context.Background(), m); err != nil {
			t.Fatalf("Destroy of a baremetal machine = %v, want nil\n"+
				"  reason: an error here BLOCKS deletion and wedges the finalizer forever", err)
		}
		if _, err := os.Stat(cfg); err != nil {
			t.Fatalf("Destroy deleted %s: %v\n"+
				"  reason: that is the ONLY credential that reaches a node this tool "+
				"cannot destroy and cannot re-adopt", cfg, err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("Destroy removed the state dir of a machine it does not own: %v", err)
		}
	})

	t.Run("qemu", func(t *testing.T) {
		h := &hvf{stateRoot: t.TempDir(), imageRoot: t.TempDir()}
		m := &unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "vm0"},
			"spec":     map[string]interface{}{"site": "lab", "image": "talos.iso"},
		}}
		m.SetUID("vm0-uid")
		dir, _ := seed(t, h, m, "system.qcow2")

		if err := h.Destroy(context.Background(), m); err != nil {
			t.Fatalf("Destroy of a VM = %v, want nil", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("Destroy left the state dir of a VM behind (stat: %v)\n"+
				"  reason: the baremetal guard must not cost the QEMU path its sweep", err)
		}
	})
}

// Observe must not call hardware Absent or Stopped: both are answers about
// system.qcow2, both read as "not up yet", and plan() turns either into a
// Create against a machine on a desk on the very next tick.
func TestObserveDoesNotCallHardwareAbsent(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir(), imageRoot: t.TempDir()}
	m := baremetalMachine()
	m.SetUID("bm0-uid")

	state, status, err := h.Observe(context.Background(), m)
	if err != nil {
		t.Fatalf("Observe of a baremetal machine = %v, want nil", err)
	}
	if state != driverkit.Running {
		t.Fatalf("Observe reported %v for hardware, want Running\n"+
			"  reason: Absent and Stopped both make plan() ask Create to build a "+
			"machine that already exists", state)
	}
	if got := status["apiEndpoint"]; got != "192.168.1.50:50000" {
		t.Errorf("status apiEndpoint = %v, want the node's own address\n"+
			"  reason: Running here is not a liveness claim, so status has to carry "+
			"the address that can actually answer one", got)
	}
	if _, ok := status["pid"]; ok {
		t.Error("status carries a pid for a process this host never started")
	}
}

// Create and Stop must return nil, not an error. The controller retries a
// failed verb on every tick and this one could never clear — a permanent error
// spin is noise that teaches an operator to stop reading the log.
func TestCreateAndStopDoNotSpinOnHardware(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir(), imageRoot: t.TempDir(),
		detect: func() (*platform.Platform, error) {
			t.Error("a refusal must not probe the host")
			return nil, fmt.Errorf("no accelerator")
		}}
	m := baremetalMachine()
	m.SetUID("bm0-uid")

	if err := h.Create(context.Background(), m); err != nil {
		t.Errorf("Create on hardware = %v, want nil", err)
	}
	if err := h.Stop(context.Background(), m); err != nil {
		t.Errorf("Stop on hardware = %v, want nil", err)
	}
	if entries, err := os.ReadDir(h.stateRoot); err != nil || len(entries) != 0 {
		t.Errorf("state root holds %v (err %v) after a refusal, want nothing", entries, err)
	}
}
