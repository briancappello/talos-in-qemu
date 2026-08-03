package cluster

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// A NODE HAS TWO ADDRESSES IN ITS LIFE, and this file is what keeps them from
// disagreeing.
//
// Before the install it answers at the maintenance address — from DHCP, or from
// an `ip=` kernel argument typed at a GRUB prompt on a segment that serves no
// DHCP at all. After the install it answers at Network.Address, because the
// installed system writes its own kernel command line and inherits nothing from
// the ISO.
//
// The certificate SAN, the talosconfig endpoint and the kubeconfig server are
// all baked at generation time from the SECOND of those. That is why a mismatch
// here is not merely a node that is hard to find: it is a node whose PKI names
// an address nobody dials, which no amount of re-pointing a client repairs.

// Network is a static address for a node, or nil for DHCP.
//
// All four fields are required together. There is no useful half-configured
// state: a static block with no nameservers is a node that cannot resolve a
// registry, and therefore cannot pull the image it was just told to install.
type Network struct {
	// Address is the node's address after the install, in CIDR —
	// 192.168.2.10/24. The prefix is not decoration: it is how the node knows
	// which addresses are on its own segment, and it is what CheckNetwork
	// compares the maintenance address against.
	Address string
	// Gateway is the default route's next hop. It must be inside Address's
	// prefix, because a node has no route to a gateway off its own segment.
	Gateway string
	// Nameservers is at least one resolver. On a segment with no DHCP there is
	// no other source of one.
	Nameservers []string
	// HardwareAddr is the MAC of the NIC to configure.
	//
	// A MAC rather than an interface name, for the reason the install disk is
	// selected by serial rather than by size: a stable identity, not an
	// enumeration artifact. Predictable interface names come from slot and
	// firmware indices and change when the card moves.
	HardwareAddr string
}

// IP is the host part of Address with the prefix stripped.
//
// This value becomes the apid certificate's subject alt name, the talosconfig
// endpoint and the kubeconfig server, so cmd/tinq uses it to build BOTH
// post-install endpoints.
func (n *Network) IP() (string, error) {
	p, err := netip.ParsePrefix(n.Address)
	if err != nil {
		return "", fmt.Errorf("the static address %q is not an address with a prefix length: %w", n.Address, err)
	}

	return p.Addr().String(), nil
}

