package main

import (
	"bytes"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

func TestARPProbeRoundTrip(t *testing.T) {
	target := netip.MustParseAddr("10.254.0.11")
	req := arpProbe(probeMAC, target)

	if len(req) != 42 || !bytes.Equal(req[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("the request is not a 42-byte broadcast frame: % x", req)
	}

	// The request itself is not a reply, so the prober skips its own echo.
	if _, ok := arpReplyFrom(req, target); ok {
		t.Error("the prober's own request was taken for a reply")
	}

	reply := fakeARPReply(req, net.HardwareAddr{0x52, 0x54, 0, 1, 2, 3}, target)

	if mac, ok := arpReplyFrom(reply, target); !ok || mac.String() != "52:54:00:01:02:03" {
		t.Errorf("arpReplyFrom(reply) = %v, %v", mac, ok)
	}

	if _, ok := arpReplyFrom(reply, netip.MustParseAddr("10.254.0.12")); ok {
		t.Error("a reply from another address was accepted")
	}
}

// fakeARPReply is what the owner's kernel sends back to a probe.
func fakeARPReply(req []byte, mac net.HardwareAddr, from netip.Addr) []byte {
	r := bytes.Clone(req)
	copy(r[0:6], req[6:12]) // to the requester
	copy(r[6:12], mac)
	r[21] = 2 // reply
	copy(r[22:28], mac)
	f := from.As4()
	copy(r[28:32], f[:])
	copy(r[32:38], req[6:12])
	copy(r[38:42], []byte{0, 0, 0, 0})

	return r
}

// The REAL probe, over real multicast on this host's loopback, against a fake
// owner that answers ARP the way a node's kernel does. Skipped where loopback
// multicast is unavailable, which is the very condition the probe reports.
func TestProbeSegmentOverLoopback(t *testing.T) {
	lo, err := loopback()
	if err != nil {
		t.Skip(err)
	}

	group, port := netip.MustParseAddr("239.255.254.1"), 29997
	gaddr := &net.UDPAddr{IP: group.AsSlice(), Port: port}
	target := netip.MustParseAddr("10.254.0.11")

	owner, err := net.ListenMulticastUDP("udp4", lo, gaddr)
	if err != nil {
		t.Skipf("no multicast on %s: %v", lo.Name, err)
	}
	defer owner.Close()

	pc := ipv4.NewPacketConn(owner)
	_ = pc.SetMulticastInterface(lo)
	_ = pc.SetMulticastLoopback(true)

	go func() {
		buf := make([]byte, 2048)

		for {
			n, _, err := owner.ReadFromUDP(buf)
			if err != nil {
				return
			}

			f := buf[:n]
			if n >= 42 && f[12] == 0x08 && f[13] == 0x06 && f[21] == 1 && bytes.Equal(f[38:42], target.AsSlice()) {
				_, _ = owner.WriteToUDP(fakeARPReply(f, net.HardwareAddr{0x52, 0x54, 0, 9, 9, 9}, target), gaddr)
			}
		}
	}()

	start := time.Now()
	if err := probeSegment(group, port, target); err != nil {
		t.Fatalf("probeSegment against a live responder: %v", err)
	}

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the probe took %s against a responder on loopback", took)
	}

	// And silence is reported, naming the loopback check.
	if err := probeSegment(group, port, netip.MustParseAddr("10.254.0.99")); err == nil ||
		!strings.Contains(err.Error(), "ip maddr show") {
		t.Errorf("a probe nobody answers = %v, want the loopback multicast refusal", err)
	}
}
