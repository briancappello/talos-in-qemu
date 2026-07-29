# Talos in QEMU (TinQ)

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
go install github.com/coglative/talos-in-qemu/cmd/tinq@latest
```

Then fetch a Talos **ISO** and drop it where TinQ resolves profile names
(default `~/.hvf/images`):

```sh
mkdir -p ~/.hvf/images
curl -Lo ~/.hvf/images/talos-v1.9.5.iso \
  https://github.com/siderolabs/talos/releases/download/v1.9.5/metal-arm64.iso
```

Note the version — you will pin the installer to it below, and that pin is not
optional.

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
  image: talos-v1.9.5.iso   # resolved under -image-root when not absolute
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

## From a booted VM to a cluster

Not wrapped in a command yet (see Status), but this is the exact sequence, and
every flag in it is load-bearing — each one corresponds to a way it fails.

```sh
tinq -apply machine.yaml          # ~5s to Talos maintenance mode

cat > patch.yaml <<'YAML'
machine:
  install:
    # SELECT BY SIZE, never /dev/vdX. Enumeration is decided by qemu arg order,
    # and the read-only boot ISO is also a virtio disk — name it and you may
    # install onto the install media.
    diskSelector:
      size: '> 10GB'
    # PIN THE INSTALLER TO THE ISO'S VERSION. Unset, it defaults to talosctl's
    # own version, silently turning a fresh install into a cross-version
    # upgrade. Then nothing fits: a config generated for the newer version is
    # REJECTED by the older maintenance system that has to apply it, and a
    # config for the older one gets installed as the newer, which can hang at
    # /sbin/init with no console output.
    image: ghcr.io/siderolabs/installer:v1.9.5
    # The installed system writes its OWN kernel cmdline and does NOT inherit
    # the ISO's console=ttyAMA0, so it goes silent on serial at exactly the
    # moment you need to watch it boot.
    extraKernelArgs:
      - console=ttyAMA0
YAML

talosctl gen config mycluster https://127.0.0.1:6443 \
  --talos-version v1.9.5 --additional-sans 127.0.0.1 \
  --config-patch @patch.yaml --output-dir . --force

talosctl apply-config --insecure -n 127.0.0.1 -e 127.0.0.1 -f controlplane.yaml
# installs (~25s), reboots, and now boots from DISK because of bootindex

export TALOSCONFIG=$PWD/talosconfig
# BOOTSTRAP WHILE THE NODE IS `booting`, NOT `running`. Waiting for running is
# circular: the node cannot reach running until etcd is bootstrapped.
talosctl -n 127.0.0.1 -e 127.0.0.1 bootstrap        # silent on success
talosctl -n 127.0.0.1 -e 127.0.0.1 kubeconfig ./kubeconfig --force

KUBECONFIG=$PWD/kubeconfig kubectl get nodes -w
```

Measured on an M5 Max: maintenance ~5s, install ~25s, Talos API ~20s after
reboot, `running` ~10s after bootstrap, node registered ~70s, `Ready` ~30s later
— roughly **3 minutes cold to Ready**.

Two probes that look right and are not: a TCP connect to a forwarded port
succeeds even when nothing listens in the guest (qemu accepts on the host), and
`talosctl version` always prints the *client's* tag. Use `talosctl get
machinestatus` for liveness.

### If you plan to run workloads

Talos is not kind, and three defaults differ:

- Control-plane nodes are **tainted**. A single-node cluster schedules nothing
  until `cluster.allowSchedulingOnControlPlanes: true`.
- There is **no StorageClass**. kind bundles rancher local-path; install it
  yourself if anything wants a PVC.
- **PodSecurity is enforced** (`baseline`, only `kube-system` exempt). Anything
  using `hostPath` — including local-path's own helper pod — needs its namespace
  labelled `pod-security.kubernetes.io/enforce=privileged`.

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

- **A real cluster, end to end.** Single-node control plane, Kubernetes v1.36.1
  on Talos v1.9.5, kernel 6.12.18-talos arm64, containerd 2.0.3, node `Ready`,
  with Crossplane and a real workload serving HTTP on it. ~3 minutes cold.

Not done yet — stated plainly rather than implied:

- **No one-command cluster.** After `-apply` you still run the sequence above by
  hand. Wrapping it is the next thing, and it is what would make TinQ genuinely
  competitive with `kind create cluster` on ergonomics.
- **TCP-only host forwards.** `hostForwards` emits `hostfwd=tcp:` only, so a
  UDP service (QUIC, WebTransport, DNS) has no path from the host. Multi-protocol
  forwards are a small change and not yet made.
- **Newer ISOs may not boot.** v1.9.5 boots in ~5s here; the v1.13.4 ISO hangs at
  `executing /sbin/init` at 199% CPU with no console output and no API, on a blank
  disk with 5.9 GB free. Uninvestigated. Pin the installer to whatever ISO you
  find boots rather than assuming newer is safer.
- **No multi-node topology.** One NIC on user-mode networking; no VM-to-VM
  links, so no multi-node cluster and no simulated switch fabric. The QEMU
  backends needed (`socket`/`hubport`) are unprivileged, so this is a modeling
  gap in the resource, not a platform limit.
- **Apple silicon only.** `qemu-system-aarch64` with `accel=hvf` is hardcoded.
- **No tests.** The QEMU invocation is verified by running it, which is not the
  same thing.

## License

MIT — see [LICENSE](LICENSE).
