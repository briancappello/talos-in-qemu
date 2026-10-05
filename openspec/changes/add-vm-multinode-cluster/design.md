# Design: Multi-Node VM Clusters

## Context

See `proposal.md` for motivation. The current state is below (checked 2026-09-24, branch `feat/secureboot`, `dc250d0`, machinery v1.14.0).

- **One NIC, user-mode.** `create` builds QEMU with `-netdev user,id=n0,hostfwd=...` and `-device virtio-net-pci,netdev=n0` (`cmd/tinq/main.go`, around line 1097 and 1206). The guest sees `10.0.2.15`, and each VM has a private SLIRP NAT. Guests cannot reach each other.
- **The endpoint is a host forward.** `kubeEndpoint(m)` returns `https://<hostAddr>:<hostPort>` for the `6443` forward. It is written into the generated config as the control-plane endpoint **and** into the kubeconfig (`cmd/tinq/main.go:425-435`). No other guest can reach that address.
- **Joining is bare-metal only.** `joinOptions` (`cmd/tinq/adopt.go:615`) reads `spec.baremetal.joins`, loads the owner's `secrets.yaml` and `kubeconfig` from `<stateRoot>/<site>/bootstrap-<ns>-<owner>/`, and takes the endpoint from the kubeconfig. `cluster.joined` (`cluster/up.go:1178`) reuses the kubeconfig and waits for *this* node by address.
- **Always a control plane.** Config generation calls `input.Config(machine.TypeControlPlane)` (`cluster/config.go:388`). The CRD enum allows `talos-worker`, but nothing generates a worker.
- **Identity is already derived per machine.** `-uuid machineUUID(name)` gives a stable SMBIOS UUID. The state directory is `<stateRoot>/<site>/<uid>`.
- **Talos 1.14 document model.** Machinery generates `ResolverConfig`, `KubeletConfig` and other documents. It rejects the matching v1alpha1 fields next to them: the consumer hit `.machine.network.nameservers is already set in v1alpha1 config` and `kubelet config is already set in v1alpha1 config`. New configuration must use the documents machinery v1.14 accepts.

## Goals / Non-Goals

**Goals:**

- Two or three tinq VMs on one Linux host form one Talos cluster. Nodes, etcd and pods talk over a shared network, without root.
- A node lost from a three-member cluster leaves a working control plane. That is the consumer's P4 and P7 rehearsal.
- A single VM without the new fields behaves exactly as it does today.

**Non-Goals:**

- Worker nodes, multi-host clusters, host access to the cluster network, and bridge or tap networking.
- macOS parity. The design must not break macOS single-node use. Multi-node on macOS is best-effort: implement it if QEMU's socket netdev works there, and report it if not.

## Decisions

### D1. A second NIC on a QEMU multicast socket netdev

A VM with `spec.clusterNetwork` gets a second NIC:

```
-netdev socket,id=n1,mcast=<group>:<port>,localaddr=127.0.0.1
-device virtio-net-pci,netdev=n1,mac=<derived MAC>
```

Every VM on the same site and network joins the same multicast group, so QEMU forwards Ethernet frames between them. The first NIC stays user-mode: egress, DNS and the host port forwards do not change.

- `localaddr=127.0.0.1` keeps the traffic on the loopback interface. Nothing leaves the host.
- The group and port are **derived** from `site` + `clusterNetwork.name` (a stable hash into `239.255.0.0/16` and a port in `20000-29999`). They can be overridden in the spec. Two sites on one host must not share a segment by accident.
- The MAC is **derived** from the machine name, in the same way `machineUUID` is. It is locally administered (`52:54:00:xx:xx:xx`) and stable across `destroy`/`up`, so the node's link configuration can select the NIC by MAC.

Alternatives considered:

- *Linux bridge + tap, or `qemu-bridge-helper`*: needs root or a setuid helper. tinq is deliberately unprivileged.
- *`-netdev socket,listen=/connect=`*: point to point, so it cannot join a third node.
- *`-netdev dgram` unicast pairs*: needs one link per pair of nodes.
- *VDE or passt*: an extra daemon to supervise, or no guest-to-guest path.
- *Multicast on the host's LAN interface*: leaks cluster traffic onto the physical network.

