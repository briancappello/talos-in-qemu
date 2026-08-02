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
  and `Destroy` have no honest implementation.
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
`cdrom` — is the flag that identifies it: `client_test.go:929-935` records that
a Talos ISO presents as a read-only virtio-blk device rather than a CDROM, and
so does the squashfs loop device. A table that only flagged CDROM would show
the boot medium as an ordinary candidate disk.

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

1. **The maintenance-mode version tag may be empty.** `WaitMaintenance` already
   calls `c.Version(ctx)` as its probe (`client.go:169`), so the call certainly
   works; what is unverified is whether the response carries a populated tag
   before a config is applied. Fallback: an optional
   `spec.baremetal.talosVersion` override. The step-3 guard already refuses an
   unknown version loudly rather than guessing, so the failure mode is a clear
   refusal, not a bad install.
2. **Real disk serials may be blank** on some consumer NVMe. WWID is then the
   fallback selector; the discovery table shows it, so this surfaces before
   anything is installed rather than after.
3. **DHCP is assumed.** A node that comes up without a lease is unreachable and
   this tool cannot tell that apart from a node that did not boot.

## Out of scope — the next branch

Multi-node is the obvious sequel and this design deliberately does not prepare
for it beyond not blocking it. D2's single derived SAN is the one decision that
would need revisiting: a VIP needs several, and that is an additive field.
