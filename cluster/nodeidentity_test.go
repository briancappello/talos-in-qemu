package cluster

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	coreconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
)

func networkedInput() ConfigInput {
	in := testInput()
	in.TalosVersion = "v1.14.1"
	in.Endpoint = "https://10.254.0.11:6443"
	in.ClusterNetwork = &ClusterNetwork{
		Address:      netip.MustParsePrefix("10.254.0.11/24"),
		HardwareAddr: "52:54:00:27:92:95",
	}

	return in
}

// loaded parses the generated config the way a node does and validates it in
// metal mode, so every assertion below is about a config Talos accepts.
func loaded(t *testing.T, in ConfigInput) []coreconfig.Document {
	t.Helper()

	cfg, err := configloader.NewFromBytes(mustGenerate(t, in).ControlPlane)
	if err != nil {
		t.Fatalf("the generated config does not load: %s", redactErr(err))
	}

	if _, err := cfg.Validate(metalMode{}); err != nil {
		t.Fatalf("the generated config does not validate: %s", redactErr(err))
	}

	return cfg.Documents()
}

func docOf[T coreconfig.Document](t *testing.T, docs []coreconfig.Document) []T {
	t.Helper()

	var out []T

	for _, d := range docs {
		if v, ok := d.(T); ok {
			out = append(out, v)
		}
	}

	return out
}

func TestClusterNetworkRendersTheLinkByMAC(t *testing.T) {
	docs := loaded(t, networkedInput())

	aliases := docOf[*network.LinkAliasConfigV1Alpha1](t, docs)
	if len(aliases) != 1 || aliases[0].MetaName != clusterLinkName {
		t.Fatalf("want one LinkAliasConfig %q, got %d", clusterLinkName, len(aliases))
	}

	if got := aliases[0].Selector.Match.String(); !strings.Contains(got, `"52:54:00:27:92:95"`) ||
		!strings.Contains(got, "link.permanent_addr") {
		t.Errorf("the alias selects %q, want the cluster NIC's permanent MAC", got)
	}

	links := docOf[*network.LinkConfigV1Alpha1](t, docs)
	if len(links) != 1 || links[0].MetaName != clusterLinkName {
		t.Fatalf("want one LinkConfig %q, got %d", clusterLinkName, len(links))
	}

	if got := links[0].LinkAddresses; len(got) != 1 || got[0].AddressAddress.String() != "10.254.0.11/24" {
		t.Errorf("the cluster link addresses are %v, want [10.254.0.11/24]", got)
	}

	// Egress stays on the user-mode NIC. A default route here points into a
	// segment with no gateway.
	if len(links[0].LinkRoutes) != 0 {
		t.Errorf("the cluster link carries routes %v, want none", links[0].LinkRoutes)
	}
}

func TestClusterNetworkPinsKubeletEtcdAndFlannel(t *testing.T) {
	docs := loaded(t, networkedInput())

	nodes := docOf[*k8s.KubeNodeConfigV1Alpha1](t, docs)
	if len(nodes) != 1 {
		t.Fatalf("want one KubeNodeConfig, got %d", len(nodes))
	}

	if got := nodes[0].NodeIPConfig.NodeIPValidSubnets; !slices.Equal(got, []string{"10.254.0.0/24"}) {
		t.Errorf("kubelet nodeIP.validSubnets = %v, want [10.254.0.0/24]", got)
	}

	// EDITED, not replaced: the labels machinery set must survive.
	if _, ok := nodes[0].LabelsConfig["node-role.kubernetes.io/control-plane"]; !ok {
		t.Errorf("the control-plane label is gone from KubeNodeConfig: %v", nodes[0].LabelsConfig)
	}

	flannel := docOf[*k8s.KubeFlannelCNIConfigV1Alpha1](t, docs)
	if len(flannel) != 1 {
		t.Fatalf("want one KubeFlannelCNIConfig, got %d", len(flannel))
	}

	if !slices.Contains(flannel[0].FlannelExtraArgs, "--iface-can-reach=10.254.0.0") {
		t.Errorf("flannel extraArgs = %v, want --iface-can-reach=10.254.0.0", flannel[0].FlannelExtraArgs)
	}

	if flannel[0].FlannelBackendType != "vxlan" {
		t.Errorf("the flannel backend is %q after the edit, want machinery's vxlan", flannel[0].FlannelBackendType)
	}

	v1 := docOf[*v1alpha1.Config](t, docs)
	if len(v1) != 1 {
		t.Fatalf("want one v1alpha1 document, got %d", len(v1))
	}

	if got := v1[0].ClusterConfig.EtcdConfig.EtcdAdvertisedSubnets; !slices.Equal(got, []string{"10.254.0.0/24"}) {
		t.Errorf("etcd advertisedSubnets = %v, want [10.254.0.0/24]", got)
	}
}

