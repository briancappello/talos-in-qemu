package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	blockres "github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// NODE FACTS, NOT PROBES. Everything in this file asks a maintenance-mode node
// a QUESTION and returns the answer. Nothing here decides whether a node is
// ready, and nothing here may be used to.
//
// That distinction is why this file exists at all rather than living in
// client.go, whose header rule (2) forbids any probe from comparing, returning
// or logging a version — because `talosctl version` prints a constant compiled
// into the binary and will do so with no node in sight. These functions run
// AFTER readiness has been established by a real round trip, and their answers
// become values written to the node's disk.

// NodeVersion asks a maintenance-mode node for its own Talos version.
//
// It NEVER errors on an unidentifiable version, only on a failed call: "" is a
// real answer and matches platform.InspectImageVersion's contract, so both
// sources of a version fail the same way and step 3's guard is the single place
// that refuses one. Returning an error here instead would put the refusal in
// two places and let them drift.
func NodeVersion(ctx context.Context, endpoint string) (string, error) {
	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		return "", err
	}

	defer c.Close() //nolint:errcheck

	resp, err := c.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("asking the node its Talos version: %w", err)
	}

	return versionTag(resp), nil
}

// versionTag picks a version tag out of a Version reply, or "" when the reply
// carries none.
//
// It is split out of NodeVersion because this loop IS the "never errors on an
// unidentifiable version" contract above, and a pure function is the only way
// to pin it: reaching these branches through NodeVersion would take a fake
// apid, so left inline they are asserted by nothing but a doc comment.
func versionTag(resp *machineapi.VersionResponse) string {
	// One node, so one message — but ranging costs nothing and a nil Messages
	// slice is a real reply shape rather than a panic.
	for _, m := range resp.GetMessages() {
		if tag := m.GetVersion().GetTag(); tag != "" {
			return tag
		}
	}

	return ""
}

// Disk is one of a node's disks, reduced to what choosing an install target
// needs. It is a struct of our own rather than machinery's DiskSpec so the
// table below cannot drift with a field we never render.
type Disk struct {
	ID         string
	Serial     string
	Model      string
	Size       string
	Transport  string
	WWID       string
	Rotational bool
	Readonly   bool
	CDROM      bool
}

// ListDisks asks a maintenance-mode node what disks it has.
//
// Same COSI call TestAgainstARealNode has made against real hardware since the
// bring-up branch (client_test.go:847); this is that call given an exported
// caller, not new capability.
func ListDisks(ctx context.Context, endpoint string) ([]Disk, error) {
	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	defer c.Close() //nolint:errcheck

	list, err := safe.StateListAll[*blockres.Disk](ctx, c.COSI)
	if err != nil {
		return nil, fmt.Errorf("listing the node's disks: %w", err)
	}

	out := make([]Disk, 0, list.Len())

	for d := range list.All() {
		s := d.TypedSpec()
		out = append(out, Disk{
			ID: d.Metadata().ID(), Serial: s.Serial, Model: s.Model,
			Size: s.PrettySize, Transport: s.Transport, WWID: s.WWID,
			Rotational: s.Rotational, Readonly: s.Readonly, CDROM: s.CDROM,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return out, nil
}

// FormatDisks renders the table that is the REMEDY for both refusals below.
// Without talosctl there is no other way to learn a serial, so this is not
// diagnostic decoration — it is the only path forward.
func FormatDisks(disks []Disk) string {
	var b strings.Builder

	fmt.Fprintf(&b, "  %-8s %-24s %-22s %-10s %s\n", "DEVICE", "SERIAL", "MODEL", "SIZE", "NOTES")

	for _, d := range disks {
		var notes []string
		// READONLY FIRST, and it is the one that matters: the medium you booted
		// from presents as a read-only virtio-blk device rather than a cdrom.
		if d.Readonly {
			notes = append(notes, "readonly — probably the medium you booted from")
		}

		if d.CDROM {
			notes = append(notes, "cdrom")
		}

		if d.Rotational {
			notes = append(notes, "rotational")
		}

		if d.Transport != "" {
			notes = append(notes, d.Transport)
		}

		if d.Serial == "" && d.WWID != "" {
			notes = append(notes, "no serial; wwid "+d.WWID)
		}

		serial := d.Serial
		if serial == "" {
			serial = "(none)"
		}

		fmt.Fprintf(&b, "  %-8s %-24s %-22s %-10s %s\n",
			d.ID, serial, d.Model, d.Size, strings.Join(notes, ", "))
	}

	return b.String()
}

// RequireDisk refuses unless serial names a disk this node actually has.
//
// TWO refusals, ONE table, because they are the same remedy. The empty case is
// a first run. The unmatched case is a TYPO, which is the realistic failure and
// the expensive one: Talos with a selector matching nothing installs nowhere
// and reports it as a hang, with nothing pointing at a mistyped serial.
//
// Auto-selecting by size was rejected — config.go already calls that "a coin
// flip once there are two large disks", and on hardware the losing side
// overwrites a disk that may hold data, which is the one failure here that
// re-running cannot repair.
func RequireDisk(disks []Disk, serial, what string) error {
	if serial == "" {
		return fmt.Errorf("no serial given for the %s, and one cannot be guessed\n\n"+
			"this node's disks:\n\n%s\n"+
			"  put one of those serials in the machine file, then run adopt again",
			what, FormatDisks(disks))
	}

	for _, d := range disks {
		if d.Serial == serial {
			return nil
		}
	}

	return fmt.Errorf("the %s serial %q matches none of this node's disks\n\n"+
		"this node's disks:\n\n%s\n"+
		"  a serial that matches nothing is almost always a typo. Talos does not "+
		"report it as one:\n  it installs nowhere and the bring-up hangs.",
		what, serial, FormatDisks(disks))
}
