# Baremetal foundation: node facts, and a verb that adopts hardware

Date: 2026-08-02
Branch: `feat/lifecycle`
Status: designed, not yet implemented.

## Goal

Make `cluster/` describe a Talos **node** rather than a QEMU **guest**, and add
one verb that brings up a node this tool did not create.

Today `cluster.Up` is a correct Talos bring-up sequence wearing a QEMU
assumption in five places. Four of the five are invisible under QEMU because
QEMU makes them true by construction: the guest's architecture equals the
host's, the API is always reached at loopback, and the boot medium is always a
local ISO file. On hardware every one of those is false, and each fails in a way
that points somewhere other than its cause.

The outcome is a `tinq adopt <machine.yaml>` that takes an already-booted
maintenance-mode node at a real address and drives it to a Ready single-node
cluster, using the same ten-step sequence `up` uses.

## Non-goals

- **Multi-node.** `config.go:174` emits only `machine.TypeControlPlane` and
  `Up` bootstraps exactly one etcd. Workers, VIP, extra control-plane members
  and etcd join are a different feature, not a port of this one.
- **PXE / netboot.** The node is booted by hand from USB or virtual media.
- **BMC, power control, Wake-on-LAN.** Nothing here powers a machine on.
- **A baremetal `driverkit` driver.** With no power control, `Create`, `Stop`
  and `Destroy` have no honest implementation, and none was written. What
  shipped instead — after the review found that the controller reaches the
  driver without passing any of D4's CLI refusals — is the qemu driver
  DECLINING on all four methods: `Observe` reports `Running` so the loop
  converges on doing nothing, `Create` and `Stop` log a refusal and return nil
  so a tick cannot spin forever, and `Destroy` forgets the machine rather than
  deleting the only credential that reaches it. Four no-ops that make a
  controller leave hardware alone are not a driver for it; `tinq adopt` is
  still the only verb that acts.
- **Network configuration.** DHCP only. Static addressing, bonds and VLANs have
  nowhere to live in `ConfigInput` and are not being given one yet.

## Context: the five couplings, and why only one of them is visible

Verified by reading the tree at `3d00924`.

| # | Location | Coupling |
|---|---|---|
| 1 | `config.go:78,167-168` | `const loopback = "127.0.0.1"` is the ONLY value in `certSANs` and `endpointList` |
| 2 | `up.go:465` | `ConsoleArg` for the NODE derived from the HOST's `runtime.GOARCH` |
| 3 | `up.go:459` | `disableKexec` keyed on the HOST's OS/arch |
| 4 | `version.go`, `up.go:203` | Talos version parsed from a local ISO9660 volume id |
| 5 | `config.go` | No network configuration surface at all |

Only #1 fails loudly. It fails at the TLS handshake, on every authenticated
call, with an error about certificates that says nothing about the config that
generated them.

#2 and #3 share a root cause worth naming, because it is the reason this is a
refactor and not three patches: **`platform.Platform` is a host-facts struct
carrying two guest hints** (`ConsoleArg`, `ImageArch`). Under QEMU that is
sound — the README requires the image architecture to match the host, so host
facts and node facts are the same facts. Baremetal breaks the identity, and
nothing in the type system notices. A Mac driving an amd64 node would write
`console=ttyAMA0` into that node's kernel cmdline and disable kexec on hardware
that has no kexec bug.

`up.go:103` already claims this package "knows nothing about qemu". `up.go:12`
imports `platform` and `up.go:197` prints `host.QEMUBinary`. The claim is
aspirational; D1 makes it true.

## Decisions

### D1 — `cluster/` stops importing `platform`

`UpOptions` loses two fields and gains five.

Removed:

- `ImagePath string` — its only uses were the step-2 transcript line and
  feeding the ISO version probe. Deleting it is what actually severs the
  dependency, because it was the last field only a local disk image could fill.
- `Detect func() (*platform.Platform, error)` — host facts leaking in.

Added:

- `TalosVersion string` — resolved by the CALLER.
- `VersionSource string` — transcript only. `"talos-v1.13.7-amd64.iso (ISO
  volume id)"` or `"the node's maintenance API"`.
