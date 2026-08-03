package cluster

import (
	"slices"
	"strings"
	"testing"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	blockres "github.com/siderolabs/talos/pkg/machinery/resources/block"
	netres "github.com/siderolabs/talos/pkg/machinery/resources/network"
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

// InstalledNodeVersion is reached on a RESUMED bring-up, where the whole point
// is that nothing waits ten minutes. Both refusals below must therefore land
// without a dial: an empty endpoint and a credential that does not parse are
// both provable from the arguments alone.
func TestInstalledNodeVersionRefusesBeforeDialling(t *testing.T) {
	_, err := InstalledNodeVersion(t.Context(), []byte("not a talosconfig"), "")
	if err == nil {
		t.Fatal("InstalledNodeVersion accepted an empty endpoint\n" +
			"  reason: an empty address spends the caller's whole timeout proving " +
			"that \"\" is not an address")
	}

	if !strings.Contains(err.Error(), "host:port") {
		t.Errorf("error does not say what shape an endpoint has: %s", redactErr(err))
	}

	// THE TALOSCONFIG IS A PRIVATE KEY. The parser's message can quote the
	// document it failed on, so none of it may travel — the same rule
	// AuthenticatedClient obeys, asserted again here because this is a second
	// caller of it. What this catches is NOT a %w added inside
	// InstalledNodeVersion: all it ever sees is errSecretParse's already-redacted
	// text, so wrapping that leaks nothing. It catches a future fmt.Errorf on
	// this path that interpolates the talosconfig ARGUMENT into its own message.
	//
	// Built the way client_test.go builds its own: valid YAML to the scanner and
	// a type error to the decoder, carrying the seven-character marker that a
	// decoder's truncated quote is short enough to show. The assertion is that
	// file's helper rather than a substring of our own, because a marker long
	// enough to be truncated away is a test that cannot fail — measured there,
	// not assumed.
	broken := []byte("context: default\ncontexts: " + marker + strings.Repeat("A", 200) + "\n")

	_, err = InstalledNodeVersion(t.Context(), broken, "192.0.2.1:50000")

	assertNoSecretParserOutput(t, "talosconfig", err)
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

// testLinks is the target machine's real shape: two physical NICs, one up and
// one down, plus the loopback every node has. The DOWN one is the point — a
// name or a MAC typo that lands on it strands the machine just as thoroughly as
// a wrong disk serial overwrites one.
func testLinks() []Link {
	return []Link{
		{ID: "enp1s0", HardwareAddr: "84:47:09:47:35:f9", Driver: "igc", OperState: "up", Carrier: true, Physical: true},
		{ID: "enp2s0", HardwareAddr: "84:47:09:47:35:f8", Driver: "igc", OperState: "down", Carrier: false, Physical: true},
	}
}

// newLinkResource builds the machinery resource ListLinks would have been
// handed by COSI. No node and no network, which is the whole point of toLinks
// being a function of its own.
//
// Ether by default: a zero LinkType is not LinkEther, so a fixture left at the
// zero value would be dropped by the Physical() filter before reaching the
// assertion it was written for, and every test below would pass on nothing.
func newLinkResource(id string, mutate func(*netres.LinkStatusSpec)) *netres.LinkStatus {
	l := netres.NewLinkStatus(netres.NamespaceName, id)
	l.TypedSpec().Type = nethelpers.LinkEther

	if mutate != nil {
		mutate(l.TypedSpec())
	}

	return l
}

// Every value below is DISTINCT from every other, because the failure this
// pins is a swapped pair in the composite literal — and a table whose MAC
// column shows drivers compiles, renders, and is the one thing a human cannot
// copy a MAC out of.
func TestToLinksPutsEveryFieldInItsOwnColumn(t *testing.T) {
	got := toLinks([]*netres.LinkStatus{
		newLinkResource("enp1s0", func(s *netres.LinkStatusSpec) {
			s.HardwareAddr = nethelpers.HardwareAddr{0x84, 0x47, 0x09, 0x47, 0x35, 0xf9}
			s.PermanentAddr = nethelpers.HardwareAddr{0x84, 0x47, 0x09, 0x47, 0x35, 0xf8}
			s.Driver = "driver-value"
			s.OperationalState = nethelpers.OperStateUp
			s.LinkState = true
			// Never rendered, and named so that reaching for one by mistake
			// shows up in the diff rather than passing as an empty string.
			s.BusPath = "bus-path-value"
			s.Product = "product-value"
			s.Vendor = "vendor-value"
			s.DriverVersion = "driver-version-value"
		}),
	})

	want := Link{
		ID: "enp1s0", HardwareAddr: "84:47:09:47:35:f9", PermanentAddr: "84:47:09:47:35:f8",
		Driver: "driver-value", OperState: "up", Carrier: true, Physical: true,
	}

	if len(got) != 1 {
		t.Fatalf("toLinks returned %d links for one input: %+v", len(got), got)
	}

	if got[0] != want {
		t.Errorf("toLinks mapped the spec wrongly\n  got:  %+v\n  want: %+v\n"+
			"  reason: Carrier is LinkState, not OperationalState; ID is the resource's "+
			"metadata, not a spec field. A pair swapped here still renders a table — one "+
			"nobody can act on", got[0], want)
	}
}

// CARRIER IS LinkState, and it is the field this whole task exists for. A node
// can report a link operationally up with no cable in it, and that is exactly
// the NIC an operator picks by mistake on a two-port box.
func TestToLinksReadsCarrierFromLinkStateNotOperState(t *testing.T) {
	got := toLinks([]*netres.LinkStatus{
		newLinkResource("enp2s0", func(s *netres.LinkStatusSpec) {
			s.OperationalState = nethelpers.OperStateUp
			s.LinkState = false
		}),
	})

	if len(got) != 1 || got[0].Carrier {
		t.Errorf("toLinks reported carrier on a link whose LinkState is false: %+v\n"+
			"  reason: an administratively up link with no cable in it is the NIC that "+
			"strands this machine", got)
	}
}

// Loopback, bonds, bridges and vlans arrive through the same resource and none
// of them is something an operator can plug a cable into. Offering one in the
// table invites a choice that cannot work.
func TestToLinksKeepsOnlyLinksACableGoesInto(t *testing.T) {
	got := toLinks([]*netres.LinkStatus{
		newLinkResource("enp1s0", nil),
		newLinkResource("lo", func(s *netres.LinkStatusSpec) { s.Type = nethelpers.LinkLoopbck }),
		newLinkResource("br0", func(s *netres.LinkStatusSpec) { s.Kind = "bridge" }),
		newLinkResource("bond0", func(s *netres.LinkStatusSpec) { s.Kind = "bond" }),
	})

	ids := make([]string, 0, len(got))
	for _, l := range got {
		ids = append(ids, l.ID)
	}

	if want := []string{"enp1s0"}; !slices.Equal(ids, want) {
		t.Errorf("toLinks returned %v, want %v\n"+
			"  reason: a bridge or a loopback in the table is a NIC the operator can "+
			"choose and can never cable", ids, want)
	}
}

// Ordering is a binding constraint, not a side effect: the table is read by a
// human copying a MAC out of it, and COSI promises no order. Inputs are
// supplied out of order so that returning them untouched fails.
func TestToLinksOrdersByID(t *testing.T) {
	got := toLinks([]*netres.LinkStatus{
		newLinkResource("enp3s0", nil),
		newLinkResource("enp1s0", nil),
		newLinkResource("eth0", nil),
		newLinkResource("enp2s0", nil),
	})

	ids := make([]string, 0, len(got))
	for _, l := range got {
		ids = append(ids, l.ID)
	}

	if want := []string{"enp1s0", "enp2s0", "enp3s0", "eth0"}; !slices.Equal(ids, want) {
		t.Errorf("toLinks returned %v, want %v\n"+
			"  reason: a table that reshuffles between two runs of adopt has to be "+
			"re-read from the top every time", ids, want)
	}
}

func TestToLinksOnANodeWithNoLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []*netres.LinkStatus
	}{
		{"a nil list", nil},
		{"an empty list", []*netres.LinkStatus{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toLinks(tc.in); len(got) != 0 {
				t.Errorf("toLinks(%s) = %+v, want nothing", tc.name, got)
			}
		})
	}
}

