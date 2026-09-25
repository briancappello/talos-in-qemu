package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coglative/talos-in-qemu/cluster"
	"github.com/coglative/talos-in-qemu/driverkit"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// A CLUSTER NETWORK is the shared L2 segment that lets several VMs of one site
// form one Talos cluster. Each VM keeps its user-mode NIC for egress and host
// forwards; the cluster NIC is a second one on a QEMU multicast socket bound to
// loopback. See openspec/changes/add-vm-multinode-cluster/design.md, D1-D3.
//
// Everything in this file is decided FROM FILES, before any state directory
// exists: a refusal here costs nothing, while the same mistake found after the
// boot costs a VM, a state dir and an etcd join timeout that names none of it.

// defaultClusterNetworkName is the segment name when spec.clusterNetwork.name
// is absent.
const defaultClusterNetworkName = "cluster"

// slirpNet is QEMU user-mode networking's guest segment. Every guest's first
// NIC sits on it at 10.0.2.15, so a cluster network overlapping it would give
// the node two routes to one prefix.
var slirpNet = netip.MustParsePrefix("10.0.2.0/24")

// clusterNetwork is a parsed spec.clusterNetwork.
type clusterNetwork struct {
	Name    string
	CIDR    netip.Prefix
	Address netip.Addr
	// Group and Port are the optional overrides of the derived multicast
	// pair. Zero values mean "derive".
	Group netip.Addr
	Port  int
}

// specClusterNetwork reads and validates spec.clusterNetwork, or returns nil
// when the machine has none. Every refusal names the field and the machine.
func specClusterNetwork(m *unstructured.Unstructured) (*clusterNetwork, error) {
	raw, ok, _ := unstructured.NestedMap(m.Object, "spec", "clusterNetwork")
	if !ok {
		return nil, nil
	}

	name := m.GetName()
	cn := &clusterNetwork{Name: str(raw["name"], defaultClusterNetworkName)}

	cidrText := str(raw["cidr"], "")
	if cidrText == "" {
		return nil, fmt.Errorf("spec.clusterNetwork.cidr is required (%s)\n\n"+
			"  there is no default: every machine on the network declares the same CIDR,\n"+
			"  e.g. 10.254.0.0/24, and a disagreement is refused rather than guessed", name)
	}

	cidr, err := netip.ParsePrefix(cidrText)
	if err != nil || !cidr.Addr().Is4() {
		return nil, fmt.Errorf("spec.clusterNetwork.cidr %q is not an IPv4 CIDR (%s)\n\n"+
			"  it looks like 10.254.0.0/24", cidrText, name)
	}

	// The CANONICAL form, so two machines writing 10.254.0.0/24 and
	// 10.254.0.7/24 are not quietly treated as agreeing, and so the network
	// address handed to Flannel is the real one.
	if cidr != cidr.Masked() {
		return nil, fmt.Errorf("spec.clusterNetwork.cidr %q has host bits set (%s)\n\n"+
			"  write the segment, %s — the machine's own address goes in spec.clusterNetwork.address",
			cidrText, name, cidr.Masked())
	}

	if cidr.Bits() > 30 {
		return nil, fmt.Errorf("spec.clusterNetwork.cidr %s is too small (%s)\n\n"+
			"  a /%d has no room for a cluster; use /24, or at most /30", cidr, name, cidr.Bits())
	}

	cn.CIDR = cidr

	for _, other := range []struct {
		prefix netip.Prefix
		what   string
	}{
		{slirpNet, "QEMU's user-mode network, where every VM's first NIC lives"},
		{netip.MustParsePrefix(constants.DefaultIPv4PodCIDR), "the pod CIDR"},
		{netip.MustParsePrefix(constants.DefaultIPv4ServiceCIDR), "the service CIDR"},
	} {
		if cidr.Overlaps(other.prefix) {
			return nil, fmt.Errorf("spec.clusterNetwork.cidr %s overlaps %s, %s (%s)\n\n"+
				"  the node would have two routes into one prefix. Pick a segment outside\n"+
				"  10.0.2.0/24, %s and %s, e.g. 10.254.0.0/24",
				cidr, other.prefix, other.what, name,
				constants.DefaultIPv4PodCIDR, constants.DefaultIPv4ServiceCIDR)
		}
	}

	addrText := str(raw["address"], "")

	addr, err := netip.ParseAddr(addrText)
	if err != nil || !addr.Is4() {
		return nil, fmt.Errorf("spec.clusterNetwork.address %q is not an IPv4 address (%s)\n\n"+
			"  it is this machine's address on %s, with no prefix, e.g. %s",
			addrText, name, cidr, cidr.Addr().Next())
	}

	if !cidr.Contains(addr) {
		return nil, fmt.Errorf("spec.clusterNetwork.address %s is outside spec.clusterNetwork.cidr %s (%s)",
			addr, cidr, name)
	}

	// The network address is refused for two reasons. It is not a host, and
	// it is what Flannel is pointed at (--iface-can-reach) on every node, so
	// it must belong to no node.
	if addr == cidr.Addr() {
		return nil, fmt.Errorf("spec.clusterNetwork.address %s is the network address of %s (%s)\n\n"+
			"  the host part is all zeroes; use a host address such as %s",
			addr, cidr, name, cidr.Addr().Next())
	}

	if !cidr.Contains(addr.Next()) {
		return nil, fmt.Errorf("spec.clusterNetwork.address %s is the broadcast address of %s (%s)",
			addr, cidr, name)
	}

	cn.Address = addr

	if g := str(raw["group"], ""); g != "" {
		group, err := netip.ParseAddr(g)
		if err != nil || !netip.MustParsePrefix("239.0.0.0/8").Contains(group) {
			return nil, fmt.Errorf("spec.clusterNetwork.group %q is not in 239.0.0.0/8 (%s)\n\n"+
				"  only administratively scoped multicast is used; omit the field to derive one", g, name)
		}

		cn.Group = group
	}

	if p, ok := raw["port"]; ok {
		port := toInt(p)
		if port < 1024 || port > 65535 {
			return nil, fmt.Errorf("spec.clusterNetwork.port %v is not in 1024-65535 (%s)", p, name)
		}

		cn.Port = port
	}

	return cn, nil
}

