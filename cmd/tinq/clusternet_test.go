package main

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// netMachine is a VM of site s forwarding hostPort to the Talos API. cluster,
// when non-empty, is the body of spec.clusterNetwork.
func netMachine(t *testing.T, name string, hostPort int, cluster string) *unstructured.Unstructured {
	t.Helper()

	doc := `apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata:
  name: ` + name + `
  namespace: default
spec:
  site: s
  role: talos-cp
  image: talos.iso
  cpu: 2
  memory: 2Gi
  disk: 64Mi
  hostForwards:
    - hostPort: ` + itoa(hostPort) + `
      guestPort: 50000
`
	if cluster != "" {
		doc += "  clusterNetwork:\n" + indent(cluster, "    ")
	}

	var obj map[string]interface{}
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		t.Fatalf("fixture: %v\n%s", err, doc)
	}

	m := &unstructured.Unstructured{Object: obj}
	m.SetUID(types.UID("bootstrap-default-" + name))

	return m
}

func itoa(i int) string {
	b, _ := yaml.Marshal(i)
	return strings.TrimSpace(string(b))
}

func indent(s, prefix string) string {
	var out strings.Builder

	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out.WriteString(prefix + l + "\n")
	}

	return out.String()
}

// seed records other as an existing machine of the site, as create() would.
func seed(t *testing.T, h *hvf, other *unstructured.Unstructured) {
	t.Helper()

	dir := h.dir(other)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writeMachineRecord(dir, other); err != nil {
		t.Fatal(err)
	}
}

// refusedBeforeState asserts create() refused m with every fragment in want,
// and that m's state directory does not exist afterwards.
func refusedBeforeState(t *testing.T, h *hvf, m *unstructured.Unstructured, want ...string) {
	t.Helper()

	dir := h.dir(m)

	_, err := h.create(m, dir)
	if err == nil {
		t.Fatal("create accepted a machine it must refuse")
	}

	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal does not name %q:\n%v", w, err)
		}
	}

	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the state directory %s exists after a refusal (stat: %v) — the check must run first", dir, statErr)
	}
}

func TestClusterNetworkRefusalsFromTheFileAlone(t *testing.T) {
	requireQEMUImg(t)

	for _, tc := range []struct {
		name, cluster string
		want          []string
	}{
		{"missing cidr", "address: 10.254.0.11\n", []string{"cidr is required", "no default"}},
		{"address outside the cidr", "cidr: 10.254.0.0/24\naddress: 10.254.1.11\n",
			[]string{"10.254.1.11", "outside", "10.254.0.0/24"}},
		{"cidr overlaps the user-mode network", "cidr: 10.0.0.0/16\naddress: 10.0.9.11\n",
			[]string{"10.0.2.0/24", "user-mode"}},
		{"cidr overlaps the pod cidr", "cidr: 10.244.5.0/24\naddress: 10.244.5.11\n",
			[]string{"10.244.0.0/16", "pod CIDR"}},
		{"cidr overlaps the service cidr", "cidr: 10.100.0.0/24\naddress: 10.100.0.11\n",
			[]string{"10.96.0.0/12", "service CIDR"}},
		{"cidr with host bits", "cidr: 10.254.0.7/24\naddress: 10.254.0.11\n",
			[]string{"host bits", "10.254.0.0/24"}},
		{"the network address", "cidr: 10.254.0.0/24\naddress: 10.254.0.0\n", []string{"network address"}},
		{"the broadcast address", "cidr: 10.254.0.0/24\naddress: 10.254.0.255\n", []string{"broadcast"}},
		{"a group outside 239/8", "cidr: 10.254.0.0/24\naddress: 10.254.0.11\ngroup: 224.0.0.1\n",
			[]string{"239.0.0.0/8"}},
		{"a port below 1024", "cidr: 10.254.0.0/24\naddress: 10.254.0.11\nport: 80\n", []string{"1024-65535"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGoldenHost(t, "talos.iso")
			refusedBeforeState(t, g.h, netMachine(t, "cp1", 50001, tc.cluster), tc.want...)
		})
	}
}

