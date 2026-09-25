package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coglative/talos-in-qemu/cluster"
	"github.com/coglative/talos-in-qemu/driverkit"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	ownerNet  = "cidr: 10.254.0.0/24\naddress: 10.254.0.11\n"
	joinerNet = "cidr: 10.254.0.0/24\naddress: 10.254.0.12\n"
	// The owner's kubeconfig server, as up writes it for a networked owner:
	// the HOST forward. A joiner that took its endpoint from here would point
	// at itself.
	hostForwardServer = "https://127.0.0.1:6443"
	inClusterEndpoint = "https://10.254.0.11:6443"
)

func testKubeconfig(t *testing.T, server string) []byte {
	t.Helper()

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: server}
	cfg.AuthInfos["a"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["x"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "a"}
	cfg.CurrentContext = "x"

	b, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// ownerArtifacts selects which of the owner's state-dir files exist.
type ownerArtifacts struct{ secrets, kubeconfig, endpoint bool }

var allArtifacts = ownerArtifacts{true, true, true}

// seedOwner records owner as brought up, with the chosen artifacts.
func seedOwner(t *testing.T, h *hvf, owner *unstructured.Unstructured, a ownerArtifacts) {
	t.Helper()

	seed(t, h, owner)
	dir := h.dir(owner)

	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if a.secrets {
		write("secrets.yaml", []byte("fake-secrets"))
	}

	if a.kubeconfig {
		write("kubeconfig", testKubeconfig(t, hostForwardServer))
	}

	if a.endpoint {
		write(cluster.ClusterEndpointArtifact, []byte(inClusterEndpoint+"\n"))
	}
}

// joiner is cp1 joining cp0, on a v1.14 ISO.
func joiner(t *testing.T, g *goldenHost, cluster string) *unstructured.Unstructured {
	t.Helper()

	fakeISO(t, filepath.Join(g.images, "talos.iso"), "TALOS_V1_14_1")

	m := netMachine(t, "cp1", 50001, cluster)
	m.Object["spec"].(map[string]interface{})["joins"] = "cp0"

	return m
}

// THE TRAP D4 names. The owner's kubeconfig says 127.0.0.1 — correct for the
// host, and inside the joining guest the guest itself. The joiner must take the
// recorded in-cluster endpoint instead.
func TestVMJoinerNeverGetsTheHostForwardAsItsEndpoint(t *testing.T) {
	g := newGoldenHost(t, "talos.iso")
	seedOwner(t, g.h, netMachine(t, "cp0", 50000, ownerNet), allArtifacts)

	opts, err := upOptions(g.h, joiner(t, g, joinerNet), driverkit.Absent, nil)
	if err != nil {
		t.Fatalf("upOptions: %v", err)
	}

	if opts.Join == nil {
		t.Fatal("spec.joins resolved no join")
	}

	if got := opts.Join.ClusterEndpoint; got != inClusterEndpoint {
		t.Errorf("the joiner's cluster endpoint is %q, want the owner's in-cluster %s", got, inClusterEndpoint)
	}

	if strings.Contains(opts.Join.ClusterEndpoint, "127.0.0.1") {
		t.Error("the joiner points at 127.0.0.1, which inside its guest is itself")
	}

	// REUSED, not minted: the kubeconfig and the secrets are the owner's.
	if string(opts.Join.SecretsBundle) != "fake-secrets" {
		t.Error("the joiner does not carry the owner's secrets bundle")
	}

	if server, _ := cluster.EndpointFromKubeconfig(opts.Join.Kubeconfig); server != hostForwardServer {
		t.Errorf("the joiner's kubeconfig server is %q, want the owner's, unchanged", server)
	}
}

// Every refusal is decided from files, and none leaves a state directory.
func TestVMJoinRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, g *goldenHost) *unstructured.Unstructured
		wanted []string
	}{
		{"joins itself", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			m := joiner(t, g, joinerNet)
			m.Object["spec"].(map[string]interface{})["joins"] = "cp1"

			return m
		}, []string{"cp1", "this machine"}},
		{"no cluster network on the joiner", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			seedOwner(t, g.h, netMachine(t, "cp0", 50000, ownerNet), allArtifacts)

			return joiner(t, g, "")
		}, []string{"cp1", "no spec.clusterNetwork"}},
		{"owner never brought up", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			return joiner(t, g, joinerNet)
		}, []string{"cp0", "bring cp0 up first"}},
		{"owner on another site", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			owner := netMachine(t, "cp0", 50000, ownerNet)
			owner.Object["spec"].(map[string]interface{})["site"] = "elsewhere"
			seedOwner(t, g.h, owner, allArtifacts)

			return joiner(t, g, joinerNet)
		}, []string{"cp0", "elsewhere", "site"}},
		{"owner without a cluster network", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			seedOwner(t, g.h, netMachine(t, "cp0", 50000, ""), allArtifacts)

			return joiner(t, g, joinerNet)
		}, []string{"cp0", "no cluster network"}},
		{"different network name", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			seedOwner(t, g.h, netMachine(t, "cp0", 50000, "name: a\n"+ownerNet), allArtifacts)

			return joiner(t, g, "name: b\n"+joinerNet)
		}, []string{"cp0", `"a"`, `"b"`}},
		{"owner without secrets", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			seedOwner(t, g.h, netMachine(t, "cp0", 50000, ownerNet), ownerArtifacts{kubeconfig: true, endpoint: true})

			return joiner(t, g, joinerNet)
		}, []string{"cp0", "secrets", "SECOND cluster"}},
		{"owner without the in-cluster endpoint", func(t *testing.T, g *goldenHost) *unstructured.Unstructured {
			seedOwner(t, g.h, netMachine(t, "cp0", 50000, ownerNet), ownerArtifacts{secrets: true, kubeconfig: true})

			return joiner(t, g, joinerNet)
		}, []string{"cp0", "in-cluster endpoint", cluster.ClusterEndpointArtifact}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGoldenHost(t, "talos.iso")
			m := tc.setup(t, g)

			_, err := upOptions(g.h, m, driverkit.Absent, nil)
			if err == nil {
				t.Fatal("upOptions accepted a join it must refuse")
			}

			for _, w := range tc.wanted {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal does not name %q:\n%v", w, err)
				}
			}

			if _, statErr := os.Stat(g.h.dir(m)); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the joiner's state directory exists after the refusal")
			}
		})
	}
}

