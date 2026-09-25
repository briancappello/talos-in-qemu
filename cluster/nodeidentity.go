package cluster

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/config"
	coreconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/types/k8s"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
)

// ClusterNetwork is this node's address on a network it shares with the other
// nodes of its cluster, when the node's default-route NIC cannot be used for
// that: under QEMU every guest's first NIC is 10.0.2.15 behind its own NAT, so
// nodes cannot reach each other there and would all report one InternalIP.
//
// nil means no such network, and the config is exactly what it was before this
// field existed.
type ClusterNetwork struct {
	// Address is the node's address WITH the segment's prefix length, e.g.
	// 10.254.0.11/24. The prefix is the segment: it is what the kubelet node
	// IP and the etcd advertised address are picked from.
	Address netip.Prefix
	// HardwareAddr is the MAC of the NIC on that segment. The link is selected
	// by it, never by name, for the same reason Network.HardwareAddr is.
	HardwareAddr string
}

// clusterLinkName is the Talos-side alias of the cluster NIC. It names the
// link inside the machine config only; nothing outside Talos relies on it.
const clusterLinkName = "cluster0"

// errNeedsDocumentModel refuses a node identity feature on an image whose
// Talos version predates the document model it is written in.
func errNeedsDocumentModel(field, version string) error {
	return fmt.Errorf("%s needs Talos v1.14 or later, and this image is %s\n\n"+
		"  it is written as Talos 1.14 config documents (LinkConfig, KubeNodeConfig,\n"+
		"  HostnameConfig, KubeFlannelCNIConfig), which an older node cannot read.\n"+
		"  Boot a v1.14 ISO, e.g. talos-v1.14.1-amd64.iso", field, version)
}

// CheckNodeIdentity refuses a cluster network or hostname for an image whose
// Talos version cannot carry them. GenerateConfig refuses the same thing; this
// is for callers that know the version BEFORE they boot anything, so the
// refusal costs no VM. An empty version is left to the step that refuses it.
func CheckNodeIdentity(talosVersion string, cn *ClusterNetwork, hostname string) error {
	if talosVersion == "" || (cn == nil && hostname == "") {
		return nil
	}

	contract, err := config.ParseContractFromVersion(talosVersion)
	if err != nil {
		return fmt.Errorf("parsing Talos version %q: %w", talosVersion, err)
	}

	return checkNodeIdentity(ConfigInput{TalosVersion: talosVersion, ClusterNetwork: cn, Hostname: hostname}, contract)
}

// checkNodeIdentity refuses the identity fields on a contract that cannot
// carry them. Called before generation, so the refusal names the field.
func checkNodeIdentity(in ConfigInput, contract *config.VersionContract) error {
	if contract.Greater(config.TalosVersion1_13) {
		return nil
	}

	if in.ClusterNetwork != nil {
		return errNeedsDocumentModel("a cluster network", in.TalosVersion)
	}

	if in.Hostname != "" {
		return errNeedsDocumentModel("a declared hostname", in.TalosVersion)
	}

	return nil
}

