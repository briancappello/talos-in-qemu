package cluster

import (
	"strings"
	"testing"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

func testDisks() []Disk {
	return []Disk{
		{ID: "sda", Serial: "S1", Model: "Samsung SSD", Size: "500 GB", Transport: "sata"},
		{ID: "sdb", Serial: "", Model: "SanDisk Cruzer", Size: "32 GB", Transport: "usb", Readonly: true},
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

// The boot medium is identified by READONLY, not CDROM: client_test.go:929-935
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