// Other nodes reach the API server at the cluster address; the host reaches it
// at the forward. Both must be in the certificates.
func TestClusterNetworkAddressIsInBothCertificates(t *testing.T) {
	docs := loaded(t, networkedInput())

	v1 := docOf[*v1alpha1.Config](t, docs)[0]
	for _, want := range []string{"127.0.0.1", "10.254.0.11"} {
		if !slices.Contains(v1.MachineConfig.MachineCertSANs, want) {
			t.Errorf("machine.certSANs = %v, missing %s", v1.MachineConfig.MachineCertSANs, want)
		}
	}

	api := docOf[*k8s.KubeAPIServerConfigV1Alpha1](t, docs)
	if len(api) != 1 {
		t.Fatalf("want one KubeAPIServerConfig, got %d", len(api))
	}

	for _, want := range []string{"127.0.0.1", "10.254.0.11"} {
		if !slices.Contains(api[0].PodCertExtraSANs, want) {
			t.Errorf("API server certExtraSANs = %v, missing %s", api[0].PodCertExtraSANs, want)
		}
	}
}

func TestHostnameIsDeclaredWithAutoOff(t *testing.T) {
	in := networkedInput()
	in.Hostname = "ci-worker-vm"

	docs := loaded(t, in)

	h := docOf[*network.HostnameConfigV1Alpha1](t, docs)
	if len(h) != 1 {
		t.Fatalf("want one HostnameConfig, got %d", len(h))
	}

	if h[0].ConfigHostname != "ci-worker-vm" || h[0].ConfigAuto == nil || *h[0].ConfigAuto != nethelpers.AutoHostnameKindOff {
		t.Errorf("HostnameConfig = {auto: %v, hostname: %q}, want {off, ci-worker-vm}", h[0].ConfigAuto, h[0].ConfigHostname)
	}
}

// A hostname alone, with no cluster network, is its own feature.
func TestHostnameWithoutAClusterNetwork(t *testing.T) {
	in := testInput()
	in.TalosVersion = "v1.14.1"
	in.Hostname = "solo"

	docs := loaded(t, in)

	if h := docOf[*network.HostnameConfigV1Alpha1](t, docs); len(h) != 1 || h[0].ConfigHostname != "solo" {
		t.Fatalf("the hostname was not rendered: %+v", h)
	}

	if len(docOf[*network.LinkConfigV1Alpha1](t, docs)) != 0 {
		t.Error("a hostname alone emitted cluster link documents")
	}
}

func TestNodeIdentityNeedsTalos114(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ConfigInput)
		want string
	}{
		{"cluster network", func(in *ConfigInput) {
			in.ClusterNetwork = networkedInput().ClusterNetwork
		}, "a cluster network needs Talos v1.14"},
		{"hostname", func(in *ConfigInput) { in.Hostname = "x" }, "a declared hostname needs Talos v1.14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput() // v1.13.7
			tc.edit(&in)

			_, err := GenerateConfig(in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GenerateConfig on v1.13.7 = %v, want a refusal naming %q", redactErr(err), tc.want)
			}
		})
	}
}
