package cluster

import (
	"strings"
	"testing"
)

// validNetwork is the block the target machine actually uses, and every case
// below mutates ONE field of it. A fixture built per-case would let a test pass
// because of a second mistake nobody noticed.
func validNetwork() *Network {
	return &Network{
		Address:      "192.168.2.10/24",
		Gateway:      "192.168.2.1",
		Nameservers:  []string{"1.1.1.1"},
		HardwareAddr: "84:47:09:47:35:f9",
	}
}

func TestCheckNetworkAcceptsANodeThatDoesNotMove(t *testing.T) {
	// The no-DHCP case: the operator gave the node its final address at the
	// GRUB prompt, so the two are equal and Contains is trivially true.
	if err := CheckNetwork(validNetwork(), "192.168.2.10"); err != nil {
		t.Errorf("a node adopted at the address it will keep was refused: %s", err)
	}
}

func TestCheckNetworkAcceptsASameWireRePin(t *testing.T) {
	// Adopted over DHCP at .186, installed with a static .10 on the SAME
	// segment. The node reboots, re-pins itself and is still reachable, so
	// nothing physical has to happen and the run can finish.
	n := validNetwork()
	n.Address = "192.168.1.50/24"
	n.Gateway = "192.168.1.1"

	if err := CheckNetwork(n, "192.168.1.186"); err != nil {
		t.Errorf("a same-wire re-pin was refused: %s", err)
	}
}

func TestCheckNetworkRefusesAnAddressOnAnotherWire(t *testing.T) {
	// THE REFUSAL THIS WHOLE FEATURE IS BUILT AROUND. The node would come back
	// on an address that does not exist on the wire it is plugged into, the run
	// cannot finish, and a re-run cannot repair it because the node will never
	// serve the maintenance API again.
	err := CheckNetwork(validNetwork(), "192.168.1.186")
	if err == nil {
		t.Fatal("an address on a different segment than the maintenance address was accepted\n" +
			"  reason: after the install reboot that node is unreachable, and adopt cannot " +
			"resume because maintenance mode is gone")
	}

	for _, want := range []string{"192.168.2.10/24", "192.168.1.186"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so it cannot be acted on:\n%s", want, err)
		}
	}
}

func TestCheckNetworkRefusesAGatewayOffTheSegment(t *testing.T) {
	n := validNetwork()
	n.Gateway = "192.168.3.1"

	if err := CheckNetwork(n, "192.168.2.10"); err == nil {
		t.Error("a gateway outside the node's own prefix was accepted\n" +
			"  reason: the node has no route to it, so it has no route off its segment")
	}
}

func TestCheckNetworkRefusesANetworkAddress(t *testing.T) {
	n := validNetwork()
	n.Address = "192.168.2.0/24"

	if err := CheckNetwork(n, "192.168.2.10"); err == nil {
		t.Error("192.168.2.0/24 was accepted as a node address, but it names the segment")
	}
}

