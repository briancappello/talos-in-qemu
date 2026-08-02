package cluster

import (
	"slices"
	"strings"
	"testing"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	blockres "github.com/siderolabs/talos/pkg/machinery/resources/block"
)

func testDisks() []Disk {
	return []Disk{
		{ID: "sda", Serial: "S1", Model: "Samsung SSD", Size: "500 GB", Transport: "sata"},
		{ID: "sdb", Serial: "", Model: "SanDisk Cruzer", Size: "32 GB", Transport: "usb", Readonly: true},
	}
}

// newDiskResource builds the machinery resource ListDisks would have been
// handed by COSI. Three lines, no node, no network — which is the whole point
// of toDisks being a function of its own.
func newDiskResource(id string, mutate func(*blockres.DiskSpec)) *blockres.Disk {
	d := blockres.NewDisk(blockres.NamespaceName, id)
	if mutate != nil {
		mutate(d.TypedSpec())
	}

	return d
}

// Every value below is DISTINCT from every other, because the failure this
// pins is a swapped pair in the composite literal — and a fixture where two
// fields share a value proves nothing about which one the reader got. A table
// whose SERIAL column shows models compiles, passes a shape check, and is the
// one thing a human cannot act on.
func TestToDisksPutsEveryFieldInItsOwnColumn(t *testing.T) {
	got := toDisks([]*blockres.Disk{
		newDiskResource("vdb", func(s *blockres.DiskSpec) {
			// FIRST: SetSize recomputes PrettySize from the byte count, so a
			// literal PrettySize assigned before it is silently overwritten.
			s.SetSize(4096)

			s.Serial = "serial-value"
			s.Model = "model-value"
			s.PrettySize = "pretty-size-value"
			s.Transport = "transport-value"
			s.WWID = "wwid-value"
			// Never rendered, and named so that reaching for one by mistake
			// shows up in the diff rather than passing as an empty string.
			s.DevPath = "dev-path-value"
			s.UUID = "uuid-value"
			s.BusPath = "bus-path-value"
			s.SubSystem = "sub-system-value"
			s.Modalias = "modalias-value"
		}),
	})

	want := Disk{
		ID: "vdb", Serial: "serial-value", Model: "model-value",
		Size: "pretty-size-value", Transport: "transport-value", WWID: "wwid-value",
	}

	if len(got) != 1 {
		t.Fatalf("toDisks returned %d disks for one input: %+v", len(got), got)
	}

	if got[0] != want {
		t.Errorf("toDisks mapped the spec wrongly\n  got:  %+v\n  want: %+v\n"+
			"  reason: Size is PrettySize, not Size; ID is the resource's metadata, not a "+
			"spec field. A pair swapped here still renders a table — one nobody can act on",
			got[0], want)
	}
}

// The three flags separately, because all-true cannot tell them apart and each
// one drives a different note in the table.
func TestToDisksPutsEachFlagInItsOwnField(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*blockres.DiskSpec)
		want Disk
	}{
		{"rotational", func(s *blockres.DiskSpec) { s.Rotational = true }, Disk{ID: "vdb", Rotational: true}},
		{"readonly", func(s *blockres.DiskSpec) { s.Readonly = true }, Disk{ID: "vdb", Readonly: true}},
		{"cdrom", func(s *blockres.DiskSpec) { s.CDROM = true }, Disk{ID: "vdb", CDROM: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toDisks([]*blockres.Disk{newDiskResource("vdb", tc.set)})
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("toDisks(%s) = %+v, want [%+v]\n"+
					"  reason: readonly is how the boot medium is recognised; swapped with "+
					"another flag it stops flagging the one disk that must not be installed to",
					tc.name, got, tc.want)
			}
		})
	}
}

// Ordering is a binding constraint, not a side effect: the table is read by a
// human copying a serial out of it, and COSI promises no order. Inputs are
// supplied out of order so that returning them untouched fails.
func TestToDisksOrdersByID(t *testing.T) {
	got := toDisks([]*blockres.Disk{
		newDiskResource("vdc", nil),
		newDiskResource("vda", nil),
		newDiskResource("sdb", nil),
		newDiskResource("vdb", nil),
	})

	ids := make([]string, 0, len(got))
	for _, d := range got {
		ids = append(ids, d.ID)
	}

	if want := []string{"sdb", "vda", "vdb", "vdc"}; !slices.Equal(ids, want) {
		t.Errorf("toDisks returned %v, want %v\n"+
			"  reason: a table that reshuffles between two runs of adopt has to be "+
			"re-read from the top every time", ids, want)
	}
}

func TestToDisksOnANodeWithNoDisks(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []*blockres.Disk
	}{
		{"a nil list", nil},
		{"an empty list", []*blockres.Disk{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toDisks(tc.in); len(got) != 0 {
				t.Errorf("toDisks(%s) = %+v, want nothing", tc.name, got)
			}
		})
	}
}

func TestRequireDiskRefusesAnEmptySerialAndShowsTheTable(t *testing.T) {
	err := RequireDisk(testDisks(), "", "install target")
	if err == nil {
		t.Fatal("RequireDisk accepted an empty serial\n" +
			"  reason: nothing may install until a human has chosen a disk")
	}

	for _, want := range []string{"S1", "Samsung SSD", "500 GB", "SanDisk Cruzer", "readonly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not show %q:\n%s\n"+
				"  reason: the table IS the remedy — without it there is no way to "+
				"learn a serial without talosctl", want, err)
		}
	}
}

func TestRequireDiskRefusesAnUnmatchedSerialAsATypo(t *testing.T) {
	err := RequireDisk(testDisks(), "S9", "install target")
	if err == nil {
		t.Fatal("RequireDisk accepted a serial matching no disk\n" +
			"  reason: this is the realistic failure — a typo installs nowhere, and " +
			"Talos reports it as a hang")
	}

	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the refusal does not quote the serial that matched nothing: %s", err)
	}
}