// Hardware keeps taking its endpoint from the owner's kubeconfig, whose server
// is the owner's own LAN address. The shared resolver must not change that.
func TestBaremetalJoinStillReadsTheKubeconfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "lab", "bootstrap-default-bm0")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	for name, b := range map[string][]byte{
		"secrets.yaml": []byte("s"),
		"kubeconfig":   testKubeconfig(t, "https://192.168.1.10:6443"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := baremetalJoiner(t)

	join, err := joinOptions(&hvf{stateRoot: root}, m)
	if err != nil {
		t.Fatal(err)
	}

	if join.ClusterEndpoint != "https://192.168.1.10:6443" {
		t.Errorf("hardware join endpoint = %q, want the owner's kubeconfig server", join.ClusterEndpoint)
	}

	// And a reconfigure of that joiner regenerates against the OWNER's
	// endpoint. It used its own address, re-pointing a member at itself.
	opts, err := reconfigureOptions(&hvf{stateRoot: root}, m)
	if err != nil {
		t.Fatal(err)
	}

	if opts.KubeEndpoint != "https://192.168.1.10:6443" {
		t.Errorf("reconfigure of a hardware joiner uses %q, want the owner's https://192.168.1.10:6443",
			opts.KubeEndpoint)
	}
}

func baremetalJoiner(t *testing.T) *unstructured.Unstructured {
	t.Helper()

	path := filepath.Join(t.TempDir(), "machine.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata: {name: bm1, namespace: default}
spec:
  site: lab
  role: talos-cp
  baremetal:
    maintenanceEndpoint: 192.168.1.11
    systemDiskSerial: S1
    joins: bm0
`), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := readMachine(path)
	if err != nil {
		t.Fatal(err)
	}

	return m
}
