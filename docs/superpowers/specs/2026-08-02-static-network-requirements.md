# Static network configuration — requirements and research

Date: 2026-08-02
Branch: to be created off `feat/lifecycle`
Status: **REQUIREMENTS ONLY.** Not a design. Open decisions are marked and need
a brainstorming pass before a plan is written.

## Why this exists

`tinq adopt` shipped in the baremetal-foundation branch and was proven against a
QEMU node on a LAN address. It cannot yet adopt the machine it was built for,
because that machine's network has no DHCP.

This was recorded as non-goal 5 of the baremetal foundation:

> **Network configuration.** DHCP only. Static addressing, bonds and VLANs have
> nowhere to live in `ConfigInput` and are not being given one yet.

The "yet" has arrived. This document is the evidence and the research so the
next agent does not repeat either.

## The evidence — this is not hypothetical

Measured on 2026-08-02 against the operator's homelab (`~/dev/homelab`):

- `ansible/roles/openwrt/tasks/main.yml:198` — a task literally named *"Disable
  DHCP on homelab interface"*, running `uci set dhcp.lab.ignore='1'`. The lab
  vnet (`192.168.2.0/24`, gateway `192.168.2.1`, OpenWrt port `lan5`) is
  static-only **by design**.
- `ansible/roles/proxmox_host/templates/dnsmasq-homelab.conf.j2` — DNS only.
  `interface=vmbr1`, `bind-interfaces`, upstream forwarding, an internal-domain
  override. **No `dhcp-range`, no `dhcp-host`.**
- `ansible/roles/lxc_container/tasks/main.yml:43` — containers get static
  addresses at creation: `net0: "name=eth0,bridge=...,ip={{ lxc_ip }},gw={{ lxc_gateway }}"`.

So **no network in this homelab serves DHCP** except the OpenWrt trusted and
guest zones, neither of which is where a cluster node belongs. A DHCP
reservation is not available as a workaround, and this holds whether the node
is bare metal or a Proxmox VM.

The target machine's intended identity, from `~/dev/homelab/targets/prod.example.yml`:

```
address: 192.168.2.10/24
gateway: 192.168.2.1
dns:     1.1.1.1
```

Its NIC, read off the live node over COSI while it was in maintenance mode:
`enp1s0`, MAC `84:47:09:47:35:f9`, up with carrier. A second NIC `enp2s0`
(`…:f8`) is down.

## The chicken-and-egg, and why it shapes the design

On a segment with no DHCP, a node booted from the Talos ISO has **no address at
all**. There is therefore no maintenance API to adopt over, and `adopt` cannot
even begin.

The escape is the kernel command line, which configures the **maintenance boot
only**:

```
ip=192.168.2.10::192.168.2.1:255.255.255.0::enp1s0:off
```

(fields: `client::gateway:netmask:hostname:device:autoconf`; set at the ISO's
GRUB menu with `e`, then Ctrl-X)

That gets the node reachable. But the installed system writes its own kernel
cmdline and **inherits nothing from the ISO** — the same fact this repo already
encodes for the console argument (`cluster/config.go`, the `ConsoleArg` option).
So without this feature, `adopt` would apply a config with no network section,
the node would reboot, DHCP would find nothing, and the machine would be
unreachable with no recovery but re-imaging from USB.

**Therefore:** the static address must appear in BOTH places — the kernel
cmdline for the maintenance boot, and the machine config for the installed
system — and they must agree. Whether tinq should help with the first is an
open decision below.

## Verified machinery API — do not re-research this

All confirmed by reading `machinery@v1.13.7` source. There is a supported
generate option; no `PatchV1Alpha1` is required.

**Entry point** — `config/generate/options.go:88`:

```go
generate.WithNetworkOptions(opts ...v1alpha1.NetworkConfigOption)
```

**Option constructors** — `config/types/v1alpha1/v1alpha1_network_options.go`:

```go
v1alpha1.WithNetworkConfig(c *NetworkConfig) n
v1alpha1.WithNetworkNameservers(nameservers ...string) n
v1alpha1.WithNetworkInterfaceIgnore(iface IfaceSelector) n
```

