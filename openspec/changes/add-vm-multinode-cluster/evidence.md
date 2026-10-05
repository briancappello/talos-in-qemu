# Evidence: Multi-Node VM Clusters

Live acceptance runs for `add-vm-multinode-cluster`. The results are sanitized: no secrets, keys or tokens.

## Environment

- Host: Linux, KVM, `qemu-system-x86_64`, 62 GiB RAM.
- Image: `talos-v1.14.1-amd64.iso`. Installer: `factory.talos.dev/installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.14.1` (see "Findings").
- Machinery: v1.14.0. Kubernetes: v1.37.0 (derived).
- Machine files: `examples/multinode/cp0.yaml` (owner), `cp1.yaml` and `cp2.yaml` (joiners). Site `multinode`, CIDR `10.254.0.0/24`.
- Derived segment for site `multinode`, network `cluster`: `239.255.129.215:27494`.

## 6.1 Two-node cluster

```sh
tinq up examples/multinode/cp0.yaml
tinq up examples/multinode/cp1.yaml
```

The joiner's transcript:

```
[ 8/10] bootstrap     skipped (joining an existing cluster)
[ 9/10] kubeconfig    wrote the cluster's kubeconfig, node 10.254.0.12 Ready after 17s
[10/10] storage       skipped (the cluster this node joined owns its StorageClass)
```

Both nodes Ready, with distinct cluster addresses. No node shows `10.0.2.15`:

```
$ kubectl get nodes -o wide
NAME   STATUS   ROLES           VERSION   INTERNAL-IP   OS-IMAGE
cp0    Ready    control-plane   v1.37.0   10.254.0.11   Talos (v1.14.1)
cp1    Ready    control-plane   v1.37.0   10.254.0.12   Talos (v1.14.1)
```

Two etcd members, with peer URLs on the cluster network:

```
$ talosctl -e 127.0.0.1:50010 -n 127.0.0.1 etcd members
ID                 HOSTNAME   PEER URLS                  CLIENT URLS                LEARNER
2d1a1ebbd7d7271e   cp1        https://10.254.0.12:2380   https://10.254.0.12:2379   false
5f2e794a59987ea0   cp0        https://10.254.0.11:2380   https://10.254.0.11:2379   false
```

Flannel picks the cluster NIC through `--iface-can-reach=10.254.0.0` (D3):

```
match.go:206] Determining interface to use based on given ifcanreach: 10.254.0.0
match.go:269] Using interface with name enp0s6 and address 10.254.0.11
```

Cross-node pod-to-pod. The server pod runs on cp1 and the client pod on cp0:

```
srv on cp1 at 10.244.1.3; cli on cp0 at 10.244.0.4
$ kubectl exec cli -- wget -qO- -T 5 http://10.244.1.3:8080/index.html
cross-node-ok
```

The joiner reused the owner's kubeconfig (`cmp` equal), and recorded the owner's in-cluster endpoint, `https://10.254.0.11:6443`.

## 6.2 Three-node cluster, loss of one member

```sh
tinq up examples/multinode/cp2.yaml
tinq stop examples/multinode/cp0.yaml     # the OWNER, the hardest case
```

The stopped member was the owner, which every joiner's config points at. `kubectl` went through cp1's own forward (`https://127.0.0.1:6454`). This also proves that a joiner's API certificate names the host forward:

```
$ talosctl -e 127.0.0.1:50011 -n 127.0.0.1 etcd status
MEMBER             LEADER             RAFT TERM
2d1a1ebbd7d7271e   2d1a1ebbd7d7271e   3
$ kubectl get nodes
cp0    NotReady,SchedulingDisabled   control-plane
cp1    Ready                         control-plane
cp2    Ready                         control-plane
$ kubectl create configmap quorum-proof --from-literal=at=...
configmap/quorum-proof created
```

Start the owner again:

```
$ tinq up examples/multinode/cp0.yaml
[ 5/10] maintenance   skipped (already configured)
[ 6/10] config        skipped (reusing the talosconfig in the state dir)
[ 8/10] bootstrap     already bootstrapped (the node refused a second one)
[ 9/10] kubeconfig    wrote kubeconfig, node Ready after 18s
$ kubectl get nodes          # cp0, cp1, cp2 all Ready
$ talosctl ... etcd members  # three members
$ kubectl get configmap quorum-proof -o jsonpath='{.data.at}'
2026-09-25T05:44:38Z         # the write made while cp0 was down
```

## 6.3 Egress and per-node Talos API

Egress: each node pulled `busybox:1.36` from Docker Hub through its user-mode NIC (the `srv` pod on cp1, the `cli` pod on cp0). Each node's link state:

```
enp0s4/10.0.2.15/24       # user-mode NIC, DHCP (DHCPv4Config egress0)
enp0s6/10.254.0.11/24     # cluster NIC, static (LinkConfig cluster0)
```