func TestRequireDiskAcceptsAMatch(t *testing.T) {
	if err := RequireDisk(testDisks(), "S1", "install target"); err != nil {
		t.Fatalf("RequireDisk rejected a serial that matches: %s", err)
	}
}

// A node reporting no disks at all is a different problem with a different
// remedy, and the two refusals below it both end by telling the reader to pick
// a serial out of the table — which here is a header over nothing.
func TestRequireDiskSaysWhenTheNodeHasNoDisksAtAll(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serial string
	}{
		{"with no serial given", ""},
		{"with a serial that cannot possibly match", "S1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RequireDisk(nil, tc.serial, "install target")
			if err == nil {
				t.Fatal("RequireDisk accepted a node with no disks")
			}

			if !strings.Contains(err.Error(), "no disks at all") {
				t.Errorf("the refusal does not say the node reports no disks:\n%s", err)
			}

			if strings.Contains(err.Error(), "put one of those serials") {
				t.Errorf("the refusal tells the reader to pick a serial out of an empty table:\n%s\n"+
					"  reason: there is nothing to pick — the remedy is a drive the kernel can see",
					err)
			}
		})
	}
}

// The notes column, branch by branch. The WWID fallback matters most: it is
// what a disk with NO serial is identified by, which is exactly the disk a
// human has the least other way to recognise.
func TestFormatDisksNotesEveryDistinguishingFact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		disk    Disk
		want    []string
		notWant []string
	}{
		{
			name: "a cdrom",
			disk: Disk{ID: "sr0", Serial: "S3", CDROM: true},
			want: []string{"cdrom"},
		},
		{
			name: "a spinning disk",
			disk: Disk{ID: "sda", Serial: "S4", Rotational: true},
			want: []string{"rotational"},
		},
		{
			name:    "no serial, but a wwid to name it by",
			disk:    Disk{ID: "vda", WWID: "naa.5000c500a1b2c3d4"},
			want:    []string{"(none)", "no serial; wwid naa.5000c500a1b2c3d4"},
			notWant: []string{"readonly", "cdrom", "rotational"},
		},
		{
			name:    "neither a serial nor a wwid",
			disk:    Disk{ID: "vda", Model: "QEMU HARDDISK"},
			want:    []string{"(none)", "QEMU HARDDISK"},
			notWant: []string{"wwid"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := FormatDisks([]Disk{tc.disk})

			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the table does not show %q:\n%s\n"+
						"  reason: this column is the only way a human learns which disk is "+
						"which without talosctl", want, out)
				}
			}

			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("the table claims %q about a disk that is not:\n%s", notWant, out)
				}
			}
		})
	}
}

// The boot medium is identified by READONLY, not CDROM: client_test.go:954-957
// records that a Talos ISO presents as a read-only virtio-blk device, and so
// does the squashfs loop device. A table flagging only cdrom shows the stick
// you booted from as an ordinary candidate.
func TestFormatDisksFlagsReadonlyNotJustCDROM(t *testing.T) {
	out := FormatDisks([]Disk{{ID: "sdb", Serial: "S2", Readonly: true}})
	if !strings.Contains(out, "readonly") {
		t.Errorf("readonly is not flagged:\n%s", out)
	}
}

func TestNodeVersionRefusesAnEmptyEndpoint(t *testing.T) {
	_, err := NodeVersion(t.Context(), "")
	if err == nil {
		t.Fatal("NodeVersion accepted an empty endpoint\n" +
			"  reason: an empty address spends the caller's whole timeout proving " +
			"that \"\" is not an address")
	}

	if !strings.Contains(err.Error(), "host:port") {
		t.Errorf("error does not say what shape an endpoint has: %s", redactErr(err))
	}
}

// TestVersionTag pins the half of NodeVersion a real node cannot be asked to
// demonstrate: that an unidentifiable version is ANSWERED with "" rather than
// raised as an error. Every reply shape below is one a nil-safe getter chain
// silently absorbs, which is exactly why the scanning around it needs saying
// out loud — swap the final "" for an error and only this test notices.
func TestVersionTag(t *testing.T) {
	message := func(tag string) *machineapi.Version {
		return &machineapi.Version{Version: &machineapi.VersionInfo{Tag: tag}}
	}

	for _, tc := range []struct {
		name string
		resp *machineapi.VersionResponse
		want string
	}{
		{
			name: "no reply at all",
			resp: nil,
		},
		{
			name: "a reply with no messages",
			resp: &machineapi.VersionResponse{},
		},
		{
			name: "a nil message",
			resp: &machineapi.VersionResponse{Messages: []*machineapi.Version{nil}},
		},
		{
			name: "a message carrying no version",
			resp: &machineapi.VersionResponse{Messages: []*machineapi.Version{{}}},
		},
		{
			name: "a version with an empty tag",
			resp: &machineapi.VersionResponse{Messages: []*machineapi.Version{message("")}},
		},
		{
			name: "a version with a tag",
			resp: &machineapi.VersionResponse{Messages: []*machineapi.Version{message("v1.11.2")}},
			want: "v1.11.2",
		},
		{
			// Not Messages[0]: the FIRST answer that identifies a version.
			name: "a blank tag ahead of a real one",
			resp: &machineapi.VersionResponse{Messages: []*machineapi.Version{message(""), message("v1.11.2")}},
			want: "v1.11.2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionTag(tc.resp); got != tc.want {
				t.Errorf("versionTag() = %q, want %q\n"+
					"  reason: step 3's guard is the single place that refuses an unknown "+
					"version, and it only ever sees one because this returns %q", got, tc.want, tc.want)
			}
		})
	}
}
