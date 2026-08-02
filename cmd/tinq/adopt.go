package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/coglative/talos-in-qemu/cluster"
	"github.com/coglative/talos-in-qemu/driverkit"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// adoptMaintenanceTimeout covers a node that may still be booting when adopt is
// run. It is generous because the operator has just walked over and pressed a
// power button, and firmware on real hardware is slower than QEMU's.
const adoptMaintenanceTimeout = 10 * time.Minute

// specBaremetal returns spec.baremetal, or nil when the machine is a VM.
//
// Its PRESENCE is the discriminator, not a mode field. A machine either
// describes hardware that already exists or a guest this tool creates, and
// there is no third thing — so an explicit `provider:` string would be a second
// source of truth that could contradict the block beside it.
func specBaremetal(m *unstructured.Unstructured) map[string]interface{} {
	v, _, _ := unstructured.NestedMap(m.Object, "spec", "baremetal")
	return v
}

func isBaremetal(m *unstructured.Unstructured) bool { return specBaremetal(m) != nil }

// The two endpoints of an adopted node. NO FORWARD IS INVOLVED: apid and
// kube-apiserver serve their own default ports on the node itself, so these are
// the same constants the guest side uses, applied to a real address.
func baremetalTalosEndpoint(m *unstructured.Unstructured) string {
	if a := str(specBaremetal(m)["endpoint"], ""); a != "" {
		return fmt.Sprintf("%s:%d", a, talosAPIGuestPort)
	}
	return ""
}

func baremetalKubeEndpoint(m *unstructured.Unstructured) string {
	if a := str(specBaremetal(m)["endpoint"], ""); a != "" {
		return fmt.Sprintf("https://%s:%d", a, kubeAPIGuestPort)
	}
	return ""
}

// refuseWrongSubstrate rejects a verb applied to the substrate it cannot serve.
//
// The four VM verbs are not merely inapplicable to hardware, they are unsafe on
// it. `destroy` is the sharp one: its contract is to take the entire SCC, and
// on a machine it did not create it can take only the artifacts — including the
// sole talosconfig that reaches a node it has no way to destroy. A verb that
// half-honours its contract while deleting the only credential to the surviving
// machine is worse than one that refuses.
func refuseWrongSubstrate(m *unstructured.Unstructured, verb string) error {
	bm := isBaremetal(m)

	if verb == "adopt" {
		if bm {
			return nil
		}
		return fmt.Errorf("`tinq adopt` needs spec.baremetal (the node's address and its "+
			"disk serials); %s describes a VM, so `tinq up` is the verb that builds it",
			m.GetName())
	}

	if !bm {
		return nil
	}

	return fmt.Errorf("`tinq %s` cannot act on %s: it has spec.baremetal, so it is a machine "+
		"this tool did not create and cannot power-cycle\n\n  `tinq adopt` is the verb that "+
		"brings it up\n\n(there is no `forget` verb yet, so clearing its state directory is "+
		"`rm -rf` for now)", verb, m.GetName())
}

// The three guards below are refuseWrongSubstrate for the CONTROLLER's path.
//
// refuseWrongSubstrate covers the four CLI verbs, and the controller reaches
// the very same driver methods without passing any of them: driverkit's
// reconcile handles a deletion timestamp BEFORE it Observes, so `kubectl delete
// talosmachine bm0` lands in Destroy directly. The refusals therefore have to
// exist twice, in two shapes — a CLI verb refuses by erroring at the operator,
// a driver method cannot, because an error there is retried on every tick
// forever and, on the delete path, wedges a finalizer.