Each node's Talos API answers through its own forward, with its declared hostname:

```
$ talosctl -e 127.0.0.1:50010 -n 127.0.0.1 get hostname   ->  cp0
$ talosctl -e 127.0.0.1:50011 -n 127.0.0.1 get hostname   ->  cp1
```

## 4.4 Segment probe against the live segment

The host joined the live segment and sent ARP probes:

```
239.255.129.215:27494 probe 10.254.0.11 -> answered (0s)
239.255.129.215:27494 probe 10.254.0.12 -> answered (0s)
239.255.129.215:27494 probe 10.254.0.99 -> refused  (5.003s)
```

`TestProbeSegmentOverLoopback` runs the same probe against a responder on loopback in the unit suite.

## 5.1 and 5.3 Lifecycle

Destroying the OWNER while joiners exist is refused:

```
$ tinq destroy examples/multinode/cp0.yaml
tinq: cp0 owns a cluster that cp1, cp2 joined, so it is not destroyed
    tinq destroy --with-joiners examples/multinode/cp0.yaml
```

A running joiner leaves etcd, then goes. The name survives destroy and up (5.1):

```
$ tinq destroy examples/multinode/cp2.yaml
cp2 left etcd
$ talosctl ... etcd members         # cp0, cp1
$ kubectl create configmap after-cp2-destroy ...   # created
$ tinq up examples/multinode/cp2.yaml
[ 9/10] kubeconfig    wrote the cluster's kubeconfig, node 10.254.0.13 Ready after 20s
$ kubectl get node cp2 -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}'
cp2
```

The two-member case, where a member left in etcd would cost quorum:

```
$ tinq destroy examples/multinode/cp1.yaml    # 3 -> 2: cp0, cp2
cp1 left etcd
$ tinq destroy examples/multinode/cp2.yaml    # 2 -> 1
cp2 left etcd
$ talosctl ... etcd members
cp0 https://10.254.0.11:2380
$ kubectl create configmap after-two-to-one ...   # created
```

The whole cluster:

```
$ tinq up examples/multinode/cp1.yaml          # rejoin: owner-up, ARP probe, memory checks all pass
$ tinq destroy --with-joiners examples/multinode/cp0.yaml
destroying cp1, which joined cp0
cp1 left etcd
$ ls ~/.hvf/multinode      # gone; no qemu process left
```

## 6.4 Single node, unchanged

`examples/bootstrap-machine.yaml`, which has none of the new fields, on `talos-v1.13.7-amd64.iso`:

```
[ 8/10] bootstrap     etcd bootstrapped
[ 9/10] kubeconfig    wrote kubeconfig, node Ready after 1m28s
[10/10] storage       local-path-provisioner v0.0.31, default StorageClass
NAME            STATUS   INTERNAL-IP   OS-IMAGE
talos-c28-pdd   Ready    10.0.2.15     Talos (v1.13.7)
server: https://127.0.0.1:6443
```

No `cluster-endpoint` is written. The one new file in its state dir is `machine.yaml`, the site record. The golden tests (`TestGoldenSingleNodeVMConfig`, `TestGoldenSingleNodeMachineFile`) pass.

## Findings During The Live Runs

### F1. A LinkConfig switches off default DHCP on every link (fixed)

The first owner bring-up hung at step 7. The node never installed (`system.qcow2` stayed at 196 KB), and its Talos API timed out even with `--insecure`. The cause is in machinery v1.14.0, `config/container/container.go`:

```go
func (container *Container) RunDefaultDHCPOperators() bool {
	return len(findMatchingDocs[config.NetworkCommonLinkConfig](container.documents)) == 0 &&
		len(findMatchingDocs[config.NetworkDHCPConfig](container.documents)) == 0
}
```

The cluster NIC's `LinkConfig` therefore stopped DHCP on the user-mode NIC. The node lost `10.0.2.15`, the host forwards went dead, and the installer could not be pulled. Fix (`7113253`): a networked VM's user-mode NIC gets a derived MAC, a `LinkAliasConfig` (`egress0`) and an explicit `DHCPv4Config`. `TestClusterNetworkKeepsDHCPOnTheEgressNIC` pins it.

### F2. No `ghcr.io/siderolabs/installer` for v1.14 (not fixed, reported)

tinq pins the installer to `ghcr.io/siderolabs/installer:<image version>`. That repository has tags up to v1.13.x and none for v1.14:

```
ghcr.io/siderolabs/installer:v1.14.1        MANIFEST_UNKNOWN
ghcr.io/siderolabs/installer:v1.13.7        sha256:6b0e5ed9...
factory.talos.dev/installer/<vanilla>:v1.14.1  sha256:29ae92d2...
```

