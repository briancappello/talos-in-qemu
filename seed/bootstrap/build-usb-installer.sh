#!/usr/bin/env bash
# Build a headless, unattended Debian 13 (trixie) USB installer for the physical
# seed host. The result installs to the single NVMe over DHCP and comes up with
# root SSH (key-only), ready for `ansible-playbook --limit seed-metal`.
#
# Idempotent: re-run to rebuild. Nothing here touches the seed hardware — you dd
# the produced ISO to a USB stick yourself (instructions printed at the end).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ISO_NAME="debian-13.6.0-amd64-netinst.iso"
ISO_URL="https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/${ISO_NAME}"
ISO_SHA512="ce0eeee7b51fdcdbed1e5116668c1fee27e528767bdf488e5f115a67b225e5dfd0afca1d456aaa9408ceb6b8527521ff7b6b5d62fdbe6f8c5faaf8df56a96292"

SRC_ISO="${SRC_ISO:-$HOME/downloads/${ISO_NAME}}"
OUT_ISO="${OUT_ISO:-$HERE/seed-metal-autoinstall.iso}"
PUBKEY="${PUBKEY:-$HOME/.ssh/id_ed25519.pub}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

for t in curl xorriso sha512sum openssl python3; do
  command -v "$t" >/dev/null || { echo "MISSING tool: $t" >&2; exit 1; }
done
[ -r "$PUBKEY" ] || { echo "No SSH pubkey at $PUBKEY" >&2; exit 1; }

# 1. Fetch + verify the stock ISO.
if [ ! -f "$SRC_ISO" ]; then
  echo ">> downloading $ISO_NAME ..."
  mkdir -p "$(dirname "$SRC_ISO")"
  curl -fSL -o "$SRC_ISO" "$ISO_URL"
fi
echo ">> verifying SHA512 ..."
echo "${ISO_SHA512}  ${SRC_ISO}" | sha512sum -c -

# 2. Sanity-check the boot layout we rely on exists in the ISO.
for p in /install.amd/vmlinuz /install.amd/initrd.gz /isolinux/isolinux.cfg /boot/grub/grub.cfg; do
  xorriso -indev "$SRC_ISO" -find "$p" >/dev/null 2>&1 \
    || { echo "ISO is missing expected path $p" >&2; exit 1; }
done

# 3. Break-glass root password (random; only its hash goes on the media).
ROOT_PW="$(openssl rand -base64 12)"
ROOT_HASH="$(openssl passwd -6 "$ROOT_PW")"
PUB="$(cat "$PUBKEY")"

# 4. Render preseed (robust substitution — pubkey/hash contain regex-hostile chars).
python3 - "$HERE/preseed.cfg" "$ROOT_HASH" "$PUB" "$TMP/preseed.cfg" <<'PY'
import sys
src, h, pub, dst = sys.argv[1:5]
t = open(src).read().replace("__ROOT_PW_HASH__", h).replace("__SSH_PUBKEY__", pub)
open(dst, "w").write(t)
PY

# 5. Minimal bootloader configs that AUTO-BOOT the text installer headless
#    (bypass the interactive menu on both BIOS/isolinux and UEFI/grub).
cat > "$TMP/isolinux.cfg" <<'CFG'
default autoinstall
prompt 0
timeout 1
label autoinstall
  kernel /install.amd/vmlinuz
  append auto=true priority=critical preseed/file=/cdrom/preseed.cfg vga=788 initrd=/install.amd/initrd.gz --- quiet
CFG

cat > "$TMP/grub.cfg" <<'CFG'
set default=0
set timeout=1
menuentry "Seed autoinstall (headless)" {
    linux  /install.amd/vmlinuz auto=true priority=critical preseed/file=/cdrom/preseed.cfg vga=788 --- quiet
    initrd /install.amd/initrd.gz
}
CFG

# 6. Repack: copy the ISO, replay its isohybrid/El-Torito boot records (so the
#    USB still boots BIOS *and* UEFI), and overwrite only the 3 config files +
#    add preseed.cfg.
echo ">> building $OUT_ISO ..."
rm -f "$OUT_ISO"
xorriso -indev "$SRC_ISO" -outdev "$OUT_ISO" \
  -boot_image any replay \
  -map "$TMP/preseed.cfg"  /preseed.cfg \
  -map "$TMP/isolinux.cfg" /isolinux/isolinux.cfg \
  -map "$TMP/grub.cfg"     /boot/grub/grub.cfg \
  -commit

echo
echo "=================================================================="
echo " Built: $OUT_ISO"
echo " Break-glass root password (console only; SSH is key-only):"
echo "     $ROOT_PW"
echo " (store it or discard it — Ansible manages the real cred later.)"
echo
echo " Flash to USB (REPLACE sdX with your USB device — NOT an internal disk):"
echo "     lsblk -o NAME,SIZE,MODEL,TRAN"
echo "     sudo dd if=$OUT_ISO of=/dev/sdX bs=4M oflag=direct status=progress conv=fsync"
echo
echo " Then boot the seed from USB (one-time boot menu). It installs headless,"
echo " wipes the single NVMe, and reboots onto DHCP with root SSH."
echo " Find it and log in from here:"
echo "     for i in \$(seq 1 254); do ping -c1 -W1 192.168.1.\$i >/dev/null 2>&1 & done; wait"
echo "     ip neigh | grep -i 9c:6b:00:dc:c0:88     # -> its DHCP IP"
echo "     ssh root@<that-ip>"
echo "=================================================================="
