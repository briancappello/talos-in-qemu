# TinQ — Talos in QEMU

Kubernetes nodes on macOS as **real VMs running the real production OS**, driven
by a Kubernetes custom resource. No Docker. No root. No nested virtualization.

```
tinq -apply examples/bootstrap-machine.yaml
```

Boots a [Talos Linux](https://www.talos.dev) control-plane VM on
Hypervisor.framework via QEMU, with the Talos API forwarded to `127.0.0.1:50000`.
Cold boot to "machine is reachable" measured at **~10 seconds** on an M5 Max.

## Why this exists

`kind` is excellent and it needs a Docker-API container runtime — `docker`,
`podman`, or `nerdctl`. On macOS that means a Linux VM shim (Docker Desktop,
Colima, Rancher, `podman machine`) whose only job is to host the runtime that
hosts the nodes. You maintain a VM to pretend you don't have one.

You also get a node that is a **container sharing one kernel** with every other
node. That is fine until what you are testing is kernel-adjacent — netfilter and
conntrack behavior, sysctls, MTU, offload paths, NAT64, kernel version skew,
CNI datapaths. Then "node" and "shares a kernel with the other nodes" are in
tension, and the thing you were trying to test is the thing the substrate
elides.

TinQ takes the other branch: each node is a VM with its own kernel, running
Talos — an immutable, API-driven Kubernetes OS that many people also run in
production. `minikube`'s hyperkit driver is deprecated; Lima and Colima are
Docker-shaped by design. So on macOS this niche is mostly empty.

**Not a `kind` replacement for everything.** A container node starts faster and
uses less memory, and if you are testing an application on Kubernetes, `kind` is
probably the right tool. TinQ is for when the *node* is part of the test.

## Install

Requires macOS on Apple silicon.

```sh
brew install qemu siderolabs/talos/talosctl
go install github.com/coglative/tinq/cmd/tinq@latest
```

Then fetch a Talos `nocloud` image and drop it where TinQ resolves profile names
(default `~/.hvf/images`):

```sh
mkdir -p ~/.hvf/images
# from https://github.com/siderolabs/talos/releases — the arm64 nocloud image
gunzip -c nocloud-arm64.raw.gz > ~/.hvf/images/talos-nocloud.img
```

## Use

A node is a `TalosMachine`:

```yaml
apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata:
  name: clvc-cp0
spec:
  site: clvc-local          # a path component in the state dir — see "Cleanup"
  role: talos-cp
  image: talos-nocloud.img  # resolved under -image-root when not absolute
  cpu: 4
  memory: 6Gi
  disk: 20Gi
  hostForwards:
    - { hostPort: 50000, guestPort: 50000 }   # Talos API
    - { hostPort: 6443,  guestPort: 6443 }    # Kubernetes API
```

Two ways to reconcile it:

```sh
# BOOTSTRAP: one machine from a file, no control plane needed
tinq -apply  machine.yaml
tinq -destroy machine.yaml

# CONTROLLER: watch TalosMachine resources in a cluster
kubectl apply -f crd/talosmachine.yaml
tinq --kubeconfig ~/.kube/config
```

`-apply` exists because of a chicken-and-egg: a controller needs a control plane
to read resources from, and on a fresh laptop the control plane is the thing you
are trying to create. The usual escape is a `kind` cluster — dragging in a
container runtime purely to bootstrap a hypervisor that doesn't need one. So
`-apply` reads one resource from disk and runs it through the **same driver** the
controller loop uses: identical `Observe`/`Create`/`Destroy`, identical QEMU
invocation, identical state layout. Only the source of the resource differs.
Anything else would be two ways to build a machine, and they would drift.

Once the first node is bootstrapped it can host the CRD and TinQ itself, and
every machine after that arrives the normal way.

## Unprivileged by construction

QEMU **user-mode networking** (SLIRP), so no `vmnet`, no `tap`, no bridge, no
`sudo`. `hostForwards` is how the host reaches the guest.

The tradeoff is real and worth stating: user-mode networking gives each VM NAT'd
egress and forwarded ingress, and **VMs cannot reach each other**. For a single
node, or nodes that only need to be reachable from the host, that is fine. For a
multi-node topology where nodes must be L2-adjacent, QEMU's `socket`, `dgram`
and `hubport` backends provide unprivileged VM-to-VM links — TinQ does not model
them yet (see Status).

## How it works

`driverkit` (174 lines) is the whole controller contract — three verbs:

```go
type Driver interface {
    Observe(ctx, *unstructured.Unstructured) (exists bool, status map[string]any, err error)
    Create (ctx, *unstructured.Unstructured) error
    Destroy(ctx, *unstructured.Unstructured) error
}
```

`Observe` must ask the **external system**, never a local state file. TinQ reads
the pidfile QEMU itself wrote and checks liveness, because a state file happily
reports a long-dead VM as present — that is the bug the signature exists to
prevent.

To support another hypervisor or cloud, implement those three verbs. Everything
else — the finalizer, the reconcile loop, status publication, delete ordering —
is `driverkit`'s.

## Cleanup

`spec.site` is a **path component** in the state directory:

```
~/.hvf/<site>/<uid>/{system.qcow2,efivars.fd,qemu.pid,serial.log}
```

Artifacts carry the identity they belong to, so they can be found and swept
without a registry to consult — the same property that makes cloud labels and
tags work. `Destroy` takes the whole unit: the process and the state directory,
idempotently.

## Status

Working and exercised:

- `-apply` / `-destroy`, including re-apply (`Observe` reports present, so it
  will not start a second QEMU against the same state directory)
- Talos boots on HVF; cold boot to reachable ~10s; Talos API via `hostForwards`
- Controller mode against a cluster with the CRD installed
- `Destroy` sweeps process + state directory

Not done yet — stated plainly rather than implied:

- **No one-command cluster.** After `-apply` you still run `talosctl gen config`,
  `apply-config`, `bootstrap`, `kubeconfig` by hand. Wrapping that is the next
  thing, and it is what would make TinQ genuinely competitive with
  `kind create cluster` on ergonomics.
- **No multi-node topology.** One NIC on user-mode networking; no VM-to-VM
  links, so no multi-node cluster and no simulated switch fabric. The QEMU
  backends needed (`socket`/`hubport`) are unprivileged, so this is a modeling
  gap in the resource, not a platform limit.
- **Apple silicon only.** `qemu-system-aarch64` with `accel=hvf` is hardcoded.
- **No tests.** The QEMU invocation is verified by running it, which is not the
  same thing.

## License

Not yet chosen — do not depend on this until a LICENSE file lands.