// nodeIdentity is what the machine config needs from spec.clusterNetwork and
// spec.hostname: the node's address with the segment's prefix, the cluster
// NIC's MAC (which create() sets on the device), and the hostname.
func nodeIdentity(m *unstructured.Unstructured) (*cluster.ClusterNetwork, string, error) {
	hostname := driverkit.Str(m, "spec", "hostname")

	cn, err := specClusterNetwork(m)
	if err != nil || cn == nil {
		return nil, hostname, err
	}

	return &cluster.ClusterNetwork{
		Address:      netip.PrefixFrom(cn.Address, cn.CIDR.Bits()),
		HardwareAddr: clusterMAC(m.GetName()),
	}, hostname, nil
}

// multicast is the QEMU socket-netdev endpoint of a cluster network: the group
// and UDP port every VM on the segment joins.
//
// DERIVED from site and network name, so every machine of one segment computes
// the same pair without coordinating, and two sites on one host land on
// different segments by construction rather than by care. The overrides exist
// for the rare hash collision and for a host that filters a range.
//
// The group stays inside 239.255.0.0/16 (organization-local scope) and avoids
// .0 and .255 in the last octet. The port is in 20000-29999.
func (cn *clusterNetwork) multicast(site string) (netip.Addr, int) {
	sum := sha256.Sum256([]byte("tinq-cluster-network:" + site + "/" + cn.Name))

	group := netip.AddrFrom4([4]byte{239, 255, sum[0], 1 + sum[1]%254})
	port := 20000 + int(binary.BigEndian.Uint16(sum[2:4]))%10000

	if cn.Group.IsValid() {
		group = cn.Group
	}

	if cn.Port != 0 {
		port = cn.Port
	}

	return group, port
}

// clusterMAC derives the cluster NIC's MAC from the machine name, the way
// machineUUID derives the SMBIOS UUID: stable across destroy and up, so the
// node's link configuration can select the NIC by it. 52:54:00 is QEMU's
// locally administered prefix.
func clusterMAC(name string) string {
	sum := sha256.Sum256([]byte("tinq-cluster-mac:" + name))

	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// clusterNICArgs is the second NIC for a machine on a cluster network, or nil.
//
// localaddr=127.0.0.1 keeps the multicast on loopback: nothing leaves the host,
// and no root or CAP_NET_ADMIN is needed. Any local process can join the group
// and read or inject frames, which is acceptable on a single-user development
// host and documented in the README.
func clusterNICArgs(m *unstructured.Unstructured, cn *clusterNetwork) []string {
	if cn == nil {
		return nil
	}

	group, port := cn.multicast(driverkit.Str(m, "spec", "site"))

	return []string{
		"-netdev", fmt.Sprintf("socket,id=n1,mcast=%s:%d,localaddr=127.0.0.1", group, port),
		"-device", "virtio-net-pci,netdev=n1,mac=" + clusterMAC(m.GetName()),
	}
}

// machineRecordName is the copy of the machine file tinq keeps in each VM's
// state directory. It is how one machine learns what the OTHER machines of its
// site declared: their cluster addresses, their host forwards, and who joins
// whom. It lives and dies with the state directory, so a destroyed machine
// stops claiming anything the moment its directory is swept.
const machineRecordName = "machine.yaml"

// writeMachineRecord stores the machine's object in its state directory.
func writeMachineRecord(dir string, m *unstructured.Unstructured) error {
	b, err := yaml.Marshal(m.Object)
	if err != nil {
		return fmt.Errorf("encoding the machine record: %w", err)
	}

	return os.WriteFile(filepath.Join(dir, machineRecordName), b, 0o644)
}

// siteMachine is a recorded machine and the state directory it lives in.
type siteMachine struct {
	*unstructured.Unstructured
	Dir string
}

// siteMachines returns the recorded machines of site under root, excluding the
// state directory self. A directory without a record (a VM created before
// records existed) is skipped: it cannot claim an address it never declared.
// The result is sorted by name so every message lists machines in one order.
func siteMachines(root, site, self string) ([]siteMachine, error) {
	entries, err := os.ReadDir(filepath.Join(root, site))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	var out []siteMachine

	for _, e := range entries {
		dir := filepath.Join(root, site, e.Name())
		if !e.IsDir() || filepath.Clean(dir) == filepath.Clean(self) {
			continue
		}

		b, err := os.ReadFile(filepath.Join(dir, machineRecordName))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}

		var obj map[string]interface{}
		if err := yaml.Unmarshal(b, &obj); err != nil {
			return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, machineRecordName), err)
		}

		out = append(out, siteMachine{Unstructured: &unstructured.Unstructured{Object: obj}, Dir: dir})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })

	return out, nil
}