func TestClusterNetworkRefusesADuplicateAddress(t *testing.T) {
	requireQEMUImg(t)

	g := newGoldenHost(t, "talos.iso")
	seed(t, g.h, netMachine(t, "cp0", 50000, "cidr: 10.254.0.0/24\naddress: 10.254.0.11\n"))

	refusedBeforeState(t, g.h, netMachine(t, "cp1", 50001, "cidr: 10.254.0.0/24\naddress: 10.254.0.11\n"),
		"cp0", "cp1", "10.254.0.11")
}

func TestClusterNetworkRefusesACIDRDisagreement(t *testing.T) {
	requireQEMUImg(t)

	g := newGoldenHost(t, "talos.iso")
	seed(t, g.h, netMachine(t, "cp0", 50000, "cidr: 10.254.0.0/24\naddress: 10.254.0.11\n"))

	refusedBeforeState(t, g.h, netMachine(t, "cp1", 50001, "cidr: 10.254.0.0/16\naddress: 10.254.0.12\n"),
		"cp0", "10.254.0.0/24", "cp1", "10.254.0.0/16")
}

// Two networks with different names are different segments: the same address
// on each is not a collision.
func TestClusterNetworkAddressesAreScopedByNetworkName(t *testing.T) {
	requireQEMUImg(t)

	g := newGoldenHost(t, "talos.iso")
	seed(t, g.h, netMachine(t, "cp0", 50000, "name: a\ncidr: 10.254.0.0/24\naddress: 10.254.0.11\n"))

	m := netMachine(t, "cp1", 50001, "name: b\ncidr: 10.254.0.0/24\naddress: 10.254.0.11\n")
	if _, err := g.h.create(m, g.h.dir(m)); err != nil {
		t.Fatalf("a machine on another network was refused: %v", err)
	}
}

func TestCreateWritesTheMachineRecord(t *testing.T) {
	requireQEMUImg(t)

	g := newGoldenHost(t, "talos.iso")
	m := netMachine(t, "cp0", 50000, "cidr: 10.254.0.0/24\naddress: 10.254.0.11\n")

	if _, err := g.h.create(m, g.h.dir(m)); err != nil {
		t.Fatal(err)
	}

	others, err := siteMachines(g.h.stateRoot, "s", "")
	if err != nil {
		t.Fatal(err)
	}

	if len(others) != 1 || others[0].GetName() != "cp0" {
		t.Fatalf("siteMachines = %v, want the record create() wrote for cp0", others)
	}

	cn, err := specClusterNetwork(others[0])
	if err != nil || cn == nil || cn.Address.String() != "10.254.0.11" {
		t.Fatalf("the record does not carry the cluster network: %+v, %v", cn, err)
	}
}

func TestHostForwardCollisionsAcrossTheSite(t *testing.T) {
	requireQEMUImg(t)

	t.Run("same port", func(t *testing.T) {
		g := newGoldenHost(t, "talos.iso")
		seed(t, g.h, netMachine(t, "cp0", 50000, ""))
		refusedBeforeState(t, g.h, netMachine(t, "cp1", 50000, ""), "50000", "cp0", "cp1")
	})

	t.Run("0.0.0.0 holds the port on every address", func(t *testing.T) {
		g := newGoldenHost(t, "talos.iso")
		wild := netMachine(t, "cp0", 50000, "")
		wild.Object["spec"].(map[string]interface{})["hostForwards"].([]interface{})[0].(map[string]interface{})["hostAddr"] = "0.0.0.0"
		seed(t, g.h, wild)
		refusedBeforeState(t, g.h, netMachine(t, "cp1", 50000, ""), "50000", "cp0", "0.0.0.0")
	})

	t.Run("different ports are fine", func(t *testing.T) {
		g := newGoldenHost(t, "talos.iso")
		seed(t, g.h, netMachine(t, "cp0", 50000, ""))

		m := netMachine(t, "cp1", 50001, "")
		if _, err := g.h.create(m, g.h.dir(m)); err != nil {
			t.Fatalf("distinct ports were refused: %v", err)
		}
	})

	t.Run("another site is not checked", func(t *testing.T) {
		g := newGoldenHost(t, "talos.iso")
		other := netMachine(t, "cp0", 50000, "")
		other.Object["spec"].(map[string]interface{})["site"] = "elsewhere"
		seed(t, g.h, other)

		m := netMachine(t, "cp1", 50000, "")
		if _, err := g.h.create(m, g.h.dir(m)); err != nil {
			t.Fatalf("a machine of another site was treated as a neighbour: %v", err)
		}
	})
}

