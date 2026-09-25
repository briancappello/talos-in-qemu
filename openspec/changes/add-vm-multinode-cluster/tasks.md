## 0. Ground Rules For The Implementer

- The branch is `feat/secureboot`. It has uncommitted operator files (a modified `README.md`, the `*-HANDOFF.md` files, several `examples/*.yaml`). Check `git status` first. Do not commit, revert or reformat anything this change did not create. Work in a new branch or worktree from the current HEAD, and ask the operator which one.
- Target Talos **v1.14.1** (the latest release) with the pinned machinery v1.14.0. ISOs live in `~/.hvf/images`.
- Run `go test ./...` before any change, and record the baseline. Report baseline failures separately from new ones.
- Port 18080 on the operator's host is used by another service. Integration examples use other host ports.
- The existing single-node path must not change. Task 1.2 pins it.

## 1. Confirm The Talos 1.14 Config Model

- [x] 1.1 With config-generation unit tests (no QEMU), find the machinery v1.14.0 documents for four things: a static address on a NIC selected by MAC, the kubelet node IP subnets, the etcd advertised subnets, and a declared hostname with automatic hostnames off. Also find how Flannel's interface is pinned. Verify each document passes validation next to the documents machinery generates. Record the chosen kinds in `design.md` under D2, D3 and D6.
- [x] 1.2 Add a golden test: an existing single-node machine file produces byte-identical QEMU arguments and machine config before and after this change. Verify that it passes on the unchanged tree.

## 2. Schema And Validation

- [x] 2.1 Add `spec.clusterNetwork` (required `cidr` and `address`, optional `name` defaulting to `cluster`, optional `group`, `port`), `spec.joins` and `spec.hostname` to `crd/talosmachine.yaml`. Verify with the CRD schema tests (`cmd/tinq/crd_test.go`).
- [x] 2.2 Implement the refusals from the `vm-cluster-network` spec (address outside the CIDR, overlaps, duplicate address, CIDR disagreement) as file-only checks that run before any state directory exists. Verify with one unit test per refusal, and assert that no state directory was created.
- [x] 2.3 Implement host-forward collision detection across the machines of a site. Verify with a unit test naming the port and both machines.

## 3. The Cluster NIC

- [x] 3.1 Derive the multicast group and port from site and network name, and the MAC from the machine name. Verify with tests that pin the values for fixed inputs and show that different sites and names differ.
- [x] 3.2 Add the second `-netdev socket,mcast=...,localaddr=127.0.0.1` NIC to the QEMU arguments when `clusterNetwork` is set. Verify with a `main_test.go` argument test. Verify on Linux with two VMs in maintenance mode that one can reach the other's address.
  - Unit part: `TestCreateAddsTheClusterNIC`. Live: verified with configured nodes rather than maintenance mode (the cluster NIC has no address before the config), by etcd peering, cross-node pod traffic and the ARP probe. See `evidence.md`.
- [x] 3.3 Render the static address, the node IP, the etcd subnet and the CNI interface pinning from task 1.1. Verify with config-generation tests.

## 4. Endpoint And Join

- [x] 4.1 For a networked owner, set the in-cluster endpoint to the owner's cluster address, add both addresses to the certificate SANs, keep the host forward in the kubeconfig, and write the in-cluster endpoint artifact. Verify with config tests, and with a live check that `kubectl` works from the host.
  - Live: verified, see `evidence.md` 6.1 and 6.2.
- [x] 4.2 Factor the join resolver out of `adopt.go` `joinOptions` so that `spec.baremetal.joins` and `spec.joins` share it. For VMs it reads the in-cluster endpoint artifact, never the kubeconfig server. Verify that the existing `adopt_test.go` suite still passes, and add a test that a VM joiner never gets `127.0.0.1` as its endpoint.
- [x] 4.3 Implement the join refusals (self, missing owner secrets or endpoint, site or network mismatch, no cluster network). Verify with one unit test per refusal.
- [x] 4.4 Implement the segment connectivity check before a joiner's config is applied. Verify it by blocking multicast on `lo` in a test environment, or with an injectable check in unit tests plus one manual live run, recorded in the change.

## 5. Hostname And Lifecycle

- [ ] 5.1 Render `spec.hostname`. Verify live that the Kubernetes node name matches, and that it survives `destroy` and `up`.
- [x] 5.2 `up` on a joiner refuses while the owner is not Running with an answering API. Verify with a unit test using a fake owner state.
- [ ] 5.3 `destroy` on a joiner removes the etcd member before destroying the VM. `destroy` on an owner refuses while joiners exist, unless the explicit flag is given. Verify both with unit tests, and live with a two-node cluster: etcd shows one member afterwards and API writes succeed.
- [ ] 5.4 Add the host memory check before a joiner boots. Verify with a unit test using an injected `MemTotal`.

## 6. Live Acceptance On Linux

Record the commands and sanitized results in `openspec/changes/add-vm-multinode-cluster/evidence.md`.

- [x] 6.1 Two-node cluster from `examples/`: both nodes Ready, distinct `INTERNAL-IP`s on the cluster CIDR, two etcd members with cluster-address peer URLs, and a cross-node pod-to-pod connection.
- [x] 6.2 Three-node cluster: stop any one member, then show quorum and a successful `kubectl` write. Start it again and show three healthy members.
- [x] 6.3 Each node's egress still works through its user-mode NIC (for example an image pull from a public registry), and each node's Talos API answers through its own host forward.
- [ ] 6.4 A single-node machine file without the new fields comes up as before. The golden test from 1.2 still passes.

## 7. Documentation And Hand-Back

- [ ] 7.1 Add two- and three-node examples under `examples/`, and a README section covering the cluster network, the loopback multicast requirement and its check, the two-member quorum warning, and the lifecycle rules. Verify the examples with the same file validation `up` runs.
- [ ] 7.2 Write the consumer contract into `evidence.md`, for the homelab repository. Verify that each item is a field name or command a consumer can use as written:
  - the new fields;
  - where the in-cluster endpoint and kubeconfig live;
  - the order of `up` for owner and joiners;
  - that `homelab/cluster_context.py` must accept `spec.joins` for VM profiles (it rejects it today).