`WithNetworkConfig` takes a fully-built struct and is the one that matters.

**`NetworkConfig`** — `v1alpha1_types.go:798`:

| field | yaml |
|---|---|
| `NetworkHostname` | `hostname` |
| `NetworkInterfaces` | `interfaces` |
| `NameServers` | `nameservers` |
| `Searches` | `searchDomains` |
| `ExtraHostEntries` | `extraHostEntries` |
| `NetworkKubeSpan` | `kubespan` |

**`Device`** (one interface) — `v1alpha1_types.go:1854`. Relevant subset:

| field | yaml |
|---|---|
| `DeviceInterface` | `interface` |
| `DeviceSelector` | `deviceSelector` |
| `DeviceAddresses` | `addresses` |
| `DeviceRoutes` | `routes` |
| `DeviceDHCP` | `dhcp` |
| `DeviceIgnore` | `ignore` |
| `DeviceMTU` | `mtu` |
| `DeviceVIPConfig` | `vip` |

(also `bond`, `bridge`, `bridgePort`, `vlans`, `wireguard`, `dhcpOptions`)

**`Route`** — `v1alpha1_types.go:2254`: `network`, `gateway`, `source`,
`metric`, `mtu`.

**`NetworkDeviceSelector`** — selects a NIC WITHOUT naming it:

| field | yaml |
|---|---|
| `NetworkDeviceHardwareAddress` | `hardwareAddr` |
| `NetworkDevicePermanentAddress` | `permanentAddr` |
| `NetworkDeviceBus` | `busPath` |
| `NetworkDevicePCIID` | `pciID` |
| `NetworkDeviceKernelDriver` | `driver` |
| `NetworkDevicePhysical` | `physical` |

This matters — see open decision 1.

## Requirements

1. A baremetal machine may declare a static address, gateway, and nameservers,
   and the installed system must keep them across the install reboot.
2. Omitting the block must keep today's behaviour exactly: DHCP, no
   `machine.network` section emitted. Every existing QEMU path is a regression
   target.
3. The address a client dials and the address the node is configured with must
   not be able to disagree. See the safety check below — this is the single
   most important requirement in this document.
4. The generated config must be valid for the image's Talos version (the
   existing version-contract machinery already governs this).

## THE SAFETY CHECK THIS FEATURE LIVES OR DIES BY

`adopt` already derives the certificate SAN from `spec.baremetal.endpoint`
(`cluster/up.go`'s `apiAddress`, from the baremetal-foundation branch). If an
operator writes:

```yaml
baremetal:
  endpoint: 192.168.1.186        # where it is reachable TODAY, via DHCP
  network:
    address: 192.168.2.10/24     # where it will be after install
```

…then `adopt` installs, the node reboots onto `192.168.2.10`, and every wait
after step 7 targets `192.168.1.186`, which no longer answers. The bring-up
hangs for the full budget and the node is stranded on an address the operator
must rediscover.

**This is the same defect class the baremetal-foundation branch spent nine tasks
removing** — two fields that must agree, able to be set so they don't. The
branch's answer there was to *derive* rather than duplicate (D2 in
`2026-08-02-baremetal-foundation-design.md`).

Whatever design is chosen, it must make this disagreement impossible or refuse
it loudly BEFORE applying a config. Deriving `endpoint` from the static address
when one is present is the obvious candidate, but note that the maintenance-mode
address and the post-install address are legitimately different when the
operator is adopting from a DHCP segment and moving the box afterwards — so a
naive derivation may be wrong. **This tension is the heart of the design and
should be the first thing brainstormed.**

## Open decisions — brainstorm these before planning

1. **Interface: name or selector?** `enp1s0` is a predictable name but still
   depends on enumeration; `hardwareAddr: 84:47:09:47:35:f9` survives slot and
   firmware changes but is machine-specific and ugly in a manifest. Machinery
   supports both. Recommend considering `deviceSelector` with the MAC as the
   default and a name override, but this is not decided.

