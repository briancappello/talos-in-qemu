package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coglative/talos-in-qemu/cluster"
	"github.com/coglative/talos-in-qemu/driverkit"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DESTROY ON A CLUSTER OF SEVERAL VMS. `destroy` of a single VM takes the
// process and the state directory, and nothing else is affected. A member of a
// multi-node cluster is different in two ways, and both are refused or handled
// BEFORE anything is taken:
//
//   - A JOINER leaves etcd first. Left in the member list, a destroyed member
//     is a vote the cluster can never collect again: a two-member cluster
//     loses quorum the moment the second member vanishes.
//   - An OWNER holds the state every joiner was built from, and a joiner's
//     reconfigure and a new node's join both read it. Destroying it while
//     joiners exist is refused unless the caller says what to do with them.

// destroyOptions are the destroy verb's flags.
type destroyOptions struct {
	// withJoiners destroys the owner's joiners first, each leaving etcd.
	withJoiners bool
	// force destroys regardless: an owner with joiners (they are orphaned),
	// or a joiner whose etcd member cannot be removed.
	force bool
}

// memberOps are the cluster calls destroy makes. nil fields mean the real
// ones; tests substitute them because the real ones need a running cluster.
type memberOps struct {
	leave      func(ctx context.Context, talosconfig []byte, endpoint string) error
	remove     func(ctx context.Context, talosconfig []byte, via, peerAddr string) error
	deleteNode func(ctx context.Context, kubeconfig []byte, addr string) error
}

func (h *hvf) ops() memberOps {
	o := memberOps{leave: cluster.LeaveEtcd, remove: cluster.RemoveEtcdMember, deleteNode: cluster.DeleteNodeAt}

	if h.members != nil {
		if h.members.leave != nil {
			o.leave = h.members.leave
		}

		if h.members.remove != nil {
			o.remove = h.members.remove
		}

		if h.members.deleteNode != nil {
			o.deleteNode = h.members.deleteNode
		}
	}

	return o
}

// destroyMachine is the destroy verb: the membership rules above, then the
// ordinary destroy of the machine in the file.
func destroyMachine(ctx context.Context, d *hvf, path string, opts destroyOptions) error {
	m, err := readMachine(path)
	if err != nil {
		return err
	}

	// Hardware and plain VMs take the ordinary path, unchanged.
	if isBaremetal(m) {
		return standalone(ctx, d, path, "destroy")
	}

	joiners, err := d.joinersOf(m)
	if err != nil {
		return err
	}

	if len(joiners) > 0 {
		switch {
		case opts.withJoiners:
			for _, j := range joiners {
				log.Printf("destroying %s, which joined %s", j.GetName(), m.GetName())

				if err := d.leaveCluster(ctx, j.Unstructured, opts.force); err != nil {
					return err
				}

				if err := d.Destroy(ctx, j.Unstructured); err != nil {
					return fmt.Errorf("destroying %s: %w", j.GetName(), err)
				}
			}
		case opts.force:
			log.Printf("warning: destroying %s while %s joined it; they are now ORPHANED: "+
				"their cluster lost the member that holds its secrets bundle, and no new node "+
				"can join through it", m.GetName(), names(joiners))
		default:
			return fmt.Errorf("%s owns a cluster that %s joined, so it is not destroyed\n\n"+
				"  its state directory holds the secrets bundle and endpoint those nodes were\n"+
				"  built from. Either take the whole cluster:\n\n"+
				"    tinq destroy --with-joiners %s\n\n"+
				"  or destroy the joiners first, one at a time. --force destroys %s alone and\n"+
				"  orphans them", m.GetName(), names(joiners), path, m.GetName())
		}
	}

	if driverkit.Str(m, "spec", "joins") != "" {
		if err := d.leaveCluster(ctx, m, opts.force); err != nil {
			return err
		}
	}

	return standalone(ctx, d, path, "destroy")
}

// joinersOf returns the recorded machines of m's site whose spec.joins names m.
func (h *hvf) joinersOf(m *unstructured.Unstructured) ([]siteMachine, error) {
	machines, err := siteMachines(h.stateRoot, driverkit.Str(m, "spec", "site"), h.dir(m))
	if err != nil {
		return nil, err
	}

	var out []siteMachine

	for _, o := range machines {
		if driverkit.Str(o.Unstructured, "spec", "joins") == m.GetName() && o.GetNamespace() == m.GetNamespace() {
			out = append(out, o)
		}
	}

	return out, nil
}