Risk: any local process can join the group and read or inject frames. That is acceptable for a single-user development host and must be written in the README. Some hosts drop multicast on `lo`: `up` must detect a segment that carries no traffic and fail with a clear message (see D7), not hang in etcd join.

### D2. Static addresses on the cluster network

`spec.clusterNetwork` declares:

```yaml
clusterNetwork:
  name: cluster        # segment name within the site; default "cluster"
  cidr: 10.254.0.0/24  # required; the segment; every machine on it must agree
  address: 10.254.0.11 # this machine
  # group: 239.255.x.y, port: 2xxxx  -- optional overrides of the derived pair
```

tinq validates these before it creates anything, following its "refused before the boot" rule:
- the address lies in the CIDR;
- the CIDR does not overlap `10.0.2.0/24` (SLIRP), the pod CIDR or the service CIDR;
- no other machine in the same site's state root already claims the address.

**The machine record (added in task 2.2).** A state directory held no spec, so "another machine of the site" could not be read from it. `create` now writes `<stateDir>/machine.yaml`, a copy of the machine object, right after it creates the directory. The site checks read `<stateRoot>/<site>/*/machine.yaml`, skipping the machine's own directory and its own name. The address, CIDR and host-forward checks use it, and so do the join resolver (D5) and the owner-with-joiners refusal (D7). The record lives and dies with the state directory, so a destroyed machine stops claiming anything. A stopped machine keeps its claim. A VM created before this change has no record and is invisible to the checks. This is acceptable, because such a VM has no cluster network.

Additional refusals, from the same file-only pass: a CIDR with host bits set, a prefix longer than /30, the network address or the broadcast address as `address`, a `group` outside `239.0.0.0/8`, and a `port` outside 1024–65535. Two forwards collide when protocol and port match and the addresses are equal, or when either address is `0.0.0.0`.

The node's link on that NIC gets the static address, selected by the derived MAC. It must use the Talos 1.14 network documents that machinery accepts (for example link and address config documents), **not** `.machine.network.interfaces`. See the conflict validator behavior in Context. The implementer confirms the exact document kinds against machinery v1.14.0 in the first task.

No default route is added on the cluster NIC. Egress stays on the user-mode NIC.

**Confirmed (task 1.1, machinery v1.14.0, Talos v1.14.1 contract).** Two new documents, both validated in metal mode next to the generated set with no warnings:

```yaml
apiVersion: v1alpha1
kind: LinkAliasConfig
name: cluster0
selector:
  match: mac(link.permanent_addr) == "52:54:00:xx:xx:xx"
---
apiVersion: v1alpha1
kind: LinkConfig
name: cluster0
up: true
addresses:
  - address: 10.254.0.11/24
```

The 1.14 contract does not emit these documents, so tinq adds them. It adds no `routes`. The feature needs the 1.14 document model, so `clusterNetwork` is refused for an image older than Talos v1.14.

### D3. Kubernetes and etcd use the cluster network

On a networked VM, the generated config sets:
- the kubelet node IP (`validSubnets` = the cluster CIDR), so `InternalIP` is the cluster address, not `10.0.2.15`. Every SLIRP guest has `10.0.2.15`, so without this all nodes report the **same** InternalIP.
- the etcd advertised subnets to the cluster CIDR;
- Flannel's interface, pinned to the cluster NIC (for example by `--iface-can-reach` to the owner's address, or by interface selection). Otherwise Flannel's VXLAN picks the default-route NIC, and cross-node pod traffic is lost.

**Confirmed (task 1.1).** The 1.14 contract already emits `KubeNodeConfig` (with `nodeIP: {}`) and `KubeFlannelCNIConfig`, and it keeps etcd in v1alpha1. tinq **edits the generated documents**. It does not replace them, because that would drop the labels and backend settings machinery put there:
- kubelet node IP: `KubeNodeConfig.nodeIP.validSubnets: [<cidr>]`;
- etcd: v1alpha1 `cluster.etcd.advertisedSubnets: [<cidr>]`. There is no etcd document in 1.14;
- Flannel: `KubeFlannelCNIConfig.extraArgs: ["--iface-can-reach=<cidr network address>"]`.