// checkSite refuses a machine whose declarations collide with another machine
// of its site. It reads only files, so it runs before this machine's state
// directory exists.
func (h *hvf) checkSite(m *unstructured.Unstructured) error {
	cn, err := specClusterNetwork(m)
	if err != nil {
		return err
	}

	site := driverkit.Str(m, "spec", "site")

	others, err := siteMachines(h.stateRoot, site, h.dir(m))
	if err != nil {
		return err
	}

	for _, o := range others {
		// The same machine under another UID is not "another machine": a file
		// re-read with a different identity would otherwise refuse itself.
		if o.GetName() == m.GetName() {
			continue
		}

		if err := checkForwardCollisions(m, o.Unstructured); err != nil {
			return err
		}

		if cn == nil {
			continue
		}

		ocn, err := specClusterNetwork(o.Unstructured)
		if err != nil || ocn == nil || ocn.Name != cn.Name {
			continue
		}

		if ocn.CIDR != cn.CIDR {
			return fmt.Errorf("cluster network %q of site %s: %s declares %s, but %s declares %s\n\n"+
				"  every machine on one network declares the same CIDR", cn.Name, site,
				m.GetName(), cn.CIDR, o.GetName(), ocn.CIDR)
		}

		if ocn.Address == cn.Address {
			return fmt.Errorf("cluster network %q of site %s: %s and %s both declare %s\n\n"+
				"  each machine needs its own address on the segment", cn.Name, site,
				o.GetName(), m.GetName(), cn.Address)
		}
	}

	return nil
}

// checkForwardCollisions refuses two machines that bind the same host port.
// QEMU would fail to bind the second one, but only after its state directory
// and disks exist, and its message names neither machine.
//
// A forward on 0.0.0.0 holds the port on EVERY address, so it collides with
// the same port and protocol on any address.
func checkForwardCollisions(m, other *unstructured.Unstructured) error {
	for _, a := range hostBinds(m) {
		for _, b := range hostBinds(other) {
			if a.proto != b.proto || a.port != b.port {
				continue
			}

			if a.addr == b.addr || a.addr == "0.0.0.0" || b.addr == "0.0.0.0" {
				return fmt.Errorf("host port %s %s:%d is already forwarded by %s (as %s:%d), so %s cannot bind it\n\n"+
					"  every VM of a site needs its own host ports — the host reaches each node's\n"+
					"  Talos API through its own forward. Change spec.hostForwards on %s",
					a.proto, a.addr, a.port, other.GetName(), b.addr, b.port, m.GetName(), m.GetName())
			}
		}
	}

	return nil
}

// hostBind is one socket a machine's forwards bind on the host.
type hostBind struct {
	proto, addr string
	port        int
}

// hostBinds lists the sockets the machine binds on the host, expanded the way
// create() expands hostForwards into -netdev arguments.
func hostBinds(m *unstructured.Unstructured) []hostBind {
	var out []hostBind

	for _, hf := range nestedSlice(m, "spec", "hostForwards") {
		h, _ := hf.(map[string]interface{})

		hp, gp := toInt(h["hostPort"]), toInt(h["guestPort"])
		if hp <= 0 || gp <= 0 {
			continue
		}

		addr := str(h["hostAddr"], defaultHostAddr)

		var protos []string

		switch strings.ToLower(str(h["protocol"], "tcp")) {
		case "udp":
			protos = []string{"udp"}
		case "both", "tcp+udp":
			protos = []string{"tcp", "udp"}
		default:
			protos = []string{"tcp"}
		}

		for _, p := range protos {
			out = append(out, hostBind{proto: p, addr: addr, port: hp})
		}
	}

	return out
}
