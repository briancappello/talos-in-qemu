package cluster

import (
	"context"
	"fmt"
	"net/url"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// LEAVING A CLUSTER BEFORE THE VM GOES. A control-plane member destroyed
// without leaving stays in etcd's member list: a two-member cluster then needs
// two votes it can never get, and loses quorum the moment the second member
// vanishes. The member has to be removed while the cluster is healthy, which
// is before the destroy, never after.

// membershipTimeout bounds each membership call. They are single RPCs against
// a healthy cluster; a slow answer is a cluster that is not healthy.
const membershipTimeout = 30 * time.Second

// LeaveEtcd asks the node at endpoint to leave etcd itself: the graceful path,
// for a member that is running.
//
// talosconfig is SECRET and is neither logged nor placed in an error.
func LeaveEtcd(ctx context.Context, talosconfig []byte, endpoint string) error {
	c, err := AuthenticatedClient(ctx, talosconfig, endpoint)
	if err != nil {
		return err
	}

	defer c.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(ctx, membershipTimeout)
	defer cancel()

	if err := c.EtcdLeaveCluster(ctx, &machineapi.EtcdLeaveClusterRequest{}); err != nil {
		return fmt.Errorf("asking %s to leave etcd: %w", endpoint, err)
	}

	return nil
}

// RemoveEtcdMember removes the member whose peer URL is on peerAddr, through
// the member at via: the path for a member that is NOT running and so cannot
// leave by itself. It is matched by address, not by name, because a node
// without a declared hostname has a name nobody chose.
//
// Returns nil when no member is on peerAddr: the member is already gone.
func RemoveEtcdMember(ctx context.Context, talosconfig []byte, via, peerAddr string) error {
	c, err := AuthenticatedClient(ctx, talosconfig, via)
	if err != nil {
		return err
	}

	defer c.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(ctx, membershipTimeout)
	defer cancel()

	resp, err := c.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		return fmt.Errorf("listing etcd members through %s: %w", via, err)
	}

	for _, msg := range resp.GetMessages() {
		for _, member := range msg.GetMembers() {
			if !peerURLOn(member.GetPeerUrls(), peerAddr) {
				continue
			}

			if err := c.EtcdRemoveMemberByID(ctx, &machineapi.EtcdRemoveMemberByIDRequest{
				MemberId: member.GetId(),
			}); err != nil {
				return fmt.Errorf("removing etcd member %s (%x) through %s: %w",
					member.GetHostname(), member.GetId(), via, err)
			}

			return nil
		}
	}

	return nil
}

func peerURLOn(urls []string, addr string) bool {
	for _, raw := range urls {
		if u, err := url.Parse(raw); err == nil && u.Hostname() == addr {
			return true
		}
	}

	return false
}

// DeleteNodeAt deletes the Kubernetes Node whose InternalIP is addr. Left
// behind, a destroyed member stays in `kubectl get nodes` as NotReady forever,
// and pods keep being scheduled against it until eviction.
//
// Returns nil when no node has that address. kubeconfig is SECRET.
func DeleteNodeAt(ctx context.Context, kubeconfig []byte, addr string) error {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return errSecretParse("kubeconfig")
	}

	nodes, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building a Kubernetes client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, membershipTimeout)
	defer cancel()

	list, err := nodes.Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	for i := range list.Items {
		for _, a := range list.Items[i].Status.Addresses {
			if a.Type != "InternalIP" || a.Address != addr {
				continue
			}

			if err := nodes.Nodes().Delete(ctx, list.Items[i].Name, metav1.DeleteOptions{}); err != nil {
				return fmt.Errorf("deleting node %s: %w", list.Items[i].Name, err)
			}

			return nil
		}
	}

	return nil
}