// The notes column, branch by branch. Carrier is the one an operator acts on,
// and the permanent MAC is printed only when it DIFFERS — a second identical
// MAC in the row is noise the reader has to rule out before copying either.
func TestFormatLinksNotesEveryDistinguishingFact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		link    Link
		want    []string
		notWant []string
	}{
		{
			name: "a cabled NIC",
			link: Link{ID: "enp1s0", HardwareAddr: "84:47:09:47:35:f9", Carrier: true},
			want: []string{"carrier"},
		},
		{
			name:    "a dark NIC",
			link:    Link{ID: "enp2s0", HardwareAddr: "84:47:09:47:35:f8"},
			want:    []string{"NO CARRIER"},
			notWant: []string{"permanent"},
		},
		{
			name:    "a MAC the firmware overrode",
			link:    Link{ID: "enp1s0", HardwareAddr: "02:00:00:00:00:01", PermanentAddr: "84:47:09:47:35:f9"},
			want:    []string{"permanent 84:47:09:47:35:f9"},
			notWant: []string{"permanent 02:00:00:00:00:01"},
		},
		{
			name:    "a MAC nothing overrode",
			link:    Link{ID: "enp1s0", HardwareAddr: "84:47:09:47:35:f9", PermanentAddr: "84:47:09:47:35:f9"},
			notWant: []string{"permanent"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := FormatLinks([]Link{tc.link})

			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the table does not show %q:\n%s\n"+
						"  reason: this column is the only way a human learns which NIC is "+
						"cabled without talosctl", want, out)
				}
			}

			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("the table claims %q about a link that is not:\n%s", notWant, out)
				}
			}
		})
	}
}

