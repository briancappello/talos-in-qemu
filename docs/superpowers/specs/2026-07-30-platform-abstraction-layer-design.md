# Platform Abstraction Layer — Design

Date: 2026-07-30
Branch: `feat/platform-abstraction` (branch 1 of 2)
Status: approved for implementation

## Goal

Make TinQ run unmodified on any host QEMU supports, detecting the host
architecture and hypervisor at runtime instead of hardcoding Apple silicon.
Concrete targets today are macOS/arm64 and Linux/amd64; the design must not
assume either.

## Non-goals

- Cluster bring-up. That is branch 2 and gets its own spec.
- Multi-node topology. Structurally impossible today: one NIC on QEMU
  user-mode networking, no VM-to-VM path.
- Cross-architecture emulation. The guest arch always equals the host arch.
- TCG fallback. Explicitly rejected — see "Accelerator" below.

## Context: what is macOS-only today

`cmd/tinq/main.go` is 425 lines and hardcodes Apple silicon in four places.
`driverkit/driverkit.go` is platform-neutral and is not touched.

| Site | Current value | Why it breaks off macOS/arm64 |
|---|---|---|
| `main.go:280` | `-machine virt,accel=hvf` | x86_64 QEMU has **no `virt` machine** (only `pc`/`q35`) and **no `hvf` accel** |
| `main.go:319` | `qemu-system-aarch64` | wrong binary for an amd64 host |
| `main.go:356` `edk2Code()` | Homebrew paths only | no Linux path; silently returns a nonexistent path on failure |
| `main.go:369` `makeEFIVars()` | pads vars to 64 MiB | fatal on x86_64 (see below) |

Verified on Arch Linux, QEMU 11.0.2, x86_64:

```
$ qemu-system-x86_64 -machine help | grep -E '^(virt|q35)'
q35   Standard PC (Q35 + ICH9, 2009)          # no `virt`
$ qemu-system-x86_64 -accel help
tcg mshv nitro kvm                            # no `hvf`
```

`makeEFIVars` carries two independent defects, both invisible on macOS:

1. `strings.Replace(edk2Code(), "code.fd", "vars.fd", 1)` does not match Arch's
   `OVMF_CODE.4m.fd` (uppercase `CODE`, `.4m.` infix). It returns the **code**
   path unchanged, so firmware would be loaded as the variable store.
2. The 64 MiB pad is correct on aarch64 only by coincidence — edk2's aarch64
   `QEMU_VARS.fd` genuinely is 67108864 bytes. On x86_64 the vars template is
   540672 bytes, and padding it produces:

```
qemu-system-x86_64: combined size of system firmware exceeds 8388608 bytes
```

Everything else in the QEMU invocation was tested unchanged on q35/KVM and
works: the pflash pair, `virtio-blk-pci` + `bootindex` boot ordering,
`if=none,media=cdrom`, user-mode netdev with both tcp and udp `hostfwd`,
`-daemonize`, `-pidfile`, `-serial file:`. Host listeners appeared on
`50000/tcp` and `51000/udp`; firmware reached BdsDxe.

## Architecture

New package `platform/`, one implementation file plus tests. `main.go` calls
`Detect()` once inside `create()` and reads fields.

```go
type Platform struct {
    QEMUBinary   string // qemu-system-x86_64 | qemu-system-aarch64
    Machine      string // q35 | virt
    Accel        string // kvm | hvf
    CPU          string // host
    FirmwareCode string // read-only pflash
    FirmwareVars string // nvram template, copied VERBATIM — never padded
    ConsoleArg   string // console=ttyS0 | console=ttyAMA0  (guest hint, branch 2)
    ImageArch    string // amd64 | arm64                    (guest hint)
}

func Detect() (*Platform, error)
```

### Why a struct of facts, not build tags

Build tags (`platform_darwin.go` / `platform_linux.go`) were considered and
rejected. They mean the macOS path is **not compiled** when building on Linux:
no type checking, no test execution, no compile error until someone builds on a
Mac — on a repo with zero tests, on a branch intended for an upstream PR. The
variance is four values plus a firmware lookup. A runtime `switch` on
`runtime.GOOS`/`runtime.GOARCH` keeps every path compiled and testable on every
host.

Having `platform` build the QEMU arg slice was also rejected. This repo keeps
the invocation in one readable block, and the `bootindex` comment at
`main.go:285-306` is load-bearing documentation sitting with the args it
explains. Splitting that across a package boundary makes the code worse.

## Resolution

### 1. Architecture

`runtime.GOARCH` mapped into QEMU's vocabulary:

| GOARCH | QEMUBinary | Machine | ConsoleArg | ImageArch |
|---|---|---|---|---|
| `amd64` | `qemu-system-x86_64` | `q35` | `console=ttyS0` | `amd64` |
| `arm64` | `qemu-system-aarch64` | `virt` | `console=ttyAMA0` | `arm64` |

Any other GOARCH is an explicit "unsupported host architecture" error naming
what was detected, not a silent fallthrough.

### 2. Accelerator — two stages, because the failures differ

`-cpu` and `accel` are **coupled** and must be resolved together. Verified:

```
-machine q35,accel=tcg  -cpu host  ->  CPU model 'host' requires KVM or HVF
-machine virt,accel=tcg -cpu host  ->  unable to find CPU model 'host'
-machine q35,accel=tcg  -cpu max   ->  boots
```

Stage 1 — is it compiled in? Parse `qemu-system-X -accel help`.
Stage 2 — is it usable now? Linux: `/dev/kvm` exists and opens RW. macOS: HVF.

`CPU` is set to `host` only when a hardware accelerator is confirmed.

**No TCG fallback.** Talos under emulation is slow enough to be
indistinguishable from a hang, and would present exactly like the v1.13.4 ISO
failure the README documents as uninvestigated. A silent fallback converts
"your host cannot do this" into "TinQ is broken". Failure is loud and names
which of the three cases applies:

```
no hardware accelerator available on linux/amd64

  /dev/kvm exists but is not readable by uid 1000
  fix: sudo usermod -aG kvm $USER   (then log out and back in)

TinQ requires KVM (Linux) or HVF (macOS). Talos under TCG
emulation is slow enough to be indistinguishable from a hang.
```

Distinguishing *absent* from *permission-denied* from *not-compiled-in* is the
part that earns its keep; "enable hardware acceleration" alone does not tell
the user which case they are in.

No `-accel tcg` opt-in flag until someone needs it (YAGNI). It is a one-line
addition if CI demands it later.

### 3. Firmware — registry first

QEMU ships a machine-readable firmware registry (`docs/interop/firmware.json`,
the same one libvirt consumes). Scan `/etc/qemu/firmware` then
`/usr/share/qemu/firmware`, lexically by basename — lower numeric prefix means
higher priority.

```json
// 60-edk2-ovmf-x86_64-4m.json
"mapping": {
  "device": "flash",
  "executable":     { "filename": "/usr/share/edk2/x64/OVMF_CODE.4m.fd" },
  "nvram-template": { "filename": "/usr/share/edk2/x64/OVMF_VARS.4m.fd" }
},
"targets": [{ "architecture": "x86_64", "machines": ["pc-i440fx-*","pc-q35-*"] }]
```

This is why distro portability is solved rather than approximated: Debian's
`OVMF_CODE_4M.fd`, Fedora's `ovmf/OVMF_CODE.fd` and SUSE's
`ovmf-x86_64-code.bin` all self-describe through the same schema. We query it
instead of maintaining a path table that rots.

Accept an entry only when **all** hold:

- `interface-types` contains `uefi`
- `mapping.device == "flash"`
- `targets[].architecture` matches
- some `targets[].machines` glob matches our machine type
- `features` contains **neither** `requires-smm` **nor** `secure-boot`

The last filter is not hypothetical. On Arch, `50-edk2-ovmf-x86_64-secure-4m.json`
sorts **first** by priority and carries `requires-smm` + `secure-boot`, needing
`-machine q35,smm=on` and more. `60-edk2-ovmf-microvm-4m.json` is
`device: memory`, not `flash`. Take-the-first-match selects a broken entry.

`nvram-template` is the file to copy **verbatim**. It is 67108864 bytes on
aarch64 and 540672 on x86_64 — so both `makeEFIVars` defects delete, and vars
creation becomes an `io.Copy`. Correct on both arches for the same reason,
rather than correct on one by accident.

### 4. Firmware — fallback table

When no registry is found (expected on Homebrew), fall back to a static table:
the current Homebrew paths, plus common pre-descriptor Linux locations. If both
strategies fail, error listing **every path tried** — unlike today's
`edk2Code()`, which returns a nonexistent path and lets QEMU emit a confusing
downstream error.

## Image architecture inspection

Booting an arm64 image on an amd64 host yields a UEFI screen, no bootable
media, no console output and no API — a silent hang indistinguishable from a
real bug. The guard makes that self-identifying.

### Methods that look correct and are not

Verified against real Talos v1.9.5 `metal-amd64.iso` and `metal-arm64.iso`:

| Method | amd64 ISO | arm64 ISO | Verdict |
|---|---|---|---|
| Filename | — | — | weak; repo's own example is `talos-nocloud.img` |
| ESP boot filenames | `BOOTX64.EFI` **and** `BOOTAA64.EFI` | both | Talos ships both stubs; reports dual-arch |
| Whole-file PE histogram | `{0x8664:4, 0xaa64:2}` | `{0x8664:3, 0xaa64:3}` | ambiguous |
| `ARM\x64` magic @0x38 | absent | **absent** | arm64 kernel is an EFI stub, not a raw Image |
| `HdrS` magic (aligned) | 1 | 0 | valid *negative* x86 test only |

Both ESP stubs are real PE binaries with correct-but-contradictory machine
types (`BOOTX64.EFI` 241664 B / 0x8664; `BOOTAA64.EFI` 266240 B / 0xaa64),
inside the *same* amd64 image.

### The method that works

```
ISO9660 PVD @ sector 16, verify "CD001" at +1
  -> root directory extent from PVD+156
    -> "BOOT" directory -> child prefixed "VMLINUZ"
      -> seek extent; read MZ, e_lfanew, "PE\0\0", machine u16
         0x8664 -> amd64      0xaa64 -> arm64
```

Verified both directions: amd64 ISO → `0x8664`, arm64 ISO → `0xaa64`. Reads
about 8 KB, not the 100 MB file. ~60 lines of Go, no dependency.

### Boundaries

- **ISO9660 only.** `spec.image` also permits raw disk images; those need
  GPT+FAT parsing and are out of scope.
- **`unknown` is never an error.** No PVD, no kernel, no PE header, unreadable
  → skip the guard silently. A detector that fails closed would break every
  valid image it does not understand.
- **Warn, never fail**, and only on a confident mismatch:

```
warning: image is arm64 but host is amd64
  the VM will start, reach UEFI, find no bootable media,
  and sit there with no console output and no API.
  this is not a hang — it is the wrong image.
```

## Integration points in `main.go`

1. `create()` calls `platform.Detect()` once, early; error aborts before any
   state directory or disk is created.
2. `-machine virt,accel=hvf` → `p.Machine + ",accel=" + p.Accel`; `-cpu` → `p.CPU`.
3. `edk2Code()` deleted; pflash reads `p.FirmwareCode`.
4. `makeEFIVars()` becomes `io.Copy` from `p.FirmwareVars`. The 64 MiB buffer
   and the `code.fd`→`vars.fd` replace are removed.
5. `exec.Command("qemu-system-aarch64", ...)` → `p.QEMUBinary`.
6. After image path resolution, call the arch inspector and warn on mismatch.
7. `nested()` at `main.go:416` is dead code; remove it (Tier 3, separate commit).

## Testing

First tests in the repo. All run on any host without booting a VM.

- **Firmware matcher** — table-driven over fixture descriptors: the
  `requires-smm` priority trap, the `device: memory` microvm trap, priority
  ordering, arch mismatch, machine-glob matching, empty registry → fallback,
  everything missing → error naming all paths tried.
- **Arch mapping** — GOARCH → quad, including the unsupported-arch error.
- **Accelerator messages** — the three failure cases produce distinct,
  actionable text.
- **ISO inspection** — hand-built minimal ISO9660 fixtures of a few KB,
  committed: amd64, arm64, not-an-ISO, ISO-without-kernel, truncated.
- **Integration** — against the real ISOs in `~/.hvf/images/`, skipped when
  absent so the suite stays runnable everywhere. The ISOs are ~200 MB and are
  not committed.

## Risks and unverified assumptions

- **The macOS path is unverified.** It is written from best guesses per
  explicit decision; a macOS agent will validate later. In particular it is
  **not confirmed** whether Homebrew's QEMU ships `firmware/*.json` — the
  fallback table exists precisely because it probably does not. Every
  macOS-specific assumption carries a `TODO(macos-verify):` comment to grep for.
- **Only Arch Linux is verified.** Debian, Fedora and SUSE layouts are handled
  via the registry, which they all ship, but are untested here.
- **Firmware registry search order** follows the QEMU spec's documented
  directories. Some consumers also read `$XDG_CONFIG_HOME/qemu/firmware`; not
  implemented until needed.
- **Talos ISO layout is version-coupled.** `/BOOT/VMLINUZ.` held for v1.9.5. If
  a future layout differs, detection returns `unknown` and the guard silently
  disables — degrades safely by design.

## Out of scope — branch 2

Cluster bring-up: config patch generation (`diskSelector` by size, installer
pinned to the ISO version, arch-correct `extraKernelArgs` console), `talosctl
gen config`, `apply-config`, the reboot wait, `bootstrap` timed while the node
is `booting` not `running`, `kubeconfig`, and readiness polling. It consumes
`ConsoleArg` and `ImageArch` from this layer, which is what gives those two
fields a real caller.