// CheckNetwork refuses a static block that would strand the node.
//
// ONE function with TWO callers, for the same reason errUnknownTalosVersion is
// one: Up calls it before Boot, so nothing can bypass it, and adopt calls it
// first, so the answer arrives in a second rather than after a ten-minute
// maintenance wait. Two copies would drift into two explanations of one refusal.
//
// A nil Network passes. Absent is the answer every machine gave before this
// feature existed, and it has to stay an answer rather than become a missing
// field.
//
// maintenanceAddr is a BARE address with no port, for the same reason
// ConfigInput.APIAddress is one: what has to sit inside the prefix is a host,
// not a socket.
func CheckNetwork(n *Network, maintenanceAddr string) error {
	if n == nil {
		return nil
	}

	prefix, err := netip.ParsePrefix(n.Address)
	if err != nil {
		return fmt.Errorf("spec.baremetal.network.address %q is not an address with a prefix "+
			"length\n\n  it must be CIDR, e.g. 192.168.2.10/24 — the prefix is how the node "+
			"knows which\n  addresses are on its own segment, and there is no default that is "+
			"right for two networks", n.Address)
	}

	// Refused by NAME rather than by accident. Left to fall through, a v6
	// prefix would fail the containment check below against a v4 maintenance
	// address and report a mismatch, which is a true statement about the wrong
	// problem.
	if !prefix.Addr().Is4() {
		return fmt.Errorf("spec.baremetal.network.address %q is IPv6, which is not supported yet\n\n"+
			"  spec.baremetal.maintenanceEndpoint cannot carry a v6 literal either — see the "+
			"refusal\n  in adopt for why", n.Address)
	}

	if prefix.Addr() == prefix.Masked().Addr() {
		return fmt.Errorf("spec.baremetal.network.address %q names the SEGMENT, not a node on it\n\n"+
			"  the host part is all zeroes. A node needs its own address inside that prefix, "+
			"e.g. %s",
			n.Address, netip.PrefixFrom(prefix.Masked().Addr().Next(), prefix.Bits()))
	}

	gateway, err := netip.ParseAddr(n.Gateway)
	if err != nil {
		return fmt.Errorf("spec.baremetal.network.gateway %q is not an address", n.Gateway)
	}

	if !prefix.Contains(gateway) {
		return fmt.Errorf("spec.baremetal.network.gateway %s is outside %s\n\n"+
			"  a node has no route to a gateway off its own segment, so this one is "+
			"unreachable\n  from the moment the node boots — and with it, everything past it",
			n.Gateway, n.Address)
	}

	if len(n.Nameservers) == 0 {
		return errors.New("spec.baremetal.network.nameservers is required alongside a static address\n\n" +
			"  a segment with no DHCP offers no resolver either, and a node that cannot resolve\n" +
			"  a registry cannot pull the image it was just told to install")
	}

	for i, ns := range n.Nameservers {
		if _, err := netip.ParseAddr(ns); err != nil {
			return fmt.Errorf("spec.baremetal.network.nameservers[%d] %q is not an address", i, ns)
		}
	}

	// PRESENCE IS REFUSED HERE, from the file, and the remedy cannot be "run
	// adopt to see the table" — this check fires before the node is dialled, so
	// there is no table yet. The realistic failure is not an omitted MAC but a
	// MISTYPED one, and that is the case RequireLink catches with the node's
	// real links printed.
	if n.HardwareAddr == "" {
		return errors.New("spec.baremetal.network.hardwareAddr is required alongside a static address\n\n" +
			"  the node's own console lists its interfaces and their MACs, and you are already\n" +
			"  standing at it — the ip= kernel argument was typed on that same screen.\n\n" +
			"  A MAC that is well-formed but wrong is refused later, with this node's real\n" +
			"  links printed for you to copy from.")
	}

	if _, err := net.ParseMAC(n.HardwareAddr); err != nil {
		return fmt.Errorf("spec.baremetal.network.hardwareAddr %q is not a MAC address\n\n"+
			"  it looks like 84:47:09:47:35:f9, and adopt prints this node's real ones", n.HardwareAddr)
	}

	maintenance, err := netip.ParseAddr(maintenanceAddr)
	if err != nil {
		return fmt.Errorf("the maintenance address %q is not a bare address\n\n"+
			"  a port does not belong here: what has to sit inside %s is a host",
			maintenanceAddr, n.Address)
	}

	// THE REFUSAL THIS FEATURE IS BUILT AROUND, and it is last because every
	// check above has to have passed for this one to mean anything.
	//
	// Inside the prefix, the node re-pins itself on the same wire and comes
	// back reachable — nothing physical happens and the run finishes. Outside
	// it, the node boots with an address that does not exist on the wire it is
	// plugged into. Nothing on this side can reach it, and re-running cannot
	// repair it either: adopt needs the maintenance API, and a node that has
	// installed never serves it again.
	if !prefix.Contains(maintenance) {
		return fmt.Errorf("this node is adopted at %s, but %s would put it on another segment\n\n"+
			"  after the install reboot it would answer at neither: not at %s, which it no "+
			"longer\n  holds, and not at %s, which does not exist on the wire it is plugged "+
			"into.\n\n  Re-running cannot repair that — adopt needs the maintenance API, and an "+
			"installed\n  node never serves it again. Move the machine to its final segment "+
			"FIRST, give it\n  the address there with an ip= kernel argument, and adopt it at "+
			"that address.",
			maintenanceAddr, n.Address, maintenanceAddr, n.Address)
	}

	return nil
}
