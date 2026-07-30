# Cluster Bring-Up — Design

Date: 2026-07-30
Branch: `feat/cluster-bringup` (branch 2 of 2, stacked on `feat/platform-abstraction`)
Status: approved for implementation

## Goal

Turn a booted Talos VM into a working single-node Kubernetes cluster with one
command, transparently enough that the operator learns Talos rather than
trusting a black box.

```sh
tinq -up examples/bootstrap-machine.yaml
# ... 9 announced steps ...
export KUBECONFIG=~/.hvf/<site>/<uid>/kubeconfig
kubectl get nodes
```

## Non-goals

- **Multi-node clusters.** Structurally impossible today: one NIC on QEMU
  user-mode networking, no VM-to-VM path. Requires `socket`/`hubport` backends
  and CRD modelling first.
- **Cluster lifecycle management** — upgrades, scaling, node replacement. This
  is bootstrap only. See "Architectural boundary" below; that scoping is what
  keeps this legitimate.
- **Replacing provider-talos.** Steady-state Talos management remains its job.
- **Replicated storage.** Single-node hostPath only.

## Architectural boundary (the question this design had to answer first)

The repo contains two claims that do not agree, and this feature sits on the
seam between them.

`crd/talosmachine.yaml:3-7`:

> Everything Talos-side (secrets, machine config, config-apply, bootstrap,
> kubeconfig, Image Factory schematics) is crossplane-contrib/provider-talos.
> This owns only the thing no project anywhere provides: a QEMU/HVF virtual
> machine.

`crd/talosmachine.yaml:76-85` on `machineConfigSecretRef`: *"We consume it; we
never generate it."* — declared in the CRD, **zero Go implementation**.

`README.md:293`:

> **No one-command cluster.** After `-apply` you still run the sequence above by
> hand. Wrapping it is the next thing.

The README never mentions provider-talos or Crossplane.

**Resolution: this extends the existing `-apply` escape hatch, and is scoped to
bootstrap only.**

`main.go:59` already documents a deliberate exception to the controller
contract: *"a controller needs a control plane to read resources from, and on a
fresh laptop the control plane is the thing you are trying to create."*

Cluster bring-up is the same seam one layer up. provider-talos runs **inside** a
Kubernetes cluster as a Crossplane controller, so it cannot create your first
cluster — you would need `kind` (the tool this project exists to avoid) to
bootstrap the thing that bootstraps Talos. The CRD's boundary governs the
**controller** contract, which is unchanged: controller mode still consumes
`machineConfigSecretRef` from provider-talos in steady state.

Discipline this imposes: `-up` must stay bootstrap-only. The moment it grows
upgrade or scaling verbs it has become a competing cluster manager and the CRD
comment becomes false.

For reference, provider-talos (6 stars, 52 commits, v0.2.0, self-described
"work in progress") manages: Machine Secrets, Machine Configuration,
Configuration Apply, Bootstrap, Cluster Health, Cluster Kubeconfig, Image
Factory Schematic — the CRD comment's list verbatim. It integrates with the
same Talos Go SDK this design adopts.

## Decision: link `pkg/machinery`, do not shell out to `talosctl`

Measured, not assumed:

| | Link `pkg/machinery` | Shell out to `talosctl` |
|---|---|---|
| `go.sum` modules | **45 → 84** | unchanged |
| External install | none | user must install, and can change it underneath us |
| Version location | pinned in `go.mod` | ambient on `PATH` |

The API surface needed is small: `ApplyConfiguration`, `Bootstrap`,
`Kubeconfig`, plus `Version`/`ServiceInfo` for readiness. All present and
building.

The decisive point is that **`talosctl` *is* machinery compiled into a binary** —
`talosctl gen config --talos-version` runs the same `VersionContract` code. So
the choice does not change version semantics at all; it only changes where the
version lives. Linking makes the mandatory version guard (below) a comparison
against a compile-time constant instead of a subprocess parse against a moving
target.

Pin machinery to **v1.13.7**, matching the ISO verified to boot.

## The version guard (mandatory, and the reason is a silent failure)

`config.VersionContract` is documented as **backwards-only**:

> Config generation only supports backwards compatibility (e.g. Talos 0.9 can
> generate configs for Talos 0.9 and 0.8). Matching version of the machinery
> package is required to generate configs for the current version of Talos.

Verified with machinery v1.13.7 (`gendata.VersionTag == "v1.13.7"`) — the
contract genuinely changes output:

```
target      bytes    kubePrism  hostDNS
v1.0.0      20833    false      false
v1.9.5      25788    true       true
v1.13.7     25873    true       true
```

Machinery v1.13.7 carries contracts `TalosVersion1_0` through
`TalosVersion1_13` — one per minor release up to its own. So **the pin is a
floor, not a ceiling**: the newest machinery supports every older ISO. Pinning
an old machinery would be exactly backwards.

**The failure that makes the guard mandatory:**

```
target v1.99.0 -> 25881 bytes, NO ERROR
                  (identical to the nil/current contract)
```

This was also confirmed against a real version gap rather than only a synthetic
one: machinery **v1.9.5** asked to target **v1.13.7** likewise produced a config
with no error and no warning. That is the exact scenario a contributor hits by
pinning an older machinery and using a current ISO, and nothing in the library
surfaces it.

`ParseContractFromVersion` accepts a version it has never heard of, every
`contract.XxxSupported()` predicate returns true because 99 outranks
everything, and a plausible-looking config is generated for a Talos that does
not exist. No warning. Same silent-failure class this project designs against
elsewhere.

So: detect the ISO's Talos version, compare against machinery's, and **refuse
loudly** when the image is newer than the generator. Branch 1 already parses the
ISO; the volume ID reads `TALOS_V1_13_7` directly.

## Command shape

`-apply` stays VM-only. `-up` is `-apply` plus bring-up. `-destroy` is unchanged
and must keep working without a hypervisor or a reachable node.

Output is the teaching surface — every step announces the operation, and the
non-obvious ones announce the reason:

```
[ 1/10] platform      linux/amd64, kvm, qemu-system-x86_64
[ 2/10] image         talos-v1.13.7-amd64.iso -> v1.13.7 (ISO volume id)
[ 3/10] version guard machinery v1.13.7 >= image v1.13.7  ok
[ 4/10] boot          pid 163166, api 127.0.0.1:50000
[ 5/10] maintenance   reachable after 11s
[ 6/10] config        wrote controlplane.yaml, talosconfig
                        diskSelector: serial talos-system
                        installer: ghcr.io/siderolabs/installer:v1.13.7 (pinned to YOUR image)
                        extraKernelArgs: console=ttyS0 (this host's serial)
                        userVolume: local-path-provisioner on serial talos-data
[ 7/10] apply-config  installing... rebooting... api back after 24s
[ 8/10] bootstrap     etcd bootstrapped (fired while 'booting', not 'running' —
                        waiting for 'running' deadlocks: it cannot reach running
                        until etcd exists)
[ 9/10] kubeconfig    node Ready after 68s
[10/10] storage       local-path-provisioner v0.0.31, default StorageClass
                        root /var/mnt/local-path-provisioner (Talos root is read-only)
                        namespace local-path-storage labelled privileged
```

Step 10 is skipped, with a printed note, when `spec.dataDisk` is unset. The
`UserVolumeConfig` in step 6 is likewise only emitted when `dataDisk` is set,
so the two halves of storage cannot disagree.

## Readiness probes (two that look right and are not)

1. **A TCP connect to a forwarded port succeeds even when nothing listens in the
   guest** — QEMU accepts on the host. Use a real Talos API call
   (`Version`/`ServiceInfo`), not a dial.
2. **`talosctl version` prints the client's tag**, not the node's.
3. **Bootstrap must fire while the node is `booting`, not `running`.** Waiting
   for `running` deadlocks — the node cannot reach `running` until etcd is
   bootstrapped.

## Artifact layout

Generated artifacts live in the existing per-machine state directory, beside
`system.qcow2` and `serial.log`:

```
~/.hvf/<site>/<uid>/{talosconfig,controlplane.yaml,kubeconfig,secrets.yaml}
```

This follows the repo's existing GC contract — artifacts carry the identity they
belong to, so `-destroy` sweeps them with everything else, and secrets do not
outlive the cluster. The exact `export` lines are printed at the end so the
paths never have to be hunted for.

Permissions: `0600` for `talosconfig`, `kubeconfig` and `secrets.yaml`.

## Named disks

`spec.image` resolution and the install target both currently depend on
heuristics. Replace the heuristic with an identity.

QEMU sets a serial on each virtio-blk device; Talos selects on it:

```
-device virtio-blk-pci,drive=sys,serial=talos-system,bootindex=0
-device virtio-blk-pci,drive=data,serial=talos-data

install:
  diskSelector:
    serial: talos-system
```

Verified: `qemu-system-x86_64 -device virtio-blk-pci,help` lists `serial=`, and
a two-disk VM with distinct serials starts. `InstallDiskSelector.Serial` exists
in machinery and maps to `/sys/block/<dev>/serial`.