With the default pin, **no v1.14 image can install**, multi-node or not. The examples set `installerImage` to the Image Factory vanilla installer. Changing the default is a design decision (a dependency on factory.talos.dev, and what a factory ISO with extensions needs), so it is outside this change.

### F3. The transcript named the default installer even when `installerImage` overrode it (fixed, Tier 3)

Step 6 printed `installer: ghcr.io/siderolabs/installer:v1.14.1 (pinned to YOUR image)` while the config used the factory image. Fixed in `5d8427a`, pinned by `TestStep6NamesTheInstallerOverride`.

## 7.2 Consumer Contract (homelab repository)

Each item is a field name, a path or a command that the consumer can use as written.

### Fields on `TalosMachine.spec` (VMs only)

| Field | Required | Meaning |
|---|---|---|
| `clusterNetwork.cidr` | yes, with `clusterNetwork` | The segment, for example `10.254.0.0/24`. The same on every VM of the network. No default. |
| `clusterNetwork.address` | yes, with `clusterNetwork` | This VM's address, with no prefix, for example `10.254.0.11`. |
| `clusterNetwork.name` | no | The segment name within the site. Default `cluster`. |
| `clusterNetwork.group`, `clusterNetwork.port` | no | Overrides of the derived multicast group (in `239.0.0.0/8`) and port. |
| `joins` | no | `metadata.name` of the owner VM. Requires `clusterNetwork`. |
| `hostname` | no | The Kubernetes node name. The same after `destroy` and `up`. |
| `installerImage` | in practice, for Talos v1.14 (not enforced) | `factory.talos.dev/installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.14.1` (F2). |

The CRD refuses `clusterNetwork`, `joins` and `hostname` beside `spec.baremetal`. The cluster network needs a Talos v1.14 or later ISO.

### Files in the state directory

The state directory is `<stateRoot>/<site>/<uid>/`. For a file-driven machine, `<uid>` is `bootstrap-<namespace>-<name>`. The default state root is `~/.hvf`.

| File | Contents |
|---|---|
| `kubeconfig` | The cluster's admin kubeconfig. Its server is the owner's host forward, for example `https://127.0.0.1:6453`. Every member holds the same file. |
| `cluster-endpoint` | The in-cluster API endpoint, for example `https://10.254.0.11:6443`. Only other VMs can reach it. It is not for the host. |
| `talosconfig` | This node's Talos client configuration. Its endpoint is this node's own host forward. |
| `machine.yaml` | The machine record that the site checks read. Do not edit it. |

### Order of operations

```sh
tinq up <owner.yaml>                      # first, and alone
tinq up <joiner.yaml>                     # then each joiner, one at a time
tinq stop <any.yaml>                      # any member
tinq up <owner.yaml>; tinq up <joiner.yaml> ...   # restart: owner first
tinq destroy <joiner.yaml>                # leaves etcd first
tinq destroy --with-joiners <owner.yaml>  # the whole cluster
```

A joiner's `up` refuses while the owner is down. Run one `up` or `destroy` at a time per site.

On a restart of a stopped cluster, the owner's `up` does not wait for Kubernetes while a member that joined it is stopped: without that member etcd may have no quorum. It names the stopped members and succeeds. Each joiner's `up` then waits until its own node is Ready, so the last member's `up` ends when Kubernetes answers. A consumer that waits for Kubernetes itself must do so after the last member's `up`, not after the owner's.

### Behaviour a consumer can rely on (found by the homelab consumer)

- **Forwarded NodePorts work.** With `clusterNetwork`, tinq sets kube-proxy `nodePortAddresses` to the cluster segment and `10.0.2.0/24`, so a host forward to a NodePort (ingress) reaches it. kube-proxy in nftables mode otherwise serves NodePorts only on the primary address, which the cluster network makes 10.254.x. Do not restate the addresses in a config patch: machinery APPENDS list values, so they would appear twice.
- **`tinq stop` waits up to 3 minutes** for Talos's own shutdown sequence (cordon and drain, then the kubelet's graceful node shutdown) before escalating. Measured on the three-node cluster: drain 28s, kubelet 22s, power-off phase at 51.5s; the former 60s budget escalated to SIGTERM on every stop of a node that ran workloads.
- **Known limit:** `tinq controller` reconciles serially, so one stopping machine holds the loop for up to ~3m25s. The file verbs are unaffected. See the note on `driverkit.Run`.

### Required changes in the homelab repository

- `homelab/cluster_context.py` must accept `spec.joins` for VM profiles. Today it refuses the field.
- `topology.yml` and `environments/vm.yml` must declare the VM nodes, each with `clusterNetwork.address`, `hostname` and its own `hostForwards` ports.
- For a two-node VM profile, do not expect fault tolerance. Use three nodes for the P4 and P7 rehearsals.