// A re-run of the same machine must not refuse itself, even when its record
// sits in its own directory.
func TestCheckSiteIgnoresTheMachineItself(t *testing.T) {
	requireQEMUImg(t)

	g := newGoldenHost(t, "talos.iso")
	m := netMachine(t, "cp0", 50000, "cidr: 10.254.0.0/24\naddress: 10.254.0.11\n")

	for i := range 2 {
		if _, err := g.h.create(m, g.h.dir(m)); err != nil {
			t.Fatalf("create #%d: %v", i+1, err)
		}
	}

	if _, err := os.Stat(filepath.Join(g.h.dir(m), machineRecordName)); err != nil {
		t.Fatal(err)
	}
}

// The derivations are PINNED. A refactor that changes them moves every
// segment and every NIC: running VMs land on a new group and cannot hear the
// ones started before, and the link selector in each node's config names a MAC
// the NIC no longer has.
func TestClusterNetworkDerivationsArePinned(t *testing.T) {
	cn := &clusterNetwork{Name: "cluster"}

	group, port := cn.multicast("homelab")
	if group.String() != "239.255.72.12" || port != 26707 {
		t.Errorf("multicast(homelab/cluster) = %s:%d, want 239.255.72.12:26707", group, port)
	}

	if got := clusterMAC("cp0"); got != "52:54:00:27:92:95" {
		t.Errorf("clusterMAC(cp0) = %s, want 52:54:00:27:92:95", got)
	}

	otherSite, _ := cn.multicast("other")
	otherName, _ := (&clusterNetwork{Name: "storage"}).multicast("homelab")

	if otherSite == group || otherName == group {
		t.Errorf("different sites or names share group %s — two segments would merge", group)
	}

	if clusterMAC("cp1") == clusterMAC("cp0") {
		t.Error("two machine names derive the same MAC")
	}

	if !netipPrefix("239.255.0.0/16").Contains(group) || port < 20000 || port > 29999 {
		t.Errorf("derived %s:%d is outside 239.255.0.0/16 or 20000-29999", group, port)
	}
}

func TestClusterNetworkOverridesWin(t *testing.T) {
	cn := &clusterNetwork{Name: "cluster", Group: netipAddr("239.1.2.3"), Port: 31000}

	if group, port := cn.multicast("homelab"); group.String() != "239.1.2.3" || port != 31000 {
		t.Errorf("overrides ignored: got %s:%d", group, port)
	}
}

// The cluster NIC is APPENDED: the argv of a machine with a cluster network is
// the plain argv plus exactly these four elements, at the end.
func TestCreateAddsTheClusterNIC(t *testing.T) {
	requireQEMUImg(t)

	plain := newGoldenHost(t, "talos.iso")
	pm := netMachine(t, "cp0", 50000, "")

	if _, err := plain.h.create(pm, plain.h.dir(pm)); err != nil {
		t.Fatal(err)
	}

	g := newGoldenHost(t, "talos.iso")
	m := netMachine(t, "cp0", 50000, "cidr: 10.254.0.0/24\naddress: 10.254.0.11\n")

	if _, err := g.h.create(m, g.h.dir(m)); err != nil {
		t.Fatal(err)
	}

	base := strings.Split(plain.scrub(strings.Join(plain.argv(), "\n")), "\n")
	got := strings.Split(g.scrub(strings.Join(g.argv(), "\n")), "\n")

	group, port := (&clusterNetwork{Name: "cluster"}).multicast("s")
	want := append(base,
		"-netdev", "socket,id=n1,mcast="+group.String()+":"+itoa(port)+",localaddr=127.0.0.1",
		"-device", "virtio-net-pci,netdev=n1,mac="+clusterMAC("cp0"))

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("argv with a cluster network:\n got: %q\nwant: %q", got, want)
	}
}

func netipPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func netipAddr(s string) netip.Addr { return netip.MustParseAddr(s) }