This replaces the README's `size: '> 10GB'`, which was a heuristic that only
worked while there was exactly one large disk. Adding a data disk would have
made it a coin flip between the OS target and the data disk — the same class of
error the README warns about for `/dev/vdX`, arriving through a different door.

**The boot ISO is also a virtio-blk device**, which is why a bare
`!system_disk` selector is insufficient. Talos's disk spec carries a
`cdrom: bool` field for exactly this reason.

## Storage

New optional CRD field, beside the existing `disk`:

```yaml
spec:
  disk:     20Gi   # system — Talos install target
  dataDisk: 40Gi   # PVCs
```

Omitting `dataDisk` yields today's behaviour: one disk, no StorageClass, nothing
breaks. `-apply` as a plain VM launcher is unaffected.

With `dataDisk` set, `-up` provisions a Talos **user volume** on it and installs
`rancher/local-path-provisioner` with the three patches Talos's own local-storage
guide specifies:

1. root path `/opt/local-path-provisioner` → `/var/mnt/local-path-provisioner`
   — **Talos's root filesystem is read-only**; `/opt` is not writable, `/var` is
   the EPHEMERAL partition. The stock manifest simply fails without this.
2. mark `local-path` the default StorageClass, so an ordinary PVC with no
   `storageClassName` binds.
3. label namespace `local-path-storage` with
   `pod-security.kubernetes.io/enforce: privileged`.

Manifests are applied through the `client-go` already in `go.mod` — no
`kubectl`, `kustomize` or `helm` requirement on the host. The provisioner
version is pinned in code and printed at bring-up.

**Why a separate disk rather than a directory on `/var`:** PVCs would otherwise
share the EPHEMERAL partition with etcd, container images and logs. A runaway
PVC can then wedge etcd on the only control-plane node — a failure that presents
as "the cluster stopped" with nothing pointing at the cause. A separate volume
bounds the blast radius, and it teaches Talos's real disk model.

## The three hardened defaults

A freshly bootstrapped Talos cluster is production-shaped, not a permissive
sandbox. Three defaults differ from `kind` and each is decided deliberately:

- **Control-plane taint — removed** (`allowSchedulingOnControlPlanes: true`).
  Not a security weakening but a topology correction: in production there would
  be worker nodes. On a single-node cluster the taint means nothing can ever
  schedule.
- **PodSecurity — left enforced** at `baseline`. Genuine production behaviour
  and a real difference from `kind`. Bring-up prints what it means and how to
  label a namespace.
- **StorageClass — installed** when `dataDisk` is set (above). Talos's own docs
  label the provisioner's namespace privileged, and every real cluster runs
  privileged CSI drivers in an infra namespace, so this is production-realistic
  rather than a compromise.

## Risks and unverified assumptions

- **`disk.serial` in a CEL volume selector is unconfirmed.** The disk spec
  carries `serial` and `talosctl get disks` displays it, and
  `disk.transport`/`disk.rotational`/`disk.size` are documented CEL variables —
  but no doc example uses `disk.serial`. **Implementation must verify this
  empirically** with `talosctl get disks` against a two-disk VM before relying
  on it. Fallback: `!system_disk && !disk.cdrom`. The *install* selector is not
  affected; it uses the v1alpha1 `serial` field, which is confirmed.
- **macOS remains unverified**, inherited from branch 1.
- **Talos v1.13.7 verified to boot on Linux/KVM** (API reachable, TLS handshake
  returned `CN=maintenance-service.talos.dev`). The README's "newer ISOs may not
  boot" bullet was observed on macOS/HVF/aarch64 and does not apply here; it
  should be corrected when docs are next touched.
- **PVC data does not survive `-destroy`.** Correct for a laptop cluster, and
  stated at bring-up rather than discovered.
- **`+36 modules` in `go.sum`.** Real cost, accepted for the version-guard
  benefit. Worth flagging in the upstream PR description.
- **Bring-up is not resumable mid-flight in v1.** A failure part-way leaves a
  VM and a state dir; recovery is `-destroy` and retry. Resumability is
  deliberately deferred until the step boundaries have proven stable.

## Out of scope, deliberately

Worker nodes, HA control planes, upgrades, cluster health beyond node-Ready,
Image Factory schematics, custom CNI, and any steady-state reconciliation. Each
of those pushes `-up` from a bootstrap escape hatch toward a cluster manager,
which is the line the architectural boundary above depends on.
