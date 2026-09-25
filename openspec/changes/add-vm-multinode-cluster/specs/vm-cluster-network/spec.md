## Purpose

Gives the VMs of one site a shared, unprivileged network, so they can form a multi-node Talos cluster on a single host. Each VM keeps its existing user-mode networking for egress and host port forwards.

## ADDED Requirements

### Requirement: A VM can attach to a site cluster network without privileges

A VM with `spec.clusterNetwork` SHALL get a second NIC on an L2 segment shared by every VM of the same site that declares the same network name. The segment SHALL need no root, no `CAP_NET_ADMIN`, no host bridge and no helper daemon. Its traffic SHALL stay on the host's loopback interface. The first NIC SHALL keep its user-mode egress and host port forwards.

#### Scenario: Two VMs of a site reach each other

- **WHEN** two VMs of site `s` declare `clusterNetwork` with the same name and CIDR and different addresses, and both are running
- **THEN** each VM can reach the other's cluster address
- **AND** each VM still reaches the internet through its first NIC

#### Scenario: Two sites on one host stay separate

- **WHEN** VMs of site `a` and site `b` both declare a cluster network named `cluster`
- **THEN** a VM of site `a` cannot reach a VM of site `b` over the cluster network

#### Scenario: No cluster network means no change

- **WHEN** a machine file declares no `clusterNetwork`
- **THEN** its QEMU arguments and its generated machine config are identical to those produced before this change

### Requirement: Cluster network declarations are refused before boot when invalid

tinq SHALL validate a cluster network declaration before it creates a state directory or starts QEMU. It SHALL refuse:
- a declaration without a `cidr` (there is no default CIDR);
- an address outside the declared CIDR;
- a CIDR that overlaps the user-mode network, the pod CIDR or the service CIDR;
- an address already declared by another machine of the same site and network;
- a CIDR that disagrees with another machine of the same site and network.

#### Scenario: A duplicate address is refused

- **WHEN** a machine declares a cluster address that another machine of the site already uses
- **THEN** `tinq up` fails before creating anything, and names both machines and the address

#### Scenario: An overlapping CIDR is refused

- **WHEN** a machine declares a cluster CIDR that overlaps `10.0.2.0/24`
- **THEN** `tinq up` fails before creating anything, and names the overlap

### Requirement: Stable NIC identity

The cluster NIC's MAC address SHALL be derived from the machine name. It SHALL be locally administered and identical across `destroy` and `up` of the same machine. Different machine names SHALL get different MACs.

#### Scenario: A recreated machine keeps its MAC

- **WHEN** a machine is destroyed and brought up again from the same file
- **THEN** its cluster NIC has the same MAC as before

### Requirement: Kubernetes, etcd and the CNI use the cluster network

On a VM with a cluster network, the node's static cluster address SHALL be its Kubernetes `InternalIP` and its etcd peer address. The CNI SHALL carry pod traffic between nodes over the cluster network. The cluster NIC SHALL NOT receive a default route.

#### Scenario: Nodes report distinct cluster addresses

- **WHEN** a two-node VM cluster is Ready
- **THEN** `kubectl get nodes -o wide` shows each node's declared cluster address as its `INTERNAL-IP`, and no node shows `10.0.2.15`

#### Scenario: Pods talk across nodes

- **WHEN** a pod on node A connects to a pod on node B by pod IP
- **THEN** the connection succeeds

#### Scenario: etcd peers use the cluster network

- **WHEN** a two-node VM cluster is Ready
- **THEN** `talosctl etcd members` lists both members with peer URLs on their cluster addresses

### Requirement: An unusable segment fails fast

When a joining VM cannot reach the owner's cluster address over the segment, `tinq up` SHALL fail before it applies a join config. The error SHALL name the cluster network and host multicast on loopback as the likely cause.

#### Scenario: Multicast on loopback is blocked

- **WHEN** the host drops multicast on `lo` and a joiner is brought up
- **THEN** `tinq up` fails within its connectivity check, not an etcd join timeout
- **AND** the message names the cluster network and the loopback multicast check