// forgetBaremetal is Destroy for hardware: it removes NOTHING and succeeds.
//
// Destroy's contract is to take the entire SCC, and returning nil without
// taking it is normally the exact leak this design exists to prevent. Here the
// contract is satisfied differently, because THE SCC DOES NOT CONTAIN THE
// MACHINE: this driver did not create the node, cannot power it off and cannot
// wipe its disks. What sits in the state dir is not the resource, it is the
// CREDENTIAL to it — the talosconfig and kubeconfig for a node that left
// maintenance mode the moment it was adopted and can therefore never be
// adopted again.
//
// So the choice is not "sweep or leak", it is which direction to be wrong in:
// strand a credential for a machine that outlives its registration, or delete
// the only key to a machine still running on a desk hosting the cluster. The
// first is one `rm -rf` away; the second is a reinstall. Deleting a
// REGISTRATION must not delete the machine's credential.
//
// nil rather than an error is required as well as correct: an error BLOCKS
// deletion, so `kubectl delete talosmachine` would hang on the finalizer
// forever for a resource this driver has no teardown work to do on at all.
func forgetBaremetal(m *unstructured.Unstructured, dir string) error {
	log.Printf("%s has spec.baremetal: FORGETTING it, not destroying it. The node stays up, "+
		"its disks stay installed, and nothing under %s was removed — the talosconfig there "+
		"is the only credential that reaches it, and this driver cannot make another. "+
		"Delete that directory yourself once the node is genuinely gone.", m.GetName(), dir)
	return nil
}

// ignoreBaremetalOp is Create and Stop for hardware: change nothing, say so,
// return nil.
//
// nil, not an error, and the reason is the reconcile loop rather than
// politeness. An error is retried every tick and this one could never clear,
// because the driver has no way to alter what it observes — a tick that always
// fails is noise that teaches an operator to stop reading the log. Create in
// particular would fail in resolveImage, which is a baffling thing to print
// about a machine that has no image because it is not a VM.
//
// nil converges instead: observeBaremetal reports Running, so plan() never
// asks for Create again. Stop stays reachable through spec.powerState:
// Stopped, a request this driver cannot serve on hardware; it then logs this
// line each tick and changes nothing, which is the honest outcome.
func ignoreBaremetalOp(m *unstructured.Unstructured, op string) error {
	log.Printf("%s has spec.baremetal: refusing to %s it — this driver did not create the node "+
		"and cannot power-cycle it; `tinq adopt` is the verb that brings it up",
		m.GetName(), op)
	return nil
}

// observeBaremetal is Observe for hardware: Running, always.
//
// Running is the least wrong of the three. Absent and Stopped are both answers
// about a FILE — system.qcow2, which a machine on a desk does not have — and
// both read downstream as "not up yet", which is precisely the reading that
// has plan() call Create on hardware. Running is the only state that converges
// the controller to doing NOTHING, and that is the whole of what this driver
// truthfully knows here: it did not create this node, it cannot power-cycle
// it, there is no work.
//
// It is NOT a liveness claim and must not be read as one. A truthful liveness
// answer costs a dial of the node, and Observe is host-side, read-only and run
// every tick by contract. status carries the endpoint so the operator can ask
// the node itself, which is the only thing that can answer.
//
// No pid: this process did not start that node and holds no handle on it — the
// same honesty adopt's Boot func uses when it returns 0.
func observeBaremetal(m *unstructured.Unstructured, dir string) (driverkit.State, map[string]interface{}, error) {
	return driverkit.Running, map[string]interface{}{
		"stateDir": dir, "apiEndpoint": baremetalTalosEndpoint(m),
	}, nil
}