func TestRequireLinkAcceptsALinkWithCarrier(t *testing.T) {
	if err := RequireLink(testLinks(), "84:47:09:47:35:f9"); err != nil {
		t.Errorf("the node's only NIC with carrier was refused: %s", err)
	}
}

func TestRequireLinkIsCaseInsensitive(t *testing.T) {
	// A MAC copied out of a datasheet or a switch's web UI is upper case, and
	// the node reports lower. Refusing that is a refusal over presentation.
	if err := RequireLink(testLinks(), "84:47:09:47:35:F9"); err != nil {
		t.Errorf("an upper-case MAC was refused: %s", err)
	}
}

func TestRequireLinkRefusesALinkWithNoCarrier(t *testing.T) {
	err := RequireLink(testLinks(), "84:47:09:47:35:f8")
	if err == nil {
		t.Fatal("a NIC with no carrier was accepted\n" +
			"  reason: the node installs, reboots, brings up a cable that is not there,\n" +
			"  and is never heard from again")
	}

	if !strings.Contains(err.Error(), "enp2s0") {
		t.Errorf("the refusal does not name the link it found: %s", err)
	}

	// THE HEADLINE, not the whole error. Every refusal embeds FormatLinks, and
	// that table prints "NO CARRIER" for enp2s0 whatever the refusal says — so
	// both the assertion above and a whole-error carrier check are satisfied by
	// the table alone. Collapsing this arm into the no-match text would leave a
	// dark port reported as a typo, and a reader hunting a typo re-enters the
	// same correct MAC while the wrong NIC stays chosen.
	if headline, _, _ := strings.Cut(err.Error(), "\n"); !strings.Contains(headline, "NO CARRIER") {
		t.Errorf("the refusal's first line does not say NO CARRIER, so it reads as a typo:\n%s", err)
	}
}

func TestRequireLinkRefusesAMACThisNodeDoesNotHave(t *testing.T) {
	err := RequireLink(testLinks(), "00:00:00:00:00:01")
	if err == nil {
		t.Fatal("a MAC matching none of the node's links was accepted")
	}

	// The table IS the remedy: without talosctl there is no other way to learn
	// this node's MACs.
	for _, want := range []string{"enp1s0", "84:47:09:47:35:f9", "enp2s0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not print %s, so it cannot be acted on:\n%s", want, err)
		}
	}

	// THE OTHER SIDE of the carrier assertion, and on the headline for the same
	// reason: the table below it prints "NO CARRIER" for enp2s0 in this refusal
	// too. A no-match headline that blamed the carrier would send a reader to
	// plug in a cable for a MAC this node does not have. Two operator mistakes,
	// two remedies, and these two tests have to be able to tell them apart.
	if headline, _, _ := strings.Cut(err.Error(), "\n"); strings.Contains(headline, "NO CARRIER") {
		t.Errorf("a no-match refusal blames the carrier, sending the reader to the wrong remedy:\n%s", err)
	}
}

func TestRequireLinkRefusesAnEmptyMAC(t *testing.T) {
	// DEFENSIVE, and deliberately kept. CheckNetwork refuses an empty
	// hardwareAddr from the manifest before adopt ever reaches the node, so
	// this arm is reachable only by a direct caller of ListLinks. It stays
	// because the table is the right answer to "which one" no matter who asks,
	// and because a future caller that skips CheckNetwork must not get an
	// interface selected by an empty MAC.
	err := RequireLink(testLinks(), "")
	if err == nil {
		t.Fatal("no hardwareAddr was accepted")
	}

	if !strings.Contains(err.Error(), "84:47:09:47:35:f9") {
		t.Errorf("the first-run refusal does not print the table:\n%s", err)
	}
}

func TestRequireLinkRefusesANodeWithNoLinks(t *testing.T) {
	err := RequireLink(nil, "84:47:09:47:35:f9")
	if err == nil {
		t.Fatal("a node reporting no links at all was accepted")
	}

	// The remedy is NOT "choose one" — there is nothing to choose from.
	if strings.Contains(err.Error(), "DEVICE") {
		t.Error("the refusal prints an empty table as if it were a menu")
	}
}
