## Purpose

Lets a tinq VM join another tinq VM's Talos cluster as a control-plane member, with a stable hostname and safe lifecycle rules. Two- and three-node VM clusters can then rehearse what the metal cluster runs.

## ADDED Requirements

### Requirement: The owner's endpoint is reachable from other VMs

For a VM owner with a cluster network, the cluster's control-plane endpoint SHALL be the owner's cluster address on port 6443. The API server certificate SHALL be valid for that address and for the host forward address. The kubeconfig written for the host SHALL keep using the host forward. tinq SHALL record the in-cluster endpoint in the owner's state directory.

#### Scenario: The host and the cluster use different addresses for one API

- **WHEN** a networked VM owner is Ready
- **THEN** `kubectl` on the host works with the written kubeconfig through `127.0.0.1:<forward>`
- **AND** the node's control-plane endpoint is `https://<owner cluster address>:6443`
- **AND** TLS verification succeeds for both addresses

### Requirement: A VM can join another VM's cluster

A VM with `spec.joins: <owner name>` SHALL join the owner's existing cluster as a control-plane member. It SHALL use the owner's secrets bundle and in-cluster endpoint, and reuse the owner's kubeconfig. It SHALL never generate new cluster secrets. The bare-metal `spec.baremetal.joins` SHALL keep its current behavior, and both paths SHALL share one join resolver.

#### Scenario: A second VM joins

- **WHEN** a Ready networked owner exists and `tinq up` runs for a VM that declares `joins: <owner>`
- **THEN** the cluster has two Ready control-plane nodes and two etcd members
- **AND** the joiner's state directory holds the owner's kubeconfig, not a newly minted one

#### Scenario: A joiner never uses the host forward as its endpoint

- **WHEN** the owner's kubeconfig server is `https://127.0.0.1:<forward>`
- **THEN** the joiner's control-plane endpoint is the owner's cluster address, not `127.0.0.1`

#### Scenario: A join without the owner's secrets is refused

- **WHEN** the owner's state directory has no secrets bundle or no in-cluster endpoint
- **THEN** `tinq up` fails before creating anything and does not build a second cluster

#### Scenario: A join across sites or networks is refused

- **WHEN** a joiner declares a different site or a different cluster network than its owner, or declares no cluster network
- **THEN** `tinq up` fails before creating anything and names the mismatch

#### Scenario: Colliding host forwards are refused

- **WHEN** a joiner declares a host port that another machine of the site already forwards
- **THEN** `tinq up` fails before creating anything and names the port and both machines

### Requirement: A three-member VM cluster survives the loss of one member

A VM cluster of three networked control-plane members SHALL keep etcd quorum, and SHALL accept an API write, while any one member is stopped.

#### Scenario: One of three members stops

- **WHEN** a three-member VM cluster is Ready and `tinq stop` runs on any one member
- **THEN** the remaining two members keep quorum, and a `kubectl` write through a remaining member succeeds
- **AND** after `tinq up` of the stopped member, all three members are healthy again

### Requirement: A VM can declare a stable hostname

A VM with `spec.hostname` SHALL register with Kubernetes under exactly that name. The name SHALL be the same after `destroy` and `up`. Without `spec.hostname`, behavior SHALL be unchanged.

#### Scenario: The node name is the declared hostname

- **WHEN** a VM declares `hostname: ci-worker-vm` and is Ready
- **THEN** its Kubernetes node name is `ci-worker-vm`, and `kubernetes.io/hostname` has that value

### Requirement: Lifecycle commands respect cluster membership

- `tinq up` on a joiner SHALL refuse while its owner is not Running.
- `tinq up` on a joiner that has never joined (no talosconfig in its state directory) SHALL also refuse while the owner's Kubernetes API does not answer.
- `tinq up` on a joiner that is already a member (a talosconfig in its state directory) SHALL accept a Running owner whose authenticated Talos API answers in the installed system's stage. It SHALL NOT require the owner's Kubernetes API, because that API needs the etcd quorum this member is part of. It SHALL then wait until its own node is Ready.
- `tinq up` on an owner whose etcd already exists SHALL NOT wait for a Ready Kubernetes node while a configured member that joined it, directly or through another member, is not Running. It SHALL end with success after its Talos API answers and the node refuses a second bootstrap, and it SHALL name the stopped members.
- `tinq up` on a running cluster SHALL stay a no-op success, and the first bring-up of an owner or a joiner SHALL be unchanged.
- `tinq destroy` on a joiner SHALL remove the member from etcd before it destroys the VM.
- `tinq destroy` on an owner SHALL refuse while joiners of its site exist, unless an explicit flag destroys the joiners first.

#### Scenario: A joiner does not start before its owner

- **WHEN** the owner is stopped and `tinq up` runs on a joiner
- **THEN** it fails and says to start the owner first

#### Scenario: A new joiner does not start beside an owner that does not serve

- **WHEN** the owner is Running, its Kubernetes API does not answer, and `tinq up` runs on a joiner that has never joined
- **THEN** it fails before creating anything and says to start the owner first

#### Scenario: A stopped three-member cluster restarts in the documented order

- **WHEN** all three VMs of a bootstrapped three-member cluster are stopped
- **AND** `tinq up` runs on the owner, then on each joiner, one at a time
- **THEN** the owner's `up` succeeds without waiting for Kubernetes and names the two stopped joiners
- **AND** the first joiner's `up` is accepted while the owner's Kubernetes API does not answer, and succeeds when its own node is Ready
- **AND** the second joiner's `up` succeeds when its own node is Ready
- **AND** afterwards etcd lists three members and all three nodes are Ready

#### Scenario: Destroying a joiner leaves a healthy cluster

- **WHEN** a two-member VM cluster is Ready and `tinq destroy` runs on the joiner
- **THEN** afterwards etcd lists one member and the owner's API accepts writes

#### Scenario: Destroying an owner with joiners is refused

- **WHEN** a joiner's state directory exists and `tinq destroy` runs on the owner without the explicit flag
- **THEN** nothing is destroyed, and the message names the joiners and the flag

### Requirement: Host capacity is checked before a joiner boots

Before it boots a joiner, tinq SHALL compare the site's total declared VM memory, including the joiner, with host memory. It SHALL refuse when the total exceeds the declared safety margin.

#### Scenario: A joiner that would overcommit the host is refused

- **WHEN** the site's VMs plus the joiner declare more memory than the margin allows
- **THEN** `tinq up` fails before creating anything and prints the totals
