# seed/bootstrap — physical seed OS installer

The seed is the one node installed by hand; everything else bootstraps from it.
This builds a **headless, unattended Debian 13 USB installer** for the physical
seed host, so the base OS is reproducible instead of a hand-clicked install.
Once it comes up with SSH, `ansible-playbook site.yml --limit seed-metal` takes
over and makes it the seed (registry, CA, DNS, netboot, etc.).

## What it produces

`seed-metal-autoinstall.iso` — a Debian 13.6 netinst repacked to auto-boot the
text installer (BIOS **and** UEFI, Secure Boot OK — shim-signed), which:

- wipes the **single NVMe** (first detected disk) and installs a minimal system;
- brings the NIC up via **DHCP** (no static — gmtek owns `.10` on both segments;
  the box is found by MAC `9C:6B:00:DC:C0:88`);
- installs `openssh-server python3 sudo`, drops your `~/.ssh/id_ed25519.pub` into
  `root`'s authorized_keys, sets `PermitRootLogin prohibit-password`;
- reboots into a reachable, key-only root SSH box named `seed-metal`.

## Use

```sh
./build-usb-installer.sh              # -> seed-metal-autoinstall.iso (+ prints a break-glass root pw)
lsblk -o NAME,SIZE,MODEL,TRAN         # identify the USB stick
sudo dd if=seed-metal-autoinstall.iso of=/dev/sdX bs=4M oflag=direct status=progress conv=fsync
# boot the seed from USB (one-time boot menu) -> unattended install -> reboot
for i in $(seq 1 254); do ping -c1 -W1 192.168.1.$i >/dev/null 2>&1 & done; wait
ip neigh | grep -i 9c:6b:00:dc:c0:88  # its DHCP IP
ssh root@<ip>
cd .. && ansible-playbook site.yml --limit seed-metal
```

Overrides: `PUBKEY=`, `SRC_ISO=`, `OUT_ISO=` env vars.

## Notes / decisions

- **DHCP, not static.** Deterministic-by-MAC discovery instead of a baked IP that
  could collide (gmtek). Pin a reservation / final address later.
- **Headless auto-boot** is done by overwriting `isolinux/isolinux.cfg` (BIOS) and
  `boot/grub/grub.cfg` (UEFI) with a zero-menu entry that appends
  `auto=true priority=critical preseed/file=/cdrom/preseed.cfg`. The isohybrid
  boot records are replayed by xorriso so the USB boots both firmware modes.
- **Single-disk assumption.** `partman/early_command` selects the first detected
  disk — correct for this box (one 512 GB NVMe). Revisit for multi-disk hosts.
- The build never touches the seed hardware; flashing + booting is the only
  physical step.