- `Substrate string` — the pre-rendered step-1 line.
- `ConsoleArg string` — `""` means emit no console kernel argument at all.
- `DisableKexec bool` — the caller decides.

`hooks.detectVersion` is deleted with `ImagePath`.

**Why the caller resolves the version.** It is what lets both substrates share
one sequence without a branch. QEMU reads the ISO before booting anything;
`adopt` asks the node, which is already booted. Both still refuse at step 3
before anything is spent — QEMU because nothing has booted, baremetal because
this tool did not do the booting.

### D2 — The certSAN is DERIVED from the endpoint, not configured beside it

`const loopback` is deleted. `ConfigInput` gains one field — `APIAddress
string`, the address the client dials — which feeds both
`WithAdditionalSubjectAltNames` and `WithEndpointList`. `Up` fills it by
splitting `TalosEndpoint`'s host part with `net.SplitHostPort`.

The certificate must name the address the client dials. `TalosEndpoint` **is**
the address the client dials. Deriving one from the other makes disagreement
structurally impossible, which is the defect class this whole document exists
to close — a separate `certSANs` field would restore exactly the ability to set
them inconsistently.

Cost, accepted: no second SAN, so no hostname and no VIP. Both are additive
fields on the day multi-node arrives, and neither has a caller today.

Under QEMU this derives `127.0.0.1` and the generated config is unchanged.
`config_test.go:394` currently asserts the literal; it gets inverted to assert
the derivation.

### D3 — A `baremetal` spec block, and its presence is the discriminator

```yaml
spec:
  baremetal:
    endpoint: 192.168.1.50      # required; ports are Talos defaults
    systemDiskSerial: "..."     # optional on first run — see D4
    dataDiskSerial: "..."       # optional; absent = no user volume, no StorageClass
    consoleArg: ""              # optional; default NONE — see D5
```

Ports are not configurable: they are Talos's own defaults on the node itself,
already spelled `talosAPIGuestPort` / `kubeAPIGuestPort` in `main.go:317`. So
`endpoint: 192.168.1.50` yields `TalosEndpoint 192.168.1.50:50000` and
`KubeEndpoint https://192.168.1.50:6443`. There is no forward to describe,
which is why `hostForwards` has no meaning here.

The QEMU provisioning fields — `image`, `cpu`, `memory`, `disk`, `dataDisk`,
`hostForwards` — are unused on a baremetal machine. `dataDiskSerial` replaces
`dataDisk` as the single field gating both halves of storage, preserving the
property `up.go:412` depends on: the user volume and the StorageClass read one
field and therefore cannot disagree.

**Strengthened as shipped: unused became REFUSED, for all six.** This design
said "unused", and the plan's task 7 drafted one CEL rule for one field —
`!(has(self.baremetal) && has(self.hostForwards))`. The rule that shipped
rejects a machine carrying `spec.baremetal` alongside any of `image`, `cpu`,
`memory`, `disk`, `dataDisk` or `hostForwards`, and `cmd/tinq/crd_test.go`
holds the CRD to what the Go code reads from it.

"Unused" is a silent discard, and a manifest carrying both shapes then reads as
configuration that is in effect when none of it is — an operator who writes
`disk: 500Gi` on a hardware machine has said something about the install that
nothing anywhere honours. The six are one class, so refusing only
`hostForwards` would have left the other five discarded silently. The price is
paid on migration: converting a VM manifest to a hardware one means DELETING
those fields, not merely adding `spec.baremetal`. One explicit edit, against a
discard that never announces itself.

### D4 — `adopt`, and four verbs that refuse

`tinq adopt <machine.yaml>` requires `spec.baremetal`. `apply`, `up`, `stop`
and `destroy` refuse a machine that has it, each naming `adopt`.

`destroy` refusing is deliberate and is the decision most likely to annoy. Its
contract is "takes the entire SCC, NOT RECOVERABLE". On hardware it can only
take the artifacts — including the sole talosconfig that can reach a node it
has no way to destroy. A verb that half-honours its contract while deleting the
only credential to the surviving machine is worse than one that refuses. The
honest fix is a `forget` verb; it has no caller yet, so it is not built.

