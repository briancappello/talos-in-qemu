# Why `-up` disables kexec on macOS

A single-node bring-up on Apple silicon fails part way through roughly six times
in ten, always at the same place: step 7 announces `installing... rebooting...`
and then waits out its whole budget for a node that never comes back.

The cause is Talos's **kexec** reboot path, which under QEMU on macOS/arm64 dies
in the guest. `-up` therefore asks the node not to use it:

```yaml
machine:
  sysctls:
    kernel.kexec_load_disabled: "1"
```

That is one line, it is deterministic, and it is what upstream's own
`talosctl cluster create` sets on main today. The rest of this document is the
evidence, because the sysctl looks arbitrary without it.

## Symptom

```
[ 6/10] config        wrote controlplane.yaml, talosconfig, secrets.yaml
2026/…  up …: gave up waiting for the authenticated Talos API at 127.0.0.1:50000:
        context deadline exceeded
```

`installTimeout` is ten minutes and a healthy node returns in 45s–1m4s, so this
is not a tight budget. The node is dead, not slow.

## Where it dies: inside kexec

Talos does not reboot through firmware on the happy path — it kexecs straight
into the kernel it just installed. Every run, wedged ones included, reaches
`kexec_core: Starting new kernel`, and the failure lands between that line and
the new kernel's banner:

| | `task reboot` → `kexec` | `Linux version` banners | outcome |
|---|---|---|---|
| 4 vCPU, wedged | 57.5s → **81.7s** (24.2s) | **1** — new kernel never ran | died inside kexec |
| 1 vCPU | 21.8s → **28.2s** (6.5s) | **2** — new kernel booted | clean |

Two `Linux version` lines and one UEFI banner is the signature of a HEALTHY
kexec: two kernels, one firmware boot. A wedge has one of each.

On arm64 `machine_kexec()` must offline the secondary CPUs before it can turn
off the MMU and jump. That is the 24-second gap, and it is the only thing that
differs between those two rows. `rcu_exp_par_gp_` — the task the fault usually
lands in — is an RCU expedited grace-period worker, which is exactly what
CPU-offline synchronisation goes through.

## It presents at least three ways

The console dies with the kernel, so there is no single string to grep for:

| variant | console |
|---|---|
| stack-overflow panic | `Kernel panic - not syncing: kernel stack overflow`, `pc : efi_runtime_fixup_exception+0x34/0x120` |
| NULL-deref | oops truncated mid-dump, ends at `FSC = 0x04: level 0 translation fault` — **no panic line ever printed** |
| hang | nothing further at all |

The panic variant is itself a nested fault: `lr` is `__do_kernel_fault`, so a
fault had already happened, and `efi_runtime_fixup_exception` — the handler that
asks "were we inside an EFI runtime service?" — faults again and overflows the
stack.

This variety is worth recording because it is a trap for anyone trying to detect
the failure rather than prevent it.

## Measured, 16 bring-ups

| vCPU | ✅ | ❌ | died in kexec | notes |
|---|---|---|---|---|
| 4 | 4 | **6** | most runs | 60% wedge at step 7 |
| 2 | 2 | 1 | **every run** | usually survives: the guest reboots itself and comes up anyway |
| 1 | 4 | 0 | **never** | clean kexec, fastest (89–140s) |

The `cpu=2` row explains the confusing early data: at two or more vCPUs the
guest usually still dies in kexec, the kernel reboots ITSELF afterwards, and the
node comes up regardless — a successful `-up` with a crash in `serial.log`. A
wedge is the case where it is too broken to manage even that.

## Why the sysctl, and not the alternatives

**`machine.sysctls` reaches the right kernel.** Talos applies machine-config
sysctls in MAINTENANCE MODE, so `kernel.kexec_load_disabled=1` lands on the
ISO's running kernel before the install and therefore before the reboot it has
to change. machined then reports kexec support disabled via sysctl and reboots
through firmware instead. Confirmed working on v1.13.5 by the reporter of
siderolabs/talos#13769.

Ruled out:

- **`machine.install.extraKernelArgs`** configures the INSTALLED system. In a
  failed kexec that system never boots, so `efi=noruntime` and friends are
  unreachable. The ISO's own cmdline comes from its GRUB config.
- **`RebootRequest_POWERCYCLE`.** The mode exists and is Talos's documented
  skip-kexec path, but there is no seam for it here: in maintenance mode the
  single `ApplyConfiguration(mode=REBOOT)` call is what drives
  install-then-reboot, and `NO_REBOOT`/`STAGED` skip the install entirely.
- **Fewer vCPUs.** It works — 4/4 clean at `cpu: 1` — but a one-vCPU control
  plane is a permanent tax to dodge an intermittent bug, and `cpu: 2` produced a
  different failure at step 9. That is a diagnosis, not a fix.
- **Detect-the-crash and power-cycle the VM.** This was built and it worked: the
  install completes before the guest dies, so the disk is bootable and
  `bootindex=0` makes it win over the ISO. It was removed in favour of the
  sysctl, which is deterministic, saves the ~24s spent dying, and needs none of
  the "the install already succeeded, so boot order saves us" reasoning. Also,
  detecting it is genuinely hard — see the three variants above.
- **Bumping Talos for a newer 6.18.y.** Not a path to a fix. The kernel-side
  change needed is 6.19-based.

**Kexec is only disabled on macOS/arm64.** It works under KVM and skips a whole
firmware boot, so disabling it more widely would be a tax paid for one platform's
bug. Both halves of the gate are load-bearing: the bug is arm64's, and upstream
gates its own workaround on the architecture too (`TargetArch == "arm64"` in
talosctl, `GOARCH == "arm64"` in machined), so an Intel Mac has nothing to work
around and keeps the boot it saves.

`up.go` keys the decision on `platform.Platform.OS` and `.ImageArch`, which is
why those fields exist rather than `runtime.GOOS`/`GOARCH` reads — a workaround
for a host the test binary is not running on has to be provable, and all three
cases are, from one Mac: `TestUpLeavesKexecAloneOnLinux`,
`TestUpLeavesKexecAloneOnAnIntelMac`, `TestUpDisablesKexecOnAppleSilicon`.

## Scope

Talos v1.13.7 (kernel 6.18.39-talos), QEMU 11.0.2 with
edk2-stable202408-prebuilt, `-machine virt -cpu host -accel hvf` on darwin/arm64
(macOS 26.6, Apple M-series).

**QEMU-on-macOS-specific.** This matches upstream: arm64 kexec worked fine on
AWS in smira's testing, and it does not reproduce on Linux/KVM.

## When this can be removed

Two things have to land, and neither is imminent:

1. Talos re-enabling kexec on arm64 for `cluster create` — a revert of
   `cf3eb1cad1ee` (2026-06-08), which is the SECOND time upstream disabled it.
   #12396 and #12402 did it first in December 2025; #13265 re-enabled it in May
   2026 on the belief that the kernel was fixed; cf3eb1cad1ee turned it off again
   eight weeks later. Upstream has been round this loop twice, which is the
   reason to wait for their third answer rather than guess at it.
2. `dd4d71f587f3` reaching whatever kernel Talos ships. Talos main is still on
   6.18.41, and the fix is 6.19-based, so a 6.18.y patch bump will not bring it.

Until both are true, the sysctl stays. The tell that it is no longer needed is
upstream dropping it from `talosctl cluster create`, not anything observable
here.
