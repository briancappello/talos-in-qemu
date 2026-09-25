package cluster

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// kubeconfigFor is a minimal, real kubeconfig pointing at server, the way
// Talos renders one: against the control-plane endpoint.
func kubeconfigFor(t *testing.T, server string) []byte {
	t.Helper()

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: []byte("ca")}
	cfg.AuthInfos["a"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["x"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "a"}
	cfg.CurrentContext = "x"

	b, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func testClusterNetwork() *ClusterNetwork {
	return &ClusterNetwork{
		Address:            netip.MustParsePrefix("10.254.0.11/24"),
		HardwareAddr:       "52:54:00:27:92:95",
		EgressHardwareAddr: "52:54:00:0e:0e:0e",
	}
}

// networkedFixture is an owner on a cluster network, whose node renders its
// kubeconfig against the in-cluster endpoint, as Talos does.
func networkedFixture(t *testing.T) *upFixture {
	t.Helper()

	f := newFixture(t)
	f.opts.TalosVersion = "v1.14.1"
	f.opts.ClusterNetwork = testClusterNetwork()
	f.opts.hooks.kubeconfig = func(_ context.Context, talosconfig []byte, endpoint string) ([]byte, error) {
		if err := f.rec.at("kubeconfig", endpoint, talosconfig); err != nil {
			return nil, err
		}

		return kubeconfigFor(t, "https://10.254.0.11:6443"), nil
	}

	return f
}

func TestNetworkedOwnerEndpointIsItsClusterAddress(t *testing.T) {
	f := networkedFixture(t)
	f.mustRun(t)

	if got := f.rec.input.Endpoint; got != "https://10.254.0.11:6443" {
		t.Errorf("control-plane endpoint = %q, want https://10.254.0.11:6443 — the host forward "+
			"is 127.0.0.1 inside every guest, so no other node could reach it", got)
	}

	// The HOST still dials the forward, so the certificate must name it.
	if !slices.Contains(f.rec.input.ExtraSubjectAltNames, "127.0.0.1") {
		t.Errorf("ExtraSubjectAltNames = %v, want the host forward 127.0.0.1", f.rec.input.ExtraSubjectAltNames)
	}
}

func TestNetworkedOwnerKubeconfigKeepsTheHostForward(t *testing.T) {
	f := networkedFixture(t)
	f.mustRun(t)

	b, err := os.ReadFile(filepath.Join(f.dir, "kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}

	server, err := EndpointFromKubeconfig(b)
	if err != nil {
		t.Fatal(err)
	}

	if server != "https://127.0.0.1:6443" {
		t.Errorf("the written kubeconfig's server is %q, want the host forward https://127.0.0.1:6443 — "+
			"the host cannot reach the cluster network", server)
	}
}

func TestNetworkedNodeWritesTheInClusterEndpoint(t *testing.T) {
	f := networkedFixture(t)
	f.mustRun(t)

	got, err := ReadClusterEndpoint(f.dir)
	if err != nil {
		t.Fatal(err)
	}

	if got != "https://10.254.0.11:6443" {
		t.Errorf("%s = %q, want https://10.254.0.11:6443", ClusterEndpointArtifact, got)
	}
}

// Without a cluster network nothing changes: the endpoint is the forward, no
// extra SAN, no endpoint artifact, and the kubeconfig is written as fetched.
func TestPlainOwnerIsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.mustRun(t)

	if got := f.rec.input.Endpoint; got != "https://127.0.0.1:6443" {
		t.Errorf("endpoint = %q, want the host forward", got)
	}

	if f.rec.input.ExtraSubjectAltNames != nil {
		t.Errorf("ExtraSubjectAltNames = %v, want none", f.rec.input.ExtraSubjectAltNames)
	}

	if _, err := os.Stat(filepath.Join(f.dir, ClusterEndpointArtifact)); err == nil {
		t.Errorf("a node with no cluster network wrote %s", ClusterEndpointArtifact)
	}

	b, _ := os.ReadFile(filepath.Join(f.dir, "kubeconfig"))
	if string(b) != fakeKubeconfig {
		t.Error("the kubeconfig of a node with no cluster network was rewritten")
	}
}

// A networked JOINER points at the owner's endpoint, writes it as its own
// artifact (so it can be joined in turn), and is waited for at its CLUSTER
// address — the forward it is dialled through is no node's InternalIP.
func TestNetworkedJoinerUsesTheOwnerAndWaitsAtItsClusterAddress(t *testing.T) {
	f := newFixture(t)
	f.opts.TalosVersion = "v1.14.1"
	f.opts.ClusterNetwork = &ClusterNetwork{
		Address:      netip.MustParsePrefix("10.254.0.12/24"),
		HardwareAddr: "52:54:00:a2:d4:e1",
	}
	f.opts.TalosEndpoint = "127.0.0.1:50001"
	f.opts.KubeEndpoint = "https://127.0.0.1:6444"
	f.opts.Join = &JoinOptions{
		SecretsBundle:   []byte(fakeSecrets),
		ClusterEndpoint: "https://10.254.0.11:6443",
		Kubeconfig:      kubeconfigFor(t, "https://127.0.0.1:6443"),
	}

	f.mustRun(t)

	if got := f.rec.input.Endpoint; got != "https://10.254.0.11:6443" {
		t.Errorf("joiner endpoint = %q, want the owner's https://10.254.0.11:6443", got)
	}

	if got := f.rec.endpoint["waitNodeReadyAt"]; got != "10.254.0.12" {
		t.Errorf("the joiner is waited for at %q, want its cluster address 10.254.0.12", got)
	}

	if got, _ := ReadClusterEndpoint(f.dir); got != "https://10.254.0.11:6443" {
		t.Errorf("the joiner's %s = %q, want the cluster's", ClusterEndpointArtifact, got)
	}

	if f.rec.did("bootstrap") {
		t.Error("a joiner bootstrapped etcd")
	}
}

func TestKubeconfigWithServerKeepsTheCredentials(t *testing.T) {
	in := kubeconfigFor(t, "https://10.254.0.11:6443")

	out, err := KubeconfigWithServer(in, "https://127.0.0.1:6443")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.Clusters["c"].Server; got != "https://127.0.0.1:6443" {
		t.Errorf("server = %q", got)
	}

	if string(cfg.Clusters["c"].CertificateAuthorityData) != "ca" || cfg.AuthInfos["a"].Token != "t" {
		t.Error("the rewrite lost the CA or the credential")
	}
}

func TestReadClusterEndpointRefusesGarbage(t *testing.T) {
	dir := t.TempDir()

	if _, err := ReadClusterEndpoint(dir); err == nil || !strings.Contains(err.Error(), ClusterEndpointArtifact) {
		t.Errorf("a missing artifact = %v, want a refusal naming it", err)
	}

	if err := os.WriteFile(filepath.Join(dir, ClusterEndpointArtifact), []byte("127.0.0.1:6443\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadClusterEndpoint(dir); err == nil {
		t.Error("an endpoint with no scheme was accepted")
	}
}

// Reconfigure must regenerate the endpoint the node was INSTALLED with. For a
// joiner that is the owner's address, which its own manifest does not name.
func TestReconfigureRegeneratesTheInstalledEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ClusterEndpointArtifact),
		[]byte("https://10.254.0.11:6443\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	in, err := reconfigureInput(ReconfigureOptions{
		StateDir:       dir,
		KubeEndpoint:   "https://127.0.0.1:6444",
		APIAddress:     "127.0.0.1",
		ClusterNetwork: testClusterNetwork(),
	}, "v1.14.1", nil)
	if err != nil {
		t.Fatal(err)
	}

	if in.Endpoint != "https://10.254.0.11:6443" {
		t.Errorf("reconfigure endpoint = %q, want the recorded https://10.254.0.11:6443", in.Endpoint)
	}

	if !slices.Contains(in.ExtraSubjectAltNames, "127.0.0.1") {
		t.Errorf("reconfigure dropped the host-forward SAN: %v", in.ExtraSubjectAltNames)
	}

	// No record, no guess.
	if _, err := reconfigureInput(ReconfigureOptions{StateDir: t.TempDir(), ClusterNetwork: testClusterNetwork()},
		"v1.14.1", nil); err == nil {
		t.Error("a networked node with no recorded endpoint was reconfigured anyway")
	}
}