func TestCheckNetworkRefusesWhatItCannotParse(t *testing.T) {
	// wantMsg is a fragment of the refusal each case is here to provoke, and no
	// two of them can match another case's message. Asserting only that some
	// error came back would let a case pass for the wrong reason — an IPv6
	// prefix falling through to the segment mismatch, say, which is precisely
	// what the refusal by name exists to prevent.
	cases := []struct {
		name    string
		set     func(*Network)
		wantMsg string
	}{
		{"a bare address with no prefix", func(n *Network) { n.Address = "192.168.2.10" },
			`address "192.168.2.10" is not an address with a prefix length`},
		{"an address that is not an address", func(n *Network) { n.Address = "not-an-address/24" },
			`address "not-an-address/24" is not an address with a prefix length`},
		{"an IPv6 prefix", func(n *Network) { n.Address = "2001:db8::10/64" },
			`address "2001:db8::10/64" is IPv6`},
		{"a broadcast address", func(n *Network) { n.Address = "192.168.2.255/24" },
			`address "192.168.2.255/24" is the BROADCAST address`},
		{"a gateway that is not an address", func(n *Network) { n.Gateway = "192.168.2" },
			`gateway "192.168.2" is not an address`},
		// The three shapes that sit INSIDE the prefix, so containment accepts
		// them and nothing else looks at the field. One keystroke apart from
		// the router, and each is a node that boots with a default route to
		// nothing — unrepairable, because it never serves maintenance again.
		{"a gateway that is the segment", func(n *Network) { n.Gateway = "192.168.2.0" },
			"gateway 192.168.2.0 names the SEGMENT"},
		{"a gateway that is the broadcast address", func(n *Network) { n.Gateway = "192.168.2.255" },
			"gateway 192.168.2.255 is the BROADCAST address"},
		{"a gateway that is the node itself", func(n *Network) { n.Gateway = "192.168.2.10" },
			"gateway 192.168.2.10 is THIS NODE's own address"},
		{"no nameservers", func(n *Network) { n.Nameservers = nil },
			"nameservers is required"},
		{"an empty nameserver", func(n *Network) { n.Nameservers = []string{""} },
			`nameservers[0] "" is not an address`},
		// A well-formed address that this node cannot reach. It parses, so
		// only a family check catches it — and the node it produces is the one
		// the empty-list refusal exists to prevent: no resolver, no image.
		{"an IPv6 nameserver", func(n *Network) { n.Nameservers = []string{"2001:4860:4860::8888"} },
			`nameservers[0] "2001:4860:4860::8888" is IPv6`},
		{"no hardware address", func(n *Network) { n.HardwareAddr = "" },
			"hardwareAddr is required"},
		{"a hardware address that is not a MAC", func(n *Network) { n.HardwareAddr = "84:47:09" },
			`hardwareAddr "84:47:09" is not a MAC address`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := validNetwork()
			c.set(n)

			err := CheckNetwork(n, "192.168.2.10")
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}

			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("%s was refused for another reason than the one under test\n"+
					"  want a message containing: %s\n  got:\n%s", c.name, c.wantMsg, err)
			}
		})
	}
}

func TestCheckNetworkRefusesAV4MappedMaintenanceAddress(t *testing.T) {
	// ::ffff:192.168.2.10 IS 192.168.2.10, but Contains is false across address
	// families, so left to fall through this would be told it is on another
	// segment — a true-sounding sentence about the wrong problem.
	err := CheckNetwork(validNetwork(), "::ffff:192.168.2.10")
	if err == nil {
		t.Fatal("a v4-mapped IPv6 maintenance address was accepted, but it cannot be compared " +
			"to a v4 prefix")
	}

	if !strings.Contains(err.Error(), "is IPv6") {
		t.Errorf("the refusal blames the segment rather than the address family:\n%s", err)
	}
}

func TestCheckNetworkPassesTheDHCPCase(t *testing.T) {
	// Absent is the answer every machine gave before this feature existed, and
	// it must stay an answer rather than becoming a missing field.
	if err := CheckNetwork(nil, "192.168.1.50"); err != nil {
		t.Errorf("a machine with no network block was refused: %s", err)
	}
}

func TestCheckNetworkRefusesAMaintenanceAddressItCannotParse(t *testing.T) {
	// A host:port here is the realistic mistake — every other endpoint in this
	// package carries a port, and this one must not.
	if err := CheckNetwork(validNetwork(), "192.168.2.10:50000"); err == nil {
		t.Error("a maintenance address with a port was accepted, but it cannot be compared to a prefix")
	}
}

func TestNetworkIPStripsThePrefix(t *testing.T) {
	// This value becomes the certificate SAN, the talosconfig endpoint and the
	// kubeconfig server. A prefix left on it names a host nobody can dial.
	got, err := validNetwork().IP()
	if err != nil {
		t.Fatalf("IP(): %s", err)
	}

	if got != "192.168.2.10" {
		t.Errorf("IP() = %q, want 192.168.2.10", got)
	}
}

func TestNetworkIPRefusesAMachineWithNoAddressBlock(t *testing.T) {
	// A nil Network is the DHCP case, which CheckNetwork accepts, so nil is
	// part of this type's vocabulary and asking it for an address has to be an
	// error rather than a panic in tinq.
	var n *Network

	got, err := n.IP()
	if err == nil {
		t.Fatalf("IP() on a machine with no static address block returned %q, want an error", got)
	}
}

func TestNetworkIPRefusesAnAddressWithNoPrefix(t *testing.T) {
	n := validNetwork()
	n.Address = "192.168.2.10"

	if _, err := n.IP(); err == nil {
		t.Error("IP() accepted an address with no prefix length, so nothing would catch a " +
			"malformed block reaching the certificate SAN")
	}
}
