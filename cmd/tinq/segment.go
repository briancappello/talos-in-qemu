package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/ipv4"
)

// THE SEGMENT PROBE. A cluster network is a QEMU multicast socket on loopback,
// and some hosts drop multicast on lo. When they do, a joiner boots, installs,
// and then waits out an etcd join timeout that names neither multicast nor
// loopback. This probe asks the question directly, from the host, BEFORE the
// joiner is created:
//
// tinq joins the segment's multicast group on the loopback interface, exactly
// as QEMU does, and broadcasts an ARP request for the owner's cluster address.
// The owner's kernel answers it. A reply proves that loopback carries the
// segment's frames and that the owner is on it; silence means one of the two
// is false, and the message says how to tell which.
//
// The request is an ARP PROBE (sender address 0.0.0.0, RFC 5227): it asks
// without claiming an address, so no node's neighbour table learns anything
// from it. Linux answers a probe for an address it holds.

// segmentProbeTimeout bounds the probe. A reply on loopback arrives in
// milliseconds; the rest is retries for a guest that is busy.
const segmentProbeTimeout = 5 * time.Second

// probeMAC is the source MAC of the probe. Locally administered (0x02), and
// outside QEMU's 52:54:00 prefix so it can never be a node's.
var probeMAC = net.HardwareAddr{0x02, 0x74, 0x69, 0x6e, 0x71, 0x01}

// arpProbe encodes an Ethernet frame carrying an ARP request for target.
func arpProbe(src net.HardwareAddr, target netip.Addr) []byte {
	var b bytes.Buffer

	b.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) // broadcast
	b.Write(src)
	_ = binary.Write(&b, binary.BigEndian, uint16(0x0806)) // ARP
	_ = binary.Write(&b, binary.BigEndian, uint16(1))      // Ethernet
	_ = binary.Write(&b, binary.BigEndian, uint16(0x0800)) // IPv4
	b.Write([]byte{6, 4})
	_ = binary.Write(&b, binary.BigEndian, uint16(1)) // request
	b.Write(src)
	b.Write([]byte{0, 0, 0, 0}) // probe: no sender address
	b.Write(make([]byte, 6))
	t := target.As4()
	b.Write(t[:])

	return b.Bytes()
}

// arpReplyFrom reports whether frame is an ARP reply from sender, and the MAC
// it came from.
func arpReplyFrom(frame []byte, sender netip.Addr) (net.HardwareAddr, bool) {
	const arp = 14

	if len(frame) < arp+28 || binary.BigEndian.Uint16(frame[12:14]) != 0x0806 {
		return nil, false
	}

	if binary.BigEndian.Uint16(frame[arp+6:arp+8]) != 2 {
		return nil, false
	}

	spa, _ := netip.AddrFromSlice(frame[arp+14 : arp+18])
	if spa != sender {
		return nil, false
	}

	return net.HardwareAddr(bytes.Clone(frame[arp+8 : arp+14])), true
}

// loopback is the host's loopback interface: lo on Linux, lo0 on macOS.
func loopback() (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 && ifaces[i].Flags&net.FlagUp != 0 {
			return &ifaces[i], nil
		}
	}

	return nil, errors.New("this host has no loopback interface that is up")
}

// probeSegment asks the owner at target, over the segment at group:port,
// whether it is there. It returns nil on an ARP reply from target.
func probeSegment(group netip.Addr, port int, target netip.Addr) error {
	lo, err := loopback()
	if err != nil {
		return err
	}

	gaddr := &net.UDPAddr{IP: group.AsSlice(), Port: port}

	conn, err := net.ListenMulticastUDP("udp4", lo, gaddr)
	if err != nil {
		return segmentUnusable(group, port, target, lo.Name,
			fmt.Sprintf("joining the multicast group on %s failed: %v", lo.Name, err))
	}
	defer conn.Close() //nolint:errcheck

	// The same socket options QEMU's localaddr=127.0.0.1 amounts to: send on
	// loopback, and deliver our own group's traffic back to this host, which
	// is where every VM of the segment lives.
	pc := ipv4.NewPacketConn(conn)
	if err := pc.SetMulticastInterface(lo); err != nil {
		return segmentUnusable(group, port, target, lo.Name, err.Error())
	}

	if err := pc.SetMulticastLoopback(true); err != nil {
		return segmentUnusable(group, port, target, lo.Name, err.Error())
	}

	frame := arpProbe(probeMAC, target)
	deadline := time.Now().Add(segmentProbeTimeout)
	buf := make([]byte, 2048)

	for time.Now().Before(deadline) {
		if _, err := conn.WriteToUDP(frame, gaddr); err != nil {
			return segmentUnusable(group, port, target, lo.Name, "sending on the segment failed: "+err.Error())
		}

		// Read everything the segment carries for up to a second, then ask
		// again. Our own request comes back too (loopback), and is skipped.
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))

		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				break
			}

			if _, ok := arpReplyFrom(buf[:n], target); ok {
				return nil
			}
		}
	}

	return segmentUnusable(group, port, target, lo.Name, "no ARP reply within "+segmentProbeTimeout.String())
}

func segmentUnusable(group netip.Addr, port int, target netip.Addr, lo, why string) error {
	return fmt.Errorf("the cluster network does not reach %s (multicast %s:%d on %s): %s\n\n"+
		"  a joiner reaches its owner only over this segment, so it is refused now rather\n"+
		"  than left to fail as an etcd join timeout. The usual cause is a host that drops\n"+
		"  multicast on loopback:\n\n"+
		"    ip maddr show %s                  # the owner's QEMU should have joined %s\n"+
		"    sudo nft list ruleset | grep -i multicast\n\n"+
		"  If the group is there, check that the owner is running and holds %s",
		target, group, port, lo, why, lo, group, target)
}