Consequence, accepted: clearing a baremetal state dir is `rm -rf` until
`forget` exists. The refusal message says so.

### D5 — `ConsoleArg` defaults to EMPTY on baremetal

Real hardware has a firmware-configured console and usually a display. The
current code cannot express "no console argument": `config.go:161` always
passes `WithInstallExtraKernelArgs([]string{in.ConsoleArg})`. Empty must skip
the option entirely.

Serial console becomes opt-in through `spec.baremetal.consoleArg`, which is the
correct default direction: a wrong console argument is silent, and it is silent
at exactly the boot you would need it for.

**Tier 1, same defect class.** `config.go:197` disables
`InstallGrubUseUKICmdline` whenever machinery has set the field. Its entire
reason for existing (`config.go:188-196`) is that GRUB's UKI cmdline and
`extraKernelArgs` cannot coexist — so it must fire only when a console argument
is ACTUALLY being passed. Today with no console arg it would still switch off a
node's UKI cmdline for nothing.

### D6 — `DisableKexec` is always false on baremetal

The workaround exists for one substrate: QEMU on macOS/arm64, where the kexec
path dies in the guest (`up.go:445-459`). It is computed in `cmd/tinq` from
`platform.Detect()` for QEMU machines and hardcoded false for baremetal ones.

### D7 — Disk discovery reuses a hardware-proven path

`cluster/` gains a maintenance-mode disk listing built on
`safe.StateListAll[*block.Disk]` over the COSI client —
the same call `client_test.go:847` already makes against a real node. It is not
new capability, it is an existing call given an exported caller.

`adopt` pre-flight, before `cluster.Up` is entered:

1. wait for maintenance
2. list disks
3. read the Talos version from the node
4. call `Up` with `Boot` returning `(0, nil)`

Two refusals share one table renderer:

- no `systemDiskSerial` — print the table, refuse, say to write one down
- `systemDiskSerial` matches no disk — print the same table, refuse, name it as
  a typo

The second is the realistic failure and is the reason discovery is not a
separate query verb: nothing would force you to run it. The same match check
applies to `dataDiskSerial` when present.

The table shows serial, model, pretty size, transport and rotational, and flags
CDROM and readonly. The boot medium WILL appear in it, and `readonly` — not
`cdrom` — is the flag that identifies it: a Talos ISO presents as a read-only
virtio-blk device rather than a CDROM, and so does the squashfs loop device. A
table that only flagged CDROM would show the boot medium as an ordinary
candidate disk.

**Measured on a live node, 2026-08-02** (v1.13.7, maintenance mode, five
disks), on the risk-1 rehearsal VM at `127.0.0.1:50002` — `disk: 10Gi`,
`dataDisk: 20Gi`, which is why `vda` reads 11 GB here and 22 GB in the README's
sample. That table is a different machine: the adopt rehearsal, built from
`examples/adopt-machine.yaml` at `disk: 20Gi`, `dataDisk: 20Gi`. Both are
single real runs; `PrettySize` is `humanize.Bytes`, so 10 GiB renders 11 GB and
20 GiB renders 22 GB.

```
  DEVICE   SERIAL         MODEL          SIZE     NOTES
  loop0    (none)                        84 MB    readonly — probably the medium you booted from
  sr0      (none)         QEMU DVD-ROM   0 B      readonly — …, cdrom, sata
  vda      talos-system                  11 GB    rotational, virtio
  vdb      (none)                        336 MB   readonly — …, rotational, virtio
  vdc      talos-data                    22 GB    rotational, virtio
```

`vdb` is the Talos ISO attached as virtio-blk: **readonly, and NOT cdrom**. Only
`sr0` carries the cdrom flag, and it is the empty q35 DVD device, not the boot
medium. The same run reproduced why selection is by serial and not by
elimination: `disk.serial == "talos-data"` matched exactly `[vdc]`, while
`!system_disk && !disk.cdrom` matched `[loop0 vdb vdc]` — three disks, two of
them wrong.