// adoptMachine is the `adopt` verb: bring up a node this tool did not create.
//
// It does NOT go through driverkit. Observe/Create/Stop/Destroy all describe a
// resource this program owns the lifecycle of, and none of the four has an
// honest implementation for a machine on a desk with no power control.
//
// Everything before cluster.Up is a PRE-FLIGHT that a QEMU bring-up does not
// need: the version and the disks both come from the node, so both require a
// maintenance-mode node to already be answering.
func adoptMachine(ctx context.Context, d *hvf, path string) error {
	m, err := readMachine(path)
	if err != nil {
		return err
	}

	if err := refuseWrongSubstrate(m, "adopt"); err != nil {
		return err
	}

	spec := specBaremetal(m)

	endpoint := baremetalTalosEndpoint(m)
	if endpoint == "" {
		return errors.New("spec.baremetal.endpoint is required: it is the address this host " +
			"dials to reach the node, and it goes into apid's certificate")
	}

	// A BARE ADDRESS, and a port in it is a TEN-MINUTE HANG rather than a parse
	// error. The two ports are Talos's own and the helpers above append them,
	// so "10.0.0.5:50000" becomes "10.0.0.5:50000:50000" — measured: the whole
	// maintenance budget spent resolving an address that cannot exist, with one
	// "waiting for the Talos maintenance API" line to explain it. Nothing
	// downstream can tell that apart from a node that has not booted yet.
	//
	// An IPv6 literal is caught here too, and truthfully rather than by
	// accident: baremetalTalosEndpoint cannot bracket one, so a v6 address is
	// unsupported and this is where it should be said.
	if addr := str(spec["endpoint"], ""); strings.Contains(addr, ":") {
		return fmt.Errorf("spec.baremetal.endpoint %q must be a bare address with no port: "+
			"apid's %d and kube-apiserver's %d are Talos's own and are added for you\n\n"+
			"  (an IPv6 literal lands here too, and is not supported yet)",
			addr, talosAPIGuestPort, kubeAPIGuestPort)
	}

	dir := d.dir(m)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}

	log.Printf("waiting for the Talos maintenance API at %s", endpoint)

	if err := cluster.WaitMaintenance(ctx, endpoint, adoptMaintenanceTimeout); err != nil {
		return err
	}

	disks, err := cluster.ListDisks(ctx, endpoint)
	if err != nil {
		return err
	}

	systemSerial := str(spec["systemDiskSerial"], "")
	if err := cluster.RequireDisk(disks, systemSerial, "install target"); err != nil {
		return err
	}

	// Checked ONLY when asked for. An absent data disk is a legitimate choice
	// and step 10 announces what it costs; an absent one that was MEANT to be
	// present is a typo, which the same check catches.
	dataSerial := str(spec["dataDiskSerial"], "")
	if dataSerial != "" {
		if err := cluster.RequireDisk(disks, dataSerial, "data disk"); err != nil {
			return err
		}
	}

	// The node's own answer, with the spec as an override for the case Risk 1
	// of the design spec describes: a maintenance-mode node that reports no tag.
	version := str(spec["talosVersion"], "")
	source := "spec.baremetal.talosVersion"

	if version == "" {
		if version, err = cluster.NodeVersion(ctx, endpoint); err != nil {
			return err
		}
		source = "the node's maintenance API"
	}

	return cluster.Up(ctx, cluster.UpOptions{
		ClusterName:      m.GetName(),
		StateDir:         dir,
		TalosEndpoint:    endpoint,
		KubeEndpoint:     baremetalKubeEndpoint(m),
		SystemDiskSerial: systemSerial,
		DataDiskSerial:   dataSerial,
		TalosVersion:     version,
		VersionSource:    source,
		Substrate:        fmt.Sprintf("baremetal, %s", str(spec["endpoint"], "")),
		// EMPTY BY DEFAULT. Real hardware has a firmware-configured console and
		// usually a display; a console argument derived from THIS host's
		// architecture is a guess, and a wrong one is silent at exactly the
		// boot you would need it for.
		ConsoleArg: str(spec["consoleArg"], ""),
		// The kexec workaround is QEMU-on-macOS-specific. Hardware reboots
		// through its own firmware and has nothing to work around.
		DisableKexec: false,
		// ALREADY RUNNING, by definition — that is what adopt means. Returning
		// a pid of 0 is honest: this process did not start it and has no
		// handle on it.
		Boot: func() (int, error) { return 0, nil },
	})
}
