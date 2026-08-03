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
	cases := []struct {
		name string
		set  func(*Network)
	}{
		{"a bare address with no prefix", func(n *Network) { n.Address = "192.168.2.10" }},
		{"an address that is not an address", func(n *Network) { n.Address = "not-an-address/24" }},
		{"an IPv6 prefix", func(n *Network) { n.Address = "2001:db8::10/64" }},
		{"a gateway that is not an address", func(n *Network) { n.Gateway = "192.168.2" }},
		{"no nameservers", func(n *Network) { n.Nameservers = nil }},
		{"an empty nameserver", func(n *Network) { n.Nameservers = []string{""} }},
		{"no hardware address", func(n *Network) { n.HardwareAddr = "" }},
		{"a hardware address that is not a MAC", func(n *Network) { n.HardwareAddr = "84:47:09" }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := validNetwork()
			c.set(n)

			if err := CheckNetwork(n, "192.168.2.10"); err == nil {
				t.Errorf("%s was accepted", c.name)
			}
		})
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