2. **Does `up` (QEMU) get static network too, or `adopt` only?** QEMU user-mode
   networking always provides DHCP, so a VM has no need. Adding it anyway
   widens the surface for no current caller — but a spec field that exists on
   only one substrate is its own kind of confusion.

3. **Should tinq emit the kernel cmdline for the operator?** It cannot apply it
   — that is a human at a GRUB prompt. But it knows every value, and printing
   the exact `ip=` line the operator must type would remove a transcription
   error from a step that otherwise strands the machine. Cheap, and in keeping
   with a tool whose transcript is the feature.

4. **How much of `Device` to expose?** Address + gateway + nameservers covers
   this box. Bonds, VLANs, MTU, VIP and wireguard are all reachable through the
   same struct. YAGNI says start with three fields; the counter-argument is
   that VLANs are very likely next given this homelab's topology.

5. **Where does the safety check live?** `cluster/` (which owns the config) or
   `cmd/tinq` (which owns the spec)? Precedent from the last branch: the check
   belongs wherever it CANNOT be bypassed, and refusals happen before anything
   is created.

## Traps carried forward from the baremetal-foundation branch

These cost real review rounds there. They will apply here.

- **Assert against `v1alpha1Doc(t, …)`, never raw config bytes.** `v1alpha1Doc`
  (`cluster/config_test.go`) runs `code()`, which strips comments — and
  machinery's encoder emits commented-out examples. A `strings.Contains` on raw
  bytes matches a comment and reports a field that was never set. This has
  produced false-passing assertions in this repo twice.
- **Test the seam, not just the function.** Three separate tasks last branch
  shipped a correct function that nothing called, and every one passed its own
  tests. If `ConfigInput` gains network fields, assert that `configure()`
  actually threads them — with a fixture value that could not appear by
  accident.
- **`go.mod` / `go.sum` are byte-identical across the whole previous branch.**
  Nothing here should need a new dependency; machinery already has everything.
- **Secrets never reach a log or an error.** `talosconfig`, `kubeconfig` and
  `secrets.yaml` are secret; the package has `redact()` / `redactErr()`.
- **The QEMU path is the regression that matters.** `examples/bootstrap-machine.yaml`
  must keep working unchanged, and there is a live gate for it (see below).

## Validation

Unit tests in the existing suites, plus a live gate. The previous branch's gate
is the model and the environment is already proven:

1. **QEMU regression** — `tinq up examples/bootstrap-machine.yaml`, ten steps to
   Ready, no `machine.network` section in the generated config.
2. **Static config renders** — assert the generated `machine.network` against a
   parsed config, not a substring.
3. **The disagreement refusal** — a spec whose `endpoint` and static address
   conflict must be refused before anything is applied.
4. **Live, on the real box** — the operator's machine is available: boot from
   USB on `lan5` with the `ip=` cmdline, adopt with the static block, and
   confirm the node comes back at `192.168.2.10` after the install reboot **and
   that `kubectl` reaches it there**. That last clause is the whole feature; a
   node that installs and never returns is the failure this exists to prevent.

## Context the next agent needs

- Design spec of the branch this builds on:
  `docs/superpowers/specs/2026-08-02-baremetal-foundation-design.md` — read D1,
  D2 and D5 in particular.
- Its plan and task ledger:
  `docs/superpowers/plans/2026-08-02-baremetal-foundation.md` and
  `.superpowers/sdd/progress.md` (gitignored; contains every review finding).
- `cluster/config.go` — `ConfigInput` and `GenerateConfig`; the network option
  goes in the `genOpts` slice beside the console-arg append.
- `cluster/up.go` — `UpOptions`, and `apiAddress` which derives the cert SAN.
- `cmd/tinq/adopt.go` — `spec.baremetal` parsing and `adoptMachine`'s pre-flight.
- `crd/talosmachine.yaml` — the `baremetal` block and its two CEL rules; a new
  sub-block will need schema and probably a rule.

## Out of scope

Bonds, VLANs, wireguard, VIP, multi-interface, and IPv6 — unless decision 4
pulls VLANs in. Reconfiguring a node that is already installed: `cluster/` is
bootstrap-only and that boundary is deliberate.