// withNodeIdentity applies the cluster network and the hostname to a generated
// config. It returns cfg unchanged when neither is set.
//
// The generated KubeNodeConfig, KubeFlannelCNIConfig and HostnameConfig are
// EDITED, never replaced: machinery put labels, a backend and defaults in
// them, and a replacement would silently drop all of that. The link documents
// are new, because machinery emits none for a DHCP node.
func withNodeIdentity(cfg config.Provider, in ConfigInput) (config.Provider, error) {
	if in.ClusterNetwork == nil && in.Hostname == "" {
		return cfg, nil
	}

	docs := cfg.Documents()
	out := make([]coreconfig.Document, 0, len(docs)+2)

	var sawNode, sawFlannel, sawHostname bool

	for _, doc := range docs {
		switch d := doc.(type) {
		case *k8s.KubeNodeConfigV1Alpha1:
			sawNode = true

			if in.ClusterNetwork != nil {
				d = d.Clone().(*k8s.KubeNodeConfigV1Alpha1) //nolint:forcetypeassert
				// Every guest's first NIC is 10.0.2.15. Without this each
				// node picks it and all of them report ONE InternalIP.
				d.NodeIPConfig.NodeIPValidSubnets = []string{in.ClusterNetwork.Address.Masked().String()}
				doc = d
			}
		case *k8s.KubeFlannelCNIConfigV1Alpha1:
			sawFlannel = true

			if in.ClusterNetwork != nil {
				d = d.Clone().(*k8s.KubeFlannelCNIConfigV1Alpha1) //nolint:forcetypeassert
				// ONE argument for the whole cluster: Flannel runs as one
				// DaemonSet, so this cannot name a per-node address, and
				// "can reach my own address" resolves to lo. The segment's
				// network address is the same on every node, belongs to no
				// node, and routes out of the cluster NIC everywhere.
				d.FlannelExtraArgs = append(d.FlannelExtraArgs,
					"--iface-can-reach="+in.ClusterNetwork.Address.Masked().Addr().String())
				doc = d
			}
		case *network.HostnameConfigV1Alpha1:
			sawHostname = true

			if in.Hostname != "" {
				d = d.Clone().(*network.HostnameConfigV1Alpha1) //nolint:forcetypeassert
				// "stable" is stable across reboots, not across destroy and
				// up, and it is not a name anyone chose. Talos refuses a
				// hostname beside any auto mode but off.
				d.ConfigAuto = new(nethelpers.AutoHostnameKindOff)
				d.ConfigHostname = in.Hostname
				doc = d
			}
		}

		out = append(out, doc)
	}

	if in.ClusterNetwork != nil && (!sawNode || !sawFlannel) {
		return nil, errors.New("a cluster network needs the generated KubeNodeConfig and " +
			"KubeFlannelCNIConfig documents, and this config has no " +
			missing(!sawNode, "KubeNodeConfig", !sawFlannel, "KubeFlannelCNIConfig") +
			"\n\n  a config patch that removes them, or a CNI other than Flannel, is not supported " +
			"with spec.clusterNetwork")
	}

	if in.Hostname != "" && !sawHostname {
		return nil, errors.New("a declared hostname needs the generated HostnameConfig document, " +
			"and this config has none")
	}

	if in.ClusterNetwork != nil {
		links, err := clusterLinkDocuments(in.ClusterNetwork)
		if err != nil {
			return nil, err
		}

		out = append(out, links...)
	}

	next, err := container.New(out...)
	if err != nil {
		return nil, fmt.Errorf("adding the node identity documents: %w", err)
	}

	return next, nil
}

// clusterLinkDocuments aliases the cluster NIC by MAC and gives it the static
// address. No route: egress stays on the user-mode NIC, and a default route
// here would send it into a segment with no gateway.
func clusterLinkDocuments(cn *ClusterNetwork) ([]coreconfig.Document, error) {
	match, err := cel.ParseBooleanExpression(
		fmt.Sprintf("mac(link.permanent_addr) == %q", cn.HardwareAddr), celenv.LinkLocator())
	if err != nil {
		return nil, fmt.Errorf("building the cluster NIC selector for %s: %w", cn.HardwareAddr, err)
	}

	alias := network.NewLinkAliasConfigV1Alpha1(clusterLinkName)
	alias.Selector.Match = match

	link := network.NewLinkConfigV1Alpha1(clusterLinkName)
	link.LinkUp = new(true)
	link.LinkAddresses = []network.AddressConfig{{AddressAddress: cn.Address}}

	return []coreconfig.Document{alias, link}, nil
}

// missing names the absent documents, joined with "or".
func missing(a bool, aName string, b bool, bName string) string {
	switch {
	case a && b:
		return aName + " or " + bName
	case a:
		return aName
	default:
		return bName
	}
}
