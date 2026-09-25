## Why

tinq can build only single-node VM clusters. Each VM has one QEMU user-mode NIC (`netdev := "user,id=n0"`, `cmd/tinq/main.go`), which puts it behind its own private NAT. Two VMs therefore cannot reach each other, and joining is available only to bare-metal machines (`spec.baremetal.joins`).

The consumer, the homelab repository, uses the VM path to prove behavior before it touches metal. The metal cluster has two control-plane nodes, and a third one is coming. Several acceptance gates cannot be rehearsed on one node:

- CI isolation: the network proof needs two nodes and checks denial across nodes.
- Node-to-node firewall rules.
- etcd quorum with three members, and recovery after one node is lost.
- Replicas spread across nodes, and failover within 60 seconds.

## What Changes

- A VM can attach to a **cluster network**: an L2 segment shared by the VMs of one site. It needs no root and no host bridge. Each VM keeps its user-mode NIC for egress and host port forwards.
- A VM declares a static address on that network. Kubernetes and etcd use that address, so nodes reach each other directly.
- A VM can **join** another VM's cluster (`spec.joins`), with the same guarantees the bare-metal join has: the existing secrets bundle, the existing endpoint, and the reused kubeconfig.
- The cluster's control-plane endpoint for a networked VM cluster is the owner's cluster-network address. The host reaches the API through the owner's existing port forward, and the API certificate covers both addresses.
- A VM can declare a stable **hostname**. Today Talos generates a random one, so nothing can select the node by name.
- `destroy`, `stop` and `up` behave correctly for a cluster of several VMs: a joiner refuses to come up without its owner, and destroying the owner while joiners run is refused unless forced.

## Capabilities

### New Capabilities

- `vm-cluster-network`: A shared, unprivileged L2 network between the VMs of a site, static per-VM addresses on it, and the rule that Kubernetes and etcd use it.
- `vm-join`: A VM joining another VM's cluster as a control-plane member, with a stable hostname, and the lifecycle rules for a cluster of several VMs.

### Modified Capabilities

None. `tinq-adopt` and `registry-ca-trust` keep their requirements. A single-node VM without a cluster network must behave exactly as it does today.

## Impact

- `crd/talosmachine.yaml`: new optional fields `spec.clusterNetwork`, `spec.joins` and `spec.hostname`.
- `cmd/tinq/main.go`: QEMU arguments for a second NIC, endpoint selection, and join resolution for VMs (today in `adopt.go`, bare-metal only).
- `cluster/`: config generation takes the node's cluster-network address, the certificate SANs, the kubelet node IP, and the CNI interface.
- README and `examples/`: a two-node and a three-node VM example.
- **Consumer follow-up (not part of this change):** the homelab repository must accept VM joiners in `homelab/cluster_context.py`, which today rejects `spec.joins`. It must also declare the VM nodes in `topology.yml` and `environments/vm.yml`.

## Non-goals

- Worker-role nodes. Config generation always makes control-plane nodes today, and metal joins as control plane too. Worker support can follow separately.
- Host access to the cluster network. The host keeps using port forwards.
- Multi-host clusters, bridges, taps, or anything that needs root or `CAP_NET_ADMIN`.
- Changing the bare-metal join path, except to share code with the VM path.