Note also that every disk without a serial renders `(none)` and is therefore
structurally unselectable: `RequireDisk` compares `d.Serial == serial` and
refuses an empty serial up front, so no install can target the boot medium.

Nothing is filtered out of the table. The safety property here is not
exclusion, it is that no install can proceed until a human has written a serial
into the file — so the table's job is to make the boot medium *recognisable*,
not to guess which entries are ineligible.

Auto-selection by size was rejected. `config.go:184` already calls it "a coin
flip once there are two large disks", and on hardware the losing side of that
flip overwrites a disk that may hold data — the one failure here that re-running
cannot repair.

## Testing

Unit, in the existing suites:

- certSAN derived from `TalosEndpoint` (inverts `config_test.go:394`)
- `extraKernelArgs` omitted entirely when `ConsoleArg` is empty
- `InstallGrubUseUKICmdline` untouched when no console arg is passed
- both disk refusals, and the table renderer
- version and substrate lines rendered from `UpOptions`, through `upHooks`
- each of the four verb refusals

Live, both required before the hardware attempt:

1. **Regression.** `tinq up` against loopback, behaviour unchanged.
2. **Rehearsal.** `tinq adopt` against a QEMU VM publishing its Talos API on
   `192.168.1.165` — this host's LAN address — using the existing
   `spec.hostForwards[].hostAddr` field (`main.go:874`), which already defaults
   to loopback and already accepts an override.

The rehearsal is the gate that matters. It exercises the non-loopback certSAN,
the adopt verb, the no-op `Boot`, disk discovery and both refusals, with no
sudo, no bridge and no tap device. It leaves unproven only what hardware alone
can show: a real NIC taking a DHCP lease, and real disk serials.

## Risks and unverified assumptions

1. ~~**The maintenance-mode version tag may be empty.**~~ **RESOLVED
   EXPERIMENTALLY, 2026-08-02.** A Talos v1.13.7 node booted from ISO and left in
   maintenance mode — no config applied — reports `v1.13.7` from
   `NodeVersion`. Measured against a live QEMU/KVM node at `127.0.0.1:50002`
   via `TINQ_NODE=… go test ./cluster -run TestAgainstARealNode`:
   `the node reports Talos v1.13.7`, 18.1s after boot.

   `spec.baremetal.talosVersion` therefore stays an OPTIONAL override rather
   than becoming required. It is kept because a future Talos could change the
   maintenance-mode response, and because the step-3 guard refuses an unknown
   version loudly rather than guessing — so the failure mode was always a clear
   refusal, never a bad install.
2. **Real disk serials may be blank** on some consumer NVMe. WWID is then the
   fallback selector; the discovery table shows it, so this surfaces before
   anything is installed rather than after.
3. **DHCP is assumed.** A node that comes up without a lease is unreachable and
   this tool cannot tell that apart from a node that did not boot.
4. **The maintenance-mode apply is UNVERIFIED AND UNPINNED, and is no longer
   loopback-only.** `MaintenanceClient` sets `InsecureSkipVerify: true`
   (`client.go`), which was structurally bounded before this branch: the
   endpoint was always the host side of a loopback forward. D2 and D3 make it
   the node's own LAN address for `adopt`, and `hostForwards[].hostAddr` can
   publish `up`'s forward on a LAN too. What crosses that channel is
   `applyConfiguration`'s payload — five certificate authorities and the
   machine token. An attacker who can answer at `<endpoint>:50000` is handed
   the cluster's CA private keys; one in the middle can read them and edit the
   config the node installs.

   No fix is proposed and none is available: this is exactly what `talosctl
   apply-config --insecure` does, and before the config lands there is no trust
   anchor to verify against. TOFU pinning would pin a certificate the node
   generates fresh at every boot. The mitigation is operational — adopt over a
   directly-attached segment or a trusted lab LAN — and the risk is recorded
   here, in `client.go`'s comment and in the README's rough edges rather than
   left implicit in a `//nolint:gosec`.

## Out of scope — the next branch

Multi-node is the obvious sequel and this design deliberately does not prepare
for it beyond not blocking it. D2's single derived SAN is the one decision that
would need revisiting: a VIP needs several, and that is an additive field.
