package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/coglative/talos-in-qemu/cluster"
	"github.com/coglative/talos-in-qemu/driverkit"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ONE JOIN RESOLVER, TWO SUBSTRATES. Hardware names its cluster with
// spec.baremetal.joins and a VM with spec.joins, and both end in the same
// guarantee: the joining node is configured from the owner's secrets bundle,
// points at the owner's endpoint and reuses the owner's kubeconfig. It never
// mints a PKI of its own, because a node with fresh secrets does not fail — it
// quietly builds a second cluster.
//
// The two differ in exactly one place, where the ENDPOINT comes from:
//
//   - hardware reads it from the owner's kubeconfig, whose server is the
//     owner's own address, reachable from anywhere on the LAN;
//   - a VM reads the owner's cluster-endpoint artifact. Its kubeconfig server
//     is the HOST's forward, 127.0.0.1, which inside a joining guest is the
//     guest itself — a joiner pointed there never finds the cluster.

// joinArtifacts reads what a join needs from the owner's state directory.
// field is the manifest field that named the owner, for the messages; vm
// selects where the endpoint comes from (see above).
func joinArtifacts(field, target, dir string, vm bool) (*cluster.JoinOptions, error) {
	secrets, err := os.ReadFile(filepath.Join(dir, "secrets.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%s names %q, but its cluster secrets are not "+
			"readable at %s: %w\n\n"+
			"  that file is the cluster: its CAs and machine token are what make this node's\n"+
			"  certificates trusted by its peers. Without it a join is not possible, and\n"+
			"  generating fresh secrets would silently build a SECOND cluster instead.\n\n"+
			"  bring %s up first, or restore its state directory", field, target, dir, err, target)
	}

	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err != nil {
		return nil, fmt.Errorf("%s names %q, but its kubeconfig is not "+
			"readable at %s: %w\n\n"+
			"  it is the cluster's admin credential, reused by this node rather than minted,\n"+
			"  and what this run's readiness check dials. bring %s up first",
			field, target, dir, err, target)
	}

	var endpoint string

	if vm {
		if endpoint, err = cluster.ReadClusterEndpoint(dir); err != nil {
			return nil, fmt.Errorf("%s names %q, but its in-cluster endpoint is not recorded: %w",
				field, target, err)
		}
	} else if endpoint, err = cluster.EndpointFromKubeconfig(kubeconfig); err != nil {
		return nil, fmt.Errorf("reading the API endpoint from %s's kubeconfig: %w", target, err)
	}

	return &cluster.JoinOptions{
		SecretsBundle:   secrets,
		ClusterEndpoint: endpoint,
		Kubeconfig:      kubeconfig,
	}, nil
}

// errJoinsItself refuses a machine naming itself as the cluster to join. Left
// unchecked it resolves to this machine's own state directory, which on a
// first run is empty and reads as "the owner has no secrets".
func errJoinsItself(field, target string) error {
	return fmt.Errorf("%s names %q, which is this machine\n\n"+
		"  a machine cannot join the cluster it is creating; drop the field to create one",
		field, target)
}

// vmJoinOptions resolves spec.joins for a VM, or nil when the field is absent.
//
// Every refusal is decided from files before anything is created: the joiner's
// own declaration, the owner's machine record, and the owner's artifacts.
func vmJoinOptions(h *hvf, m *unstructured.Unstructured) (*cluster.JoinOptions, error) {
	const field = "spec.joins"

	target := driverkit.Str(m, "spec", "joins")
	if target == "" {
		return nil, nil
	}

	if target == m.GetName() {
		return nil, errJoinsItself(field, target)
	}

	cn, err := specClusterNetwork(m)
	if err != nil {
		return nil, err
	}

	if cn == nil {
		return nil, fmt.Errorf("%s names %q, but %s declares no spec.clusterNetwork\n\n"+
			"  a joining VM reaches its owner over the site's cluster network. Without one it\n"+
			"  sits behind its own NAT and the join ends in an etcd timeout",
			field, target, m.GetName())
	}

	owner, err := findOwner(h, m, target)
	if err != nil {
		return nil, err
	}

	ocn, err := specClusterNetwork(owner.Unstructured)
	if err != nil {
		return nil, fmt.Errorf("%s names %q, whose recorded spec.clusterNetwork is invalid: %w",
			field, target, err)
	}

	switch {
	case ocn == nil:
		return nil, fmt.Errorf("%s names %q, which has no cluster network\n\n"+
			"  its API is reachable only through its host forward, which no other VM can use.\n"+
			"  give %s a spec.clusterNetwork and bring it up again", field, target, target)
	case ocn.Name != cn.Name:
		return nil, fmt.Errorf("%s names %q, which is on cluster network %q, but %s is on %q\n\n"+
			"  they are different segments, so the two nodes cannot reach each other",
			field, target, ocn.Name, m.GetName(), cn.Name)
	case ocn.CIDR != cn.CIDR:
		return nil, fmt.Errorf("%s names %q, whose cluster network %q is %s, but %s declares %s",
			field, target, ocn.Name, ocn.CIDR, m.GetName(), cn.CIDR)
	}

	return joinArtifacts(field, target, owner.Dir, true)
}

// findOwner finds the recorded machine named target on m's site. A machine of
// that name on ANOTHER site is refused by name, because "no such machine" would
// send the operator looking for a typo that is not there.
func findOwner(h *hvf, m *unstructured.Unstructured, target string) (siteMachine, error) {
	site := driverkit.Str(m, "spec", "site")

	machines, err := siteMachines(h.stateRoot, site, h.dir(m))
	if err != nil {
		return siteMachine{}, err
	}

	for _, o := range machines {
		if o.GetName() == target && o.GetNamespace() == m.GetNamespace() {
			return o, nil
		}
	}

	sites, _ := os.ReadDir(h.stateRoot)
	for _, s := range sites {
		if !s.IsDir() || s.Name() == site {
			continue
		}

		others, err := siteMachines(h.stateRoot, s.Name(), "")
		if err != nil {
			continue
		}

		for _, o := range others {
			if o.GetName() == target && o.GetNamespace() == m.GetNamespace() {
				return siteMachine{}, fmt.Errorf("spec.joins names %q, which is on site %q, but %s is on %q\n\n"+
					"  a cluster's nodes share a site: the state root is keyed by it, and the\n"+
					"  cluster network of one site cannot reach another's", target, s.Name(),
					m.GetName(), site)
			}
		}
	}

	return siteMachine{}, fmt.Errorf("spec.joins names %q, but no VM of that name has been brought up on "+
		"site %q\n\n  bring %s up first: its state directory holds the cluster's secrets, "+
		"endpoint and kubeconfig", target, site, target)
}