Flannel's arguments go into one DaemonSet for the whole cluster, so they cannot carry a per-node address. `--iface-can-reach=<own address>` resolves to `lo`. The **network address** of the CIDR (for example `10.254.0.0`) is the same on every node, and no node can hold it, because D2 refuses a host part of all zeroes. The route lookup for it leaves through the cluster NIC on every node. A `LinkAliasConfig` name is not used for Flannel: it is a Talos-side name, and nothing proves offline that Flannel can see it. Task 6.1 confirms the choice live.

The acceptance check for this decision is behavioral, not textual: pod-to-pod traffic across nodes works, and `kubectl get nodes -o wide` shows distinct cluster addresses.

### D4. The endpoint is the owner's cluster address; the host keeps its forward

For a networked VM **owner**:
- `cluster.controlPlane.endpoint` = `https://<owner clusterNetwork.address>:6443`;
- the API server certificate SANs include that address **and** the host forward address (`127.0.0.1`, or `hostAddr`), so both are valid TLS names. On 1.14 `generate.WithAdditionalSubjectAltNames` already writes to both `machine.certSANs` and `KubeAPIServerConfig.certExtraSANs`, so the cluster address is one more entry in that list (confirmed in task 1.1);
- the **kubeconfig** written for the host keeps the host forward as its server (`https://127.0.0.1:<hostPort>`). The host cannot reach the cluster network;
- `talosconfig` endpoints stay per-node host forwards.

tinq writes the in-cluster endpoint to the owner's state directory as its own artifact (for example `cluster-endpoint`). Joiners read that artifact.

**Trap to avoid:** `joinOptions` takes the endpoint from the owner's kubeconfig (`EndpointFromKubeconfig`). For a VM owner that is the host forward. A joiner configured with it points at `127.0.0.1` inside its own guest, which is itself, and it never finds the cluster. The VM join must read the in-cluster endpoint artifact instead. A test must pin this.

A single-node VM without `clusterNetwork` keeps today's behavior: the endpoint is the host forward.

### D5. `spec.joins` for VMs, sharing the bare-metal join code

A VM declares `spec.joins: <owner metadata.name>`. The bare-metal field stays at `spec.baremetal.joins` and its behavior does not change. Both paths call one resolver, which returns `cluster.JoinOptions`. It is factored out of `joinOptions` in `adopt.go` so both substrates carry the same "secrets bundle or refuse" guarantees.

Refused before anything is created:
- the owner is this machine;
- the owner's state directory has no `secrets.yaml` or no in-cluster endpoint;
- the owner is on a different site or a different cluster network;
- this machine has no `clusterNetwork`;
- the host port forwards collide with another machine of the site. Every VM needs its own `talos-api` forward, because the host reaches each node's Talos API that way.

A joiner joins as a **control-plane** member, which is what config generation produces today and what metal does. It reuses `cluster.joined`: kubeconfig reuse, and the wait for *this* node by its cluster address.

### D6. `spec.hostname`

An optional `spec.hostname` renders the Talos 1.14 hostname document, with automatic hostname generation off, so the Kubernetes node name is the declared name. Without it, the behavior is unchanged: Talos generates a name.

**Confirmed (task 1.1).** The 1.14 contract emits `HostnameConfig` with `auto: stable`. tinq edits that document to `auto: off` with `hostname: <spec.hostname>`, and it validates. `auto: stable` already gives the same name across reboots. It does not give a name the operator chose, and it is not the same across `destroy`/`up`. The consumer schedules CI by `kubernetes.io/hostname`, so the name must be stable across `destroy`/`up`.

### D7. Lifecycle for a cluster of several VMs