// stoppedMembers returns, by name and sorted, the members of m's cluster that
// joined it — directly, or through another member — and whose VMs are not
// running.
//
// Only CONFIGURED members count: a joiner with no talosconfig never applied a
// config, so it never became an etcd member and holds no vote anyone waits for.
// The same reading leaveCluster makes.
func (h *hvf) stoppedMembers(ctx context.Context, m *unstructured.Unstructured) ([]string, error) {
	seen := map[string]bool{m.GetName(): true}
	queue := []*unstructured.Unstructured{m}

	var out []string

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		joiners, err := h.joinersOf(cur)
		if err != nil {
			return nil, err
		}

		for _, j := range joiners {
			if seen[j.GetName()] {
				continue
			}

			seen[j.GetName()] = true
			queue = append(queue, j.Unstructured)

			_, configured, err := cluster.ReadTalosconfig(j.Dir)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", j.GetName(), err)
			}

			if !configured {
				continue
			}

			state, _, err := h.Observe(ctx, j.Unstructured)
			if err != nil {
				return nil, fmt.Errorf("observing %s: %w", j.GetName(), err)
			}

			if state != driverkit.Running {
				out = append(out, j.GetName())
			}
		}
	}

	sort.Strings(out)

	return out, nil
}

// leaveCluster removes the joiner m from etcd and deletes its Kubernetes Node.
//
// A RUNNING member leaves by itself. A STOPPED one cannot, and is removed
// through another running member of the site; every member's talosconfig is
// signed by the same cluster CA, so the joiner's own credential reaches them.
// A joiner that was never configured has no talosconfig and nothing to leave.
func (h *hvf) leaveCluster(ctx context.Context, m *unstructured.Unstructured, force bool) error {
	dir := h.dir(m)

	talosconfig, err := os.ReadFile(filepath.Join(dir, "talosconfig"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	cn, err := specClusterNetwork(m)
	if err != nil {
		return err
	}

	if cn == nil {
		return fmt.Errorf("%s joins a cluster but declares no clusterNetwork, so its etcd member "+
			"cannot be identified", m.GetName())
	}

	addr := cn.Address.String()
	ops := h.ops()

	state, _, err := h.Observe(ctx, m)
	if err != nil {
		return err
	}

	var leaveErr error

	if state == driverkit.Running {
		leaveErr = ops.leave(ctx, talosconfig, talosEndpoint(m))
	} else {
		via, ok := h.runningMember(ctx, m)
		if !ok {
			leaveErr = fmt.Errorf("%s is %s and no other member of the cluster is running "+
				"to remove it through", m.GetName(), state)
		} else {
			leaveErr = ops.remove(ctx, talosconfig, via, addr)
		}
	}

	if leaveErr != nil {
		if !force {
			return fmt.Errorf("%s was not removed from etcd, so it is not destroyed: %w\n\n"+
				"  a destroyed member left in the member list is a vote the cluster can never\n"+
				"  collect again. Start the cluster and retry, or pass --force to destroy it\n"+
				"  anyway and remove the member by hand:\n\n"+
				"    talosctl etcd remove-member <id>", m.GetName(), leaveErr)
		}

		log.Printf("warning: %s was not removed from etcd (%v); destroying it anyway because of --force",
			m.GetName(), leaveErr)

		return nil
	}

	log.Printf("%s left etcd", m.GetName())

	// NOT fatal: etcd was the quorum-critical half, and it is done. A stale
	// Node object is cosmetic by comparison, and `kubectl delete node` fixes it.
	kubeconfig, err := os.ReadFile(filepath.Join(dir, "kubeconfig"))
	if err == nil {
		err = ops.deleteNode(ctx, kubeconfig, addr)
	}

	if err != nil {
		log.Printf("warning: %s's Kubernetes node was not deleted (%v); `kubectl delete node` removes it",
			m.GetName(), err)
	}

	return nil
}

// runningMember returns the Talos endpoint of a running member of m's cluster
// other than m: a machine of the site on the same cluster network.
func (h *hvf) runningMember(ctx context.Context, m *unstructured.Unstructured) (string, bool) {
	cn, _ := specClusterNetwork(m)

	machines, err := siteMachines(h.stateRoot, driverkit.Str(m, "spec", "site"), h.dir(m))
	if err != nil || cn == nil {
		return "", false
	}

	for _, o := range machines {
		ocn, _ := specClusterNetwork(o.Unstructured)
		if ocn == nil || ocn.Name != cn.Name {
			continue
		}

		if state, _, err := h.Observe(ctx, o.Unstructured); err == nil && state == driverkit.Running {
			if ep := talosEndpoint(o.Unstructured); ep != "" {
				return ep, true
			}
		}
	}

	return "", false
}

func names(ms []siteMachine) string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.GetName()
	}

	return strings.Join(out, ", ")
}