- **`up` on a joiner** refuses unless the owner is Running and its API answers. `up` on a stopped cluster therefore starts the owner first; the error message says so.
- **`up` on a stopped, bootstrapped cluster** (found live: every VM of a three-member cluster killed at once). The owner alone is one etcd vote of three, so it has no quorum and its kube-apiserver cannot serve. The first version of this design deadlocked: the owner's `up` waited for a Ready node until it timed out, and a joiner's `up` required the owner's Kubernetes API. Two rules break the cycle:
  - **Which API a joiner asks depends on whether it is already a member.** A joiner with a talosconfig in its state directory applied a config and joined etcd, so it may be part of the quorum. It asks the owner's authenticated Talos API, with the owner's talosconfig, and accepts only the `booting` or `running` stage. A joiner that has never joined still asks the owner's Kubernetes API: booted beside an owner that does not serve, it installs and then waits out an etcd join nobody answers.
  - **The owner does not wait for Kubernetes while a member is stopped.** `up` on the owner resolves the configured members that joined it, directly or through another member, whose VMs are not Running (`StoppedMembers`). When the node refuses a second bootstrap (its etcd exists) and that list is not empty, `up` skips steps 9 and 10, names the stopped members, and succeeds. A node that accepts the bootstrap had no etcd, so nothing can wait on it, and first creation is unchanged. With every member Running the list is empty and `up` waits for every node to be Ready, as before.
  - Readiness is still proved: each joiner's `up` waits until its own node is Ready, which needs quorum. The last member's `up` therefore ends only when Kubernetes answers.
- **Segment check.** Before it applies config, `up` on a joiner checks that the owner's cluster address is reachable from the new node over the segment. Use the Talos API (for example a connectivity check), or at least an ARP or ping through the maintenance API if one is available. It fails with a message about multicast on `lo`, not an etcd timeout.
- **`destroy` on a joiner** first removes the node from etcd (a graceful Talos reset, or `etcd leave`) while the cluster is healthy, then destroys the VM. Otherwise a two-member cluster loses quorum the moment the second member vanishes.
- **`destroy` on an owner** refuses while any joiner's state directory exists on the site, unless the caller passes an explicit `--with-joiners`. That flag destroys joiners first. A forced single destroy still works, and it prints that the joiners are now orphaned.
- **`stop`** of any node is allowed. The README states that stopping one of two members stops the control plane.

### D8. Host capacity

Each VM keeps its own `cpu` and `memory`. tinq checks the sum for the site against host memory before it boots a joiner, and refuses with the numbers when the new total would exceed a declared safety margin (for example 85% of `MemTotal`). A host that swaps turns an etcd heartbeat into a quorum loss, which looks like a Talos bug.

## Risks / Trade-offs

- [Host multicast on `lo` is filtered or unsupported] → The segment check in D7 fails fast with a clear message. The README documents the check, for example `ip maddr show lo` and any firewall rule to allow.
- [Talos 1.14 network documents differ from what this design assumes] → Task 1 confirms the document kinds against machinery v1.14.0 with a config-generation test before any QEMU work. D2 and D3 name the *behavior* required, not the field names.
- [Flannel picks the wrong interface] → D3 requires a cross-node pod-to-pod check in the integration test. A config test alone is not enough.
- [Two control-plane members have no fault tolerance] → The three-node example exists to show quorum survives the loss of one member. Two-node clusters are for isolation and scheduling rehearsals only, and the README says so.
- [Hostname or MAC derivation changes silently on refactor] → Pin each with a test that fixes the value for a known machine name, as for `machineUUID`.
- [State-root scanning for address and port collisions is racy if two `up`s run at once] → This is acceptable. Document it: one `up` at a time per site.

## Migration Plan

This change is additive. Existing machine files have no `clusterNetwork`, `joins` or `hostname`, and must produce byte-identical QEMU arguments and machine configs. A golden test pins that. Consumers opt in per machine file.

## Resolved Questions

- `clusterNetwork.cidr` is **required**. There is no default CIDR. Every machine on a network must declare the same CIDR, so a disagreement is a refusal, not a silent fallback.
